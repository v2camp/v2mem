package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKindCodes(t *testing.T) {
	if len(kindCodes) != 4 {
		t.Fatalf("类码表应是常数 4 行，got %d", len(kindCodes))
	}
	want := []struct{ code, kind string }{
		{"P", "preference"},
		{"D", "decision"},
		{"X", "pitfall"},
		{"T", "task"},
	}
	for i, w := range want {
		got := kindCodes[i]
		if got.Code != w.code || got.Kind != w.kind {
			t.Errorf("第 %d 行应为 %s/%s，got %+v", i, w.code, w.kind, got)
		}
		if strings.TrimSpace(got.Trigger) == "" {
			t.Errorf("%s 触发条件为空", got.Code)
		}
		if n := hanLen(got.Trigger); n > 24 {
			t.Errorf("%s 触发条件汉字 %d > 24：%s", got.Code, n, got.Trigger)
		}
		if got.Kind == "fact" || strings.Contains(got.Trigger, "fact") {
			t.Errorf("类码表不得出现 fact：%+v", got)
		}
	}
	text := renderKindCodes()
	if strings.Contains(text, "fact") {
		t.Fatalf("渲染结果不得含 fact 行：\n%s", text)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("应渲染 4 行，got %d", len(lines))
	}
	for i, code := range []string{"P", "D", "X", "T"} {
		if !strings.HasPrefix(lines[i], code+" ") {
			t.Errorf("第 %d 行应以 %s 开头，got %q", i, code, lines[i])
		}
	}
}

func TestInstructionBlockIsKindCodes(t *testing.T) {
	block := instructionBlock("workbuddy")
	for _, code := range []string{"P preference", "D decision", "X pitfall", "T task"} {
		if !strings.Contains(block, code) {
			t.Errorf("指令块应含类码行 %q", code)
		}
	}
	if strings.Contains(block, "fact") {
		t.Errorf("指令块不得出现 fact 类码行：\n%s", block)
	}
	banned := []string{
		`mem search --scope current --json`,
		`mem add --kind decision|pitfall|preference|fact`,
		"mem ingest --project",
		"mem touch <id前8位>",
	}
	for _, s := range banned {
		if strings.Contains(block, s) {
			t.Errorf("指令块仍含已替换的命令清单原文 %q", s)
		}
	}
	if !strings.Contains(block, "未检索") {
		t.Errorf("指令块应保留 mem 不可用时的降级句：\n%s", block)
	}
	if runeLen(block) > 2048 {
		t.Errorf("指令块 %d runes > 2048", runeLen(block))
	}
	// hook 与 notes 必须消费同一份渲染，不能各写一套触发条件。
	if !strings.Contains(block, strings.TrimRight(renderKindCodes(), "\n")) {
		t.Fatal("instructionBlock 未消费 renderKindCodes")
	}
}

func TestInstructionBlockFitsBudgetCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte(instructionBlock("claude")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdBudget([]string{"--db", filepath.Join(dir, "mem.db"), "--file", path, "--max-chars", "2048"}); err != nil {
		t.Fatalf("mem budget --max-chars 2048 应通过：%v", err)
	}
}

func TestSessionStartInjectsSameKindCodes(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	out, err := withStdin(t, hookStdin("SessionStart", proj, ""),
		func() error { return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0"}) })
	if err != nil {
		t.Fatal(err)
	}
	codes := renderKindCodes()
	if !strings.Contains(out, strings.TrimRight(codes, "\n")) {
		t.Fatalf("SessionStart 应注入同一份类码表，got:\n%s", out)
	}
	for _, line := range strings.Split(codes, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.Contains(out, line) {
			t.Errorf("缺少类码行 %q", line)
		}
	}
}
