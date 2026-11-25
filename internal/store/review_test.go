package store

// S10a 复核数据面：schema v3 迁移与回填、撤回可见性、候选 failed/quarantined、
// 裁决账的 model_tag/created_memory，以及供 CLI/MCP 复用的四态投影。
//
// 口径来源 docs/design-data-lifecycle.md §2.1 / §6.4：
// 「直写默认视为人工已审核」只覆盖**可证明的直写来源**，
// 迁移期不许把全库历史一律标成已审核 —— 那会让复核摘要永远看不到模型写入的行。

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/commit"
)

// createV2DB 手工建一个 S10a 之前的库：三张表都**没有**新列。
// 用真实 v2 列集（而不是 schema.sql）才能证明 Open 会补齐而非重新建表。
func createV2DB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
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
			source TEXT NOT NULL DEFAULT '', channel TEXT NOT NULL DEFAULT '',
			provenance TEXT, access_count INTEGER NOT NULL DEFAULT 0,
			scope TEXT NOT NULL DEFAULT '', adopted_count INTEGER NOT NULL DEFAULT 0,
			last_adopted_at INTEGER)`,
		`CREATE UNIQUE INDEX ux_mem_hash ON memories(content_hash, project)`,
		`CREATE TABLE candidates (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL DEFAULT '',
			project TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL, content TEXT NOT NULL,
			content_hash TEXT NOT NULL, status TEXT NOT NULL, source TEXT NOT NULL,
			channel TEXT NOT NULL DEFAULT '', salience REAL NOT NULL DEFAULT 0.5,
			obs_ids_json TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL,
			decided_at INTEGER, memory_id TEXT, run_id TEXT NOT NULL DEFAULT '',
			bundle_id TEXT NOT NULL DEFAULT '', proposal_index INTEGER NOT NULL DEFAULT 0,
			scope TEXT NOT NULL DEFAULT '', subject_key TEXT NOT NULL DEFAULT '',
			task_state TEXT NOT NULL DEFAULT '', rationale TEXT NOT NULL DEFAULT '',
			review_class TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 0,
			schema_version INTEGER NOT NULL DEFAULT 0,
			attributed_to TEXT NOT NULL DEFAULT '',
			judge_verdict TEXT NOT NULL DEFAULT '', judge_model TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE commit_decisions (
			id TEXT PRIMARY KEY, candidate_id TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 0, evidence_hash TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL, actor TEXT NOT NULL DEFAULT 'auto',
			reason_codes_json TEXT NOT NULL DEFAULT '[]',
			memory_id TEXT NOT NULL DEFAULT '', idempotency_key TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			UNIQUE(idempotency_key))`,
	}
	for _, q := range ddl {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("v2 DDL: %v", err)
		}
	}
	// FTS 索引必须与 memories 同步：真实库里由插入触发器维护，这里补建并 rebuild。
	// 索引不同步时，迁移期的 UPDATE 会触发 fts5 的 'delete' 而报 CORRUPT_VTAB，
	// 那是测试夹具的假故障，不是产品路径的问题。
	for _, q := range []string{
		`CREATE VIRTUAL TABLE fts_mem USING fts5(content_idx, content='memories', content_rowid='rowid')`,
		`CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
		   INSERT INTO fts_mem(rowid, content_idx) VALUES (new.rowid, new.content_idx);
		 END`,
		`CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
		   INSERT INTO fts_mem(fts_mem, rowid, content_idx) VALUES ('delete', old.rowid, old.content_idx);
		 END`,
		`CREATE TRIGGER memories_au AFTER UPDATE ON memories BEGIN
		   INSERT INTO fts_mem(fts_mem, rowid, content_idx) VALUES ('delete', old.rowid, old.content_idx);
		   INSERT INTO fts_mem(rowid, content_idx) VALUES (new.rowid, new.content_idx);
		 END`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("v2 FTS DDL: %v", err)
		}
	}

	// 各直写通道一条 + 复核通道（distill-accept）一条 + legacy 空 source 一条。
	sources := []string{"human", "llm", "ingest", "harness-summary", "bench", "test", "distill-accept", ""}
	for i, src := range sources {
		if _, err := db.Exec(`INSERT INTO memories
			(id, content, content_idx, kind, content_hash, project, salience,
			 created_at, updated_at, last_seen_at, origin_device, origin_tool, source)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("m-%02d", i), "v2 记忆 "+src, "v2", "fact", fmt.Sprintf("h-%02d", i),
			"p", 0.5, 1700000000+i, 1700000000+i, 1700000000+i, "dev", "cli", src); err != nil {
			t.Fatalf("insert v2 memory: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at)
		VALUES ('c-old','s','p','fact','v2 候选','hc-old','pending','distill',1700000000)`); err != nil {
		t.Fatalf("insert v2 candidate: %v", err)
	}
}

func reviewColumns(t *testing.T, s *Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT m.name, COALESCE(m.dflt_value, '')
		FROM pragma_table_info(?) m`, "memories")
	if err != nil {
		t.Fatalf("pragma memories: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = def
	}
	return out
}

func TestReviewSchemaMigratesV2AndBackfillsOnlyDirectWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	createV2DB(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v2: %v", err)
	}

	for _, c := range []struct{ table, column string }{
		{"memories", "review_state"}, {"memories", "reviewed_at"}, {"memories", "reviewed_by"},
		{"candidates", "status_reason"}, {"candidates", "judge_reason"}, {"candidates", "attempt"},
		{"commit_decisions", "model_tag"}, {"commit_decisions", "created_memory"},
	} {
		if !migrationHasColumn(t, st, c.table, c.column) {
			t.Errorf("迁移后缺列 %s.%s", c.table, c.column)
		}
	}
	var version string
	if err := st.db.QueryRow(`SELECT v FROM db_meta WHERE k = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("读 schema_version: %v", err)
	}
	if version != "3" {
		t.Fatalf("schema_version = %q, want 3", version)
	}
	for _, idx := range []string{"ix_mem_review_state", "ix_commit_model"} {
		if got := migrationIndexSQL(t, st, idx); got == "" {
			t.Errorf("缺少索引 %s", idx)
		}
	}

	// 回填：只命中可证明的直写来源；distill-accept 与空 source 一律留 ''。
	want := map[string]string{
		"m-00": "confirmed", "m-01": "confirmed", "m-02": "confirmed",
		"m-03": "confirmed", "m-04": "confirmed", "m-05": "confirmed",
		"m-06": "", "m-07": "",
	}
	for id, state := range want {
		var got, by string
		var at int64
		if err := st.db.QueryRow(`SELECT review_state, reviewed_by, reviewed_at
			FROM memories WHERE id = ?`, id).Scan(&got, &by, &at); err != nil {
			t.Fatalf("读 %s: %v", id, err)
		}
		if got != state {
			t.Errorf("%s review_state = %q, want %q", id, got, state)
		}
		if state == "confirmed" && (by == "" || at == 0) {
			t.Errorf("%s 已确认行应带 reviewed_by/reviewed_at，got %q/%d", id, by, at)
		}
		if state == "" && (by != "" || at != 0) {
			t.Errorf("%s 未回填行不得伪造审核痕迹，got %q/%d", id, by, at)
		}
	}

	// 新列默认值：老候选行不能被当成「已尝试」或「已判」。
	var attempt int
	var statusReason, judgeReason string
	if err := st.db.QueryRow(`SELECT attempt, status_reason, judge_reason
		FROM candidates WHERE id = 'c-old'`).Scan(&attempt, &statusReason, &judgeReason); err != nil {
		t.Fatalf("读老候选: %v", err)
	}
	if attempt != 0 || statusReason != "" || judgeReason != "" {
		t.Fatalf("老候选新列应为零值，got attempt=%d reason=%q judge=%q", attempt, statusReason, judgeReason)
	}
	st.Close()

	// 二次 Open 幂等：列齐、版本不变、**不回填第二次**。
	st, err = Open(path)
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	defer st.Close()
	if err := st.db.QueryRow(`SELECT v FROM db_meta WHERE k = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("二次读版本: %v", err)
	}
	if version != "3" {
		t.Fatalf("二次 Open 后 schema_version = %q, want 3", version)
	}
	if n := migrationCount(t, st, "memories"); n != 8 {
		t.Fatalf("二次 Open 改变了行数: %d", n)
	}
}

