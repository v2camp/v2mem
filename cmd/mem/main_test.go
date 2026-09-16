package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
