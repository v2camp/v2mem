package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func migrationHasColumn(t *testing.T, st *Store, table, column string) bool {
	t.Helper()
	rows, err := st.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("column rows: %v", err)
	}
	return false
}

func migrationCount(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func migrationIndexSQL(t *testing.T, st *Store, name string) string {
	t.Helper()
	var sqlText string
	err := st.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&sqlText)
	if err != nil {
		t.Fatalf("index %s: %v", name, err)
	}
	return sqlText
}

func createLegacyDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()

	ddl := []string{
		`CREATE TABLE memories (
			id TEXT PRIMARY KEY, content TEXT NOT NULL, content_idx TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'fact', content_hash TEXT NOT NULL,
			project TEXT NOT NULL DEFAULT '', salience REAL NOT NULL DEFAULT 0.5,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			last_seen_at INTEGER NOT NULL, expires_at INTEGER, superseded_by TEXT,
			origin_device TEXT NOT NULL, origin_tool TEXT NOT NULL DEFAULT '',
			access_count INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE observations (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, project TEXT NOT NULL DEFAULT '',
			harness TEXT NOT NULL DEFAULT '', event TEXT NOT NULL,
			tool_name TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
			summary_idx TEXT NOT NULL DEFAULT '', raw_ref TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL, created_at INTEGER NOT NULL,
			meta_json TEXT NOT NULL DEFAULT '{}')`,
		`CREATE UNIQUE INDEX ux_obs_dedup ON observations(session_id, content_hash)`,
		`CREATE TABLE candidates (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL DEFAULT '', project TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL, content TEXT NOT NULL, content_hash TEXT NOT NULL,
			status TEXT NOT NULL, source TEXT NOT NULL, salience REAL NOT NULL DEFAULT 0.5,
			obs_ids_json TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL,
			decided_at INTEGER, memory_id TEXT)`,
		`CREATE TABLE distill_runs (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, provider TEXT NOT NULL,
			status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', cand_count INTEGER NOT NULL DEFAULT 0,
			started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL)`,
	}
	for _, q := range ddl {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("legacy DDL: %v", err)
		}
	}
	for i := 0; i < 9; i++ {
		if _, err := db.Exec(`INSERT INTO memories
			(id, content, content_idx, kind, content_hash, project, salience, created_at, updated_at,
			 last_seen_at, origin_device, origin_tool, access_count)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("legacy-mem-%02d", i), fmt.Sprintf("legacy memory %d", i),
			fmt.Sprintf("legacy memory %d", i), "fact", fmt.Sprintf("hash-mem-%02d", i),
			"legacy-project", 0.5, 1700000000+i, 1700000000+i, 1700000000+i,
			"legacy-device", "legacy", i); err != nil {
			t.Fatalf("insert legacy memory: %v", err)
		}
	}
	for i := 0; i < 19; i++ {
		if _, err := db.Exec(`INSERT INTO observations
			(id, session_id, project, harness, event, tool_name, summary, summary_idx,
			 raw_ref, content_hash, created_at, meta_json)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("legacy-obs-%02d", i), "legacy-session", "legacy-project",
			"workbuddy", "stop", "", "stop", "stop", "", fmt.Sprintf("hash-obs-%02d", i),
			1700000000+i, "{}"); err != nil {
			t.Fatalf("insert legacy observation: %v", err)
		}
	}
	for i := 0; i < 19; i++ {
		status := "empty"
		if i < 6 {
			status = "ok"
		}
		if _, err := db.Exec(`INSERT INTO distill_runs
			(id, session_id, provider, status, error, cand_count, started_at, finished_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("legacy-run-%02d", i), "legacy-session", "none", status, "",
			i%3, 1700000000+i, 1700000000+i); err != nil {
			t.Fatalf("insert legacy run: %v", err)
		}
	}
}

func TestMigrationFreshDBHasFullEvidenceSchema(t *testing.T) {
	st := newTestStore(t)
	for _, c := range evidenceColumns {
		if !migrationHasColumn(t, st, c.table, c.column) {
			t.Errorf("fresh DB 缺列 %s.%s", c.table, c.column)
		}
	}
	for _, table := range []string{"evidence_sources", "evidence_archives", "evidence_bundles", "evidence_bundle_items", "migration_marks", "candidate_evidence", "commit_decisions", "memory_evidence", "read_traces", "read_trace_items"} {
		if n := migrationCount(t, st, table); n != 0 {
			t.Errorf("fresh DB 的 %s 应为空，got %d", table, n)
		}
	}
	var oldIndex int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='ux_obs_dedup'`).Scan(&oldIndex); err != nil {
		t.Fatalf("count old index: %v", err)
	}
	if oldIndex != 0 {
		t.Fatalf("fresh DB 不应再建 ux_obs_dedup")
	}
	for _, idx := range []string{"ux_obs_dedup_legacy", "ux_obs_event_uid", "ux_obs_source_seq"} {
		sqlText := migrationIndexSQL(t, st, idx)
		if idx == "ux_obs_dedup_legacy" && !strings.Contains(strings.ToUpper(sqlText), "WHERE") {
			t.Errorf("legacy 去重索引必须是部分索引: %s", sqlText)
		}
	}
}