// 回填是**迁移一次性动作**，不是每次 Open 都跑：
// 人工把一条已确认记忆撤回后，重开库不得把它变回 confirmed。
func TestReviewBackfillRunsOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	createV2DB(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.SetMemoryReviewState("m-00", ReviewStateWithdrawn, "human", 1700009999); err != nil {
		t.Fatalf("SetMemoryReviewState: %v", err)
	}
	st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	defer st.Close()
	var state string
	if err := st.db.QueryRow(`SELECT review_state FROM memories WHERE id = 'm-00'`).Scan(&state); err != nil {
		t.Fatalf("读回: %v", err)
	}
	if state != ReviewStateWithdrawn {
		t.Fatalf("回填不得重跑，withdrawn 被改回 %q", state)
	}
}

func TestAddMarksDirectWriteConfirmed(t *testing.T) {
	s := newTestStore(t)
	res, err := s.Add(AddInput{Content: "直写默认视为人工已审核", Source: "human"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	var state, by string
	var at int64
	if err := s.db.QueryRow(`SELECT review_state, reviewed_by, reviewed_at FROM memories WHERE id = ?`,
		res.ID).Scan(&state, &by, &at); err != nil {
		t.Fatalf("读回: %v", err)
	}
	if state != ReviewStateConfirmed || by != "human" || at == 0 {
		t.Fatalf("直写应标 confirmed，got state=%q by=%q at=%d", state, by, at)
	}

	// 覆盖同一内容不得把 withdrawn 复活：撤回是明确的人工决定。
	if err := s.SetMemoryReviewState(res.ID, ReviewStateWithdrawn, "human", 1700000000); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if _, err := s.Add(AddInput{Content: "直写默认视为人工已审核", Source: "human"}); err != nil {
		t.Fatalf("覆盖写入: %v", err)
	}
	if err := s.db.QueryRow(`SELECT review_state FROM memories WHERE id = ?`, res.ID).Scan(&state); err != nil {
		t.Fatalf("覆盖后读回: %v", err)
	}
	if state != ReviewStateWithdrawn {
		t.Fatalf("覆盖写入不得把 withdrawn 改回，got %q", state)
	}
}

func TestSetMemoryReviewStateValidation(t *testing.T) {
	s := newTestStore(t)
	res, err := s.Add(AddInput{Content: "状态取值必须收敛", Source: "human"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.SetMemoryReviewState(res.ID, "bogus", "human", 1); err == nil {
		t.Fatal("非法 review_state 必须报错，而不是静默写入")
	}
	if err := s.SetMemoryReviewState("missing-id", ReviewStateConfirmed, "human", 1); err == nil {
		t.Fatal("不存在的记忆必须报错")
	}
	for _, st := range []string{ReviewStateNone, ReviewStateConfirmed, ReviewStateCorrected, ReviewStateWithdrawn} {
		if err := s.SetMemoryReviewState(res.ID, st, "human", 1700000000); err != nil {
			t.Fatalf("合法状态 %q 应可写: %v", st, err)
		}
	}
}

// 撤回 = 立即退出检索，但行与证据保留（可追溯、可恢复）。
func TestWithdrawnMemoryLeavesSearchAndList(t *testing.T) {
	s := newTestStore(t)
	res, err := s.Add(AddInput{Content: "撤回后不能再被检索到的错误记忆", Source: "human", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.SetMemoryReviewState(res.ID, ReviewStateWithdrawn, "human", 1700000000); err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	hits, err := s.Search(SearchQuery{Query: "撤回 检索 错误记忆", Scope: "all", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if h.ID == res.ID {
			t.Fatal("撤回的记忆必须退出全文检索")
		}
	}
	listed, err := s.List(ListQuery{Scope: "all", Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, h := range listed {
		if h.ID == res.ID {
			t.Fatal("撤回的记忆必须退出 List（Search/List 同一份可见性判据）")
		}
	}
	// 行还在：撤回不是删除。
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, res.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatal("撤回必须保留记忆行（可追溯、可 restore）")
	}
	// 恢复（''）即刻回到检索：restore 的动作面留给 S10b，数据面在这里证明可行。
	if err := s.SetMemoryReviewState(res.ID, ReviewStateNone, "human", 1700000001); err != nil {
		t.Fatalf("restore: %v", err)
	}
	listed, err = s.List(ListQuery{Scope: "all", Limit: 50})
	if err != nil {
		t.Fatalf("List after restore: %v", err)
	}
	found := false
	for _, h := range listed {
		if h.ID == res.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("恢复后应重新进入检索")
	}
}

func TestMarkCandidateFailedRetriesAndQuarantinedIsTerminal(t *testing.T) {
	s := newTestStore(t)
	res := newCandidate(t, s, "fact", "p", "失败候选要能重试")

	if err := s.MarkCandidateFailed(res.Candidate.ID, "executor_timeout"); err != nil {
		t.Fatalf("MarkCandidateFailed: %v", err)
	}
	c, err := s.ResolveCandidate(res.Candidate.ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Status != "failed" {
		t.Fatalf("status = %q, want failed", c.Status)
	}
	st, err := s.ListReviewQueue(ReviewQueueFailed, 10)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	if len(st.States[ReviewQueueFailed]) != 1 ||
		st.States[ReviewQueueFailed][0].StatusReason != "executor_timeout" ||
		st.States[ReviewQueueFailed][0].Attempt != 1 {
		t.Fatalf("failed 态投影应带 reason/attempt，got %+v", st.States[ReviewQueueFailed])
	}
	// failed 可重试：再失败一次 attempt 继续涨。
	if err := s.MarkCandidateFailed(res.Candidate.ID, "executor_timeout"); err != nil {
		t.Fatalf("二次 MarkCandidateFailed: %v", err)
	}
	st, _ = s.ListReviewQueue(ReviewQueueFailed, 10)
	if got := st.States[ReviewQueueFailed]; len(got) != 1 || got[0].Attempt != 2 {
		t.Fatalf("重试应递增 attempt，got %+v", got)
	}

	// 超限 → quarantined，终态：不能再次隔离、不能再被提交。
	if err := s.MarkCandidateQuarantined(res.Candidate.ID, "attempts_exceeded"); err != nil {
		t.Fatalf("MarkCandidateQuarantined: %v", err)
	}
	c, _ = s.ResolveCandidate(res.Candidate.ID)
	if c.Status != "quarantined" {
		t.Fatalf("status = %q, want quarantined", c.Status)
	}
	if err := s.MarkCandidateQuarantined(res.Candidate.ID, "again"); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("quarantined 是终态，二次隔离应报 ErrCandidateClosed，got %v", err)
	}
	if err := s.MarkCandidateFailed(res.Candidate.ID, "again"); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("quarantined 不得回到 failed，got %v", err)
	}
	if _, err := s.CommitAccept(res.Candidate.ID, -1, commit.ActorHuman); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("quarantined 不得被提交，got %v", err)
	}
	if _, err := s.RejectCandidate(res.Candidate.ID); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("quarantined 不得被拒绝，got %v", err)
	}
	// 也不能被判定器当作待判定项再抓一遍。
	if list, err := s.ListUnjudgedCandidates("", 10); err != nil {
		t.Fatalf("ListUnjudgedCandidates: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("quarantined 不得进待判定队列，got %+v", list)
	}
}

func TestMarkCandidateFailedRejectsUnknownCandidate(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkCandidateFailed("missing", "x"); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("不存在的候选应报错，got %v", err)
	}
	if err := s.MarkCandidateQuarantined("missing", "x"); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("不存在的候选应报错，got %v", err)
	}
}

// 裁决账要能回答「哪套提示词写的这条记忆」以及「这条记忆是不是这次新建的」。
func TestRecordDecisionCarriesModelTagAndCreatedMemory(t *testing.T) {
	s := newTestStore(t)
	ev := captureEvidence(t, s, "uid-model", "s", "p", "构建日志：ok")
	fact := newCandidate(t, s, "fact", "p", "模型自动采纳要留 model_tag")
	attachCandidateEvidence(t, s, fact.Candidate.ID, ev.ID, ev.EventUID, ev.ArchiveHash, "direct")
	if err := s.MarkJudgeVerdict(fact.Candidate.ID, "accept", "deepseek:flash@judge-v2", "durable"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	view, err := s.CommitCandidate(fact.Candidate.ID)
	if err != nil {
		t.Fatalf("CommitCandidate: %v", err)
	}
	res, err := s.AutoCommitCandidate(fact.Candidate.ID, view.Revision)
	if err != nil {
		t.Fatalf("AutoCommitCandidate: %v", err)
	}
	if !res.CreatedMemory {
		t.Fatal("首次采纳应新建记忆（CreatedMemory=true）")
	}
	var modelTag string
	var created int
	if err := s.db.QueryRow(`SELECT model_tag, created_memory FROM commit_decisions
		WHERE candidate_id = ?`, fact.Candidate.ID).Scan(&modelTag, &created); err != nil {
		t.Fatalf("读裁决账: %v", err)
	}
	if modelTag != "deepseek:flash@judge-v2" || created != 1 {
		t.Fatalf("裁决账应带 model_tag/created_memory，got %q/%d", modelTag, created)
	}

	// 幂等键行为不变：同一 (revision, action, evidence_hash) 重放只留一条裁决。
	cur, err := s.CommitCandidate(fact.Candidate.ID)
	if err != nil {
		t.Fatalf("CommitCandidate: %v", err)
	}
	if _, err := s.AutoCommitCandidate(fact.Candidate.ID, cur.Revision); err != nil {
		t.Fatalf("重放: %v", err)
	}
	replayed := len(mustDecisions(t, s, fact.Candidate.ID))
	if _, err := s.AutoCommitCandidate(fact.Candidate.ID, cur.Revision); err != nil {
		t.Fatalf("再次重放: %v", err)
	}
	if again := len(mustDecisions(t, s, fact.Candidate.ID)); again != replayed {
		t.Fatalf("同一 revision 重放不得新增裁决行: %d -> %d", replayed, again)
	}
}

func mustDecisions(t *testing.T, s *Store, candidateID string) []commit.Decision {
	t.Helper()
	ds, err := s.CommitDecisions(candidateID)
	if err != nil {
		t.Fatalf("CommitDecisions: %v", err)
	}
	return ds
}

func TestListReviewQueueFourStates(t *testing.T) {
	s := newTestStore(t)
	// pending：模型还没跑（judge_verdict=''）
	pending := newCandidate(t, s, "fact", "p", "还没判定过的候选")
	// pending 但已判定：不进 pending 视图
	judged := newCandidate(t, s, "fact", "p", "已判定但仍待裁决")
	if err := s.MarkJudgeVerdict(judged.Candidate.ID, "accept", "m@v1", "durable"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	// failed / quarantined
	failed := newCandidate(t, s, "fact", "p", "执行失败的候选")
	if err := s.MarkCandidateFailed(failed.Candidate.ID, "boom"); err != nil {
		t.Fatalf("failed: %v", err)
	}
	quar := newCandidate(t, s, "fact", "p", "被隔离的候选")
	if err := s.MarkCandidateQuarantined(quar.Candidate.ID, "attempts"); err != nil {
		t.Fatalf("quarantined: %v", err)
	}
	// summary：模型自动写库、记忆仍未审核（S10b 前没有生产者，这里用夹具造）
	memID := seedModelAutoMemory(t, s, "模型自动写进库、等着人复核的记忆")

	q, err := s.ListReviewQueue(ReviewQueueAll, 50)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	if got := idsOf(q.States[ReviewQueuePending]); len(got) != 1 || got[0] != pending.Candidate.ID {
		t.Fatalf("pending 只应是未判定那条，got %v", got)
	}
	if got := idsOf(q.States[ReviewQueueFailed]); len(got) != 1 || got[0] != failed.Candidate.ID {
		t.Fatalf("failed 视图错，got %v", got)
	}
	if got := idsOf(q.States[ReviewQueueQuarantined]); len(got) != 1 || got[0] != quar.Candidate.ID {
		t.Fatalf("quarantined 视图错，got %v", got)
	}
	summary := q.States[ReviewQueueSummary]
	if len(summary) != 1 || summary[0].MemoryID != memID {
		t.Fatalf("summary 视图应按记忆投影，got %+v", summary)
	}
	if summary[0].Actor != commit.ActorModelAuto || summary[0].Action != string(commit.ActionAutoCommit) {
		t.Fatalf("summary 应带最新裁决 actor/action，got %+v", summary[0])
	}
	for k, want := range map[string]int{
		ReviewQueuePending: 1, ReviewQueueSummary: 1, ReviewQueueFailed: 1, ReviewQueueQuarantined: 1,
	} {
		if q.Counts[k] != want {
			t.Errorf("counts[%s] = %d, want %d", k, q.Counts[k], want)
		}
	}
	// 人工确认后退出复核摘要：summary 只列「还没人看过」的。
	if err := s.SetMemoryReviewState(memID, ReviewStateConfirmed, "human", 1700000000); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	q, err = s.ListReviewQueue(ReviewQueueSummary, 50)
	if err != nil {
		t.Fatalf("ListReviewQueue(summary): %v", err)
	}
	if len(q.States[ReviewQueueSummary]) != 0 || q.Counts[ReviewQueueSummary] != 0 {
		t.Fatalf("已确认记忆不得再进 summary，got %+v", q)
	}
	// 单态过滤：只填被请求的那一段，其余为空（计数仍给全量）。
	if len(q.States[ReviewQueuePending]) != 0 || len(q.States[ReviewQueueFailed]) != 0 {
		t.Fatalf("--state 过滤时其余段落必须为空，got %+v", q.States)
	}
}

// 同一记忆被多次自动裁决（重放/多次候选命中同一知识）时只算一行。
func TestListReviewQueueDedupesByMemory(t *testing.T) {
	s := newTestStore(t)
	memID := seedModelAutoMemory(t, s, "同一条知识被两次自动裁决")
	seedModelAutoDecision(t, s, "cand-b", memID, 1700000100)

	q, err := s.ListReviewQueue(ReviewQueueSummary, 50)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	if len(q.States[ReviewQueueSummary]) != 1 {
		t.Fatalf("同一记忆应去重成一行，got %+v", q.States[ReviewQueueSummary])
	}
	if q.Counts[ReviewQueueSummary] != 1 {
		t.Fatalf("计数也应按记忆去重，got %d", q.Counts[ReviewQueueSummary])
	}
}

func TestListReviewQueueUnknownStateAndLimit(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ListReviewQueue("bogus", 10); err == nil {
		t.Fatal("未知 state 必须报错，而不是静默返回空")
	}
	for i := 0; i < 5; i++ {
		newCandidate(t, s, "fact", "p", fmt.Sprintf("第 %d 条待判定", i))
	}
	q, err := s.ListReviewQueue(ReviewQueuePending, 2)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	if len(q.States[ReviewQueuePending]) != 2 {
		t.Fatalf("limit 应生效，got %d", len(q.States[ReviewQueuePending]))
	}
	if q.Counts[ReviewQueuePending] != 5 {
		t.Fatalf("计数不受 limit 影响，got %d", q.Counts[ReviewQueuePending])
	}
	// limit<=0 回落默认值，仍给结果
	q, err = s.ListReviewQueue(ReviewQueueAll, 0)
	if err != nil {
		t.Fatalf("limit=0: %v", err)
	}
	if len(q.States[ReviewQueuePending]) != 5 {
		t.Fatalf("默认 limit 应覆盖全部 5 条，got %d", len(q.States[ReviewQueuePending]))
	}
	// 空库 / 空态：四个键都存在，返回空切片而不是 nil（JSON 输出要稳定）。
	empty := newTestStore(t)
	q, err = empty.ListReviewQueue(ReviewQueueAll, 50)
	if err != nil {
		t.Fatalf("空库: %v", err)
	}
	for _, st := range ReviewQueueStates() {
		if q.States[st] == nil {
			t.Errorf("空库的 states[%s] 应为空切片而不是 nil", st)
		}
		if q.Counts[st] != 0 {
			t.Errorf("空库 counts[%s] = %d, want 0", st, q.Counts[st])
		}
	}
}

// seedModelAutoMemory 造一条「模型自动写库、尚未人工审核」的记忆，
// 并补一条 model-auto 裁决账（S10b 之前没有生产者，只能由夹具模拟）。
func seedModelAutoMemory(t *testing.T, s *Store, content string) string {
	t.Helper()
	res, err := s.Add(AddInput{Content: content, Source: "distill-accept", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE memories SET review_state = '' WHERE id = ?`, res.ID); err != nil {
		t.Fatalf("reset review_state: %v", err)
	}
	seedModelAutoDecision(t, s, "cand-"+res.ID[:8], res.ID, 1700000050)
	return res.ID
}

func seedModelAutoDecision(t *testing.T, s *Store, candidateID, memoryID string, at int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at)
		VALUES (?, 's', 'p', 'fact', ?, ?, 'accepted', 'distill', ?)`,
		candidateID, "cand content "+candidateID, "h-"+candidateID, at); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO commit_decisions
		(id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
		 memory_id, model_tag, created_memory, idempotency_key, created_at)
		VALUES (?,?,0,'','auto_commit','model-auto','["low_confidence"]',?,'m@v1',0,?,?)`,
		"dec-"+candidateID, candidateID, memoryID, "key-"+candidateID, at); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
}

func idsOf(items []ReviewItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// 直写来源白名单必须与 Add 的 source 取值域一致；
// 漏一个会让该通道的历史行永久停在「未审核」，多一个会把复核通道洗成已审核。
func TestDirectWriteSourceWhitelist(t *testing.T) {
	for _, src := range []string{"human", "llm", "ingest", "harness-summary", "bench", "test"} {
		if !isDirectWriteSource(src) {
			t.Errorf("%q 是直写通道，必须回填 confirmed", src)
		}
	}
	for _, src := range []string{"", "distill-accept", "mystery", "proposal"} {
		if isDirectWriteSource(src) {
			t.Errorf("%q 不是可证明的直写来源，必须留未审核", src)
		}
	}
	if !strings.Contains(directWriteSourcesSQL, "'harness-summary'") {
		t.Errorf("SQL 白名单与 Go 白名单必须同源: %s", directWriteSourcesSQL)
	}
}
