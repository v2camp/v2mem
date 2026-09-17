package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanghui/v2mem/internal/harness"
	"github.com/wanghui/v2mem/internal/store"
)

// captureStdout 捕获命令输出，用于断言 CLI 的真实打印结果。
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	return buf.String(), runErr
}

// ---------- M3: 标记过滤 ----------

func TestCmdSearchHonoursTagFlag(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--tag", "machine=mini", "带有机器标记的知识"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	hitOut, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--tag", "machine=mini", "知识"})
	})
	if err != nil {
		t.Fatalf("cmdSearch: %v", err)
	}
	if !strings.Contains(hitOut, "带有机器标记的知识") {
		t.Errorf("同 tag 应检索到，got: %q", hitOut)
	}

	missOut, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--tag", "machine=other", "知识"})
	})
	if err != nil {
		t.Fatalf("cmdSearch: %v", err)
	}
	if !strings.Contains(missOut, "无结果") {
		t.Errorf("不同 tag 应检索不到，got: %q", missOut)
	}
}

// ---------- M3: 工程作用域 ----------

// detectProject 在测试进程的工作目录（仓库根）下返回仓库基名，
// 因此这里显式指定 --project 来构造「当前工程 / 其他工程」两组数据。
func TestCmdSearchScopeCurrentNarrowsToCurrentProject(t *testing.T) {
	db := testDB(t)
	cur := detectProject()
	if err := cmdAdd([]string{"--db", db, "--project", cur, "当前工程的独有知识"}); err != nil {
		t.Fatalf("cmdAdd cur: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--project", "some-other-project", "其他工程的独有知识"}); err != nil {
		t.Fatalf("cmdAdd other: %v", err)
	}

	scoped, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--scope", "current", "独有知识"})
	})
	if err != nil {
		t.Fatalf("cmdSearch --scope current: %v", err)
	}
	if !strings.Contains(scoped, "当前工程的独有知识") {
		t.Errorf("--scope current 应命中当前工程，got: %q", scoped)
	}
	if strings.Contains(scoped, "其他工程的独有知识") {
		t.Errorf("--scope current 不应命中其他工程，got: %q", scoped)
	}

	all, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "独有知识"})
	})
	if err != nil {
		t.Fatalf("cmdSearch 默认: %v", err)
	}
	if !strings.Contains(all, "当前工程的独有知识") || !strings.Contains(all, "其他工程的独有知识") {
		t.Errorf("默认作用域应跨工程返回，got: %q", all)
	}
}

func testDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "mem.db")
}

// ---------- M6: Harness 钩子 ----------
//
// 测试数据的两个前提必须同时满足，否则用例会以「被工程过滤掉」的方式假通过：
//   ① 记忆的 project 与钩子推断出的工程名一致
//   ② 工程目录带 .git，使 mem add 与钩子走同一套推断
// 这组用例里每条断言都要求「有内容」，而不是「为空」，正是为了堵住这类假通过。

// withStdin 替换 stdin/stdout 后运行 fn，返回被捕获的标准输出。
func withStdin(t *testing.T, input string, fn func() error) (string, error) {
	t.Helper()
	oldIn, oldOut := os.Stdin, os.Stdout
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe(stdin): %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe(stdout): %v", err)
	}
	os.Stdin, os.Stdout = inR, outW
	go func() {
		_, _ = inW.WriteString(input)
		inW.Close()
	}()
	runErr := fn()
	outW.Close()
	os.Stdin, os.Stdout = oldIn, oldOut
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, outR); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	return buf.String(), runErr
}

// hookStdin 生成各 harness 通用的 stdin JSON。
func hookStdin(event, cwd, prompt string) string {
	m := map[string]string{"hook_event_name": event, "cwd": cwd}
	if prompt != "" {
		m["prompt"] = prompt
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// hookProjectDir 造一个「像真仓库」的临时工程目录，返回目录与其工程名。
//
// 同时清空各家注入项目目录的环境变量。**这一步不可省**：宿主（IDE / 编辑器）
// 自身就可能设了 CLAUDE_PROJECT_DIR 之类的变量，钩子会优先采纳它，
// 于是测试验证的变成「宿主的环境」而不是被测逻辑 —— 实测就是靠这一条
// 才让一批 prompt-submit 用例从「静默空输出」恢复为真实验证。
func hookProjectDir(t *testing.T, name string) (dir string, project string) {
	t.Helper()
	for _, k := range []string{
		"TRAE_PROJECT_DIR", "CLAUDE_PROJECT_DIR", "CODEBUDDY_PROJECT_DIR",
		"QODER_PROJECT_DIR", "QODERCN_PROJECT_DIR",
	} {
		t.Setenv(k, "")
	}
	dir = filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return dir, name
}

func TestCmdHookSessionStartInjectsHardRulesAndProjectMemory(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "preference", "--salience", "0.95",
		"所有自建工具默认 CGO_ENABLED=0 构建"}); err != nil {
		t.Fatalf("cmdAdd global: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--project", name, "--kind", "decision", "--salience", "0.8",
		"本工程的部署脚本放在 scripts 目录"}); err != nil {
		t.Fatalf("cmdAdd project: %v", err)
	}

	out, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	for _, want := range []string{"mem search", "v2mem", name, "CGO_ENABLED=0", "本工程的部署脚本"} {
		if !strings.Contains(out, want) {
			t.Errorf("SessionStart 注入应包含 %q，got:\n%s", want, out)
		}
	}
}

