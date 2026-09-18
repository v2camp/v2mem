package main

// WS2 — 写侧统一收录（mem ingest --source）：锚定「harness 任务收尾总结 → v2mem」
// 这一条路径的写入来源，保证双路径写里 machine 收尾总结的记忆 source='harness-summary'，
// 且不借助 --source 冒充别的来源。

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wanghui/v2mem/internal/audit"
)

// memSourceAndTool 直接从库文件读出某条正文对应的 (source, origin_tool)。
// Hit/Export 不携带 source，这里用裸 SQL 断言写侧治理落库，最直接。
func memSourceAndTool(t *testing.T, db, content string) (source, tool string) {
	t.Helper()
	dbh, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer dbh.Close()
	if err := dbh.QueryRow(
		`SELECT source, origin_tool FROM memories WHERE content = ? AND project = 'demo'`,
		content,
	).Scan(&source, &tool); err != nil {
		t.Fatalf("查询 source/tool: %v", err)
	}
	return source, tool
}

const anchorNote = "- harness 任务收尾总结落到记忆库的关键结论\n"

// --source harness-summary 是白名单内：该来源写入的记忆必须 source='harness-summary'，
// 且 tool 被正确标记为 ingest（收尾收录走 ingest 入口）。
func TestCmdIngestSourceHarnessSummaryAnchorsStoreAndTool(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "summary.md", anchorNote)

	if _, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo",
			"--source", "harness-summary", "--no-audit", src})
	}); err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}

	source, tool := memSourceAndTool(t, db, "harness 任务收尾总结落到记忆库的关键结论")
	if source != "harness-summary" {
		t.Errorf("白名单来源应锚定 source='harness-summary'，got %q", source)
	}
	if tool != "ingest" {
		t.Errorf("收尾收录应标记 tool='ingest'，got %q", tool)
	}
}

// ingest 提取记忆时应带内容级溯源（源文件相对路径 + 起始行），供资产联动/词库反查。
func TestCmdIngestWritesProvenanceFileAndLine(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "AGENTS.md", "- 钩子护栏只读不写记忆库\n- 记忆与数据分离\n")

	if _, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo", "--no-audit", src})
	}); err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}

	dbh, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer dbh.Close()

	var prov string
	if err := dbh.QueryRow(
		`SELECT provenance FROM memories WHERE content = '钩子护栏只读不写记忆库' AND project = 'demo'`,
	).Scan(&prov); err != nil {
		t.Fatalf("读取 provenance: %v", err)
	}
	if prov == "" {
		t.Fatal("ingest 应写入 provenance")
	}
	var p struct {
		File   string `json:"file"`
		Line   int    `json:"line"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(prov), &p); err != nil {
		t.Fatalf("provenance 应为 JSON: %v", err)
	}
	if p.File == "" {
		t.Errorf("provenance.file 应为源文件名，got %q", p.File)
	}
	if p.Line <= 0 {
		t.Errorf("provenance.line 应为正行号，got %d", p.Line)
	}
	if p.Source != "asset" {
		t.Errorf("ingest 溯源类型应为 asset，got %q", p.Source)
	}
}

// 未显式给 --source 时不得冒充 harness-summary：回落默认 ingest。
func TestCmdIngestSourceDefaultFallsBackToIngest(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "note.md", anchorNote)

	if _, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo", "--no-audit", src})
	}); err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}

	source, _ := memSourceAndTool(t, db, "harness 任务收尾总结落到记忆库的关键结论")
	if source != "ingest" {
		t.Errorf("未显式给 source 应回落默认 ingest，got %q", source)
	}
}

// 白名单外（显式传了别的值，如 llm/human/随便写）一律回落默认 ingest：
// 写侧治理不能让 --source 变成自由文本、冒充别的入口。
func TestCmdIngestSourceNonWhitelistFallsBackToIngest(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "note.md", anchorNote)

	if _, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo",
			"--source", "llm", "--no-audit", src})
	}); err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}

	source, _ := memSourceAndTool(t, db, "harness 任务收尾总结落到记忆库的关键结论")
	if source != "ingest" {
		t.Errorf("白名单外来源应回落默认 ingest，got %q", source)
	}
}

// WS2 审计：ingest 写库的审计记录必须携带 source（含白名单锚定的 harness-summary）。
func TestCmdIngestAuditCarriesSource(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "summary.md", anchorNote)
	ap := filepath.Join(t.TempDir(), "audit.jsonl")

	if _, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo",
			"--source", "harness-summary", "--audit-file", ap, src})
	}); err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}
	if _, err := os.Stat(ap); err != nil {
		t.Fatalf("应写出审计日志: %v", err)
	}

	recs, err := audit.Read(ap, 0)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("审计日志应至少一条记录")
	}
	last := recs[len(recs)-1]
	if last.Event != "manual-add" {
		t.Errorf("ingest 审计事件应为 manual-add，got %q", last.Event)
	}
	if last.Source != "harness-summary" {
		t.Errorf("ingest 审计应携带 source，got %q", last.Source)
	}
}

// 审计回落：未显式给 source 时，审计里的 source 也是默认 ingest，不冒充。
func TestCmdIngestAuditSourceFallsBackToIngest(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "note.md", anchorNote)
	ap := filepath.Join(t.TempDir(), "audit.jsonl")

	if _, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo", "--audit-file", ap, src})
	}); err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}
	recs, err := audit.Read(ap, 0)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("审计日志应至少一条记录")
	}
	if last := recs[len(recs)-1]; last.Source != "ingest" {
		t.Errorf("默认来源审计应为 ingest，got %q", last.Source)
	}
}
