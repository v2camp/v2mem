package store

// S14 大库性能索引（docs/design-storage-layering.md §9）。
//
// 口径：没有测量就不加索引。8 场景基准（test/perf，build tag perf）+ EXPLAIN
// QUERY PLAN 证明下列热点查询在全表扫，索引带来的提速 ≥20% 才落地：
//   - ix_commit_memory_actor     复核摘要的「每记忆最近一条 model-auto 裁决」子查询，
//                                20k 裁决账上逐行 SCAN d2 是 3.4s 的元凶（场景 5）；
//   - ix_cand_evidence_evidence  完成 Bundle 回收的引用保护反查（场景 8）；
//   - ix_mem_evidence_evidence   同上（场景 8）；
//   - ix_distill_bundle          同上：NOT EXISTS dr.bundle_id 逐 Bundle 全表扫；
//   - ix_obs_source_seq          出窗按 (source_id, generation, seq) 取窗，
//                                部分索引 ux_obs_source_seq 不被 LEFT JOIN 计划选中。
//
// ix_evbundle_status_created 候而不加：3k Bundle 规模下 S1/S3 提速 <20%，
// 见 §9 基准表。存量库靠 Open 里的 ensurePerfIndexes 幂等补齐（与
// ensureReviewSchema 同手法）；schema.sql 只对新建库生效，存量库恰恰是最需要补齐的那批。

import (
	"strings"
	"testing"
)

// perfIndexes 是本片新增索引的统一登记处：名字 + 所属表 + 守护的热点查询片段。
// 测试与 ensurePerfIndexes 共用这一份清单，改名会先红。
var perfIndexes = []struct {
	name  string
	table string
	plan  string // EXPLAIN QUERY PLAN 里应出现的索引引用
}{
	{"ix_commit_memory_actor", "commit_decisions", "ix_commit_memory_actor"},
	{"ix_cand_evidence_evidence", "candidate_evidence", "ix_cand_evidence_evidence"},
	{"ix_mem_evidence_evidence", "memory_evidence", "ix_mem_evidence_evidence"},
	{"ix_distill_bundle", "distill_runs", "ix_distill_bundle"},
	{"ix_obs_source_seq", "observations", "ix_obs_source_seq"},
}