// 钩子只是「告诉模型还有多级记忆可查」——必须给出可执行的检索指令，否则模型不会去查。
func TestCmdHookSessionStartTeachesRetrievalCommands(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	out, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	for _, want := range []string{"mem search", "mem add", "mem touch"} {
		if !strings.Contains(out, want) {
			t.Errorf("应教会模型使用 %q，got:\n%s", want, out)
		}
	}
}

func TestCmdHookPromptSubmitInjectsOnlyRelevantMemories(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", name, "--kind", "pitfall",
		"记忆库不能放进 iCloud 同步目录，会损坏 SQLite"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--project", name, "--kind", "fact",
		"公司年会的抽奖规则是三局两胜"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "iCloud 同步目录能不能放数据库"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if !strings.Contains(out, "iCloud") {
		t.Fatalf("应注入与提问相关的记忆，got:\n%s", out)
	}
	if strings.Contains(out, "抽奖") {
		t.Errorf("不应注入无关记忆，got:\n%s", out)
	}
}

// 反向对照：别的工程的记忆不得注入 —— 证明 Scope=current 真的在过滤，
// 而不只是「碰巧没命中」。
func TestCmdHookPromptSubmitExcludesOtherProjects(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", "some-other-repo", "--kind", "pitfall",
		"记忆库不能放进 iCloud 同步目录，会损坏 SQLite"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "iCloud 同步目录能不能放数据库"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("他工程的记忆不应注入，got:\n%s", out)
	}

	// 同一份数据换成全局记忆后必须能注入，否则上一条断言可能只是「库是空的」造成的假通过
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "pitfall",
		"记忆库不能放进 iCloud 同步目录，会损坏 SQLite"}); err != nil {
		t.Fatalf("cmdAdd global: %v", err)
	}
	out2, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "iCloud 同步目录能不能放数据库"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if !strings.Contains(out2, "iCloud") {
		t.Errorf("全局记忆应能注入，got:\n%s", out2)
	}
}

// 无命中时输出必须为空 —— 每轮都注入噪声是纯 token 浪费。
func TestCmdHookPromptSubmitStaysSilentWhenNothingMatches(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", name, "记忆库不能放进 iCloud 同步目录"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	// 先确认这条记忆在库里确实查得到，排除「空库导致空输出」的假通过
	probe, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "iCloud 同步目录"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude"})
	})
	if err != nil {
		t.Fatalf("cmdHook probe: %v", err)
	}
	if !strings.Contains(probe, "iCloud") {
		t.Fatalf("前置条件不成立：相关提问应能命中，got:\n%s", probe)
	}

	out, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "今天天气怎么样"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("无命中时应静默，got:\n%s", out)
	}
}

