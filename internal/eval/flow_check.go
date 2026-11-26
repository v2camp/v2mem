package eval

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// G 类逐跳自动检查（docs/design-eval-flows.md §3.1）。
//
// 每条只回答「这一跳有没有被真实数据验证、有没有违例」，全部来自库表/审计账：
//   - 没有样本 = 失败（空库必须显式红，而不是静默通过）；
//   - 单点破坏只允许对应跳变红，其余跳不受影响；
//   - 判定在纯函数里，DB 只负责投影（flow_snapshot.go）。
//
// 口径改动必须同步 design-eval-flows.md 的 G 表；这是准入→准出验收的一部分。

const (
	// flowMaxAttempts 与 proposal.MaxAttempts 同步（内容类失败的隔离阈值）。
	// 为什么不在 eval 里 import proposal：本包要保持零业务依赖；
	// 同步关系由 test/flow 直接对 proposal.MaxAttempts 断言守卫。
	flowMaxAttempts = 5
	// flowDefaultWindow 是悬空候选的默认处理 SLA（design-data-lifecycle §5：15 天）。
	flowDefaultWindow = 15 * 24 * time.Hour
)

// FlowCheck 是单条 G 检查的结果。
type FlowCheck struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// FlowCheckReport 是 eval flow 的完整报告。
type FlowCheckReport struct {
	Checks        []FlowCheck `json:"checks"`
	Passed        int         `json:"passed"`
	Failed        int         `json:"failed"`
	ManualRate    float64     `json:"manual_rate"`
	WindowSeconds int64       `json:"window_seconds,omitempty"`
	Profile       string      `json:"profile"`
	LegacyExempt  int         `json:"legacy_exempt"`
}

// FlowOptions 是 eval flow 的剖面选项。
type FlowOptions struct {
	Profile    string // "ci"（默认，严格）| "prod"（legacy 豁免 + 无样本降级）
	AllowEmpty bool   // 仅 prod：把「无样本」降级为 PASS-with-note
}

// OK 表示全部 G 检查通过（cmd 据此决定退出码）。
func (r FlowCheckReport) OK() bool { return r.Failed == 0 }

// CheckFlow 对快照跑全部 13 条 G 检查（严格 ci 剖面）。window<=0 取 15 天 SLA。
func CheckFlow(s FlowSnapshot, window time.Duration) FlowCheckReport {
	return CheckFlowWithOptions(s, window, FlowOptions{})
}

// CheckFlowWithOptions 是带剖面的 CheckFlow；默认行为与 CheckFlow 完全一致。
//
// prod 剖面只做两件事：
//   - 把 legacy 候选（bundle_id 为空）从 G8/G9/G11/G12/G13 的输入中剔除，
//     把 legacy 运行账从 G7 的输入中剔除；
//   - AllowEmpty 时把「无样本」分支降级为 PASS-with-note。
//
// 当前行的真实违例仍照常判红：豁免只认结构性的 bundle_id 空值。
func CheckFlowWithOptions(s FlowSnapshot, window time.Duration, opts FlowOptions) FlowCheckReport {
	if window <= 0 {
		window = flowDefaultWindow
	}
	profile := "ci"
	if opts.Profile == "prod" {
		profile = "prod"
	}
	now := s.Now
	if now <= 0 {
		now = time.Now().Unix()
	}
	allowEmpty := profile == "prod" && opts.AllowEmpty

	filtered := s
	exemptCandidates, exemptRuns := 0, 0
	if profile == "prod" {
		filtered.Candidates = make([]FlowCandidate, 0, len(s.Candidates))
		for _, c := range s.Candidates {
			if IsLegacyCandidate(c) {
				exemptCandidates++
				continue
			}
			filtered.Candidates = append(filtered.Candidates, c)
		}
		filtered.Runs = make([]FlowRun, 0, len(s.Runs))
		for _, r := range s.Runs {
			if IsLegacyRun(r) {
				exemptRuns++
				continue
			}
			filtered.Runs = append(filtered.Runs, r)
		}
	}
	idx := newFlowIndex(filtered)
	checks := []FlowCheck{
		flowG1(filtered.Observations),
		flowG2(filtered.Observations, idx),
		flowG3(filtered.Archives, idx),
		flowG4(filtered.Archives, idx),
		flowG5(filtered.Bundles, idx),
		flowG6(filtered.Bundles, idx),
		flowG7(filtered.Runs, filtered.Bundles, idx, allowEmpty),
		flowG8(filtered.Candidates, now, window, allowEmpty),
		flowG9(filtered.Candidates, idx, allowEmpty),
		flowG10(filtered.Candidates, filtered.Decisions, idx),
		flowG11(filtered.Candidates, idx, allowEmpty),
		flowG12(filtered.Candidates, allowEmpty),
		flowG13(filtered.Candidates, idx, allowEmpty),
	}
	markLegacyExempt(checks, exemptCandidates, exemptRuns)
	rep := FlowCheckReport{Checks: checks, WindowSeconds: int64(window / time.Second),
		Profile: profile, LegacyExempt: exemptCandidates + exemptRuns}
	for _, c := range checks {
		if c.Pass {
			rep.Passed++
		} else {
			rep.Failed++
		}
	}
	manual, auto := 0, 0
	for _, d := range s.Decisions {
		if d.Actor == "human" {
			manual++
		} else {
			auto++
		}
	}
	rep.ManualRate = ManualRate(manual, auto)
	return rep
}