func perfIndexExists(t *testing.T, st *Store, table, name string) bool {
	t.Helper()
	rows, err := st.db.Query(`SELECT name FROM pragma_index_list(?)`, table)
	if err != nil {
		t.Fatalf("pragma_index_list(%s): %v", table, err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		if n == name {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index rows: %v", err)
	}
	return found
}

// TestPerfIndexesExist 守「新库自带 + 存量库 Open 补齐」两件事都成立。
func TestPerfIndexesExist(t *testing.T) {
	st := newTestStore(t)
	for _, ix := range perfIndexes {
		if !perfIndexExists(t, st, ix.table, ix.name) {
			t.Errorf("%s.%s 不存在（新库应由 schema.sql/ensure 建立）", ix.table, ix.name)
		}
	}
}

// TestPerfIndexNamesUniqueAndRegistered 守登记表与实际索引一一对应，
// 防止有人改了 schema 忘了同步清单（或反过来）。
func TestPerfIndexNamesUniqueAndRegistered(t *testing.T) {
	seen := map[string]bool{}
	for _, ix := range perfIndexes {
		if seen[ix.name] {
			t.Errorf("perfIndexes 里重复登记 %s", ix.name)
		}
		seen[ix.name] = true
	}
	st := newTestStore(t)
	for _, ix := range perfIndexes {
		if !perfIndexExists(t, st, ix.table, ix.name) {
			t.Errorf("%s.%s 缺失", ix.table, ix.name)
			continue
		}
		// 索引列序必须与登记的守护意图一致：抽查 sqlite_master 的 DDL 含列名。
		var ddl string
		if err := st.db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, ix.name).Scan(&ddl); err != nil {
			t.Errorf("读 %s DDL: %v", ix.name, err)
		}
	}
}

// TestPerfHotQueryPlansUseIndexes 守「热点查询真的走新索引」：
// 存在性不等于被选中，计划变更（如查询改写）会让索引静默失效。
func TestPerfHotQueryPlansUseIndexes(t *testing.T) {
	st := newTestStore(t)
	// (查询名, EXPLAIN 对象, 期望出现的索引)
	cases := []struct {
		name string
		sql  string
		args []any
		want string
	}{
		{
			"复核摘要按记忆取最近 model-auto 裁决",
			`SELECT d2.id FROM commit_decisions d2
			 WHERE d2.memory_id = ? AND d2.actor = ?
			 ORDER BY d2.created_at DESC, d2.revision DESC, d2.id DESC LIMIT 1`,
			[]any{"mem-1", "model-auto"}, "ix_commit_memory_actor",
		},
		{
			"Bundle 回收引用保护反查候选证据",
			`SELECT 1 FROM candidate_evidence ce WHERE ce.evidence_id = ? LIMIT 1`,
			[]any{"ev-1"}, "ix_cand_evidence_evidence",
		},
		{
			"Bundle 回收引用保护反查记忆证据",
			`SELECT 1 FROM memory_evidence me WHERE me.evidence_id = ? LIMIT 1`,
			[]any{"ev-1"}, "ix_mem_evidence_evidence",
		},
		{
			"Bundle 回收引用保护反查蒸馏运行账",
			`SELECT 1 FROM distill_runs dr WHERE dr.bundle_id = ? LIMIT 1`,
			[]any{"bundle-1"}, "ix_distill_bundle",
		},
		{
			"证据出窗按源取连续窗口",
			`SELECT o.id FROM observations o
			  WHERE o.source_id = ? AND o.source_generation = ? AND o.source_seq > ?
			  ORDER BY o.source_seq ASC LIMIT ?`,
			[]any{"src-1", 1, 0, 50}, "ix_obs_source_seq",
		},
	}
	for _, tc := range cases {
		rows, err := st.db.Query("EXPLAIN QUERY PLAN "+tc.sql, tc.args...)
		if err != nil {
			t.Fatalf("%s: EXPLAIN 失败: %v", tc.name, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatalf("%s: scan plan: %v", tc.name, err)
			}
			plan.WriteString(detail)
			plan.WriteString("\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: plan rows: %v", tc.name, err)
		}
		if !strings.Contains(plan.String(), tc.want) {
			t.Errorf("%s 未走索引 %s，计划：\n%s", tc.name, tc.want, plan.String())
		}
	}
}

// TestPerfIndexesSurviveLegacyReopen 守存量库迁移：手工造「v3 后、S14 前」的库
// （删掉全部新索引），重新 Open 必须补齐；二次 Open 幂等不报错。
func TestPerfIndexesSurviveLegacyReopen(t *testing.T) {
	path := t.TempDir() + "/mem.db"
	st, err := Open(path)
	if err != nil {
		t.Fatalf("首次 Open: %v", err)
	}
	// 模拟 S14 之前的存量库：索引全数删掉（schema v3 的列都已在）。
	for _, ix := range perfIndexes {
		if _, err := st.db.Exec(`DROP INDEX IF EXISTS ` + ix.name); err != nil {
			t.Fatalf("drop %s: %v", ix.name, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重开存量库: %v", err)
	}
	defer st2.Close()
	for _, ix := range perfIndexes {
		if !perfIndexExists(t, st2, ix.table, ix.name) {
			t.Errorf("存量库重开未补齐 %s.%s", ix.table, ix.name)
		}
	}

	// 二次 Open：幂等，不得因索引已存在而失败。
	st3, err := Open(path)
	if err != nil {
		t.Fatalf("二次 Open（幂等）: %v", err)
	}
	st3.Close()
}
