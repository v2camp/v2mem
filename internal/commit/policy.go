package commit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 本文件实现 T3 的确定性提交矩阵。
//
// 为什么矩阵必须是纯函数：同一候选在自动调度（T5）与人工命令（mem accept）
// 两条路径上会被反复裁决，任何依赖时钟/随机/库状态的判断都会让「昨天能自动提交、
// 今天不能」变成不可复现的行为。时钟只用于 CreatedAt，且不参与动作判定。

// Role 是证据在候选论证中的角色，来自 candidate_evidence.evidence_role。
//
// 角色与 Grade 正交：Grade 回答「这条证据有多可信」，Role 回答「它在论证里顶什么用」。
// pitfall 必须同时具备 error 与 resolution 两种角色，缺一不可——只有报错没有解法，
// 记下来的是噪音而不是可复用的教训。
type Role string

const (
	RoleDirect     Role = "direct"
	RoleError      Role = "error"
	RoleResolution Role = "resolution"
	RoleCompletion Role = "completion"
	RoleContext    Role = "context"
)

// Valid 报告角色是否属于已知集合。未知角色按 context 处理（不能支撑自动提交）。
func (r Role) Valid() bool {
	switch r {
	case RoleDirect, RoleError, RoleResolution, RoleCompletion, RoleContext:
		return true
	}
	return false
}

// Item 是带角色的单条证据，供矩阵做角色级判断。
//
// 冻结契约 commit.Link 只有 Grade 没有 Role：Link 是「记忆 → 原始证据」的
// 持久化形态，角色只存在于候选阶段。直接在 Link 上加字段会改动冻结接口，
// 因此用 Item 包一层，由 Store 在提交时从 candidate_evidence 读出角色。
type Item struct {
	EvidenceID string
	Link       Link
	Role       Role
}

// CanonicalRole 把空/未知角色收成 context。
func CanonicalRole(s string) Role {
	r := Role(strings.TrimSpace(strings.ToLower(s)))
	if r.Valid() {
		return r
	}
	return RoleContext
}

// 原因码。它们是决策的可解释输出，也是审计与排障的稳定键（不要改字面值）。
const (
	ReasonAutoEligible          = "auto_eligible"
	ReasonManualKind            = "manual_kind"
	ReasonGlobalScope           = "global_scope"
	ReasonCrossProjectScope     = "cross_project_scope"
	ReasonConflict              = "conflict"
	ReasonMissingEvidence       = "missing_evidence"
	ReasonGradeBelowA           = "grade_below_a"
	ReasonMissingDirectEvidence = "missing_direct_evidence"
	ReasonPitfallNoError        = "pitfall_missing_error"
	ReasonPitfallNoResolution   = "pitfall_missing_resolution"
	ReasonPitfallWeakEvidence   = "pitfall_weak_evidence"
	ReasonTaskStateInvalid      = "task_state_invalid"
	ReasonProjectMissing        = "project_missing"
	ReasonUnknownKind           = "unknown_kind"
)

// 决策者标识。自动矩阵与人工命令必须可区分，否则审计无法回答
// 「这条记忆是机器放的还是人放的」。
const (
	ActorAuto  = "auto"
	ActorHuman = "human"
)

// taskStates 是 task 类候选允许的明确状态。
//
// 只认 done/blocked/next 三个字面值：模糊状态（"进行中"/"大概完成"）无法判定
// 任务是否收尾，放行只会把半成品写进长期记忆。
var taskStates = map[string]bool{
	"done":    true,
	"blocked": true,
	"next":    true,
}

// manualKinds 一律人工。这些类型的共同点是「判断依赖人」：
// decision/preference/rule 是价值取向，机器没有裁决权。
var manualKinds = map[string]bool{
	"decision":   true,
	"preference": true,
	"rule":       true,
}

// Matrix 是默认提交策略。零值可用。
type Matrix struct{}