// flowIndex 是检查共用的反查表，避免每条检查各扫一遍全表。
type flowIndex struct {
	archiveByHash    map[string]FlowArchive
	obsByArchive     map[string][]FlowObservation
	archivesBySource map[string][]FlowArchive
	bundlesBySource  map[string][]FlowBundle
	bundleByID       map[string]FlowBundle
	sourceByKey      map[string]FlowSource
	runsByBundle     map[string][]FlowRun
	candByID         map[string]FlowCandidate
	decisionsByCand  map[string][]FlowDecision
	candEvByCand     map[string][]FlowCandidateEvidence
	candsByBundle    map[string][]FlowCandidate
	memEvSet         map[string]bool
	memSet           map[string]bool
}

func flowKey(source string, generation int) string {
	return fmt.Sprintf("%s#%d", source, generation)
}

func flowMemEvKey(memoryID, evidenceID string) string {
	return memoryID + "\x00" + evidenceID
}

func newFlowIndex(s FlowSnapshot) *flowIndex {
	idx := &flowIndex{
		archiveByHash:    make(map[string]FlowArchive, len(s.Archives)),
		obsByArchive:     make(map[string][]FlowObservation),
		archivesBySource: make(map[string][]FlowArchive),
		bundlesBySource:  make(map[string][]FlowBundle),
		bundleByID:       make(map[string]FlowBundle, len(s.Bundles)),
		sourceByKey:      make(map[string]FlowSource, len(s.Sources)),
		runsByBundle:     make(map[string][]FlowRun),
		candByID:         make(map[string]FlowCandidate, len(s.Candidates)),
		decisionsByCand:  make(map[string][]FlowDecision),
		candEvByCand:     make(map[string][]FlowCandidateEvidence),
		candsByBundle:    make(map[string][]FlowCandidate),
		memEvSet:         make(map[string]bool, len(s.MemoryEvidence)),
		memSet:           make(map[string]bool, len(s.Memories)),
	}
	for _, a := range s.Archives {
		idx.archiveByHash[a.Hash] = a
		k := flowKey(a.SourceID, a.Generation)
		idx.archivesBySource[k] = append(idx.archivesBySource[k], a)
	}
	for _, o := range s.Observations {
		idx.obsByArchive[o.ArchiveHash] = append(idx.obsByArchive[o.ArchiveHash], o)
	}
	for _, b := range s.Bundles {
		idx.bundleByID[b.ID] = b
		k := flowKey(b.SourceID, b.Generation)
		idx.bundlesBySource[k] = append(idx.bundlesBySource[k], b)
	}
	for _, src := range s.Sources {
		idx.sourceByKey[flowKey(src.ID, src.Generation)] = src
	}
	for _, r := range s.Runs {
		idx.runsByBundle[r.BundleID] = append(idx.runsByBundle[r.BundleID], r)
	}
	for _, c := range s.Candidates {
		idx.candByID[c.ID] = c
		idx.candsByBundle[c.BundleID] = append(idx.candsByBundle[c.BundleID], c)
	}
	for _, d := range s.Decisions {
		idx.decisionsByCand[d.CandidateID] = append(idx.decisionsByCand[d.CandidateID], d)
	}
	for _, ce := range s.CandidateEvidence {
		idx.candEvByCand[ce.CandidateID] = append(idx.candEvByCand[ce.CandidateID], ce)
	}
	for _, me := range s.MemoryEvidence {
		idx.memEvSet[flowMemEvKey(me.MemoryID, me.EvidenceID)] = true
	}
	for _, m := range s.Memories {
		idx.memSet[m.ID] = true
	}
	return idx
}