func TestMigrationLegacyDBCountsAndMarks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDB(t, path)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	defer st.Close()

	for table, want := range map[string]int{
		"memories": 9, "observations": 19, "candidates": 0, "distill_runs": 19,
		"evidence_sources": 0, "evidence_archives": 0, "evidence_bundles": 0, "evidence_bundle_items": 0,
	} {
		if got := migrationCount(t, st, table); got != want {
			t.Errorf("%s = %d, want %d", table, got, want)
		}
	}
	var memMarks, obsMarks int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM migration_marks WHERE entity_type='memory' AND mark='legacy/no_original_evidence'`).Scan(&memMarks); err != nil {
		t.Fatalf("memory marks: %v", err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM migration_marks WHERE entity_type='observation' AND mark='legacy_unpositioned'`).Scan(&obsMarks); err != nil {
		t.Fatalf("observation marks: %v", err)
	}
	if memMarks != 9 || obsMarks != 19 {
		t.Fatalf("legacy marks = memory:%d observation:%d, want 9/19", memMarks, obsMarks)
	}
	var version int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != 1 {
		t.Fatalf("user_version = %d, want 1", version)
	}
	var dirty int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM observations
		WHERE event_uid <> '' OR source_id <> '' OR source_seq <> 0 OR archive_hash <> ''`).Scan(&dirty); err != nil {
		t.Fatalf("legacy evidence check: %v", err)
	}
	if dirty != 0 {
		t.Fatalf("legacy observations 不应伪造 Evidence 字段，got %d", dirty)
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE scope <> '' OR adopted_count <> 0 OR last_adopted_at IS NOT NULL`).Scan(&count); err != nil {
		t.Fatalf("legacy memory scope check: %v", err)
	}
	if count != 0 {
		t.Fatalf("legacy memories 不应伪造 scope/采用统计，got %d", count)
	}
}

func TestMigrationIndexSeparation(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.db.Exec(`INSERT INTO observations
		(id, session_id, project, harness, event, summary, summary_idx, content_hash, created_at,
		 event_uid, source_id, source_generation, source_seq, parser_version)
		VALUES ('obs-a','s','p','h','tool','same','same','same-hash',1,'evt-a','src',1,1,'v1')`); err != nil {
		t.Fatalf("insert first evidence: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO observations
		(id, session_id, project, harness, event, summary, summary_idx, content_hash, created_at,
		 event_uid, source_id, source_generation, source_seq, parser_version)
		VALUES ('obs-b','s','p','h','tool','same','same','same-hash',2,'evt-b','src',1,2,'v1')`); err != nil {
		t.Fatalf("同摘要不同事件应可共存: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO observations
		(id, session_id, project, harness, event, summary, summary_idx, content_hash, created_at,
		 event_uid, source_id, source_generation, source_seq, parser_version)
		VALUES ('obs-c','s','p','h','tool','same','same','same-hash',3,'evt-a','src',1,3,'v1')`); err == nil {
		t.Fatalf("重复 event_uid 应被唯一索引拒绝")
	}
	if _, err := st.db.Exec(`INSERT INTO observations
		(id, session_id, project, harness, event, summary, summary_idx, content_hash, created_at,
		 event_uid, source_id, source_generation, source_seq, parser_version)
		VALUES ('obs-d','s','p','h','tool','same','same','same-hash',4,'evt-d','src',1,2,'v1')`); err == nil {
		t.Fatalf("重复 source_seq 应被唯一索引拒绝")
	}
}

func TestMigrationIsIdempotentOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDB(t, path)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	firstMarks := migrationCount(t, st, "migration_marks")
	st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer st.Close()
	if got := migrationCount(t, st, "migration_marks"); got != firstMarks {
		t.Fatalf("重复 Open 改变了 marks: %d -> %d", firstMarks, got)
	}
	if got := migrationCount(t, st, "memories"); got != 9 {
		t.Fatalf("重复 Open 改变了 memories: %d", got)
	}
}