// Decide 实现冻结的 Policy 接口。没有角色信息的调用方按 direct 处理：
// 这样 fact 的「A 级直接证据」判据仍然成立，而 pitfall 会因缺少 error/resolution
// 角色退回人工，不会误自动提交。
func (m Matrix) Decide(candidate Candidate, evidence []Link) (Decision, error) {
	items := make([]Item, len(evidence))
	for i, l := range evidence {
		items[i] = Item{EvidenceID: l.EventUID, Link: l, Role: RoleDirect}
	}
	return m.DecideItems(candidate, items)
}

// DecideItems 是带角色输入的完整裁决。
func (m Matrix) DecideItems(candidate Candidate, evidence []Item) (Decision, error) {
	if strings.TrimSpace(candidate.ID) == "" {
		return Decision{}, errors.New("候选 id 为空，无法裁决")
	}
	action, reasons := m.judge(candidate, evidence)
	hash := HashEvidence(evidence)
	return Decision{
		CandidateID:    candidate.ID,
		Revision:       candidate.Revision,
		EvidenceHash:   hash,
		Action:         action,
		Actor:          ActorAuto,
		ReasonCodes:    reasons,
		IdempotencyKey: IdempotencyKey(candidate.ID, candidate.Revision, action, hash),
		CreatedAt:      time.Now().Unix(),
	}, nil
}

func (m Matrix) judge(candidate Candidate, evidence []Item) (Action, []string) {
	// 冲突优先于一切：同一 subject 已有互斥结论时，自动写任何一条都会让库自相矛盾。
	if isConflict(candidate) {
		return ActionConflict, []string{ReasonConflict}
	}
	if manualKinds[strings.ToLower(strings.TrimSpace(candidate.Kind))] {
		return ActionHumanAccept, []string{ReasonManualKind}
	}
	switch strings.ToLower(strings.TrimSpace(candidate.Scope)) {
	case "global":
		return ActionHumanAccept, []string{ReasonGlobalScope}
	case "cross_project":
		return ActionHumanAccept, []string{ReasonCrossProjectScope}
	}

	switch strings.ToLower(strings.TrimSpace(candidate.Kind)) {
	case "fact":
		if strings.TrimSpace(candidate.Project) == "" {
			// 无工程归属的 fact 是全局知识，按矩阵一律人工。
			return ActionHumanAccept, []string{ReasonProjectMissing}
		}
		if !hasDirectA(evidence) {
			if len(evidence) == 0 {
				return ActionDefer, []string{ReasonMissingEvidence}
			}
			if !hasRole(evidence, RoleDirect) {
				return ActionDefer, []string{ReasonMissingDirectEvidence}
			}
			return ActionDefer, []string{ReasonGradeBelowA}
		}
		return ActionAutoCommit, []string{ReasonAutoEligible}

	case "pitfall":
		hasError := hasRole(evidence, RoleError)
		hasResolution := hasRole(evidence, RoleResolution)
		switch {
		case !hasError && !hasResolution:
			return ActionDefer, []string{ReasonPitfallNoError, ReasonPitfallNoResolution}
		case !hasError:
			return ActionDefer, []string{ReasonPitfallNoError}
		case !hasResolution:
			return ActionDefer, []string{ReasonPitfallNoResolution}
		case !rolesAtLeastB(evidence, RoleError) || !rolesAtLeastB(evidence, RoleResolution):
			return ActionDefer, []string{ReasonPitfallWeakEvidence}
		}
		return ActionAutoCommit, []string{ReasonAutoEligible}

	case "task":
		state := strings.ToLower(strings.TrimSpace(candidate.TaskState))
		if !taskStates[state] {
			return ActionDefer, []string{ReasonTaskStateInvalid}
		}
		return ActionAutoCommit, []string{ReasonAutoEligible}
	}

	return ActionHumanAccept, []string{ReasonManualKind, ReasonUnknownKind}
}

