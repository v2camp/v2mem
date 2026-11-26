package main

// 候选裁决自动化闭环（docs/design-write-governance.md §12）的四条落点：
// ① SessionStart 按工程注入待裁决计数 ② MCP 显式裁决三工具
// ③ review ls --state pending 就近查看 ④ 采纳率判据的采样前提（纯文档，无测试）。

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

type candSpec struct {
	project string
	session string
	content string
}

func seedVia(t *testing.T, st *store.Store, spec []candSpec) {
	t.Helper()
	for _, c := range spec {
		if _, err := st.InsertCandidate(store.CandidateInput{
			SessionID: c.session, Project: c.project, Kind: "decision",
			Content: c.content, Source: "distill",
		}); err != nil {
			t.Fatalf("InsertCandidate(%s/%s): %v", c.project, c.session, err)
		}
	}
}

func twoProjectSpec(self, other string) []candSpec {
	return []candSpec{
		{self, "s1", self + " 决策一"},
		{self, "s2", self + " 决策二"},
		{other, "s3", other + " 决策一"},
		{other, "s4", other + " 决策二"},
		{other, "s5", other + " 决策三"},
	}
}

func sessionStartOut(t *testing.T, db, proj string, maxChars ...int) string {
	t.Helper()
	t.Setenv("MEM_NO_AUTOMATION", "1") // 只测注入，不拉起写侧子进程
	args := []string{
		"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-",
	}
	if len(maxChars) > 0 {
		args = append(args, "--max-chars", strconv.Itoa(maxChars[0]))
	}
	out, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook(args)
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	return out
}

// ① 注入只报本工程计数，不全局累加；且必须在 600 字符预算内完整存活。
func TestHookSessionStartPendingLineIsProjectScoped(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "alpha-repo")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	seedVia(t, st, twoProjectSpec(name, "beta-repo"))
	st.Close()

	out := sessionStartOut(t, db, proj)
	if !strings.Contains(out, "[待裁决]") {
		t.Fatalf("有 pending 时应注入待裁决提示，got:\n%s", out)
	}
	if !strings.Contains(out, "本工程 2 条") {
		t.Errorf("应报本工程 2 条，got:\n%s", out)
	}
	if strings.Contains(out, "本工程 5 条") {
		t.Error("不得把别工程候选全局累加")
	}
	for _, want := range []string{"review_ls", "mem review ls", "mem review withdraw"} {
		if !strings.Contains(out, want) {
			t.Errorf("提示应含可执行入口 %q", want)
		}
	}
	if n := len([]rune(out)); n > defaultHookBudget {
		t.Errorf("注入长度 %d 超出默认预算 %d", n, defaultHookBudget)
	}
}

// 小预算回归：增量提示必须给类码表与 hint 让路，不能把注入撑破预算。
func TestHookSessionStartPendingLineYieldsToSmallBudget(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "alpha-repo")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	seedVia(t, st, twoProjectSpec(name, "beta-repo"))
	st.Close()

	out := sessionStartOut(t, db, proj, 600)
	if n := len([]rune(out)); n > 600 {
		t.Errorf("注入长度 %d 超出预算 600", n)
	}
	if !strings.Contains(out, "mem search") {
		t.Error("预算不足时 hint（钩子本体）必须存活")
	}
	if strings.Contains(out, "[待裁决]") {
		t.Error("预算不足时应放弃增量提示，而不是挤掉 hint")
	}
}

// 换个工程开会话，看到的是它自己的 3 条 —— 就近裁决的数据基础。
func TestHookSessionStartPendingLineFollowsProject(t *testing.T) {
	db := testDB(t)
	_, name := hookProjectDir(t, "alpha-repo")
	proj, _ := hookProjectDir(t, "beta-repo")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	seedVia(t, st, twoProjectSpec(name, "beta-repo"))
	st.Close()

	out := sessionStartOut(t, db, proj)
	if !strings.Contains(out, "本工程 3 条") {
		t.Errorf("beta-repo 会话应看到 3 条，got:\n%s", out)
	}
}

func TestHookSessionStartNoPendingLineWhenEmpty(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "empty-repo")
	out := sessionStartOut(t, db, proj)
	if strings.Contains(out, "[待裁决]") {
		t.Errorf("无 pending 时不应多注入一行，got:\n%s", out)
	}
}

// 失败开放：库已关闭 / 未识别工程，都不得让 hook 报错或注入半截提示。
func TestPendingCandidatesLineFailsOpen(t *testing.T) {
	st, err := store.Open(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if got := pendingCandidatesLine(st, "p"); got != "" {
		t.Errorf("查询失败应静默当 0，got %q", got)
	}
	if got := pendingCandidatesLine(st, ""); got != "" {
		t.Errorf("未识别工程不应提示，got %q", got)
	}
}

// ---------- ② MCP 复核视图（§12.3 统一后的落点，端到端用例在 mcp_test.go） ----------

func mcpErrCode(t *testing.T, s *mcpServer, name string, args map[string]any) int {
	t.Helper()
	resp := s.dispatch(mcpReq(1, "tools/call", map[string]any{"name": name, "arguments": args}))
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s 应返回错误，got %v", name, resp)
	}
	code, ok := errObj["code"].(int)
	if !ok {
		t.Fatalf("%s 错误码类型异常: %v", name, errObj)
	}
	return code
}

func mcpJSON(t *testing.T, result map[string]any, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(mcpText(result)), dst); err != nil {
		t.Fatalf("解析工具结果失败: %v / %s", err, mcpText(result))
	}
}

// ---------- ③ CLI 就近查看 ----------
