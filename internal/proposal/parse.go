package proposal

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/scrub"
)

// ErrBadOutput 表示模型回话不是约定的 JSON。它与「模型没跑」必须分开：
// 前者要重试一次修复提示，后者应落到更下一级 executor（Local/backlog）。
var ErrBadOutput = errors.New("Proposal 输出不是约定的 JSON")

// Status* 是执行器与 Bundle 的完成语义。
//
//   - ok：产出了合法候选（可能部分落库失败，由 partial 覆盖）
//   - noop：合法空结果（模型明确说没有事实），消费窗口
//   - partial：部分候选无效或部分落库失败，不消费窗口
//   - invalid：输出坏 JSON 且一次 repair 后仍不可解析，不消费窗口
//   - backlog：没有任何 provider 可用，不消费窗口
//   - fail：provider 跑了但失败（超时/HTTP 错误/落库全败），不消费窗口
//   - would_generate：dry-run 专用，只报告会生成什么，不写任何状态
const (
	StatusOK            = "ok"
	StatusNoop          = "noop"
	StatusPartial       = "partial"
	StatusInvalid       = "invalid"
	StatusBacklog       = "backlog"
	StatusFail          = "fail"
	StatusWouldGenerate = "would_generate"
)

// wireDraft 是模型的输出契约。EvidenceIdx 是 Bundle 内的 1 起短序号，
// 模型永远不需要（也不允许）回填 UUID。
type wireDraft struct {
	Kind        string `json:"kind"`
	Scope       string `json:"scope"`
	Content     string `json:"content"`
	Rationale   string `json:"rationale"`
	SubjectKey  string `json:"subject_key"`
	TaskState   string `json:"task_state"`
	EvidenceIdx []int  `json:"evidence_idx"`
}

const maxRawForRepair = 2000

// BuildPrompt 把 Bundle 内可提案的证据渲染成带序号的提示词。
//
// 序号使用 Bundle 内位置（1 起），不可提案的证据保留空位，绝不重排——
// 一旦重排，模型返回的 evidence_idx 就会指向另一条证据。
func BuildPrompt(refs []evidence.Ref) string {
	var b strings.Builder
	b.WriteString("从下面的会话证据提炼长期记忆。只允许新增（ADD-only），不要改写或删除已有记忆。\n")
	b.WriteString("输出一行一个 JSON，不要任何解释；没有可提炼的事实就输出 []。\n")
	b.WriteString(`字段：{"kind":"decision|pitfall|preference|task","scope":"session|project|global","content":"原子事实","rationale":"一句话","subject_key":"可选","task_state":"可选","evidence_idx":[1]}` + "\n")
	b.WriteString("evidence_idx 必须是下面证据的序号，且每条候选至少引用一条。\n\n")
	for i, r := range refs {
		idx := i + 1
		if !r.ProposalEligible {
			continue
		}
		fmt.Fprintf(&b, "[%d] (%s/%s) %s\n", idx, r.EventType, r.SessionID,
			scrub.Text(oneLine(r.Summary)))
	}
	return b.String()
}

// RepairPrompt 在首次解析失败后追加一次有界修复提示。
//
// 只重试一次：模型如果第二次仍然说了非 JSON，说明这个 provider 不适合
// 结构化输出，再问下去只会把同一个错误重复成延迟。
func RepairPrompt(prompt, previous string) string {
	prev := strings.TrimSpace(previous)
	if len(prev) > maxRawForRepair {
		prev = prev[:maxRawForRepair]
	}
	return prompt + "\n上次输出无法解析为约定 JSON。只输出 JSON 数组或每行一个 JSON，不要解释。\n上次输出：\n" + scrub.Text(prev) + "\n"
}

// parseResult 是一次输出解析的账目。err 只表示「完全不可解析」，
// 部分行坏掉记在 Invalid 里，交给 partial 表达。
type parseResult struct {
	Drafts  []Draft
	Invalid int
	// 解析失败的原始行数，用于区分「没事实」（0）与「说了但不像 JSON」。
	BadLines int
}