func flowPass(id, name, detail string) FlowCheck {
	return FlowCheck{ID: id, Name: name, Pass: true, Detail: detail}
}

func flowFail(id, name, detail string) FlowCheck {
	return FlowCheck{ID: id, Name: name, Pass: false, Detail: detail}
}

// markLegacyExempt 在受影响检查的 detail 末尾注明豁免数量，便于复算
// legacy_exempt（docs/design-eval-flows.md §3.1 的 prod 剖面）。
func markLegacyExempt(checks []FlowCheck, candidates, runs int) {
	for i := range checks {
		switch checks[i].ID {
		case "G7":
			if runs > 0 {
				checks[i].Detail += fmt.Sprintf("（legacy 豁免 %d）", runs)
			}
		case "G8", "G9", "G11", "G12", "G13":
			if candidates > 0 {
				checks[i].Detail += fmt.Sprintf("（legacy 豁免 %d）", candidates)
			}
		}
	}
}

// flowIDs 排序并截断点名，保证输出稳定、不刷屏。
func flowIDs(ids []string) string {
	if len(ids) == 0 {
		return "—"
	}
	sort.Strings(ids)
	if len(ids) > 3 {
		return strings.Join(ids[:3], "，") + fmt.Sprintf(" 等 %d 条", len(ids))
	}
	return strings.Join(ids, "，")
}

// G1 捕获幂等：同一 (source, generation, seq) 不得出现两行。
func flowG1(obs []FlowObservation) FlowCheck {
	const id, name = "G1", "捕获幂等"
	if len(obs) == 0 {
		return flowFail(id, name, "无观测：捕获→observations 未被验证")
	}
	seen := make(map[string]int, len(obs))
	for _, o := range obs {
		if o.Seq <= 0 {
			continue
		}
		seen[flowKey(o.SourceID, o.Generation)+fmt.Sprintf("/%d", o.Seq)]++
	}
	var dups []string
	for k, n := range seen {
		if n > 1 {
			dups = append(dups, k)
		}
	}
	if len(dups) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 条观测中重复身份 %d 组：%s",
			len(obs), len(dups), flowIDs(dups)))
	}
	return flowPass(id, name, fmt.Sprintf("%d 条观测，幂等键无重复", len(obs)))
}

// G2 观测→归档：非 legacy 观测必须有 archive_hash，且归档账存在。
func flowG2(obs []FlowObservation, idx *flowIndex) FlowCheck {
	const id, name = "G2", "观测归档"
	if len(obs) == 0 {
		return flowFail(id, name, "无观测：归档跳未被验证")
	}
	legacy := 0
	var missing []string
	for _, o := range obs {
		if o.Legacy == 1 && o.ArchiveHash == "" {
			legacy++
			continue
		}
		if _, ok := idx.archiveByHash[o.ArchiveHash]; o.ArchiveHash == "" || !ok {
			missing = append(missing, o.ID)
		}
	}
	if len(missing) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 条观测无归档账：%s", len(missing), flowIDs(missing)))
	}
	if len(obs) == legacy {
		return flowFail(id, name, "全部观测为 legacy 标记，无归档样本")
	}
	return flowPass(id, name, fmt.Sprintf("%d 条观测均有归档账（legacy 豁免 %d）", len(obs), legacy))
}

// G3 归档账本一致：raw_count 必须等于该归档实际覆盖的观测行数。
func flowG3(archives []FlowArchive, idx *flowIndex) FlowCheck {
	const id, name = "G3", "归档账本一致"
	if len(archives) == 0 {
		return flowFail(id, name, "无归档账：observations→归档 未被验证")
	}
	var bad []string
	for _, a := range archives {
		if n := len(idx.obsByArchive[a.Hash]); n != a.RawCount {
			bad = append(bad, fmt.Sprintf("%s(raw_count=%d,obs=%d)", a.Hash, a.RawCount, n))
		}
	}
	if len(bad) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 个归档计数不符：%s", len(bad), flowIDs(bad)))
	}
	return flowPass(id, name, fmt.Sprintf("%d 个归档的 raw_count 与观测数一致", len(archives)))
}

