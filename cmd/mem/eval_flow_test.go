package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/eval"
	"github.com/v2camp/v2mem/internal/store"
)

// E1/E4：空库不 panic，13 条检查全部显式 fail（不是 error），--json 结构稳定，
// 且有 fail 时命令返回非 nil（main 映射为退出码 1）。
func TestEvalFlowEmptyDBJSON(t *testing.T) {
	db := testDB(t)
	out, err := captureStdout(t, func() error {
		return cmdEvalFlow([]string{"--db", db, "--json"})
	})
	if err == nil {
		t.Fatal("空库应有多条 fail，cmdEvalFlow 必须返回非 nil 错误")
	}
	var rep eval.FlowCheckReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, out)
	}
	if len(rep.Checks) != 13 {
		t.Fatalf("checks = %d, want 13", len(rep.Checks))
	}
	if rep.Passed != 0 || rep.Failed != 13 || rep.Passed+rep.Failed != 13 {
		t.Fatalf("passed/failed = %d/%d, want 0/13", rep.Passed, rep.Failed)
	}
	for _, c := range rep.Checks {
		if c.ID == "" || c.Name == "" || c.Detail == "" {
			t.Fatalf("检查项字段不完整: %+v", c)
		}
		if c.Pass {
			t.Errorf("%s 在空库上不应通过", c.ID)
		}
	}
}

func TestEvalFlowTextOutput(t *testing.T) {
	db := testDB(t)
	out, err := captureStdout(t, func() error {
		return cmdEvalFlow([]string{"--db", db})
	})
	if err == nil {
		t.Fatal("空库应返回非 nil 错误")
	}
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "G") && strings.Contains(line, "FAIL") {
			lines++
		}
	}
	if lines != 13 {
		t.Fatalf("逐行 FAIL 条数 = %d, want 13\n%s", lines, out)
	}
	if !strings.Contains(out, "人工介入率") {
		t.Fatalf("文本输出应报告人工介入率:\n%s", out)
	}
}

// E4：eval flow 只读。除 store.Open 的既有 stamp（db_meta）外，业务表行数不得变化。
func TestEvalFlowReadOnly(t *testing.T) {
	db := testDB(t)
	// 先让 store.Open 完成建表与 stamp，再取基线。
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	before := tableCounts(t, st)
	st.Close()

	if _, err := captureStdout(t, func() error {
		return cmdEvalFlow([]string{"--db", db, "--json"})
	}); err == nil {
		t.Fatal("空库应返回非 nil 错误")
	}

	st, err = store.Open(db)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	after := tableCounts(t, st)
	st.Close()
	if len(before) != len(after) {
		t.Fatalf("表数量变化: %d → %d", len(before), len(after))
	}
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s 行数变化: %d → %d（eval flow 只能读）", table, n, after[table])
		}
	}
}

// 业务表清单与 schema.sql 对齐；db_meta 是 Open 的 writer stamp，不在只读断言内。
var flowReadOnlyTables = []string{
	"memories", "tags", "edges", "observations", "evidence_sources",
	"evidence_archives", "evidence_bundles", "evidence_bundle_items",
	"candidates", "distill_runs", "candidate_evidence", "commit_decisions",
	"memory_evidence", "read_traces", "read_trace_items", "migration_marks",
	"summary_seq", "obs_fts_map", "hook_dedup", "mem_sigs",
}

func tableCounts(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	out := make(map[string]int, len(flowReadOnlyTables))
	for _, table := range flowReadOnlyTables {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}
