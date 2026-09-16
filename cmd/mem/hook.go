package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wanghui/v2mem/internal/audit"
	"github.com/wanghui/v2mem/internal/harness"
	"github.com/wanghui/v2mem/internal/store"
)

// 钩子路径的硬约束：
//
//  1. **绝不阻断宿主**。钩子跑在用户会话的关键路径上，记忆系统出问题不该让会话中断。
//     因此本文件所有分支都返回 nil，异常一律写 stderr 后静默放过。
//  2. **stdout 只放要注入模型的内容**。诊断信息走 stderr —— CodeBuddy 文档明确
//     「stderr 仅作为 fallback，调试日志可安全写入 stderr，不会污染给 Agent 的反馈」，
//     其余各家对退出码 0 时的 stderr 也都只做展示。
//  3. **每轮注入有字符预算**。Level 1 是每轮都要注入的，不做预算会随记忆增长吃掉上下文。
//  4. **一条命令通吃**。SessionStart / UserPromptSubmit 在 Claude Code、TraeCode、
//     CodeBuddy、Qoder、QoderWork、Codex 六家都支持「stdout 纯文本被注入上下文」，
//     故输出格式取纯文本这个最大公约数，不按 harness 分支渲染。
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
	EventName      string   `json:"hook_event_name"`
	Cwd            string   `json:"cwd"`
	Prompt         string   `json:"prompt"`
	Source         string   `json:"source"`
	SessionID      string   `json:"session_id"`
	WorkspaceRoots []string `json:"workspace_roots"`
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
	text, injected := renderHookContext(c, event, project, in.Prompt, *limit, *maxChars)
	if text != "" {
		fmt.Print(text)
	}

	// 审计：为「用户会话 → mem 查询」沉淀真实样本。写失败必须静默 ——
	// 这条在宿主会话关键链上，审计问题绝不能让会话收到错误。
	if *auditPath != "-" {
		rec := audit.Record{
			TS: started.Unix(), Event: string(event), Harness: harnessName(*harnessFlag, in),
			Project: project, SessionID: in.SessionID, Query: strings.TrimSpace(in.Prompt),
			Empty: strings.TrimSpace(text) == "", MS: time.Since(started).Milliseconds(),
		}
		for _, h := range injected {
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

// renderHookContext 构造要注入模型的文本，并返回本次实际注入的记忆。
//
// 返回 hits 是为了审计：没有真实会话的「查询 → 命中」样本，
// 检索质量就无法评测（见 internal/audit 与 DESIGN.md §15）。
func renderHookContext(c common, event harness.Event, project, prompt string, limit, maxChars int) (string, []store.Hit) {
	st, err := store.Open(c.db)
	if err != nil {
		// 库打不开时仍告诉模型「有这么个库」，否则用户会以为记忆系统不存在
		fmt.Fprintln(os.Stderr, "v2mem hook: 打开记忆库失败:", err)
		if event == harness.EventSessionStart {
			return fmt.Sprintf("[v2mem] 本地分层记忆库（Level 2）。当前工程：%s\n%s",
				orGlobal(project), hintBlock()), nil
		}
		return "", nil
	}
	defer st.Close()

	switch event {
	case harness.EventSessionStart:
		return sessionStartContext(st, project, limit, maxChars)
	case harness.EventPromptSubmit:
		return promptSubmitContext(st, project, prompt, limit, maxChars)
	}
	return "", nil
}

// sessionStartContext 注入「硬规则 + 本工程记忆 + 用法」。
//
// 顺序有讲究：跨工程硬规则在前（通常是红线，且对所有工程生效），
// 本工程记忆在后，用法提示压尾并**预留预算**，保证它一定不被截断 ——
// 它的作用正是告诉模型「还有多级记忆可查」，被截断就失去了钩子的意义。
func sessionStartContext(st *store.Store, project string, limit, maxChars int) (string, []store.Hit) {
	hint := hintBlock()
	head := fmt.Sprintf("[v2mem] 本机有跨会话的本地记忆库（Level 2，正文不进上下文）。当前工程：%s\n",
		orGlobal(project))

	budget := maxChars - runeLen(head) - runeLen(hint)
	if budget < 0 {
		budget = 0
	}

	var sb strings.Builder
	sb.WriteString(head)
	var injected []store.Hit

	// 跨工程硬规则
	if hits, err := st.List(store.ListQuery{Scope: "global", Limit: limit}); err == nil && len(hits) > 0 {
		if block, used := formatHits("硬规则（跨工程，务必遵守）", hits, budget); block != "" {
			sb.WriteString(block)
			budget -= used
			injected = append(injected, hits[:countLines(block)]...)
		}
	}
	// 本工程的记忆（Scope=project 只取本工程，避免与上面的全局段重复）
	if budget > 0 && project != "" {
		if hits, err := st.List(store.ListQuery{Scope: "project", Project: project, Limit: limit}); err == nil && len(hits) > 0 {
			if block, used := formatHits("本工程记忆", hits, budget); block != "" {
				sb.WriteString(block)
				budget -= used
				injected = append(injected, hits[:countLines(block)]...)
			}
		}
	}

	sb.WriteString(hint)
	return sb.String(), injected
}

// countLines 数 formatHits 实际渲染了多少条 —— 预算截断时命中数少于候选数，
// 审计必须记「真正注入的」而非「查到的」。
func countLines(block string) int {
	n := 0
	for _, l := range strings.Split(block, "\n") {
		if strings.HasPrefix(l, "- [") {
			n++
		}
	}
	return n
}

// promptSubmitContext 依据用户提问检索并注入相关记忆。
// 无命中时返回空串：每轮注入无关内容纯属浪费 token。
func promptSubmitContext(st *store.Store, project, prompt string, limit, maxChars int) (string, []store.Hit) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", nil
	}
	hits, err := st.Search(store.SearchQuery{
		Query:   prompt,
		Project: project,
		Scope:   "current",
		Limit:   limit,
	})
	if err != nil || len(hits) == 0 {
		return "", nil
	}
	head := "[v2mem] 相关历史记忆（若与当前代码冲突，以代码为准）：\n"
	block, _ := formatHits("", hits, maxChars-runeLen(head))
	if block == "" {
		return "", nil
	}
	return head + block, hits[:countLines(block)]
}

// formatHits 渲染命中列表，返回文本与消耗的字符数。
// title 为空则不写标题行。
func formatHits(title string, hits []store.Hit, budget int) (string, int) {
	var b strings.Builder
	if title != "" {
		b.WriteString(title + "：\n")
		if runeLen(b.String()) > budget {
			return "", 0
		}
	}
	n := 0
	for _, h := range hits {
		line := fmt.Sprintf("- [%s] %s\n", h.Kind, truncateRunes(h.Content, maxHitRunes))
		if runeLen(b.String())+runeLen(line) > budget {
			break
		}
		b.WriteString(line)
		n++
	}
	if n == 0 {
		return "", 0
	}
	s := b.String()
	return s, runeLen(s)
}

// hintBlock 是「钩子」本体：告诉模型还有 Level 2 可查，并给出可直接执行的命令。
// 命令必须写全，模型不会去猜参数。
func hintBlock() string {
	return "需要时可检索本库（按需查询，不要凭空断言历史决策）：\n" +
		"  mem search --scope current --json \"<关键词>\"   # 查历史决策 / 踩坑 / 约定\n" +
		"  mem add --kind decision|pitfall|preference|fact \"<原子事实>\"   # 记下新学到的事实\n" +
		"  mem touch <id前8位>   # 命中后反馈，供过期机制参考\n"
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