// G4 归档序号连续：同源同代归档 from/to 首尾相接，且与观测序号边界一致。
func flowG4(archives []FlowArchive, idx *flowIndex) FlowCheck {
	const id, name = "G4", "归档序号连续"
	if len(archives) == 0 {
		return flowFail(id, name, "无归档账：序号连续性未被验证")
	}
	var bad []string
	for k, list := range idx.archivesBySource {
		sort.Slice(list, func(i, j int) bool { return list[i].FromSeq < list[j].FromSeq })
		for i := 1; i < len(list); i++ {
			if list[i].FromSeq != list[i-1].ToSeq+1 {
				bad = append(bad, fmt.Sprintf("%s 序号洞 %d→%d", k, list[i-1].ToSeq, list[i].FromSeq))
			}
		}
		for _, a := range list {
			obs := idx.obsByArchive[a.Hash]
			if len(obs) == 0 {
				continue
			}
			min, max := obs[0].Seq, obs[0].Seq
			for _, o := range obs[1:] {
				if o.Seq < min {
					min = o.Seq
				}
				if o.Seq > max {
					max = o.Seq
				}
			}
			if min != a.FromSeq || max != a.ToSeq {
				bad = append(bad, fmt.Sprintf("%s 边界 [%d,%d]≠观测[%d,%d]",
					a.Hash, a.FromSeq, a.ToSeq, min, max))
			}
		}
	}
	if len(bad) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 处归档不连续：%s", len(bad), flowIDs(bad)))
	}
	return flowPass(id, name, fmt.Sprintf("%d 段归档首尾相接", len(archives)))
}

// G5 Bundle 连续覆盖：从 0 开始、窗口首尾相接，且最后一个窗口覆盖已消费水位。
func flowG5(bundles []FlowBundle, idx *flowIndex) FlowCheck {
	const id, name = "G5", "Bundle 连续覆盖"
	if len(bundles) == 0 {
		return flowFail(id, name, "无 Bundle：归档→Bundle 未被验证")
	}
	var bad []string
	for k, list := range idx.bundlesBySource {
		sort.Slice(list, func(i, j int) bool { return list[i].FromSeqExclusive < list[j].FromSeqExclusive })
		if list[0].FromSeqExclusive != 0 {
			bad = append(bad, fmt.Sprintf("%s 首窗从 %d 开始", k, list[0].FromSeqExclusive))
		}
		for i := 1; i < len(list); i++ {
			if list[i].FromSeqExclusive != list[i-1].ToSeqInclusive {
				bad = append(bad, fmt.Sprintf("%s 窗口断档 %d→%d",
					k, list[i-1].ToSeqInclusive, list[i].FromSeqExclusive))
			}
		}
		last := list[len(list)-1].ToSeqInclusive
		if src, ok := idx.sourceByKey[k]; ok && last != src.CursorSeq {
			bad = append(bad, fmt.Sprintf("%s cursor=%d 但末窗到 %d", k, src.CursorSeq, last))
		}
	}
	if len(bad) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 处 Bundle 不连续：%s", len(bad), flowIDs(bad)))
	}
	return flowPass(id, name, fmt.Sprintf("%d 个 Bundle 连续覆盖 cursor", len(bundles)))
}

// G6 noop 是合法空结果：必须有 noop 运行账、attempt≥1、且该窗口零候选。
func flowG6(bundles []FlowBundle, idx *flowIndex) FlowCheck {
	const id, name = "G6", "noop 水位"
	var noops []FlowBundle
	for _, b := range bundles {
		if b.Status == "noop" {
			noops = append(noops, b)
		}
	}
	if len(noops) == 0 {
		return flowFail(id, name, "无 noop 窗口样本：0 提案水位未被验证")
	}
	var bad []string
	for _, b := range noops {
		if b.Attempt < 1 {
			bad = append(bad, b.ID+" attempt=0")
		}
		hasRun := false
		for _, r := range idx.runsByBundle[b.ID] {
			if r.Status == "noop" || r.Status == "empty" {
				hasRun = true
			}
		}
		if !hasRun {
			bad = append(bad, b.ID+" 缺 noop 运行账")
		}
		if n := len(idx.candsByBundle[b.ID]); n != 0 {
			bad = append(bad, fmt.Sprintf("%s noop 却落 %d 条候选", b.ID, n))
		}
	}
	if len(bad) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 个 noop 窗口不合法：%s", len(bad), flowIDs(bad)))
	}
	return flowPass(id, name, fmt.Sprintf("%d 个 noop 窗口均为合法空结果", len(noops)))
}