func TestMigrationBackupIsOpenable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDB(t, path)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := st.Backup(dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	st.Close()
	backup, err := Open(dest)
	if err != nil {
		t.Fatalf("Open backup: %v", err)
	}
	defer backup.Close()
	if got := migrationCount(t, backup, "memories"); got != 9 {
		t.Fatalf("backup memories = %d, want 9", got)
	}
}

func TestMigrationAllowsFactCandidate(t *testing.T) {
	st := newTestStore(t)
	res, err := st.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "fact",
		Content: "T1.0 允许项目事实候选", Source: "test",
	})
	if err != nil {
		t.Fatalf("InsertCandidate fact: %v", err)
	}
	if !res.Created || res.Candidate.Kind != "fact" {
		t.Fatalf("fact candidate 未按新语义创建: %+v", res)
	}
}

// createV3DistillRunsDB 手工建一个带旧窗口唯一键的 v3 库：
// 复现生产库「distill_runs 表内 UNIQUE(session_id, started_at)」的形态，
// 并带上当时已有的两个索引，证明迁移会保留它们。
func createV3DistillRunsDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v3 db: %v", err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE distill_runs (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, provider TEXT NOT NULL,
			status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', cand_count INTEGER NOT NULL DEFAULT 0,
			started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL,
			window_key TEXT NOT NULL DEFAULT '',
			bundle_id TEXT NOT NULL DEFAULT '', executor TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '', model_version TEXT NOT NULL DEFAULT '',
			prompt_version TEXT NOT NULL DEFAULT '', schema_version INTEGER NOT NULL DEFAULT 0,
			generated_count INTEGER NOT NULL DEFAULT 0, valid_count INTEGER NOT NULL DEFAULT 0,
			invalid_count INTEGER NOT NULL DEFAULT 0, duplicate_count INTEGER NOT NULL DEFAULT 0,
			attempt INTEGER NOT NULL DEFAULT 0, source_fingerprint TEXT NOT NULL DEFAULT '',
			UNIQUE(session_id, started_at))`,
		`CREATE UNIQUE INDEX ux_distill_window
			ON distill_runs(session_id, window_key) WHERE window_key <> ''`,
		`CREATE INDEX ix_distill_bundle ON distill_runs(bundle_id)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("v3 DDL: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		window := ""
		if i == 0 {
			window = "w-keep"
		}
		if _, err := db.Exec(`INSERT INTO distill_runs
			(id, session_id, provider, status, started_at, finished_at, window_key)
			VALUES (?,?,?,?,?,?,?)`,
			fmt.Sprintf("v3-run-%d", i), "s-v3", "none", "ok", 100+i, 100+i, window); err != nil {
			t.Fatalf("insert v3 run: %v", err)
		}
	}
}

// distillRunUniqueColumns 返回 distill_runs 上所有唯一索引的列组合。
// 旧窗口唯一键在 SQLite 里是表级 UNIQUE 生成的自增索引（origin='u'），
// 直接看索引比解析 DDL 文本更抗格式差异。
func distillRunUniqueColumns(t *testing.T, st *Store) [][]string {
	t.Helper()
	rows, err := st.db.Query(`SELECT name, "unique" FROM pragma_index_list('distill_runs')`)
	if err != nil {
		t.Fatalf("pragma_index_list: %v", err)
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var name string
		var unique int
		if err := rows.Scan(&name, &unique); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		if unique != 1 {
			continue
		}
		info, err := st.db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name)
		if err != nil {
			t.Fatalf("pragma_index_info(%s): %v", name, err)
		}
		var cols []string
		for info.Next() {
			var col string
			if err := info.Scan(&col); err != nil {
				info.Close()
				t.Fatalf("scan index_info: %v", err)
			}
			cols = append(cols, col)
		}
		info.Close()
		out = append(out, cols)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index_list rows: %v", err)
	}
	return out
}

func hasColumnPair(cols []string, a, b string) bool {
	return len(cols) == 2 && cols[0] == a && cols[1] == b
}

