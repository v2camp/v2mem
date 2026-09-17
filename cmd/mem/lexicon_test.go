package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanghui/v2mem/internal/store"
)

// seedy 工程记忆：两条「术语清单式」标准记忆，各含一组同义词（同一簇里词面互换）。
func seedLexiconProject(t *testing.T, db, project string) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	for _, c := range []string{
		"ingest 入库动作 我们常用 搬进 导入 灌库 摄入 这几种",
		"search 读取动作 从记忆库取回用 查找 查询 搜索 捞取 都行",
	} {
		if _, err := st.Add(store.AddInput{Content: c, Project: project, Source: "test"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
}

func TestCmdLexiconInitDryRun(t *testing.T) {
	db := testDB(t)
	seedLexiconProject(t, db, "proj")

	out, err := captureStdout(t, func() error {
		return cmdLexiconInit([]string{"--db", db, "--project", "proj",
			"--terms", "搬进,导入,灌库,摄入,查找,查询,搜索,捞取", "--k", "5", "--dry-run"})
	})
	if err != nil {
		t.Fatalf("cmdLexiconInit: %v", err)
	}
	if !strings.Contains(out, "同义簇: 2") {
		t.Errorf("应聚成 2 个同义簇（ingest / search 各一），got:\n%s", out)
	}
	// 恰好两行「标准词 = 别名」，且不跨概念混词
	lines := []string{}
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, " = ") {
			lines = append(lines, ln)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("应恰好 2 行同义簇，got %d:\n%s", len(lines), out)
	}
	for _, ln := range lines {
		ingestFirst := strings.Contains(ln, "搬进") || strings.Contains(ln, "摄入")
		searchSecond := strings.Contains(ln, "查找") || strings.Contains(ln, "查询")
		if ingestFirst && searchSecond {
			t.Errorf("同义簇不应跨概念混词（一行内既有 ingest 又有 search 词）: %q", ln)
		}
	}
	if strings.Contains(out, "已写入") {
		t.Error("dry-run 不应写入文件")
	}
}

func TestWriteLexiconAppendAndDedup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "synonyms.txt")
	os.WriteFile(p, []byte("检索 = 查找\n"), 0o644)

	clusters := [][]string{{"搬进", "导入", "灌库"}, {"查找", "检索"}}
	// 第一次写：只有 ingest 簇是新的（检索=查找已在库）
	if err := writeLexicon(p, "proj", clusters, map[string]int{"搬进": 3, "导入": 2, "灌库": 1, "查找": 3, "检索": 2}); err != nil {
		t.Fatalf("writeLexicon: %v", err)
	}
	b, _ := os.ReadFile(p)
	out := string(b)
	if !strings.Contains(out, "[proj]") {
		t.Errorf("应追加 [proj] 段:\n%s", out)
	}
	if !strings.Contains(out, "搬进 = 导入, 灌库") {
		t.Errorf("簇内含 3 词应写成 标准=别名,别名:\n%s", out)
	}
	// 去重：namespace 里 查找/检索 已存在，第二次写不应追加重复
	n := strings.Count(out, "搬进")
	before := strings.Count(out, "\n")
	clusterOne := [][]string{{"搬进", "导入", "灌库"}}
	writeLexicon(p, "proj", clusterOne, map[string]int{"搬进": 3, "导入": 2, "灌库": 1})
	b2, _ := os.ReadFile(p)
	if n != strings.Count(string(b2), "搬进") {
		t.Error("重复运行不应追加相同别名到既有工程段")
	}
	if strings.Count(string(b2), "\n") <= before {
		t.Log("重复运行不再新增行（预期）")
	}
}

func TestCmdLexiconRequiresProject(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return cmdLexiconInit([]string{"--dry-run"})
	})
	_ = out
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("缺 --project 应报错，got err=%v", err)
	}
}

func TestCmdLexiconInitWritesToSynonyms(t *testing.T) {
	db := testDB(t)
	seedLexiconProject(t, db, "proj")
	dir := filepath.Join(t.TempDir(), "sub")
	p := filepath.Join(dir, "synonyms.txt")

	_, err := captureStdout(t, func() error {
		return cmdLexiconInit([]string{"--db", db, "--project", "proj",
			"--terms", "搬进,导入,灌库,查找,查询,搜索", "--k", "5", "--out", p})
	})
	if err != nil {
		t.Fatalf("cmdLexiconInit(write): %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(b), "[proj]") {
		t.Errorf("应写入含 [proj] 段的文件:\n%s", b)
	}
	if !strings.Contains(string(b), " = ") {
		t.Errorf("文件应含标准行:\n%s", b)
	}
}

func TestCmdLexiconInitAutoDryRun(t *testing.T) {
	db := testDB(t)
	// 两个 memory 共享 bigram「同步」 ⇒ --auto 至少可提取该候选
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Add(store.AddInput{Content: "跨设备同步靠合并", Project: "proj", Source: "test"})
	st.Add(store.AddInput{Content: "同步冲突需要解决", Project: "proj", Source: "test"})
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdLexiconInit([]string{"--db", db, "--project", "proj", "--auto", "--min-freq", "2", "--dry-run"})
	})
	if err != nil {
		t.Fatalf("cmdLexiconInit(auto): %v", err)
	}
	if !strings.Contains(out, "候选词:") {
		t.Errorf("auto 应输出候选摘要，got:\n%s", out)
	}
}

// ---------- 提取辅助函数 ----------

func TestCandidateDistinctFreq(t *testing.T) {
	corpus := []store.Hit{
		{Content: "跨设备同步方案 A"},
		{Content: "同步冲突要解决"},
		{Content: "完全不同的内容 B"},
	}
	f := candidateDistinctFreq(corpus)
	if f["同步"] != 2 {
		t.Errorf("bigram 同步应出现在 2 条不同记忆，got %d", f["同步"])
	}
	if f["跨设"] != 1 {
		t.Errorf("仅出现 1 条记忆的 bigram 应计为 1，got %d", f["跨设"])
	}
}

func TestTopCandidatesAndPickStandard(t *testing.T) {
	freq := map[string]int{"搬进": 3, "导入": 2, "灌库": 1}
	terms := topCandidates(freq, 2)
	if len(terms) != 2 || terms[0] != "搬进" || terms[1] != "导入" {
		t.Errorf("topCandidates min=2 应取 搬进,导入，got %v", terms)
	}
	if got := pickStandard([]string{"导入", "搬进"}, freq); got != "搬进" {
		t.Errorf("标准词应为词频最高的 搬进，got %s", got)
	}
}

func TestHitJaccard(t *testing.T) {
	a := map[string]bool{"1": true, "2": true}
	b := map[string]bool{"2": true, "3": true}
	if j := hitJaccard(a, b); j < 1.0/3.0-1e-9 || j > 1.0/3.0+1e-9 {
		t.Errorf("Jaccard({1,2},{2,3}) 应为 1/3，got %v", j)
	}
	if hitJaccard(nil, b) != 0 {
		t.Error("空集 Jaccard 应为 0")
	}
}