// G7 失败不冒充 noop：失败运行必须被重试（attempt 递增）或按上限隔离。
func flowG7(runs []FlowRun, bundles []FlowBundle, idx *flowIndex, allowEmpty bool) FlowCheck {
	const id, name = "G7", "失败有重试账"
	var failedRuns []FlowRun
	for _, r := range runs {
		switch r.Status {
		case "fail", "invalid", "partial", "backlog":
			failedRuns = append(failedRuns, r)
		}
	}
	var openBad, quarantined []FlowBundle
	for _, b := range bundles {
		switch b.Status {
		case "fail", "partial", "backlog":
			openBad = append(openBad, b)
		case "quarantined":
			quarantined = append(quarantined, b)
		}
	}
	if len(failedRuns) == 0 && len(openBad) == 0 && len(quarantined) == 0 {
		if allowEmpty {
			return flowPass(id, name, "无失败窗口样本：prod 剖面按无样本降级")
		}
		return flowFail(id, name, "无失败窗口样本：失败不算 noop 未被验证")
	}
	var bad []string
	for _, r := range failedRuns {
		b, ok := idx.bundleByID[r.BundleID]
		if !ok {
			bad = append(bad, r.ID+" 无对应 Bundle")
			continue
		}
		if r.Attempt < 1 {
			bad = append(bad, r.ID+" attempt=0")
		}
		if b.Attempt <= r.Attempt && b.Status != "quarantined" {
			bad = append(bad, fmt.Sprintf("%s 失败后未重试（bundle attempt=%d）", r.ID, b.Attempt))
		}
	}
	for _, b := range openBad {
		if b.Attempt < 1 || b.LastRunID == "" {
			bad = append(bad, fmt.Sprintf("%s 失败无运行账（attempt=%d）", b.ID, b.Attempt))
		}
	}
	for _, b := range quarantined {
		if b.Attempt < flowMaxAttempts {
			bad = append(bad, fmt.Sprintf("%s 未达 %d 次即隔离", b.ID, flowMaxAttempts))
		}
	}
	if len(bad) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 处失败处理不合法：%s", len(bad), flowIDs(bad)))
	}
	return flowPass(id, name, "失败窗口均已重试或按上限隔离")
}

// G8 候选无悬空：超过处理 SLA 仍 pending 即为悬空。
func flowG8(cands []FlowCandidate, now int64, window time.Duration, allowEmpty bool) FlowCheck {
	const id, name = "G8", "无悬空候选"
	if len(cands) == 0 {
		if allowEmpty {
			return flowPass(id, name, "无候选：prod 剖面按无样本降级")
		}
		return flowFail(id, name, "无候选：候选→裁决 未被验证")
	}
	threshold := now - int64(window/time.Second)
	var dangling []string
	for _, c := range cands {
		if c.Status == "pending" && c.CreatedAt < threshold {
			dangling = append(dangling, c.ID)
		}
	}
	if len(dangling) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 条 pending 超过 %s：%s",
			len(dangling), window, flowIDs(dangling)))
	}
	return flowPass(id, name, fmt.Sprintf("%d 条候选无超窗悬空", len(cands)))
}