// ParseDrafts 解析模型输出并把每条草案校验到 Bundle 证据上。
//
// 三种结局：
//   - 合法且有内容 → (drafts/partial, nil)
//   - 合法但为空（空输出、[]）→ (0,0,nil)
//   - 完全不可解析 → ErrBadOutput
func ParseDrafts(raw string, evidenceCount int) (parseResult, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return parseResult{}, nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []wireDraft
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return parseResult{}, fmt.Errorf("%w: JSON 数组不合规: %v", ErrBadOutput, err)
		}
		if len(arr) == 0 {
			return parseResult{}, nil
		}
		return validateAll(arr, evidenceCount)
	}

	var out parseResult
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		start := strings.Index(line, "{")
		end := strings.LastIndex(line, "}")
		if start < 0 || end <= start {
			out.BadLines++
			out.Invalid++
			continue
		}
		var w wireDraft
		if err := json.Unmarshal([]byte(line[start:end+1]), &w); err != nil {
			out.BadLines++
			out.Invalid++
			continue
		}
		if d, ok := validateDraft(w, evidenceCount); ok {
			out.Drafts = append(out.Drafts, d)
		} else {
			out.Invalid++
		}
	}
	if len(out.Drafts) == 0 && out.Invalid == 0 {
		// 只有空行：等同合法空输出。
		return parseResult{}, nil
	}
	if len(out.Drafts) == 0 && out.BadLines > 0 {
		return parseResult{}, fmt.Errorf("%w: 响应里没有可解析的 JSON 行", ErrBadOutput)
	}
	return out, nil
}

func validateAll(arr []wireDraft, evidenceCount int) (parseResult, error) {
	var out parseResult
	for _, w := range arr {
		if d, ok := validateDraft(w, evidenceCount); ok {
			out.Drafts = append(out.Drafts, d)
		} else {
			out.Invalid++
		}
	}
	return out, nil
}

// validateDraft 做候选级校验。任何一条不满足都整条丢弃：
// 半条候选（比如没有证据引用）比没有候选更危险，因为它会被当成有来源的事实。
func validateDraft(w wireDraft, evidenceCount int) (Draft, bool) {
	kind := strings.ToLower(strings.TrimSpace(w.Kind))
	switch kind {
	case "preference", "decision", "pitfall", "task", "fact":
	default:
		return Draft{}, false
	}
	content := strings.TrimSpace(w.Content)
	if content == "" {
		return Draft{}, false
	}
	scope := strings.ToLower(strings.TrimSpace(w.Scope))
	switch scope {
	case "":
		scope = "project"
	case "session", "project", "global":
	default:
		return Draft{}, false
	}
	if len(w.EvidenceIdx) == 0 {
		return Draft{}, false
	}
	seen := make(map[int]bool, len(w.EvidenceIdx))
	idx := make([]int, 0, len(w.EvidenceIdx))
	for _, n := range w.EvidenceIdx {
		if n < 1 || n > evidenceCount {
			// 引用不在本 Bundle：不接受模型幻觉出来的证据。
			return Draft{}, false
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		idx = append(idx, n)
	}
	if len(idx) == 0 {
		return Draft{}, false
	}
	sort.Ints(idx)
	return Draft{
		Kind: kind, Scope: scope, Content: content,
		Rationale:   strings.TrimSpace(w.Rationale),
		SubjectKey:  strings.TrimSpace(w.SubjectKey),
		TaskState:   strings.TrimSpace(w.TaskState),
		EvidenceIdx: idx,
	}, true
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// evidenceIDsFor 把 Draft 的 1 起序号映射回 Bundle 的 evidence id。
// 调用方已保证序号在范围内；这里再做一次防御，长度不匹配时返回错误，
// 绝不把错位的 id 写进 candidate_evidence。
func evidenceIDsFor(idx []int, ids []string) ([]string, error) {
	out := make([]string, 0, len(idx))
	for _, n := range idx {
		if n < 1 || n > len(ids) {
			return nil, fmt.Errorf("证据序号 %d 超出 Bundle 范围 %d", n, len(ids))
		}
		out = append(out, ids[n-1])
	}
	return out, nil
}