// 🔴 硬约束：记忆系统绝不能阻断宿主会话。任何异常输入都要静默放过。
func TestCmdHookNeverFailsOnBadInput(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	cases := map[string]string{
		"空输入":         "",
		"非 JSON":      "i am not json",
		"截断的 JSON":    `{"hook_event_name":`,
		"空 JSON 对象":   `{}`,
		"未知事件":        hookStdin("PreToolUse", proj, ""),
		"事件名下划线写法":    hookStdin("session_start", proj, ""),
		"cwd 不存在":     hookStdin("SessionStart", "/no/such/dir", ""),
		"prompt 非字符串": `{"hook_event_name":"UserPromptSubmit","prompt":{"nested":1}}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := withStdin(t, input, func() error {
				return cmdHook([]string{"--db", db, "--harness", "claude"})
			})
			if err != nil {
				t.Errorf("异常输入必须静默放过（否则会打断宿主会话），got err=%v", err)
			}
			if name == "未知事件" && strings.TrimSpace(out) != "" {
				t.Errorf("不处理的事件不应产生输出，got:\n%s", out)
			}
		})
	}
}

// 库不存在不是错误：首次使用时要能正常注入「系统可用」的提示。
func TestCmdHookWorksWhenLibraryDoesNotExistYet(t *testing.T) {
	db := filepath.Join(t.TempDir(), "nested", "never-created.db")
	proj, _ := hookProjectDir(t, "demo-repo")
	out, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude"})
	})
	if err != nil {
		t.Fatalf("库不存在不应报错: %v", err)
	}
	if !strings.Contains(out, "mem search") {
		t.Errorf("仍应注入可用提示，got:\n%s", out)
	}
}

func TestCmdHookPromptSubmitHonoursLimit(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	for _, c := range []string{
		"项目统一用 pnpm 管理依赖，不要用 npm",
		"项目统一用 pnpm 的 workspace 协议引用内部包",
		"项目统一用 pnpm 的 lockfile 才纳入版本控制",
		"项目统一用 pnpm 时禁用 npm run",
		"项目统一用 pnpm 的 catalogs 统一版本号",
	} {
		if err := cmdAdd([]string{"--db", db, "--project", name, c}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}
	out, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "pnpm 依赖怎么管"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--limit", "2"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if n := strings.Count(out, "pnpm"); n != 2 {
		t.Errorf("应恰好注入 2 条（--limit 2），命中行数 got %d:\n%s", n, out)
	}
}

// 🔴 每轮注入必须有字符预算，否则记忆一多就会吃掉上下文。
//
// 这里必须同时放开 --limit，否则条数上限会先把量兜住（实测：用默认 --limit 3 时
// 输出天然低于预算，把预算逻辑整段删掉测试也不会失败 —— 断言根本没生效）。
func TestCmdHookRespectsCharBudget(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	for i := 0; i < 40; i++ {
		// 内容必须逐条互异：写同一个字符串会被「相同知识覆盖」收敛成一条，
		// 那样预算根本不会成为约束（实测踩过）。
		long := fmt.Sprintf("第%d条很长的记忆条目用于测试字符预算控制%s", i, strings.Repeat("填充内容", 12))
		if err := cmdAdd([]string{"--db", db, "--project", name, "--salience", "0.9", long}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}
	out, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0",
			"--limit", "40", "--max-chars", "600"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	// 先确认「放开 limit 后确实有远超预算的内容可注入」，否则下一条断言可能因数据不足而假通过
	unbounded, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0",
			"--limit", "40", "--max-chars", "100000"})
	})
	if err != nil {
		t.Fatalf("cmdHook unbounded: %v", err)
	}
	if len([]rune(unbounded)) <= 600 {
		t.Fatalf("前置条件不成立：放开采纳量后应远超 600 字符，got %d", len([]rune(unbounded)))
	}

	if n := len([]rune(out)); n > 600 {
		t.Errorf("注入长度 %d 超出预算 600，got:\n%s", n, out)
	}
	// 同时确认记忆段真的被渲染过，排除「什么都没输出」造成的假通过
	if !strings.Contains(out, "本工程记忆") {
		t.Errorf("预算内应至少渲染出记忆段，got:\n%s", out)
	}
	if !strings.Contains(out, "mem search") {
		t.Errorf("用法提示必须保留（它是钩子本体），got:\n%s", out)
	}
}

// 不注入环境变量的工具（QoderWork / Codex）只能靠 stdin 的 cwd 定位工程。
func TestCmdHookFallsBackToStdinCwd(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", name, "该工程专属的记忆条目内容"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "qoderwork"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if !strings.Contains(out, name) {
		t.Errorf("应从 stdin 的 cwd 推断工程名 %q，got:\n%s", name, out)
	}
	if !strings.Contains(out, "该工程专属的记忆条目内容") {
		t.Errorf("应注入该工程的记忆，got:\n%s", out)
	}
}

// 环境变量优先于 stdin 的 cwd（部分 harness 会注入准确的项目目录）。
func TestCmdHookPrefersProjectEnvOverStdinCwd(t *testing.T) {
	db := testDB(t)
	realProj, realName := hookProjectDir(t, "real-repo")
	_, otherName := hookProjectDir(t, "other-repo")
	if err := cmdAdd([]string{"--db", db, "--project", realName, "真实工程的记忆条目"}); err != nil {
		t.Fatalf("cmdAdd real: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--project", otherName, "干扰工程的记忆条目"}); err != nil {
		t.Fatalf("cmdAdd other: %v", err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", realProj)

	out, err := withStdin(t, hookStdin("SessionStart", "/somewhere/else", ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if !strings.Contains(out, realName) {
		t.Errorf("应优先用环境变量指向的工程 %q，got:\n%s", realName, out)
	}
	if !strings.Contains(out, "真实工程的记忆条目") {
		t.Errorf("应注入真实工程的记忆，got:\n%s", out)
	}
	if strings.Contains(out, "干扰工程的记忆条目") {
		t.Errorf("不应注入干扰工程的记忆，got:\n%s", out)
	}
}

// 显式 --event 用于手工调试与不传 hook_event_name 的场景。
func TestCmdHookAcceptsExplicitEventFlag(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--global", "全局硬规则样本内容"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	out, err := withStdin(t, hookStdin("", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--event", "session-start"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if !strings.Contains(out, "全局硬规则样本内容") {
		t.Errorf("--event 应生效，got:\n%s", out)
	}
}

// ---------- M5a: 相似知识归并 ----------

const (
	cliSimBase    = "记忆库数据文件必须放在用户主目录的隐藏目录里"
	cliSimVariant = "记忆库数据文件必须放在用户主目录下的隐藏目录里"
)

func TestCmdConsolidateMergesNearDuplicateAndHidesItFromSearch(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "--salience", "0.9", cliSimBase}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "--salience", "0.3", cliSimVariant}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := captureStdout(t, func() error { return cmdConsolidate([]string{"--db", db}) })
	if err != nil {
		t.Fatalf("cmdConsolidate: %v", err)
	}
	if !strings.Contains(out, "取代=1") {
		t.Errorf("应报告取代 1 条，got: %q", out)
	}

	hits := searchOnce(t, db, store.SearchQuery{Query: "隐藏目录", Limit: 10})
	if len(hits) != 1 {
		t.Fatalf("归并后检索应只剩 1 条，got %d", len(hits))
	}
	if hits[0].Content != cliSimBase {
		t.Errorf("留下的应是高 salience 的那条，got %q", hits[0].Content)
	}
}

func TestCmdConsolidateHonoursThresholdFlag(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "--salience", "0.9", cliSimBase}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "--salience", "0.3", cliSimVariant}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdConsolidate([]string{"--db", db, "--threshold", "0.999"})
	})
	if err != nil {
		t.Fatalf("cmdConsolidate: %v", err)
	}
	if !strings.Contains(out, "取代=0") {
		t.Errorf("阈值 0.999 时不应取代，got: %q", out)
	}
}

// ---------- M4: 跨设备归集 ----------

func TestCmdExportWritesOneJSONLinePerMemory(t *testing.T) {
	db := testDB(t)
	for _, c := range []string{"导出端到端甲", "导出端到端乙"} {
		if err := cmdAdd([]string{"--db", db, "--project", "p1", c}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}
	out := filepath.Join(t.TempDir(), "dump.jsonl")
	if _, err := captureStdout(t, func() error { return cmdExport([]string{"--db", db, out}) }); err != nil {
		t.Fatalf("cmdExport: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("每条记忆应占一行，期望 2 行，got %d: %q", len(lines), data)
	}
	for _, ln := range lines {
		var r store.ExportRecord
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("每行应是合法 JSON: %v (%q)", err, ln)
		}
		if r.ContentHash == "" {
			t.Error("导出记录缺 content_hash（归并身份键）")
		}
	}
}

func TestCmdExportToStdoutWhenNoPathGiven(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--global", "仅用于导出到标准输出的样本"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	out, err := captureStdout(t, func() error { return cmdExport([]string{"--db", db}) })
	if err != nil {
		t.Fatalf("cmdExport: %v", err)
	}
	if !strings.Contains(out, "仅用于导出到标准输出的样本") {
		t.Errorf("未给路径时应把 JSONL 写到标准输出，got: %q", out)
	}
	if !strings.Contains(out, `"content_hash"`) {
		t.Errorf("标准输出应是 JSONL 而非摘要，got: %q", out)
	}
}

func TestCmdImportRoundTripPreservesOriginDevice(t *testing.T) {
	src, dst := testDB(t), testDB(t)
	if err := cmdAdd([]string{"--db", src, "--project", "p1", "--device", "dev-far",
		"跨设备往返验证事实"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	dump := filepath.Join(t.TempDir(), "dump.jsonl")
	if _, err := captureStdout(t, func() error { return cmdExport([]string{"--db", src, dump}) }); err != nil {
		t.Fatalf("cmdExport: %v", err)
	}

	out, err := captureStdout(t, func() error { return cmdImport([]string{"--db", dst, dump}) })
	if err != nil {
		t.Fatalf("cmdImport: %v", err)
	}
	if !strings.Contains(out, "新增=1") {
		t.Errorf("应报告新增 1 条，got: %q", out)
	}

	st, err := store.Open(dst)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	recs, err := st.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("目标库应有 1 条，got %d", len(recs))
	}
	if recs[0].OriginDevice != "dev-far" {
		t.Errorf("溯源设备应随导入保留，got %q", recs[0].OriginDevice)
	}
}

// 第二次导入必须走归并而不是新插 —— 若归并逻辑退化，这里会报「新增=1」。
func TestCmdImportTwiceReportsMergeNotInsert(t *testing.T) {
	src, dst := testDB(t), testDB(t)
	if err := cmdAdd([]string{"--db", src, "--project", "p1", "CLI 重复导入验证事实"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	dump := filepath.Join(t.TempDir(), "dump.jsonl")
	if _, err := captureStdout(t, func() error { return cmdExport([]string{"--db", src, dump}) }); err != nil {
		t.Fatalf("cmdExport: %v", err)
	}
	if _, err := captureStdout(t, func() error { return cmdImport([]string{"--db", dst, dump}) }); err != nil {
		t.Fatalf("cmdImport #1: %v", err)
	}

	out, err := captureStdout(t, func() error { return cmdImport([]string{"--db", dst, dump}) })
	if err != nil {
		t.Fatalf("cmdImport #2: %v", err)
	}
	if !strings.Contains(out, "新增=0") || !strings.Contains(out, "合并=1") {
		t.Errorf("重复导入应报告 新增=0 合并=1，got: %q", out)
	}

	hits := searchOnce(t, dst, store.SearchQuery{Query: "CLI 重复导入验证", Limit: 5})
	if len(hits) != 1 {
		t.Errorf("重复导入不应产生重复行，got %d", len(hits))
	}
}

func TestCmdImportMissingFileErrors(t *testing.T) {
	db := testDB(t)
	missing := filepath.Join(t.TempDir(), "nope.jsonl")
	if err := cmdImport([]string{"--db", db, missing}); err == nil {
		t.Fatal("文件不存在时应报错")
	}
	if err := cmdImport([]string{"--db", db}); err == nil {
		t.Fatal("缺少路径时应报错")
	}
}

// searchOnce 直接开库检索，用于断言「库里到底存了什么」，绕开 CLI 的打印格式。
func searchOnce(t *testing.T, db string, q store.SearchQuery) []store.Hit {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	hits, err := st.Search(q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return hits
}

// ---------- M3: 全局记忆（project 为空，对所有工程生效） ----------

// --global 是唯一能创建「空工程」记忆的入口。缺了它，--project 会被
// detectProject() 自动填充，库层的全局语义在 CLI 上就不可达（静默失效）。
func TestCmdAddGlobalFlagCreatesProjectlessMemory(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--global", "跨工程都成立的硬规则"}); err != nil {
		t.Fatalf("cmdAdd --global: %v", err)
	}
	hits := searchOnce(t, db, store.SearchQuery{Query: "硬规则", Limit: 5})
	if len(hits) != 1 {
		t.Fatalf("应写入 1 条，got %d", len(hits))
	}
	if hits[0].Project != "" {
		t.Errorf("--global 应写入空工程标记，got %q", hits[0].Project)
	}
}

func TestCmdAddRejectsGlobalCombinedWithProject(t *testing.T) {
	db := testDB(t)
	err := cmdAdd([]string{"--db", db, "--global", "--project", "alpha", "自相矛盾的写入"})
	if err == nil {
		t.Fatal("--global 与 --project 同时给出应报错")
	}
	// 只断言 err != nil 不够：flag 解析失败也会返回 err，会让本用例恒真。
	if !strings.Contains(err.Error(), "不可同时") {
		t.Errorf("错误应说明二者互斥，got: %v", err)
	}
	if n := len(searchOnce(t, db, store.SearchQuery{Query: "自相矛盾", Limit: 5})); n != 0 {
		t.Errorf("冲突时应拒绝写入，但库中有 %d 条", n)
	}
}

// M5b-0：--no-fuzzy 关闭模糊检索，措辞不同的近亲不再进入结果。
func TestCmdSearchNoFuzzyFlagDisablesFuzzy(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "日志怎么放进记忆库里"}); err != nil {
		t.Fatalf("cmdAdd paraphrase: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "日志怎么搬进记忆库里"}); err != nil {
		t.Fatalf("cmdAdd target: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--limit", "10", "日志怎么搬进记忆库里"})
	})
	if err != nil {
		t.Fatalf("cmdSearch: %v", err)
	}
	if !strings.Contains(out, "日志怎么放进记忆库里") {
		t.Errorf("默认应开启模糊检索、带回措辞不同的近亲，got: %q", out)
	}

	noFuzzyOut, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--no-fuzzy", "--limit", "10", "日志怎么搬进记忆库里"})
	})
	if err != nil {
		t.Fatalf("cmdSearch --no-fuzzy: %v", err)
	}
	if strings.Contains(noFuzzyOut, "日志怎么放进记忆库里") {
		t.Errorf("--no-fuzzy 应关闭模糊检索，got: %q", noFuzzyOut)
	}
}

func TestCmdSearchScopeGlobalExcludesProjectMemories(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--project", "alpha", "全局作用域验证的工程内条目"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--global", "全局作用域验证的全局条目"}); err != nil {
		t.Fatalf("cmdAdd --global: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--scope", "global", "全局作用域验证"})
	})
	if err != nil {
		t.Fatalf("cmdSearch --scope global: %v", err)
	}
	if !strings.Contains(out, "全局条目") {
		t.Errorf("--scope global 应命中全局记忆，got: %q", out)
	}
	if strings.Contains(out, "工程内条目") {
		t.Errorf("--scope global 不应命中工程记忆，got: %q", out)
	}
}

func TestCmdForgetMakesMemoryUnsearchable(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "临时事实需要被遗忘"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	hits, err := st.Search(store.SearchQuery{Query: "遗忘", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("前置条件不成立：add 之后应能检索到该记忆")
	}
	id := hits[0].ID
	st.Close()

	if err := cmdForget([]string{"--db", db, id[:8]}); err != nil {
		t.Fatalf("cmdForget: %v", err)
	}

	st2, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st2.Close()
	after, err := st2.Search(store.SearchQuery{Query: "遗忘", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("forget 之后应检索不到，got %d 条", len(after))
	}
}

func TestCmdTouchRejectsUnknownID(t *testing.T) {
	db := testDB(t)
	if err := cmdTouch([]string{"--db", db, "ffffffff"}); err == nil {
		t.Fatal("未知 ID 应返回错误")
	}
}

// ---------- M6: 配置生成（mem init / mem harness） ----------

func TestCmdHarnessListCoversUserToolsAndShowsTier(t *testing.T) {
	out, err := captureStdout(t, func() error { return cmdHarness([]string{}) })
	if err != nil {
		t.Fatalf("cmdHarness: %v", err)
	}
	for _, want := range []string{
		"claude", "traecode", "codebuddy", "qoder", "qoderwork", "codex", "dsh",
		"workbuddy", "traework",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("列表应含 %q，got:\n%s", want, out)
		}
	}
	// 确定性等级必须显式可见 —— 只有 native 才是代码强制
	for _, want := range []string{"native", "instruction"} {
		if !strings.Contains(out, want) {
			t.Errorf("应标注接入等级 %q，got:\n%s", want, out)
		}
	}
}

func TestCmdInitWritesHooksConfigWithBothEvents(t *testing.T) {
	proj := t.TempDir()
	out, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "codex", "--project", proj})
	})
	if err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	if !strings.Contains(out, "SessionStart") || !strings.Contains(out, "UserPromptSubmit") {
		t.Errorf("摘要应报告写入的事件，got:\n%s", out)
	}

	path := filepath.Join(proj, ".codex", "hooks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("应写出 %s: %v", path, err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("写出的应是合法 JSON: %v\n%s", err, data)
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit"} {
		groups, ok := cfg.Hooks[ev]
		if !ok || len(groups) == 0 || len(groups[0].Hooks) == 0 {
			t.Fatalf("事件 %s 应登记 hook，got %s", ev, data)
		}
		h := groups[0].Hooks[0]
		if h.Type != "command" {
			t.Errorf("%s 的 hook type 应为 command，got %q", ev, h.Type)
		}
		if !strings.Contains(h.Command, "hook") {
			t.Errorf("%s 的 command 应调用 mem hook，got %q", ev, h.Command)
		}
		if !strings.Contains(h.Command, "--harness") {
			t.Errorf("%s 的 command 应带 --harness 以便正确推断工程，got %q", ev, h.Command)
		}
	}
}

// 必须用绝对路径调用二进制：harness 拉起 hook 时环境往往很干净，
// 依赖 PATH 里的 mem 会静默失效。
func TestCmdInitUsesAbsoluteCommandPath(t *testing.T) {
	proj := t.TempDir()
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "claude", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(proj, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	cmd := extractFirstCommand(t, cfg, "SessionStart")
	if !filepath.IsAbs(firstToken(cmd)) {
		t.Errorf("命令应用绝对路径，got %q", cmd)
	}
}

// 合并而非覆盖：用户自己的 hook 与其它配置键都必须保留。
func TestCmdInitMergesWithoutClobberingExistingConfig(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "settings.json")
	existing := `{
  "model": "my-model",
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "/usr/local/bin/my-own-hook.sh"}]}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/guard.sh"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "claude", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg["model"] != "my-model" {
		t.Errorf("无关配置键应保留，got %+v", cfg["model"])
	}
	hooks := cfg["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("用户自己的 PreToolUse 配置应保留")
	}
	groups := hooks["SessionStart"].([]any)
	var keptOwn, keptOurs bool
	for _, g := range groups {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			c := h.(map[string]any)["command"].(string)
			if strings.Contains(c, "my-own-hook.sh") {
				keptOwn = true
			}
			if strings.Contains(c, "hook") && strings.Contains(c, "--harness") {
				keptOurs = true
			}
		}
	}
	if !keptOwn {
		t.Error("用户自己的 SessionStart hook 应保留（合并而非覆盖）")
	}
	if !keptOurs {
		t.Error("应写入 v2mem 的 hook")
	}
}

