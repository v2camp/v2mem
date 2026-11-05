package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// runPromptHook 触发一次 prompt-submit，返回注入文本与本次轨迹。
func runPromptHook(t *testing.T, db, projectDir, prompt string) (string, store.ReadTrace) {
	t.Helper()
	out, err := withStdin(t, hookStdin("UserPromptSubmit", projectDir, prompt), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ts, err := st.RecentReadTraces(1)
	if err != nil || len(ts) == 0 {
		t.Fatalf("应写出读侧轨迹: %+v %v", ts, err)
	}
	return out, ts[0]
}

func TestShowMemoryDetailAndJSON(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--project", "demo", "--kind", "decision",
		"检索预算默认走类码表"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	hits, _ := st.List(store.ListQuery{Scope: "all", Limit: 10})
	st.Close()
	if len(hits) != 1 {
		t.Fatalf("前置条件不成立: %d", len(hits))
	}
	id := hits[0].ID

	out, err := captureStdout(t, func() error { return cmdShow([]string{"--db", db, id[:8]}) })
	if err != nil {
		t.Fatalf("cmdShow: %v", err)
	}
	for _, want := range []string{id, "decision", "demo", "检索预算默认走类码表", "adopted     0", "暂无"} {
		if !strings.Contains(out, want) {
			t.Errorf("show 输出应含 %q，got:\n%s", want, out)
		}
	}

	js, err := captureStdout(t, func() error { return cmdShow([]string{"--db", db, "--json", id[:8]}) })
	if err != nil {
		t.Fatalf("cmdShow json: %v", err)
	}
	var got struct {
		Memory store.MemoryDetail  `json:"memory"`
		Traces []store.MemoryTrace `json:"traces"`
	}
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("解析 show JSON: %v\n%s", err, js)
	}
	if got.Memory.ID != id || got.Memory.Content == "" || got.Memory.AdoptedCount != 0 {
		t.Fatalf("show JSON 不符: %+v", got.Memory)
	}

	if err := cmdShow([]string{"--db", db, "ffffffff"}); err == nil {
		t.Fatal("未知 id 应报错")
	}
	if err := cmdShow([]string{"--db", db}); err == nil {
		t.Fatal("缺 id 应报错")
	}
	if err := cmdShow([]string{"--db", db, "--traces", "-1", id[:8]}); err == nil {
		t.Fatal("负数 traces 应报错")
	}
}

