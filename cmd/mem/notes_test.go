package main

import (
	"strings"
	"testing"

	"github.com/wanghui/v2mem/internal/store"
)

// ---------- WS1.1: mem notes 硬规则小字条 ----------

// 幂等：同一库多次运行，输出必须逐字节一致（按稳定键排序）。
func TestNotesIdempotent(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule",
		"全局硬规则：commit 前必跑 make cover-gate"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule",
		"第二条规则，顺序不稳定测试"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out1, err := runNotes(db)
	if err != nil {
		t.Fatalf("runNotes: %v", err)
	}
	out2, err := runNotes(db)
	if err != nil {
		t.Fatalf("runNotes(2): %v", err)
	}
	if out1 != out2 {
		t.Fatalf("notes 应幂等：\n%q\n%q", out1, out2)
	}
}

// 抽取规则：kind=rule 或者「project 空 + salience 高」才算硬规则；
// 全局低重要性、工程级规则不算；结果按稳定键排序。
func TestNotesSelectsHardRules(t *testing.T) {
	db := testDB(t)
	// 规则类（kind=rule）应当入选
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule",
		"规则：提交信息必须用 conventional"}); err != nil {
		t.Fatalf("cmdAdd rule: %v", err)
	}
	// 全局 + 高重要性：红线类约定，应入选
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "preference", "--salience", "0.95",
		"红线：数据库不放代码目录"}); err != nil {
		t.Fatalf("cmdAdd high: %v", err)
	}
	// 全局 + 低重要性：不足阈值，不入选
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "preference", "--salience", "0.5",
		"普通偏好：首选 vim 编辑器"}); err != nil {
		t.Fatalf("cmdAdd low: %v", err)
	}
	// 工程级、非规则类记忆：非全局面即可被排除（普通项目记忆不入 notes）
	if err := cmdAdd([]string{"--db", db, "--project", "proj-a", "--kind", "preference",
		"工程专属偏好：本目录用 npm"}); err != nil {
		t.Fatalf("cmdAdd project: %v", err)
	}

	out, err := runNotes(db)
	if err != nil {
		t.Fatalf("runNotes: %v", err)
	}
	for _, want := range []string{"规则：提交信息必须用 conventional", "红线：数据库不放代码目录"} {
		if !strings.Contains(out, want) {
			t.Errorf("notes 应包含 %q，got:\n%s", want, out)
		}
	}
	for _, banned := range []string{"普通偏好：首选 vim 编辑器", "工程专属偏好：本目录用 npm"} {
		if strings.Contains(out, banned) {
			t.Errorf("notes 不应包含非硬规则 %q，got:\n%s", banned, out)
		}
	}

	// 幂等性的稳定性来源：重复调用输出恒定（排序稳定）
	out2, _ := runNotes(db)
	if out != out2 {
		t.Errorf("notes 应稳定，got:\n%q\n%q", out, out2)
	}
}

// cmdNotes 子命令输出一行一条，未命中时提示空。
func TestCmdNotesPrintsOneLinePerRule(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule", "规则甲"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule", "规则乙"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := captureStdout(t, func() error { return cmdNotes([]string{"--db", db}) })
	if err != nil {
		t.Fatalf("cmdNotes: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("应恰好 2 行，got %d: %q", len(lines), out)
	}
	if strings.TrimSpace(lines[0]) != "规则甲" && strings.TrimSpace(lines[0]) != "规则乙" {
		t.Errorf("第一行应为其中一条规则，got %q", lines[0])
	}

	// 空库提示
	emptyDB := testDB(t)
	emptyOut, err := captureStdout(t, func() error { return cmdNotes([]string{"--db", emptyDB}) })
	if err != nil {
		t.Fatalf("cmdNotes empty: %v", err)
	}
	if !strings.Contains(emptyOut, "没有硬规则") {
		t.Errorf("空库应提示无规则，got %q", emptyOut)
	}
}

// ---------- WS1.2: 写入分工 + MEM_NO_NOTES 回退 ----------

// 功能级验证去重：同一条规则既作为 hit 又已存在 notes 里时被去掉。
func TestFilterRulesCoveredByNotesDedups(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule",
		"commit 前必跑 make cover-gate"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	hits := []store.Hit{
		{Kind: "rule", Content: "commit 前必跑 make cover-gate", Project: ""},
		{Kind: "decision", Content: "部署用 scripts", Project: ""},
	}
	filtered := filterRulesCoveredByNotes(st, hits)
	if len(filtered) != 1 {
		t.Fatalf("应只剩 1 条非覆盖, got %d: %+v", len(filtered), filtered)
	}
	if filtered[0].Content != "部署用 scripts" {
		t.Errorf("应保留未覆盖的记忆，got %+v", filtered[0])
	}
}

// 行为级验证（钩子维度）：prompt-submit 命中一条 rule 时，默认（MEM_NO_NOTES 未置位）
// 因已被 notes 覆盖而不重复注入；置位 MEM_NO_NOTES 后回退到「仅 v2mem 原始注入」能注入。
func TestHookDedupsRuleAndMEMNoNotesReverts(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	ruleContent := "commit 前必跑 make cover-gate"
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule", ruleContent}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	// 默认：去重生效 -> 该规则不再被注入（notes 已覆盖）
	outDefault, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "commit 前要不要跑 cover"),
		func() error { return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0"}) })
	if err != nil {
		t.Fatalf("cmdHook default: %v", err)
	}
	if strings.Contains(outDefault, ruleContent) {
		t.Errorf("默认（MEM_NO_NOTES 未置位）该 rule 已被 notes 覆盖，不应重复注入，got:\n%s", outDefault)
	}

	// 置位 MEM_NO_NOTES：回到仅 v2mem 原始注入 -> 该 rule 正常注入
	t.Setenv("MEM_NO_NOTES", "1")
	outNoNotes, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "commit 前要不要跑 cover"),
		func() error { return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0"}) })
	if err != nil {
		t.Fatalf("cmdHook no-notes: %v", err)
	}
	if !strings.Contains(outNoNotes, ruleContent) {
		t.Errorf("MEM_NO_NOTES 置位应回到原始注入，能注入该 rule，got:\n%s", outNoNotes)
	}
}

// 回退同时作用于会话起始：硬规则段在 MEM_NO_NOTES 下不被 notes 去重。
func TestHookSessionStartDedupRespectsEnv(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	ruleContent := "全局红线：数据库不放代码目录"
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule", ruleContent}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	// 默认去重生效：SessionStart 的硬规则段不重复注入同一 rule
	out, err := withStdin(t, hookStdin("SessionStart", proj, ""),
		func() error { return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0"}) })
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if strings.Contains(out, ruleContent) {
		t.Errorf("SessionStart 默认不应重复注入已被 notes 覆盖的 rule，got:\n%s", out)
	}

	// 回退后恢复
	t.Setenv("MEM_NO_NOTES", "1")
	outNoNotes, err := withStdin(t, hookStdin("SessionStart", proj, ""),
		func() error { return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0"}) })
	if err != nil {
		t.Fatalf("cmdHook no-notes: %v", err)
	}
	if !strings.Contains(outNoNotes, ruleContent) {
		t.Errorf("MEM_NO_NOTES 置位应恢复原始注入，got:\n%s", outNoNotes)
	}
}