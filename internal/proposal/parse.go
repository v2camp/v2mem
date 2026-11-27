package proposal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
//   - quarantined：内容类失败累计到 MaxAttempts 的终态，等人工确认后回捞
//   - would_generate：dry-run 专用，只报告会生成什么，不写任何状态
const (
	StatusOK            = "ok"
	StatusNoop          = "noop"
	StatusPartial       = "partial"
	StatusInvalid       = "invalid"
	StatusBacklog       = "backlog"
	StatusFail          = "fail"
	StatusQuarantined   = "quarantined"
	StatusWouldGenerate = "would_generate"
)

// wireDraft 是模型的输出契约。EvidenceIdx 是 Bundle 内的 1 起短序号，
// 模型永远不需要（也不允许）回填 UUID。
type wireDraft struct {
	Kind         string `json:"kind"`
	Scope        string `json:"scope"`
	Content      string `json:"content"`
	Rationale    string `json:"rationale"`
	SubjectKey   string `json:"subject_key"`
	TaskState    string `json:"task_state"`
	AttributedTo string `json:"attributed_to"`
	EvidenceIdx  []int  `json:"evidence_idx"`
}

// Speaker 常量：attributed_to 只取这两个值。空串表示来源不明（旧行）。
const (
	SpeakerUser      = "user"
	SpeakerAssistant = "assistant"
)

// SpeakerOf 把证据事件类型映射到说话人。
//
// 与 mem0 的 attributed_to 同义：标记「这条事实的原始来源」，而不是写候选的人。
// 工具事件、unparsed、未知事件返回空串 —— 它们本来就不该进提案。
func SpeakerOf(eventType string) string {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "user-prompt", "user.prompt":
		return SpeakerUser
	case "assistant-text", "assistant.final", "stop", "session-end":
		return SpeakerAssistant
	default:
		return ""
	}
}

const maxRawForRepair = 2000

// proposalEligible 是提案准入的唯一判定，BuildPrompt 与 executor 共用。
//
// 语义（Fidelity 只管准入，不参与 A/B/C 证据等级）：
//   - ProposalEligible 是总开关，false 一律不可提案；
//   - FidelityFull / FidelitySummary 可提案；
//   - FidelityPointer / FidelityNone（以及未知值）不可提案；
//   - 空 Fidelity 表示「旧行/未声明」，按 full 兼容，避免没填就把旧证据全挡掉。
func proposalEligible(r evidence.Ref) bool {
	if !r.ProposalEligible {
		return false
	}
	switch r.Fidelity {
	case evidence.FidelityFull, evidence.FidelitySummary, "":
		return true
	default:
		return false
	}
}

// Eligible 是 proposalEligible 的导出入口，供旧规则蒸馏路径复用同一份准入判定。
// 新增调用方一律走这里，不得另写 Fidelity / ProposalEligible 的组合判断。
func Eligible(r evidence.Ref) bool { return proposalEligible(r) }

// proposalPayload 把证据正文投影成有界的提案输入：折叠空白 → 脱敏 →
// 按 rune 截到 evidence.ProposalPayloadRunes（省略标记计入预算，输出严格有界）。
//
// 截断只作用于 prompt 文本：事实源（归档 Raw）保持原文，store 结构不回写。
func proposalPayload(s string) string {
	text := scrub.Text(oneLine(s))
	runes := []rune(text)
	if len(runes) <= evidence.ProposalPayloadRunes {
		return text
	}
	const ellipsis = "…"
	runes = runes[:evidence.ProposalPayloadRunes-len([]rune(ellipsis))]
	return string(runes) + ellipsis
}

