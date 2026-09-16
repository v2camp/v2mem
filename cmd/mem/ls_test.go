package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wanghui/v2mem/internal/store"
)

// 「我到底记了些什么」此前没有对应命令：search 要求先有查询词，export 是给机器读的。
func TestCmdLsListsWithoutQuery(t *testing.T) {
	db := testDB(t)
	for _, c := range []struct{ kind, content string }{
		{"decision", "记忆库数据固定放 ~/.v2mem"},
		{"pitfall", "活库不能放进同步目录"},
	} {
		if err := cmdAdd([]string{"--db", db, "--project", "demo", "--kind", c.kind, c.content}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}
	out, err := captureStdout(t, func() error { return cmdLs([]string{"--db", db, "--scope", "all"}) })
	if err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	for _, want := range []string{"共 2 条", "记忆库数据固定放", "活库不能放进同步目录", "[decision]", "[pitfall]", "[demo]"} {
		if !strings.Contains(out, want) {
			t.Errorf("列表应含 %q，got:\n%s", want, out)
		}
	}
}

func TestCmdLsFilters(t *testing.T) {
	db := testDB(t)
	_ = cmdAdd([]string{"--db", db, "--project", "p1", "--kind", "decision", "p1 的决策"})
	_ = cmdAdd([]string{"--db", db, "--project", "p1", "--kind", "pitfall", "p1 的踩坑"})
	_ = cmdAdd([]string{"--db", db, "--project", "p2", "--kind", "decision", "p2 的决策"})

	// 按类型
	out, err := captureStdout(t, func() error {
		return cmdLs([]string{"--db", db, "--scope", "all", "--kind", "pitfall"})
	})
	if err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	if !strings.Contains(out, "共 1 条") || !strings.Contains(out, "p1 的踩坑") {
		t.Errorf("按类型过滤应只剩 1 条，got:\n%s", out)
	}
	// 按工程
	out, err = captureStdout(t, func() error {
		return cmdLs([]string{"--db", db, "--project", "p2"})
	})
	if err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	if !strings.Contains(out, "共 1 条") || !strings.Contains(out, "p2 的决策") {
		t.Errorf("按工程过滤应只剩 1 条，got:\n%s", out)
	}
	// 按标记
	if err := cmdAdd([]string{"--db", db, "--project", "p3", "--tag", "src=log", "带标记的条目"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	out, err = captureStdout(t, func() error {
		return cmdLs([]string{"--db", db, "--scope", "all", "--tag", "src=log"})
	})
	if err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	if !strings.Contains(out, "带标记的条目") || strings.Contains(out, "p2 的决策") {
		t.Errorf("按标记过滤结果不符，got:\n%s", out)
	}
}

// 拼错 --scope 不能静默变成「不限」—— 那会让人以为过滤生效了。
func TestCmdLsRejectsBadScope(t *testing.T) {
	db := testDB(t)
	if err := cmdLs([]string{"--db", db, "--scope", "curren"}); err == nil {
		t.Error("非法 --scope 应报错，而不是静默当作不限")
	}
}

// --scope all 与 --project 是矛盾用法：all 表示忽略工程。
// 静默忽略比报错更糟 —— 用户会以为过滤生效了。
func TestCmdLsRejectsContradictoryScopeAndProject(t *testing.T) {
	db := testDB(t)
	if err := cmdLs([]string{"--db", db, "--scope", "all", "--project", "p1"}); err == nil {
		t.Error("--scope all 与 --project 同时给出应报错")
	}
}

func TestCmdLsJSONAndEmpty(t *testing.T) {
	db := testDB(t)
	out, err := captureStdout(t, func() error {
		return cmdLs([]string{"--db", db, "--scope", "all", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	var hits []store.Hit
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("空库 --json 应是合法 JSON 数组，got %q: %v", out, err)
	}
	if len(hits) != 0 {
		t.Errorf("空库应返回空数组，got %d", len(hits))
	}
	if !strings.Contains(out, "[") {
		t.Errorf("应是 JSON 数组而非空输出，got %q", out)
	}
}

func TestCmdLsHonoursLimitAndHints(t *testing.T) {
	db := testDB(t)
	for i := 0; i < 5; i++ {
		if err := cmdAdd([]string{"--db", db, "--project", "demo", "--salience", "0.5",
			"第" + string(rune('1'+i)) + "条用于验证上限的记忆内容"}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}
	out, err := captureStdout(t, func() error {
		return cmdLs([]string{"--db", db, "--scope", "all", "--limit", "3"})
	})
	if err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	if !strings.Contains(out, "共 3 条") {
		t.Errorf("应只列 3 条，got:\n%s", out)
	}
	if !strings.Contains(out, "已达上限") {
		t.Errorf("触到上限应提示如何放宽/收窄，got:\n%s", out)
	}
}

// 不带参数时必须列出全部 —— 不能悄悄按当前目录推断的工程过滤。
//
// 起因：实测 `mem ls` 在 v2mem 仓库里跑返回「无匹配记忆」，
// 因为它把 cwd 推断成工程名 "v2mem" 并据此过滤，而库里的记忆属于别的工程。
// 「查我记了什么」这条命令的默认语义就是「全部」。
func TestCmdLsDefaultIsNotFilteredByCwdProject(t *testing.T) {
	db := testDB(t)
	// 记忆属于一个与测试进程 cwd 推断结果无关的工程
	if err := cmdAdd([]string{"--db", db, "--project", "some-other-project", "不应被 cwd 过滤掉的记忆"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	out, err := captureStdout(t, func() error { return cmdLs([]string{"--db", db}) })
	if err != nil {
		t.Fatalf("cmdLs: %v", err)
	}
	if !strings.Contains(out, "不应被 cwd 过滤掉的记忆") {
		t.Errorf("默认应列出全部、不按 cwd 推断的工程过滤，got:\n%s", out)
	}
	// 但显式要 current 作用域时，仍需工程才有意义 —— 此时才推断
	out2, err := captureStdout(t, func() error { return cmdLs([]string{"--db", db, "--scope", "global"}) })
	if err != nil {
		t.Fatalf("cmdLs --scope global: %v", err)
	}
	if strings.Contains(out2, "不应被 cwd 过滤掉的记忆") {
		t.Errorf("--scope global 应只列全局记忆，got:\n%s", out2)
	}
}