// G9 裁决闭环：终态候选必须有匹配裁决；expired 不能顶替消费。
//
// S10a 起两处口径变化（docs/design-eval-flows.md §3.1）：
//   - failed / quarantined / withdrawn 不是裁决驱动的状态：failed 在重试队列里，
//     quarantined/withdrawn 是明确终态。要求它们也配 accept/reject 裁决，只会逼实现
//     为了过闸去伪造一条裁决。
//   - pending 的「已有终态裁决」只看**最近一次 reopen 之后**：`mem batch --rollback`
//     把候选退回 pending 时历史裁决合法地留在账上，旧口径会把每个回滚过的候选判红。
func flowG9(cands []FlowCandidate, idx *flowIndex, allowEmpty bool) FlowCheck {
	const id, name = "G9", "候选裁决闭环"
	if len(cands) == 0 {
		if allowEmpty {
			return flowPass(id, name, "无候选：prod 剖面按无样本降级")
		}
		return flowFail(id, name, "无候选：裁决矩阵未被验证")
	}
	var bad []string
	for _, c := range cands {
		ds := idx.decisionsByCand[c.ID]
		switch c.Status {
		case "accepted":
			if !flowHasAction(ds, "auto_commit", "human_accept") {
				bad = append(bad, c.ID+" accepted 无采纳裁决")
			}
		case "rejected":
			if !flowHasAction(ds, "reject") {
				bad = append(bad, c.ID+" rejected 无拒绝裁决")
			}
		case "expired":
			if len(ds) == 0 {
				bad = append(bad, c.ID+" expired 无裁决（顶替消费）")
			}
		case "failed", "quarantined", "withdrawn":
			// 非裁决驱动的中间/终态：不要求匹配裁决，合法性由 G12/G7 与状态迁移守。
		case "pending":
			if flowHasAction(flowDecisionsSinceReopen(ds), "auto_commit", "human_accept", "reject") {
				bad = append(bad, c.ID+" pending 已有终态裁决")
			}
		default:
			bad = append(bad, c.ID+" 状态非法:"+c.Status)
		}
	}
	if len(bad) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 条候选与裁决不一致：%s", len(bad), flowIDs(bad)))
	}
	return flowPass(id, name, fmt.Sprintf("%d 条候选均有匹配裁决", len(cands)))
}

// flowDecisionsSinceReopen 只保留最近一次 reopen（含）之后的裁决。
//
// 没有 reopen 时返回原切片：绝大多数候选从没被回滚过，不必复制。
// 用 revision 而不是时间：裁决账的时间只到秒，回滚与前一次裁决常在同一个秒里。
func flowDecisionsSinceReopen(ds []FlowDecision) []FlowDecision {
	reopenRev, reopened := 0, false
	for _, d := range ds {
		if d.Action == "reopen" && (!reopened || d.Revision >= reopenRev) {
			reopenRev, reopened = d.Revision, true
		}
	}
	if !reopened {
		return ds
	}
	out := make([]FlowDecision, 0, len(ds))
	for _, d := range ds {
		if d.Revision >= reopenRev {
			out = append(out, d)
		}
	}
	return out
}

func flowHasAction(ds []FlowDecision, actions ...string) bool {
	for _, d := range ds {
		for _, a := range actions {
			if d.Action == a {
				return true
			}
		}
	}
	return false
}

// G10 裁决幂等：同一候选同一 revision 同一动作只能落一条；幂等键不得重复。
func flowG10(cands []FlowCandidate, decisions []FlowDecision, idx *flowIndex) FlowCheck {
	const id, name = "G10", "裁决幂等"
	if len(decisions) == 0 {
		return flowFail(id, name, "无裁决账：幂等键未被验证")
	}
	var bad []string
	seen := make(map[string]int, len(decisions))
	keys := make(map[string]int, len(decisions))
	for _, d := range decisions {
		seen[fmt.Sprintf("%s/%d/%s", d.CandidateID, d.Revision, d.Action)]++
		keys[d.IdempotencyKey]++
		if c, ok := idx.candByID[d.CandidateID]; ok && d.Revision > c.Revision {
			bad = append(bad, fmt.Sprintf("%s revision=%d>%d", d.CandidateID, d.Revision, c.Revision))
		}
	}
	for k, n := range seen {
		if n > 1 {
			bad = append(bad, "重复裁决 "+k)
		}
	}
	for k, n := range keys {
		if n > 1 {
			bad = append(bad, "重复幂等键 "+k)
		}
	}
	if len(bad) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 处裁决不幂等：%s", len(bad), flowIDs(bad)))
	}
	return flowPass(id, name, fmt.Sprintf("%d 条裁决幂等键唯一", len(decisions)))
}

