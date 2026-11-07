package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/recall"
	"github.com/v2camp/v2mem/internal/scrub"
	"github.com/v2camp/v2mem/internal/store"
)

// 钩子路径的硬约束：
//
//  1. **绝不阻断宿主**。钩子跑在用户会话的关键路径上，记忆系统出问题不该让会话中断。
//     因此本文件所有分支都返回 nil，异常一律写 stderr 后静默放过。
//     观测事件只写 observations（或失败时写 spool），绝不 store.Add。
//  2. **stdout 只放要注入模型的内容**。诊断信息走 stderr —— CodeBuddy 文档明确
//     「stderr 仅作为 fallback，调试日志可安全写入 stderr，不会污染给 Agent 的反馈」，
//     其余各家对退出码 0 时的 stderr 也都只做展示。
//  3. **每轮注入有字符预算**。Level 1 是每轮都要注入的，不做预算会随记忆增长吃掉上下文。
//  4. **注入格式按宿主分叉**。Claude 系的 SessionStart / UserPromptSubmit 是 stdout 纯文本。
//     Cursor 的 sessionStart / beforeSubmitPrompt / preCompact 要 JSON
//     `{"additional_context":"..."}`，纯文本不会被当成注入。
//     观测事件（postToolUse / stop / sessionEnd）不写 stdout。
const (
	defaultHookLimit = 3
	// defaultHookBudget 是注入内容的字符预算。取 1200 是权衡：
	// 约 400 个汉字，够放 1 条硬规则 + 3 条相关记忆 + 用法提示。
	defaultHookBudget = 1200
	// maxHitRunes 是单条记忆的截断长度，防一条长记忆吃光预算。
	maxHitRunes = 120
)

// hookInput 是各 harness 经 stdin 传来的 JSON。
// 字段取自多家官方文档的公共交集，未列出的字段一律忽略。
type hookInput struct {
	EventName      string          `json:"hook_event_name"`
	Cwd            string          `json:"cwd"`
	Prompt         string          `json:"prompt"`
	Source         string          `json:"source"`
	SessionID      string          `json:"session_id"`
	ConversationID string          `json:"conversation_id"`
	WorkspaceRoots []string        `json:"workspace_roots"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	EventID        string          `json:"event_id"`
	Channel        string          `json:"channel"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolOutput     json.RawMessage `json:"tool_output"`
	TranscriptPath string          `json:"transcript_path"`
	Status         string          `json:"status"`
	Reason         string          `json:"reason"`

	// Trae 的 Stop / SessionEnd 把最终回复放在这两个字段（design-capture §7.2）。
	LastAssistantMessage string `json:"last_assistant_message"`
	TextContent          string `json:"text_content"`
}

// sessionID 优先用 session_id，Cursor 有时只给 conversation_id。
func (in hookInput) sessionID() string {
	if s := strings.TrimSpace(in.SessionID); s != "" {
		return s
	}
	return strings.TrimSpace(in.ConversationID)
}