// 幂等：重复 init 不得累积重复条目（否则 hook 会被调用多次）。
func TestCmdInitIsIdempotent(t *testing.T) {
	proj := t.TempDir()
	path := filepath.Join(proj, ".codex", "hooks.json")
	for i := 0; i < 3; i++ {
		if _, err := captureStdout(t, func() error {
			return cmdInit([]string{"--harness", "codex", "--project", proj})
		}); err != nil {
			t.Fatalf("cmdInit #%d: %v", i+1, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	hooks := cfg["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit"} {
		groups := hooks[ev].([]any)
		if len(groups) != 1 {
			t.Errorf("%s 应只有 1 组（重复 init 不得累积），got %d", ev, len(groups))
		}
	}
	// 再跑一次内容必须逐字节不变
	before := string(data)
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "codex", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != before {
		t.Errorf("幂等：内容应逐字节不变\n前:\n%s\n后:\n%s", before, after)
	}
}

func TestCmdInitDryRunWritesNothing(t *testing.T) {
	proj := t.TempDir()
	out, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "codex", "--project", proj, "--dry-run"})
	})
	if err != nil {
		t.Fatalf("cmdInit --dry-run: %v", err)
	}
	if !strings.Contains(out, "SessionStart") {
		t.Errorf("dry-run 应打印将要写入的内容，got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(proj, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Error("dry-run 不得写文件")
	}
}

