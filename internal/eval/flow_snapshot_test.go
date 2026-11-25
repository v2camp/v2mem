package eval

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// 薄 DB 读取只做 SQL→快照的投影：列顺序 = 结构体字段顺序，判定全部在纯函数里。
// 这里用真实临时库跑一遍全部查询，再单独打穿错误分支。

func TestLoadFlowSnapshotHappyPath(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	db := st.DB()

	mustExec(t, db, `INSERT INTO evidence_sources(id, kind, generation, status, last_seq, cursor_seq, created_at, updated_at)
		VALUES('src1','hook',1,'active',3,3,1,1)`)
	mustExec(t, db, `INSERT INTO evidence_archives(archive_hash, source_id, generation, from_seq, to_seq, raw_count, byte_size, path, created_at)
		VALUES('ah1','src1',1,1,1,1,10,'/tmp/a.gz',1)`)
	mustExec(t, db, `INSERT INTO evidence_bundles(id, source_id, generation, from_seq_exclusive, to_seq_inclusive,
		evidence_hash, status, attempt, last_run_id, created_at, updated_at)
		VALUES('b1','src1',1,0,3,'eh','ok',2,'run1',1,1)`)
	mustExec(t, db, `INSERT INTO observations(id, session_id, event, content_hash, created_at,
		source_id, source_generation, source_seq, event_uid, archive_hash)
		VALUES('o1','s1','stop','ch1',1,'src1',1,1,'u1','ah1'),
		      ('o2','s1','stop','ch2',1,'',0,0,'u2','')`)
	mustExec(t, db, `INSERT INTO migration_marks(entity_type, entity_id, mark, marked_at)
		VALUES('observation','o2','legacy_unpositioned',1)`)
	mustExec(t, db, `INSERT INTO distill_runs(id, session_id, provider, status, started_at, finished_at, bundle_id, attempt)
		VALUES('run1','s1','fake','ok',1,1,'b1',2)`)
	mustExec(t, db, `INSERT INTO memories(id, content, content_hash, created_at, updated_at, last_seen_at, origin_device)
		VALUES('m1','正文','mh1',1,1,1,'dev')`)
	mustExec(t, db, `INSERT INTO candidates(id, session_id, kind, content, content_hash, status, source,
		created_at, memory_id, bundle_id, revision)
		VALUES('c1','s1','fact','正文','mh1','accepted','distill',1,'m1','b1',1)`)
	mustExec(t, db, `INSERT INTO commit_decisions(id, candidate_id, revision, action, actor, memory_id, idempotency_key, created_at)
		VALUES('d1','c1',0,'human_accept','human','m1','k1',1)`)
	mustExec(t, db, `INSERT INTO candidate_evidence(candidate_id, evidence_id, evidence_role, created_at)
		VALUES('c1','e1','direct',1)`)
	mustExec(t, db, `INSERT INTO memory_evidence(memory_id, evidence_id, grade, created_at)
		VALUES('m1','e1','C',1)`)

	s, err := LoadFlowSnapshot(db, 100)
	if err != nil {
		t.Fatalf("LoadFlowSnapshot: %v", err)
	}
	if s.Now != 100 {
		t.Fatalf("Now = %d, want 100", s.Now)
	}
	if len(s.Observations) != 2 || len(s.Archives) != 1 || len(s.Bundles) != 1 ||
		len(s.Sources) != 1 || len(s.Runs) != 1 || len(s.Candidates) != 1 ||
		len(s.Decisions) != 1 || len(s.CandidateEvidence) != 1 ||
		len(s.MemoryEvidence) != 1 || len(s.Memories) != 1 {
		t.Fatalf("快照行数不对: %+v", s)
	}
	if s.Observations[1].Legacy != 1 {
		t.Fatalf("legacy 标记应投影为 1，got %+v", s.Observations[1])
	}
	if s.Bundles[0].Attempt != 2 || s.Bundles[0].LastRunID != "run1" {
		t.Fatalf("bundle 字段错位: %+v", s.Bundles[0])
	}
	if s.Decisions[0].Actor != "human" || s.Decisions[0].MemoryID != "m1" {
		t.Fatalf("decision 字段错位: %+v", s.Decisions[0])
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("Exec: %v\n%s", err, q)
	}
}

// 任一查询失败都必须返回 error，而不是带着半张快照静默出结论。
func TestLoadFlowSnapshotQueryError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db := st.DB()
	_ = st.Close() // 关闭后所有查询都失败

	if _, err := LoadFlowSnapshot(db, 1); err == nil {
		t.Fatal("查询失败时 LoadFlowSnapshot 必须返回 error")
	}
}

func TestFlowQueryScanAndDestErrors(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := flowQuery[struct{ One int }](st.DB(), `SELECT 1, 2`); err == nil ||
		!strings.Contains(err.Error(), "列数") {
		t.Fatalf("列数多于字段时应报错，got %v", err)
	}
	if _, err := flowQuery[struct{ One int }](st.DB(), `SELECT 'x'`); err == nil {
		t.Fatal("类型不可转换时应报 scan 错误")
	}
}