func cmdHook(args []string) error {
	var c common
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	c.register(fs)
	eventFlag := fs.String("event", "", "事件名；默认从 stdin 的 hook_event_name 推断")
	harnessFlag := fs.String("harness", "", "harness 名；决定项目目录取自哪个环境变量")
	limit := fs.Int("limit", defaultHookLimit, "注入的记忆条数上限")
	maxChars := fs.Int("max-chars", defaultHookBudget, "注入内容的字符预算")
	auditPath := fs.String("audit", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；\"-\" 表示关闭）")
	dedupWindow := fs.Int("dedup-window", defaultDedupWindowSec,
		"重复触发抑制窗口（秒）。同一事件被多处配置并行触发时只注入一次；0 关闭")
	noScrub := fs.Bool("no-scrub", false, "关闭密钥打码（仅本地调试；默认打码观测摘要与审计 query）")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 参数解析失败，已静默放过:", err)
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 读取 stdin 失败，已按空输入继续:", err)
		raw = nil
	}
	var in hookInput
	if len(raw) > 0 {
		// 解析失败按空输入处理：有的 harness 可能不传 stdin
		_ = json.Unmarshal(raw, &in)
	}

	event, ok := resolveHookEvent(*eventFlag, in.EventName)
	if !ok {
		// 我们不处理的事件（如 PreToolUse）保持完全静默
		return nil
	}

	project, dir := hookProject(*harnessFlag, in)
	if dir != "" {
		fmt.Fprintf(os.Stderr, "v2mem hook: event=%s project=%q dir=%s\n", event, project, dir)
	}

	started := time.Now()

	// 幂等去重：同一事件若被**多处配置**并行触发（如 TraeWork 的项目级
	// .trae/hooks.json 与全局 ~/.trae-cn/hooks.json 都定义了同一事件），
	// 只让先到的那次注入。详情与设计约束见 hookDedupKey 的注释。
	if *dedupWindow > 0 {
		claimed, derr := claimHookEvent(c.db, in, event, *dedupWindow, started.Unix())
		switch {
		case derr != nil:
			// 失败开放：判不出是否重复时照常注入。
			// 注入两次只是啰嗦；**漏**注入是功能缺失 —— 两者代价不对等。
			fmt.Fprintln(os.Stderr, "v2mem hook: 去重检查失败，按未重复处理:", derr)
		case !claimed:
			if *auditPath != "-" {
				// 记下「被抑制」这件事：它本身是**并行会话是否真在重复触发**的度量。
				_ = audit.Append(*auditPath, audit.Record{
					TS: started.Unix(), Event: string(event),
					Harness: harnessName(*harnessFlag, in), Project: project,
					SessionID: in.sessionID(), Query: scrub.Apply(strings.TrimSpace(in.Prompt), *noScrub),
					Suppressed: true, MS: time.Since(started).Milliseconds(),
				})
			}
			fmt.Fprintf(os.Stderr, "v2mem hook: event=%s 的重复触发已抑制（另一处配置已注入）\n", event)
			return nil
		}
	}

	if isObserveHook(event) {
		appendObservationFailOpen(c, *harnessFlag, event, project, in, *auditPath, started, *noScrub)
		if observeEventMayDistill(event) {
			maybeSpawnDistill(c.db, in, harnessName(*harnessFlag, in))
		}
		return nil
	}

	if event == harness.EventSessionStart {
		drainSpoolFailOpen(c.db, *auditPath)
		distillPendingFailOpen(c.db, *auditPath, harnessName(*harnessFlag, in))
	}

	budget := *maxChars
	if event == harness.EventPreCompact {
		if n := positiveEnvInt("MEM_PRECOMPACT_BUDGET"); n > 0 {
			budget = n
		}
	}

	out := renderHookContext(c, event, project, in.Prompt, *limit, budget,
		hookMeta{SessionID: in.sessionID(), Harness: harnessName(*harnessFlag, in)})
	writeHookStdout(*harnessFlag, event, out.Text)

	// 审计：为「用户会话 → mem 查询」沉淀真实样本。写失败必须静默 ——
	// 这条在宿主会话关键链上，审计问题绝不能让会话收到错误。
	if *auditPath != "-" {
		// gate 主动跳过（空/客套/极短无技术语义）不是「查了没命中」，不进空注入分母。
		empty := strings.TrimSpace(out.Text) == "" && !out.Gate.Skipped()
		rec := audit.Record{
			TS: started.Unix(), Event: string(event), Harness: harnessName(*harnessFlag, in),
			Project: project, SessionID: in.sessionID(), Query: scrub.Apply(strings.TrimSpace(in.Prompt), *noScrub),
			Empty: empty, MS: time.Since(started).Milliseconds(),
		}
		for _, h := range out.Injected {
			rec.Hashes = append(rec.Hashes, h.ID)
			rec.Kinds = append(rec.Kinds, h.Kind)
		}
		_ = audit.Append(*auditPath, rec)
	}
	return nil
}

// harnessName 推断本次调用来自哪个工具：显式参数优先，否则由项目目录环境变量反推。
func harnessName(flagVal string, in hookInput) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		if h, err := harness.Get(v); err == nil {
			return h.Name
		}
		return v
	}
	for _, h := range harness.All() {
		if h.ProjectDirFromEnv() != "" {
			return h.Name
		}
	}
	return ""
}

// resolveHookEvent 依次尝试显式参数与 stdin 字段，容忍写法差异。
func resolveHookEvent(flagVal, stdinVal string) (harness.Event, bool) {
	for _, cand := range []string{flagVal, stdinVal} {
		if strings.TrimSpace(cand) == "" {
			continue
		}
		if ev, ok := harness.FromEventName(cand); ok {
			return ev, true
		}
	}
	return "", false
}