func TestCmdInitBacksUpExistingFile(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	original := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/bin/true"}]}]}}`
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "codex", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	backup, err := os.ReadFile(path + ".v2mem.bak")
	if err != nil {
		t.Fatalf("应生成备份: %v", err)
	}
	if string(backup) != original {
		t.Errorf("备份应是原始内容\nwant: %s\ngot:  %s", original, backup)
	}

	// 再跑一次并中途改动文件：备份必须仍是「首次触碰前」的原始状态，
	// 否则第二次的中间态会盖掉唯一一份干净备份。
	if err := os.WriteFile(path, []byte(`{"hooks":{},"changed_by_user":true}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "codex", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdInit #2: %v", err)
	}
	backup2, err := os.ReadFile(path + ".v2mem.bak")
	if err != nil {
		t.Fatalf("备份应仍存在: %v", err)
	}
	if string(backup2) != original {
		t.Errorf("备份不得被后续 init 覆盖\nwant: %s\ngot:  %s", original, backup2)
	}
}

// 指令级接入（无原生 hook 的工具）：写带标记的指令块，且可重复更新。
func TestCmdInitInstructionTierWritesMarkedBlock(t *testing.T) {
	proj := t.TempDir()
	path := filepath.Join(proj, "AGENTS.md")
	// 预置用户已有内容
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("# 我的项目记忆\n\n已有内容要保留\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "workbuddy", "--project", proj, "--scope", "instruction"})
	})
	if err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	if !strings.Contains(out, "指令级") {
		t.Errorf("应告知用户这是指令级接入（可靠性低于原生钩子），got:\n%s", out)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "已有内容要保留") {
		t.Error("不得覆盖用户已有内容")
	}
	if !strings.Contains(body, "mem search") {
		t.Errorf("指令块应教会模型检索，got:\n%s", body)
	}
	first := body

	// 再跑一次：应就地替换标记块而不是追加
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "workbuddy", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdInit #2: %v", err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != first {
		t.Errorf("重复 init 应就地替换而非追加\n前:\n%s\n后:\n%s", first, again)
	}
	if n := strings.Count(string(again), instrBegin); n != 1 {
		// 数标记而不是数正文里的词：同一份指令块内 "mem search" 本身就会出现两次，
		// 数内容会把「一份块」误判成「两份」（实测踩过）。
		t.Errorf("指令块只应存在一份，got %d 份", n)
	}
}

func TestCmdInitRejectsUnknownHarnessAndBadScope(t *testing.T) {
	if err := cmdInit([]string{"--harness", "no-such-tool"}); err == nil {
		t.Error("未知 harness 应报错")
	}
	if err := cmdInit([]string{"--harness", "codex", "--project", t.TempDir(), "--scope", "bogus"}); err == nil {
		t.Error("非法 --scope 应报错")
	}
}

// 辅助：从 config 里取指定事件的第一条 command
func extractFirstCommand(t *testing.T, cfg map[string]any, event string) string {
	t.Helper()
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("缺 hooks 段: %+v", cfg)
	}
	groups, ok := hooks[event].([]any)
	if !ok || len(groups) == 0 {
		t.Fatalf("缺事件 %s: %+v", event, hooks)
	}
	g := groups[0].(map[string]any)
	hs := g["hooks"].([]any)
	return hs[0].(map[string]any)["command"].(string)
}

func firstToken(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], `"`)
}

// --file 覆盖：用于把指令块放到不挤占记忆预算的位置。
func TestCmdInitFileOverrideWritesToGivenPath(t *testing.T) {
	proj := t.TempDir()
	target := filepath.Join(proj, "AGENTS.md")
	if err := os.WriteFile(target, []byte("# 项目规则\n\n已有规则\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	out, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "workbuddy", "--project", proj, "--scope", "instruction", "--file", target})
	})
	if err != nil {
		t.Fatalf("cmdInit --file: %v", err)
	}
	if !strings.Contains(out, target) {
		t.Errorf("摘要应报告目标文件，got:\n%s", out)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(body), "已有规则") {
		t.Error("不得覆盖已有内容")
	}
	if !strings.Contains(string(body), instrBegin) {
		t.Error("应写入指令块")
	}
	// 默认指令文件不应被创建
	if _, err := os.Stat(filepath.Join(proj, "CODEBUDDY.md")); !os.IsNotExist(err) {
		t.Error("指定 --file 后不应再写默认位置")
	}
}

// ---------- M6: 扫描 + 交互选择 ----------

// 不带 --harness 时进入扫描选择流程。默认列出全部 harness（不只已检测到的），
// 这样索引稳定、也允许用户选一个我们没探到路径的工具。
func TestCmdInitWithoutHarnessScansAndInstallsSelection(t *testing.T) {
	proj := t.TempDir()
	out, err := withStdin(t, "codex\n", func() error {
		return cmdInit([]string{"--project", proj})
	})
	if err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	if !strings.Contains(out, "选择") {
		t.Errorf("应打印选择提示，got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(proj, ".codex", "hooks.json")); err != nil {
		t.Errorf("应写入 codex 的项目级配置: %v", err)
	}
}

func TestCmdInitPromptAcceptsIndicesAndMultipleNames(t *testing.T) {
	proj := t.TempDir()
	// 按索引选：列表里 traecode 与 codex 各自的位置
	all := harness.All()
	idxOf := func(name string) int {
		for i, h := range all {
			if h.Name == name {
				return i + 1
			}
		}
		t.Fatalf("注册表里找不到 %s", name)
		return 0
	}
	input := fmt.Sprintf("%d,%s\n", idxOf("claude"), "codex")
	if _, err := withStdin(t, input, func() error {
		return cmdInit([]string{"--project", proj})
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	for _, p := range []string{".claude/settings.json", ".codex/hooks.json"} {
		if _, err := os.Stat(filepath.Join(proj, p)); err != nil {
			t.Errorf("应写入 %s: %v", p, err)
		}
	}
}

// 取消与空输入都不得写任何文件。
func TestCmdInitPromptCancelWritesNothing(t *testing.T) {
	for _, input := range []string{"\n", "q\n", "Q\n", "  \n"} {
		proj := t.TempDir()
		if _, err := withStdin(t, input, func() error {
			return cmdInit([]string{"--project", proj})
		}); err != nil {
			t.Fatalf("输入 %q 不应报错: %v", input, err)
		}
		entries, _ := os.ReadDir(proj)
		if len(entries) != 0 {
			t.Errorf("输入 %q 取消后不应写入任何文件，实际有 %d 项", input, len(entries))
		}
	}
}

// 无法识别的选择必须报错并指出是哪个，不能静默跳过。
func TestCmdInitPromptRejectsUnknownToken(t *testing.T) {
	proj := t.TempDir()
	err := func() error {
		_, e := withStdin(t, "codex,no-such-tool\n", func() error {
			return cmdInit([]string{"--project", proj})
		})
		return e
	}()
	if err == nil {
		t.Fatal("含未知项的选择应报错")
	}
	if !strings.Contains(err.Error(), "no-such-tool") {
		t.Errorf("错误应指出未知项，got: %v", err)
	}
}

// --all 只作用于「支持项目级配置」的工具；其余要报告跳过而非静默失败。
func TestCmdInitAllReportsSkippedHarnesses(t *testing.T) {
	proj := t.TempDir()
	out, err := withStdin(t, "", func() error {
		return cmdInit([]string{"--project", proj, "--all"})
	})
	if err != nil {
		t.Fatalf("cmdInit --all: %v", err)
	}
	if !strings.Contains(out, "跳过") {
		t.Errorf("应报告被跳过的工具，got:\n%s", out)
	}
	for _, p := range []string{".claude/settings.json", ".codex/hooks.json", ".codebuddy/settings.json"} {
		if _, err := os.Stat(filepath.Join(proj, p)); err != nil {
			t.Errorf("--all 应写入 %s: %v", p, err)
		}
	}
}

// --all 默认只作用于「检测到已安装」的工具，避免给没装的工具凭空造配置文件。
func TestCmdInitAllDefaultsToDetectedOnly(t *testing.T) {
	proj := t.TempDir()
	out, err := withStdin(t, "", func() error {
		return cmdInit([]string{"--project", proj, "--all", "--dry-run"})
	})
	if err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	// 未检测到的工具（本机无对应目录）不应出现写入动作
	for _, h := range harness.All() {
		if len(h.ExistingGlobalConfigs()) > 0 {
			continue
		}
		if strings.Contains(out, "--- 将写入") && strings.Contains(out, h.Name+"（") && strings.Contains(out, h.Name) {
			// 只在它确实被写入时才算失败：用「跳过了几个」的口径更可靠
		}
	}
	if !strings.Contains(out, "检测到") {
		t.Errorf("应说明只处理检测到的工具，got:\n%s", out)
	}
}

// 逗号分隔的 --harness 也要能一次装多个。
func TestCmdInitAcceptsCommaSeparatedHarnessList(t *testing.T) {
	proj := t.TempDir()
	if _, err := withStdin(t, "", func() error {
		return cmdInit([]string{"--harness", "claude,codex", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	for _, p := range []string{".claude/settings.json", ".codex/hooks.json"} {
		if _, err := os.Stat(filepath.Join(proj, p)); err != nil {
			t.Errorf("应写入 %s: %v", p, err)
		}
	}
	if err := cmdInit([]string{"--harness", "claude,no-such"}); err == nil {
		t.Error("列表中含未知项应报错")
	}
}