// G11 记忆绑定：采纳必须有记忆、裁决 memory_id 一致、证据链覆盖完整。
func flowG11(cands []FlowCandidate, idx *flowIndex, allowEmpty bool) FlowCheck {
	const id, name = "G11", "记忆证据绑定"
	var accepted int
	var bad []string
	withEvidence := 0
	for _, c := range cands {
		if c.Status != "accepted" {
			continue
		}
		accepted++
		if c.MemoryID == "" || !idx.memSet[c.MemoryID] {
			bad = append(bad, c.ID+" 记忆不存在")
			continue
		}
		for _, d := range idx.decisionsByCand[c.ID] {
			if d.MemoryID != "" && d.MemoryID != c.MemoryID {
				bad = append(bad, c.ID+" 裁决 memory_id 不一致")
			}
		}
		ce := idx.candEvByCand[c.ID]
		if len(ce) > 0 {
			withEvidence++
			for _, ev := range ce {
				if !idx.memEvSet[flowMemEvKey(c.MemoryID, ev.EvidenceID)] {
					bad = append(bad, c.ID+" 证据未绑定 "+ev.EvidenceID)
				}
			}
		}
	}
	if accepted == 0 {
		if allowEmpty {
			return flowPass(id, name, "无已采纳候选：prod 剖面按无样本降级")
		}
		return flowFail(id, name, "无已采纳候选：裁决→记忆 未被验证")
	}
	if withEvidence == 0 {
		if allowEmpty {
			return flowPass(id, name, "已采纳候选均无证据绑定样本：prod 剖面按无样本降级")
		}
		return flowFail(id, name, "已采纳候选均无证据绑定样本")
	}
	if len(bad) > 0 {
		return flowFail(id, name, fmt.Sprintf("%d 处绑定不完整：%s", len(bad), flowIDs(bad)))
	}
	return flowPass(id, name, fmt.Sprintf("%d 条记忆绑定完整（%d 条带证据）", accepted, withEvidence))
}

// G12 准出率账本：逐 Bundle 窗口「终态数 = 准入数」，复用 S0 的 Reconcile/Pass。
//
// 终态集合 = accepted / rejected / expired / quarantined / withdrawn。
// expired 计终态（它确实到了显式状态）；合法性由 G9 守，不在这里重复判。
// failed **不**计终态：它还在重试队列里，算成终态会让准出率虚高——这正是「失败不算 noop」
// 在准出率上的同一句话。
func flowG12(cands []FlowCandidate, allowEmpty bool) FlowCheck {
	const id, name = "G12", "准出率对账"
	if len(cands) == 0 {
		if allowEmpty {
			return flowPass(id, name, "无候选：prod 剖面按无样本降级")
		}
		return flowFail(id, name, "无候选：准出率未被验证")
	}
	window := make(map[string]*FlowLedger)
	for _, c := range cands {
		w := c.BundleID
		if w == "" {
			w = "unbundled"
		}
		l := window[w]
		if l == nil {
			l = &FlowLedger{WindowID: w}
			window[w] = l
		}
		l.Admitted++
		switch c.Status {
		case "accepted", "rejected", "expired", "quarantined", "withdrawn":
			l.Terminal++
		}
	}
	ids := make([]string, 0, len(window))
	for k := range window {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	ledgers := make([]FlowLedger, 0, len(ids))
	for _, k := range ids {
		ledgers = append(ledgers, *window[k])
	}
	rep := Reconcile(ledgers)
	if !rep.Pass() {
		return flowFail(id, name, fmt.Sprintf("准出率 %.0f%%，悬空 %d（窗口 %s）",
			rep.ExitRate*100, rep.Dangling, flowIDs(rep.DanglingWindows)))
	}
	return flowPass(id, name, fmt.Sprintf("准出率 100%%，%d 个窗口全量对账", len(ledgers)))
}

// G13 重复提交与严密度：同一会话同一内容不得重复落库；采纳不得无证据强写。
func flowG13(cands []FlowCandidate, idx *flowIndex, allowEmpty bool) FlowCheck {
	const id, name = "G13", "重复与严密度"
	if len(cands) == 0 {
		if allowEmpty {
			return flowPass(id, name, "无候选：prod 剖面按无样本降级")
		}
		return flowFail(id, name, "无候选：重复率未被验证")
	}
	dup := make(map[string]int)
	forced := 0
	for _, c := range cands {
		if c.Status == "pending" || c.Status == "accepted" {
			dup[c.SessionID+"\x00"+c.ContentHash]++
		}
		if c.Status == "accepted" && len(idx.candEvByCand[c.ID]) == 0 {
			forced++
		}
	}
	dupGroups := 0
	for _, n := range dup {
		if n > 1 {
			dupGroups++
		}
	}
	rep := Reconcile([]FlowLedger{{WindowID: "all", Duplicates: dupGroups, Forced: forced}})
	if !rep.Pass() {
		return flowFail(id, name, fmt.Sprintf("重复组 %d，无证据强写 %d", dupGroups, forced))
	}
	return flowPass(id, name, "无重复提交、无严密度违例")
}