func TestShowIncludesProvenanceAndTrace(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	res, err := st.Add(store.AddInput{
		Content: "检索结果必须带短 id", Kind: "decision", Project: name, Source: "test",
		Provenance: &store.Provenance{File: "docs/plan.md", Line: 12, Preview: "计划片段"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	st.Close()

	injected, tr := runPromptHook(t, db, proj, "检索")
	if !strings.Contains(injected, "mem:"+res.ID[:8]) {
		t.Fatalf("注入行应带 mem:<id8>，got:\n%s", injected)
	}
	if !strings.Contains(injected, " · ") {
		t.Fatalf("注入行应带紧凑 provenance，got:\n%s", injected)
	}

	out, err := captureStdout(t, func() error { return cmdShow([]string{"--db", db, res.ID[:8]}) })
	if err != nil {
		t.Fatalf("cmdShow: %v", err)
	}
	for _, want := range []string{"provenance", "docs/plan.md:12", "读侧轨迹", tr.ID[:8], "injected"} {
		if !strings.Contains(out, want) {
			t.Errorf("show 输出应含 %q，got:\n%s", want, out)
		}
	}
}

func TestFeedbackAdoptedAndNegative(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	for _, c := range []string{"检索预算默认走类码表", "检索结果必须带短 id"} {
		if err := cmdAdd([]string{"--db", db, "--project", name, "--kind", "decision", c}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	hits, _ := st.List(store.ListQuery{Scope: "all", Limit: 10})
	st.Close()
	if len(hits) != 2 {
		t.Fatalf("前置条件不成立: %d", len(hits))
	}
	first, second := hits[0].ID, hits[1].ID

	if _, tr := runPromptHook(t, db, proj, "检索"); len(tr.ID) == 0 {
		t.Fatal("应产生轨迹")
	}
	st, _ = store.Open(db)
	ts, err := st.TracesForMemory(first, 5)
	st.Close()
	if err != nil || len(ts) == 0 {
		t.Fatalf("应能用记忆反查轨迹: %+v %v", ts, err)
	}
	traceID := ts[0].TraceID

	out, err := captureStdout(t, func() error {
		return cmdFeedback([]string{"--db", db, traceID[:8], first[:8], "--adopted", "--reason", "直接可用"})
	})
	if err != nil {
		t.Fatalf("cmdFeedback adopted: %v", err)
	}
	if !strings.Contains(out, "adopted") || !strings.Contains(out, "累计采纳=1") {
		t.Fatalf("adopted 输出不符:\n%s", out)
	}

	st, _ = store.Open(db)
	d, err := st.MemoryDetailByID(first)
	st.Close()
	if err != nil || d.AdoptedCount != 1 || d.LastAdopted == nil {
		t.Fatalf("adopted 应更新记忆，got %+v %v", d, err)
	}

	out, err = captureStdout(t, func() error {
		return cmdFeedback([]string{"--db", db, "--json", traceID, second, "--negative", "--reason", "无关"})
	})
	if err != nil {
		t.Fatalf("cmdFeedback negative: %v", err)
	}
	var fb store.FeedbackResult
	if err := json.Unmarshal([]byte(out), &fb); err != nil {
		t.Fatalf("解析 feedback JSON: %v\n%s", err, out)
	}
	if fb.Feedback != "negative" || fb.AdoptedCount != 0 {
		t.Fatalf("negative 不应更新采纳计数: %+v", fb)
	}

	// 同值 adopted 幂等：不重复累加。
	out, err = captureStdout(t, func() error {
		return cmdFeedback([]string{"--db", db, traceID, first[:8], "--adopted"})
	})
	if err != nil || !strings.Contains(out, "累计采纳=1") {
		t.Fatalf("重复 adopted 应幂等: %v\n%s", err, out)
	}

	// 已 adopted 不能直接改判。
	if err := cmdFeedback([]string{"--db", db, traceID, first[:8], "--negative"}); err == nil {
		t.Fatal("已 adopted 直接改判应报错")
	}
}

func TestFeedbackRejectsBadArguments(t *testing.T) {
	db := testDB(t)
	cases := [][]string{
		{"--db", db},
		{"--db", db, "trace"},
		{"--db", db, "trace", "mem"},
		{"--db", db, "trace", "mem", "--adopted", "--negative"},
		{"--db", db, "missing", "mem", "--adopted"},
	}
	for _, args := range cases {
		if err := cmdFeedback(args); err == nil {
			t.Errorf("cmdFeedback(%v) 应报错", args)
		}
	}
}

// touch 是 adopted 的兼容别名，且只有它会更新 adopted_count / last_adopted_at。
func TestFeedbackTouchIsAdoptedAlias(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--project", "demo", "触摸即采纳的记忆"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	st, _ := store.Open(db)
	hits, _ := st.List(store.ListQuery{Scope: "all", Limit: 10})
	st.Close()
	id := hits[0].ID

	out, err := captureStdout(t, func() error { return cmdTouch([]string{"--db", db, id[:8]}) })
	if err != nil {
		t.Fatalf("cmdTouch: %v", err)
	}
	if !strings.Contains(out, "累计 1 次") {
		t.Fatalf("touch 应作为 adopted 别名: %s", out)
	}
	st, _ = store.Open(db)
	d, err := st.MemoryDetailByID(id)
	st.Close()
	if err != nil || d.AdoptedCount != 1 || d.LastAdopted == nil {
		t.Fatalf("touch 应写 adopted 字段: %+v %v", d, err)
	}

	// 读侧检索不自动 touch：再查一次，计数不变。
	if _, err := captureStdout(t, func() error { return cmdSearch([]string{"--db", db, "触摸即采纳"}) }); err != nil {
		t.Fatalf("cmdSearch: %v", err)
	}
	st, _ = store.Open(db)
	d2, _ := st.MemoryDetailByID(id)
	st.Close()
	if d2.AdoptedCount != 1 || d2.AccessCount != d.AccessCount {
		t.Fatalf("检索不应自动 touch: before=%+v after=%+v", d, d2)
	}
}