// BuildPrompt 把 Bundle 内可提案的证据渲染成带序号的提示词。
//
// 序号使用 Bundle 内位置（1 起），不可提案的证据保留空位，绝不重排——
// 一旦重排，模型返回的 evidence_idx 就会指向另一条证据。这里按 refs 下标
// 递增 idx，被 continue 跳过的条目不重编号，保证 ordinal 对齐。
func BuildPrompt(refs []evidence.Ref) string {
	var b strings.Builder
	b.WriteString("从下面的会话证据提炼长期记忆。只允许新增（ADD-only），不要改写或删除已有记忆。\n")
	b.WriteString("输出一行一个 JSON，不要任何解释；没有可提炼的事实就输出 []。\n")
	b.WriteString(`字段：{"kind":"decision|pitfall|preference|task","scope":"session|project|global","content":"可复用规则","rationale":"一句话","attributed_to":"user|assistant","subject_key":"可选","task_state":"可选","evidence_idx":[1]}` + "\n")
	b.WriteString("content 写「下次遇到同类情况能照做的规则」，不是过程记录：\n")
	b.WriteString("- decision/pitfall/preference/fact：写「触发场景 → 判断依据 → 处置」，一句话说清什么情况下该怎么做。\n")
	b.WriteString("- task：只写未完成事项的当前状态（做什么、卡在哪、下一步）。\n")
	b.WriteString("- 禁止第一人称与过程叙述（我/我们、尝试了、发现了、目前还没有）；禁止用「这个/那个」指代会话内的链接、报错、文件。\n")
	b.WriteString("- 好：「访问 Codeup 私有仓库时 WebFetch 返回登录页、git 报 could not read Username，说明缺凭证；应配置 token 或改用带凭证的地址」。\n")
	b.WriteString("  坏：「这个链接需要登录，我无法读取，尝试 git 拉取报错……」。\n")
	b.WriteString("- content 不超过 120 字。\n")
	b.WriteString("evidence_idx 必须是下面证据的序号，且每条候选至少引用一条。序号不连续属正常（被跳过或重复的条目保留空位）。\n")
	b.WriteString("证据按说话人分组，必须遵守：\n")
	b.WriteString("1. 用户原话是主来源；偏好、约束、意图、身份类事实只从 USER 组提取。\n")
	b.WriteString("2. ASSISTANT 组只贡献用户没说过的新增信息（排查结论、根因、方案）；\n")
	b.WriteString("   仅仅复述、总结或确认用户已说过的内容一律不提取（No Echo Extraction）。\n")
	b.WriteString("3. 同一事实在 USER 与 ASSISTANT 都出现时只提一次，引用 USER 组的序号。\n")
	b.WriteString("4. attributed_to 标这条事实的原始来源；引用 USER 组填 user，引用 ASSISTANT 组填 assistant。\n")
	b.WriteString("5. 完全相同的正文被不同来源重复采集时只保留首次出现的那条。\n\n")

	type line struct {
		idx  int
		ref  evidence.Ref
		body string
	}
	var userLines, assistantLines, otherLines []line
	seen := map[string]bool{}
	for i, r := range refs {
		idx := i + 1
		if !proposalEligible(r) {
			continue
		}
		body := proposalPayload(r.Summary)
		key := SpeakerOf(r.EventType) + "\x00" + body
		if seen[key] {
			continue
		}
		seen[key] = true
		l := line{idx: idx, ref: r, body: body}
		switch SpeakerOf(r.EventType) {
		case SpeakerUser:
			userLines = append(userLines, l)
		case SpeakerAssistant:
			assistantLines = append(assistantLines, l)
		default:
			otherLines = append(otherLines, l)
		}
	}
	writeSection := func(title string, lines []line) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&b, "=== %s ===\n", title)
		for _, l := range lines {
			fmt.Fprintf(&b, "[%d] (%s/%s) %s\n", l.idx, l.ref.EventType, l.ref.SessionID, l.body)
		}
		b.WriteString("\n")
	}
	writeSection("USER", userLines)
	writeSection("ASSISTANT", assistantLines)
	writeSection("OTHER", otherLines)
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
	if strings.HasPrefix(raw, "{") {
		// CLI 执行器常把「一行一个 JSON」渲染成多行美化 JSON：按行拆会把每一行
		// 都当成缺半边的碎片，整窗被误判为非法。先按 JSON 值序列解析整段输出；
		// 解析不了（例如夹了散文）再回退到逐行 JSONL。
		if res, ok := parseJSONValues(raw, evidenceCount); ok {
			if len(res.Drafts) == 0 && res.Invalid == 0 {
				return parseResult{}, nil
			}
			return res, nil
		}
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

// parseJSONValues 把整段输出当作连续的 JSON 值解析：单个对象、数组，或
// 多个对象直接串联（美化 JSONL）。返回 ok=false 表示这不是纯 JSON 值序列，
// 调用方应回退到逐行 JSONL 解析。
func parseJSONValues(raw string, evidenceCount int) (parseResult, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	var out parseResult
	for {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				return out, true
			}
			return parseResult{}, false
		}
		var w wireDraft
		if err := json.Unmarshal(v, &w); err == nil {
			if d, ok := validateDraft(w, evidenceCount); ok {
				out.Drafts = append(out.Drafts, d)
			} else {
				out.Invalid++
			}
			continue
		}
		var arr []wireDraft
		if err := json.Unmarshal(v, &arr); err == nil {
			res, _ := validateAll(arr, evidenceCount)
			out.Drafts = append(out.Drafts, res.Drafts...)
			out.Invalid += res.Invalid
			continue
		}
		return parseResult{}, false
	}
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
	// task 描述的是未完成状态，天然带「目前/还没」这类时点词，不做叙述体拦截；
	// 其余 kind 必须是可复用规则，过程叙述整条丢弃。
	if kind != "task" && isProcessNarrative(content) {
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
	attributedTo := strings.ToLower(strings.TrimSpace(w.AttributedTo))
	switch attributedTo {
	case SpeakerUser, SpeakerAssistant, "":
	default:
		// 来源标注不允许模型自造第三种值；空串表示旧格式，由落库层按证据推导。
		attributedTo = ""
	}
	return Draft{
		Kind: kind, Scope: scope, Content: content,
		Rationale:    strings.TrimSpace(w.Rationale),
		SubjectKey:   strings.TrimSpace(w.SubjectKey),
		TaskState:    strings.TrimSpace(w.TaskState),
		AttributedTo: attributedTo,
		EvidenceIdx:  idx,
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