// U5 真实缺陷回归：v3 库里的 distill_runs 旧窗口唯一键必须被 v4 迁移移除。
// 同 session 同一秒的多条运行账是 maintain 连续追账的合法形态，不是冲突。
func TestMigrationRemovesDistillRunsLegacyUniqueKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	createV3DistillRunsDB(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v3: %v", err)
	}
	for _, cols := range distillRunUniqueColumns(t, st) {
		if hasColumnPair(cols, "session_id", "started_at") {
			t.Fatalf("迁移后仍存在 UNIQUE(session_id, started_at): %v", cols)
		}
	}
	if n := migrationCount(t, st, "distill_runs"); n != 3 {
		t.Fatalf("迁移后运行账行数 = %d，want 3", n)
	}
	var window string
	if err := st.db.QueryRow(`SELECT window_key FROM distill_runs WHERE id='v3-run-0'`).Scan(&window); err != nil {
		t.Fatalf("读迁移后窗口键: %v", err)
	}
	if window != "w-keep" {
		t.Fatalf("迁移丢窗口键: %q", window)
	}
	for _, idx := range []string{"ux_distill_window", "ix_distill_bundle"} {
		if migrationIndexSQL(t, st, idx) == "" {
			t.Errorf("迁移后缺索引 %s", idx)
		}
	}
	var version string
	if err := st.db.QueryRow(`SELECT v FROM db_meta WHERE k='schema_version'`).Scan(&version); err != nil {
		t.Fatalf("读 schema_version: %v", err)
	}
	if version != fmt.Sprint(CurrentSchemaVersion) {
		t.Fatalf("schema_version = %q，want %d", version, CurrentSchemaVersion)
	}

	// 迁移后的表必须允许同 session 同秒连续写运行账，且时间戳原样保留。
	for i := 0; i < 5; i++ {
		if err := st.SaveProposalRun(ProposalRunRecord{
			SessionID: "s-v3", Provider: "none", Status: "ok",
			StartedAt: 200, FinishedAt: 200, Attempt: i + 1,
		}); err != nil {
			t.Fatalf("第 %d 条同秒运行账应落库: %v", i+1, err)
		}
	}
	if n := migrationCount(t, st, "distill_runs"); n != 8 {
		t.Fatalf("同秒追加后行数 = %d，want 8", n)
	}
	st.Close()

	// 二次 Open 幂等：不再重建、行数与版本不变。
	st, err = Open(path)
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	defer st.Close()
	if n := migrationCount(t, st, "distill_runs"); n != 8 {
		t.Fatalf("二次 Open 后行数 = %d，want 8", n)
	}
	for _, cols := range distillRunUniqueColumns(t, st) {
		if hasColumnPair(cols, "session_id", "started_at") {
			t.Fatalf("二次 Open 又出现旧唯一键: %v", cols)
		}
	}
	if err := st.SaveProposalRun(ProposalRunRecord{
		SessionID: "s-v3", Provider: "none", Status: "ok", StartedAt: 200, FinishedAt: 200,
	}); err != nil {
		t.Fatalf("二次 Open 后同秒写入应继续可用: %v", err)
	}
}

// 新库不得再带旧唯一键：schema.sql 与迁移路径必须收敛到同一形状。
func TestFreshDistillRunsHasNoLegacyUniqueKey(t *testing.T) {
	st := newTestStore(t)
	for _, cols := range distillRunUniqueColumns(t, st) {
		if hasColumnPair(cols, "session_id", "started_at") {
			t.Fatalf("fresh DB 仍建了旧唯一键: %v", cols)
		}
	}
}

// 迁移失败路径：旧库异常时必须报错而不是留下半重建的表。
func TestDistillRunsMigrationErrorPaths(t *testing.T) {
	closed, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := hasLegacyDistillRunUnique(closed); err == nil {
		t.Fatal("关闭的库应报读索引错误")
	}
	if err := ensureDistillRunsSchema(closed); err == nil {
		t.Fatal("关闭的库应报迁移错误")
	}

	// 重建目标表名已被占用：第一句 CREATE 就失败，事务回滚。
	path := filepath.Join(t.TempDir(), "conflict.db")
	createV3DistillRunsDB(t, path)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE distill_runs_v4 (x TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := ensureDistillRunsSchema(raw); err == nil {
		t.Fatal("目标表名冲突时应报错")
	}
	if n := migrationCountRaw(t, raw, "distill_runs"); n != 3 {
		t.Fatalf("失败后旧表行数应保持 3，got %d", n)
	}
}

func migrationCountRaw(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestMigrationDistillRunCountersDefaultZero(t *testing.T) {
	st := newTestStore(t)
	if err := st.SaveDistillRun(DistillRun{
		SessionID: "s", Provider: "none", Status: "empty", WindowKey: "w",
	}); err != nil {
		t.Fatalf("SaveDistillRun: %v", err)
	}
	var generated, valid, invalid, duplicate, attempt int
	if err := st.db.QueryRow(`SELECT generated_count, valid_count, invalid_count, duplicate_count, attempt
		FROM distill_runs WHERE session_id='s'`).Scan(&generated, &valid, &invalid, &duplicate, &attempt); err != nil {
		t.Fatalf("new counters: %v", err)
	}
	if generated != 0 || valid != 0 || invalid != 0 || duplicate != 0 || attempt != 0 {
		t.Fatalf("新计数列默认值应为 0: %d/%d/%d/%d/%d", generated, valid, invalid, duplicate, attempt)
	}
}