// hookProject 推断当前工程。
//
// 优先级：指定 harness 注入的环境变量 → stdin 的 cwd → workspace_roots → 进程工作目录。
// 指定了 harness 就只用它的环境变量，不再兜到别家 —— 否则在 Trae 里启动的终端
// 会把 CLAUDE_PROJECT_DIR 带到 QoderWork 的钩子里，工程判断就错了。
func hookProject(harnessName string, in hookInput) (name, dir string) {
	if strings.TrimSpace(harnessName) != "" {
		if h, err := harness.Get(harnessName); err == nil {
			if d := h.ProjectDirFromEnv(); d != "" {
				return detectProjectAt(d), d
			}
		}
	} else {
		// 未指定报哪家：按注册表顺序试所有已知的项目目录环境变量
		for _, h := range harness.All() {
			if d := h.ProjectDirFromEnv(); d != "" {
				return detectProjectAt(d), d
			}
		}
	}

	if d := strings.TrimSpace(in.Cwd); d != "" {
		return detectProjectAt(d), d
	}
	for _, d := range in.WorkspaceRoots {
		if strings.TrimSpace(d) != "" {
			return detectProjectAt(d), d
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	return detectProjectAt(wd), wd
}

// hookMeta 是轨迹需要的会话元数据。
type hookMeta struct {
	SessionID string
	Harness   string
	Event     string
}

// hookRender 是一次钩子渲染的结果。Gate 单列是为了让审计区分
// 「gate 主动跳过」与「查了但没命中」——前者不进空注入分母。
type hookRender struct {
	Text     string
	Injected []store.Hit
	Gate     recall.Gate
}

// renderHookContext 构造要注入模型的文本，并返回本次实际注入的记忆。
//
// 返回 hits 是为了审计：没有真实会话的「查询 → 命中」样本，
// 检索质量就无法评测（见 internal/audit 与 DESIGN.md §15）。
func renderHookContext(c common, event harness.Event, project, prompt string, limit, maxChars int, meta hookMeta) hookRender {
	st, err := store.Open(c.db)
	if err != nil {
		// 库打不开时仍告诉模型「有这么个库」，否则用户会以为记忆系统不存在
		fmt.Fprintln(os.Stderr, "v2mem hook: 打开记忆库失败:", err)
		if event == harness.EventSessionStart || event == harness.EventPreCompact {
			return hookRender{Text: fmt.Sprintf("[v2mem] 本地分层记忆库（Level 2）。当前工程：%s\n%s",
				orGlobal(project), hintBlock())}
		}
		return hookRender{}
	}
	defer st.Close()
	if meta.Event == "" {
		meta.Event = string(event)
	}

	switch event {
	case harness.EventSessionStart, harness.EventPreCompact:
		text, injected := sessionStartContext(st, project, limit, maxChars, meta)
		return hookRender{Text: text, Injected: injected}
	case harness.EventPromptSubmit:
		text, injected, gate := promptSubmitContext(st, project, prompt, limit, maxChars, meta)
		return hookRender{Text: text, Injected: injected, Gate: gate}
	}
	return hookRender{}
}

// notesEnabled 由 MEM_NO_NOTES 控制：置位时读侧回到「仅 v2mem 原始注入」，
// 不做 notes 小字条去重。未置位（默认）则启用去重。
func notesEnabled() bool {
	return strings.TrimSpace(os.Getenv("MEM_NO_NOTES")) == ""
}

// notesCoveredPredicate 返回「这条命中是否已被 notes 覆盖」的判定。
// nil 表示不去重（MEM_NO_NOTES 置位，或 notes 读不出来 —— 失败开放，宁可多注入）。
func notesCoveredPredicate(st *store.Store) func(store.Hit) bool {
	if !notesEnabled() {
		return nil
	}
	rules, err := collectRules(st)
	if err != nil {
		return nil
	}
	covered := make(map[string]bool, len(rules))
	for _, r := range rules {
		covered[oneLine(r.Content)] = true
	}
	return func(h store.Hit) bool {
		return h.Kind == "rule" && covered[oneLine(h.Content)]
	}
}

// filterRulesCoveredByNotes 去掉「已在开场注入里出现过」的规则细节条。
//
// 注入分工（**只有 SessionStart / PreCompact 承载 notes 正文**）：
// 硬规则的红线内容由 sessionStartContext 的「跨工程硬规则」段注入一次，
// 等价于 `mem notes` 的规则段。每轮 prompt-submit 若再检索出同一条 rule，
// 同一知识就在上下文里重复出现、白占预算，故在**检索侧**过滤掉。
//
// 注意：本函数只给检索侧用。开场注入侧若也调用它，规则将无处可去
// （钩子不注入 notes 文本），等于静默丢规则 —— 见 sessionStartContext 的注释。
func filterRulesCoveredByNotes(st *store.Store, hits []store.Hit) []store.Hit {
	pred := notesCoveredPredicate(st)
	if pred == nil {
		return hits
	}
	out := hits[:0]
	for _, h := range hits {
		if pred(h) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// sectionRender 是一段注入的渲染结果：预算不足时会继续向后扫描（补位），
// 不是截断到第一条放不下就停。
type sectionRender struct {
	block    string
	injected map[string]bool
	drops    map[string]string
	used     int
}

// renderLineWithin 从最长内容开始收缩，找到能放进 lineBudget 的最大截断。
// 返回的字符串保证 runeLen <= lineBudget。
func renderLineWithin(h store.Hit, provenance string, lineBudget int) (string, bool) {
	if lineBudget <= 0 {
		return "", false
	}
	for allow := maxHitRunes; allow >= 0; allow-- {
		line := recall.RenderLine(h, provenance, allow)
		if runeLen(line) <= lineBudget {
			return line, true
		}
	}
	return "", false
}

func renderSection(title string, hits []store.Hit, prov map[string]string, budget, cap int) sectionRender {
	r := sectionRender{injected: map[string]bool{}, drops: map[string]string{}}
	if cap <= 0 || cap > len(hits) {
		cap = len(hits)
	}
	titleLine := ""
	titleCost := 0
	if title != "" {
		titleLine = title + "：\n"
		titleCost = runeLen(titleLine)
	}
	if budget < titleCost {
		for _, h := range hits {
			r.drops[h.ID] = "budget"
		}
		return r
	}
	var body strings.Builder
	used := titleCost
	n := 0
	for _, h := range hits {
		if n >= cap {
			r.drops[h.ID] = "limit"
			continue
		}
		line := recall.RenderLine(h, prov[h.ID], maxHitRunes)
		cost := runeLen(line)
		if used+cost > budget {
			r.drops[h.ID] = "budget"
			continue
		}
		body.WriteString(line)
		used += cost
		n++
		r.injected[h.ID] = true
	}
	if n == 0 && len(hits) > 0 {
		// 极限预算：完整行一条都放不下时，把排名最高的一条按剩余空间截短，
		// 而不是整段弃答。只在「一条完整行都放不下」时触发，正常路径仍优先
		// 从合格池补位完整条目（见 baseline 的 budget 用例）。
		if line, ok := renderLineWithin(hits[0], prov[hits[0].ID], budget-titleCost); ok {
			body.WriteString(line)
			used += runeLen(line)
			n++
			r.injected[hits[0].ID] = true
			delete(r.drops, hits[0].ID)
		}
	}
	if n == 0 {
		return r
	}
	r.block = titleLine + body.String()
	r.used = used
	return r
}

// sessionStartContext 注入「硬规则 + 全局记忆 + 本工程记忆 + 用法」。
//
// 顺序有讲究：跨工程硬规则在前（通常是红线，且对所有工程生效），
// 本工程记忆在后，用法提示压尾并**预留预算**，保证它一定不被截断 ——
// 它的作用正是告诉模型「还有多级记忆可查」，被截断就失去了钩子的意义。
//
// T4 硬规则：本函数的「硬规则」段**只取 kind=rule**。全局高 salience 的普通
// 记忆仍有注入通道（紧随其后的「全局记忆」段），但不会被冒充成硬规则。
func sessionStartContext(st *store.Store, project string, limit, maxChars int, meta hookMeta) (string, []store.Hit) {
	hint := hintBlock()
	codes := renderKindCodes()
	head := fmt.Sprintf("[v2mem] 本机有跨会话的本地记忆库（Level 2，正文不进上下文）。当前工程：%s\n",
		orGlobal(project))

	budget := maxChars - runeLen(head) - runeLen(hint) - runeLen(codes)
	if budget < 0 {
		budget = 0
	}

	pool := limit * 4
	if pool < 12 {
		pool = 12
	}

	type section struct {
		title      string
		matchClass string
		cap        int
		hits       []store.Hit
	}
	seen := map[string]bool{}
	var sections []section
	add := func(title, class string, cap int, hits []store.Hit) {
		var kept []store.Hit
		for _, h := range hits {
			if h.ID == "" || seen[h.ID] {
				continue
			}
			seen[h.ID] = true
			kept = append(kept, h)
		}
		if len(kept) > 0 {
			sections = append(sections, section{title: title, matchClass: class, cap: cap, hits: kept})
		}
	}
	// 跨工程硬规则：只取 kind=rule。
	if raw, err := st.List(store.ListQuery{Scope: "global", Kind: "rule", Limit: pool}); err == nil {
		add("硬规则（跨工程，务必遵守）", "hard_rule", limit, raw)
	}
	// 其他全局记忆：保留既有注入通道，但不冒充硬规则；默认只给 1 位，避免挤占本工程。
	if raw, err := st.List(store.ListQuery{Scope: "global", Limit: pool}); err == nil {
		add("全局记忆", "global", 1, raw)
	}
	// 本工程的记忆（Scope=project 只取本工程，避免与上面的全局段重复）。
	if project != "" {
		if raw, err := st.List(store.ListQuery{Scope: "project", Project: project, Limit: pool}); err == nil {
			add("本工程记忆", "project", limit, raw)
		}
	}

	var all []store.Hit
	for _, sec := range sections {
		all = append(all, sec.hits...)
	}
	prov := recall.ProvenanceFor(st, all)
	hashes, _ := st.ContentHashes(hitIDs(all))

	var sb strings.Builder
	sb.WriteString(head)
	sb.WriteString(codes)
	var injected []store.Hit
	var items []store.ReadTraceItem
	rank := 0
	for _, sec := range sections {
		r := renderSection(sec.title, sec.hits, prov, budget, sec.cap)
		if r.block != "" {
			sb.WriteString(r.block)
			budget -= r.used
		}
		for _, h := range sec.hits {
			it := store.ReadTraceItem{
				MemoryID: h.ID, ContentHash: hashes[h.ID], SignalsJSON: "{}",
				MatchClass: sec.matchClass, Eligible: true, Provenance: prov[h.ID],
			}
			if r.injected[h.ID] {
				rank++
				it.InjectionRank = rank
				injected = append(injected, h)
			} else {
				it.DropReason = r.drops[h.ID]
				if it.DropReason == "" {
					it.DropReason = "budget"
				}
			}
			items = append(items, it)
		}
	}
	sb.WriteString(hint)

	outcome := recall.OutcomeNoHit
	if len(injected) > 0 {
		outcome = recall.OutcomeInjected
	} else if len(all) > 0 {
		outcome = recall.OutcomeLowQuality
	}
	reason := "session_start"
	if meta.Event == string(harness.EventPreCompact) {
		reason = "pre_compact"
	}
	if _, err := st.WriteReadTrace(store.ReadTrace{
		SessionID: meta.SessionID, Project: project, Harness: meta.Harness,
		QueryGate: "skip", GateReason: reason, Scope: "current",
		TargetLimit: limit, BudgetChars: maxChars, RawHits: len(all),
		Qualified: len(all), Injected: len(injected), Outcome: outcome,
	}, items); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 写读侧轨迹失败（不影响注入）:", err)
	}
	return sb.String(), injected
}

// promptSubmitContext 依据用户提问检索并注入相关记忆。
//
// 召回交给 internal/recall：gate 判定 → 完整融合池过取 → 质量筛选 →
// notes/content 去重 → scope 配额 → 选择 → 预算检查 → 合格池补位。
// 无合格项时返回空串：每轮注入无关内容纯属浪费 token。
func promptSubmitContext(st *store.Store, project, prompt string, limit, maxChars int, meta hookMeta) (string, []store.Hit, recall.Gate) {
	prompt = strings.TrimSpace(prompt)
	head := "[v2mem] 相关历史记忆（若与当前代码冲突，以代码为准）：\n"
	budget := maxChars - runeLen(head)
	if budget <= 0 {
		// 负数让预算检查必然失败；不能用 0（recall 会当成「未指定」用默认 1200）。
		budget = -1
	}
	res, err := recall.Query(st, recall.QueryOptions{
		Query: prompt, Project: project, Scope: "current",
		SessionID: meta.SessionID, Harness: meta.Harness,
		Limit: limit, Budget: budget, ContentRunes: maxHitRunes,
		NotesCovered: notesCoveredPredicate(st),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 检索失败（不影响会话）:", err)
		return "", nil, res.Gate
	}
	if len(res.Injected) == 0 {
		return "", nil, res.Gate
	}
	var sb strings.Builder
	sb.WriteString(head)
	hits := make([]store.Hit, 0, len(res.Injected))
	for _, in := range res.Injected {
		sb.WriteString(recall.RenderLine(in.Hit, in.Provenance, maxHitRunes))
		hits = append(hits, in.Hit)
	}
	return sb.String(), hits, res.Gate
}

func hitIDs(hits []store.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.ID != "" {
			out = append(out, h.ID)
		}
	}
	return out
}

// formatHits 渲染命中列表，返回文本与消耗的字符数。
// title 为空则不写标题行。prov 是 id → 紧凑 provenance，逐条带 mem:<id8>。
func formatHits(title string, hits []store.Hit, prov map[string]string, budget int) (string, int) {
	r := renderSection(title, hits, prov, budget, len(hits))
	return r.block, r.used
}

// hintBlock 是「钩子」本体：告诉模型还有 Level 2 可查，并给出可直接执行的命令。
// 命令必须写全，模型不会去猜参数。
func hintBlock() string {
	return "需要时可检索本库（按需查询，不要凭空断言）：\n" +
		"  优先 MCP 工具 search / add / touch（query 或 content 必填；一条事实一个 add）\n" +
		"  search 的 layer：memory=已定记忆（默认），observation=近期痕迹，both=两层\n" +
		"  mem search --scope current --json \"<关键词>\"\n" +
		"  mem search --layer observation --scope current --json \"<关键词>\"\n" +
		"  mem add --kind decision|pitfall|preference|fact \"<原子事实>\"\n" +
		"  mem touch <id前8位>\n"
}

func orGlobal(project string) string {
	if strings.TrimSpace(project) == "" {
		return "(未识别工程)"
	}
	return project
}

func runeLen(s string) int { return len([]rune(s)) }

func truncateRunes(s string, n int) string {
	rs := []rune(strings.TrimSpace(s))
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n]) + "…"
}

// defaultDedupWindowSec 是重复触发抑制窗口。
//
// 为什么需要窗口而不是永久去重：同一会话里用户**可能合理地重复同一句话**
// （如两次「继续」）。那是两个真实事件，应各注入一次。而重复触发发生在**毫秒级**，
// 用秒级窗口即可区分二者；且窗口内被抑制的那次，其内容与刚注入的完全相同、
// 仍在上下文里，抑制的损失可忽略。
const defaultDedupWindowSec = 10

// hookDedupKey 由「事件 + 会话 + 提问 + 目录」算出（sha256 前 32 位十六进制）。
//
// 🔴 刻意**不含 harness**：同一事件的重复触发恰恰来自两处配置用了**不同的 harness 名**
// （TraeWork 项目级写 `--harness traework`、全局写 `--harness traecode`）。
// 把 harness 纳入键，去重会永远失效 —— 那正是要防的场景。
// 不含 `--limit/--max-chars` 等同理：它们是本工具的旋钮，不该影响「是否为同一事件」。
func hookDedupKey(in hookInput, event harness.Event) string {
	h := sha256.New()
	parts := []string{string(event), in.sessionID(), strings.TrimSpace(in.Prompt), in.Cwd}
	// PostToolUse 在同一会话里会连续发生。键里只有事件+会话+提问时，
	// 10 秒窗口会把不同工具调用收成一次，观测被吞掉。
	// 补上工具名和 tool_use_id：同一次调用的并行配置仍然撞键，不同调用则分开。
	if event == harness.EventPostToolUse {
		parts = append(parts, strings.TrimSpace(in.ToolName), strings.TrimSpace(in.ToolUseID))
		if strings.TrimSpace(in.ToolUseID) == "" {
			parts = append(parts, compactRaw(in.ToolInput, 120, false))
		}
	}
	for _, part := range parts {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0}) // 分隔符，避免字段拼接歧义
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// claimHookEvent 判断本次事件是否归当前调用处理。
// 任何错误都返回 err，由调用方决定「失败开放」。
func claimHookEvent(db string, in hookInput, event harness.Event, windowSec int, now int64) (bool, error) {
	st, err := store.Open(db)
	if err != nil {
		return false, err
	}
	defer st.Close()

	claimed, err := st.ClaimHookEvent(hookDedupKey(in, event), now, now-int64(windowSec))
	if err != nil {
		return false, err
	}
	// 顺手清理过期记录（窗口的 100 倍），避免表无限增长。失败不影响主流程。
	_ = st.PruneHookDedup(now - int64(windowSec)*100)
	return claimed, nil
}