func isConflict(c Candidate) bool {
	kind := strings.ToLower(strings.TrimSpace(c.Kind))
	scope := strings.ToLower(strings.TrimSpace(c.Scope))
	review := strings.ToLower(strings.TrimSpace(c.ReviewClass))
	return kind == "conflict" || scope == "conflict" || review == "conflict"
}

// hasDirectA 要求「角色是 direct」且「等级 A」。
//
// Grade A 的定义由 Store 给出：既拿到 canonical event_uid，又落到 archive
// segment。只有摘要、没有原始字节的证据是 B/C，不能单独支撑自动提交。
func hasDirectA(items []Item) bool {
	for _, it := range items {
		if it.Role == RoleDirect && it.Link.Grade == GradeA && strings.TrimSpace(it.Link.EventUID) != "" {
			return true
		}
	}
	return false
}

func hasRole(items []Item, role Role) bool {
	for _, it := range items {
		if it.Role == role {
			return true
		}
	}
	return false
}

// rolesAtLeastB 要求该角色的证据至少是 B 级（有身份，不只是裸摘要）。
func rolesAtLeastB(items []Item, role Role) bool {
	for _, it := range items {
		if it.Role == role && (it.Link.Grade == GradeA || it.Link.Grade == GradeB) {
			return true
		}
	}
	return false
}

// HashEvidence 把证据集合收成稳定摘要，供提交幂等键使用。
//
// 排序键覆盖所有会改变证据语义的字段：同一批证据换一个顺序仍然是同一件事，
// 但换一条证据、换角色、换等级都必须得到不同的哈希，否则两次不同裁决会撞幂等键，
// 第二次被误判成重放而不落库。
func HashEvidence(items []Item) string {
	if len(items) == 0 {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:])
	}
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Link.EventUID != b.Link.EventUID {
			return a.Link.EventUID < b.Link.EventUID
		}
		if a.Link.ArchiveHash != b.Link.ArchiveHash {
			return a.Link.ArchiveHash < b.Link.ArchiveHash
		}
		if a.Link.Seq != b.Link.Seq {
			return a.Link.Seq < b.Link.Seq
		}
		if a.EvidenceID != b.EvidenceID {
			return a.EvidenceID < b.EvidenceID
		}
		return a.Role < b.Role
	})
	h := sha256.New()
	for _, it := range sorted {
		fmt.Fprintf(h, "%s|%s|%s|%s|%s|%d\n",
			it.Link.EventUID, it.EvidenceID, it.Link.ArchiveHash,
			it.Link.Grade, it.Role, it.Link.Seq)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IdempotencyKey 是 candidate + revision + action + evidence_hash 的定长摘要。
//
// 为什么把 revision 也放进去：同一候选被人工改过（内容/证据变化）后 revision 会推进，
// 新的裁决必须能落一条新 decision，而不是被旧键挡成重放。
func IdempotencyKey(candidateID string, revision int, action Action, evidenceHash string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		candidateID, strconv.Itoa(revision), string(action), evidenceHash,
	}, "|")))
	return hex.EncodeToString(sum[:])
}

// NormalizeAction 把空动作收成显式值，供调用方构造 Decision 时兜底。
func NormalizeAction(a Action) Action {
	if a == "" {
		return ActionHumanAccept
	}
	return a
}

// SortLinks 按证据发生顺序排序，读侧输出与 provenance 生成共用同一顺序。
func SortLinks(links []Link) {
	sort.SliceStable(links, func(i, j int) bool {
		if links[i].OccurredAtNS != links[j].OccurredAtNS {
			return links[i].OccurredAtNS < links[j].OccurredAtNS
		}
		if links[i].Seq != links[j].Seq {
			return links[i].Seq < links[j].Seq
		}
		if links[i].EventUID != links[j].EventUID {
			return links[i].EventUID < links[j].EventUID
		}
		return links[i].MemoryID < links[j].MemoryID
	})
}
