package store

// S13 红 case（P1/P2/P3）：先写先红。
//
// P1 rejected 89 天不删 / 91 天删且同事务清边；quarantined/withdrawn 永不被 GC。
// P2 ok/noop 超龄删、quarantined 保留、仍被候选/记忆引用的证据不删。
// P3 决策回收后现存候选的裁决仍在、G9/G12 保绿。

import (
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/eval"
	"github.com/v2camp/v2mem/internal/testkit"
)

const day = 24 * time.Hour

func countQuery(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("计数失败 (%s): %v", query, err)
	}
	return n
}

// insertRejectedWithEvidence 造一条 rejected 候选：带理由码、证据引用与孤边。
// §2.2 要求这些「留到到期」，89 天时不许动，91 天删时同事务清干净。
func insertRejectedWithEvidence(t *testing.T, s *Store, id string, at int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source,
		 created_at, decided_at, status_reason, judge_reason)
		VALUES (?, 's', 'p', 'fact', ?, ?, 'rejected', 'distill', ?, ?, 'judge_rejected', 'low_confidence')`,
		id, "被拒内容 "+id, "h-"+id, at, at); err != nil {
		t.Fatalf("insert rejected %s: %v", id, err)
	}
	if _, err := s.db.Exec(`INSERT INTO candidate_evidence
		(candidate_id, evidence_id, created_at) VALUES (?, ?, ?)`,
		id, "ev-"+id, at); err != nil {
		t.Fatalf("insert candidate_evidence %s: %v", id, err)
	}
	memID := mustAdd(t, s, "孤边源记忆 "+id)
	if _, err := s.db.Exec(`INSERT INTO edges(src, dst, rel) VALUES (?, ?, 'derived_from')`,
		memID, id); err != nil {
		t.Fatalf("insert edge %s: %v", id, err)
	}
}

// P1：rejected 89 天不删（理由码 + 证据引用保留）/ 91 天删且同事务清边；
// quarantined / withdrawn 永不被 GC（等人工 / 按版本统计）。
func TestP1RejectedRetentionAndManualTerminals(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	insertRejectedWithEvidence(t, s, "r89", now.Add(-89*day).Unix())
	insertRejectedWithEvidence(t, s, "r91", now.Add(-91*day).Unix())

	veryOld := now.Add(-200 * day).Unix()
	for _, st := range []string{"quarantined", "withdrawn"} {
		if _, err := s.db.Exec(`INSERT INTO candidates
			(id, session_id, project, kind, content, content_hash, status, source, created_at, decided_at, status_reason)
			VALUES (?, 's', 'p', 'fact', ?, ?, ?, 'distill', ?, ?, 'manual')`,
			"c-"+st, "内容 "+st, "h-"+st, st, veryOld, veryOld); err != nil {
			t.Fatalf("insert %s: %v", st, err)
		}
	}

	if _, err := s.GCCandidates(14*day, 90*day); err != nil {
		t.Fatalf("GCCandidates: %v", err)
	}

	// 89 天：还在，理由码与证据引用都留着。
	if n := countQuery(t, s, `SELECT COUNT(*) FROM candidates WHERE id='r89'`); n != 1 {
		t.Errorf("P1: rejected 89 天不应删除，got %d", n)
	}
	if got := func() string {
		var v string
		_ = s.db.QueryRow(`SELECT judge_reason FROM candidates WHERE id='r89'`).Scan(&v)
		return v
	}(); got != "low_confidence" {
		t.Errorf("P1: 89 天的理由码应保留，got %q", got)
	}
	if n := countQuery(t, s, `SELECT COUNT(*) FROM candidate_evidence WHERE candidate_id='r89'`); n != 1 {
		t.Errorf("P1: 89 天的证据引用应保留，got %d", n)
	}

	// 91 天：候选行、孤边、证据引用同事务清干净。
	if n := countQuery(t, s, `SELECT COUNT(*) FROM candidates WHERE id='r91'`); n != 0 {
		t.Errorf("P1: rejected 91 天应删除，got %d", n)
	}
	if n := countQuery(t, s, `SELECT COUNT(*) FROM edges WHERE dst='r91'`); n != 0 {
		t.Errorf("P1: 91 天的孤边应同事务清掉，got %d", n)
	}
	if n := countQuery(t, s, `SELECT COUNT(*) FROM candidate_evidence WHERE candidate_id='r91'`); n != 0 {
		t.Errorf("P1: 91 天的证据引用应同事务清掉，got %d", n)
	}

	// 人工/统计终态：永不自动 GC。
	for _, st := range []string{"quarantined", "withdrawn"} {
		if n := countQuery(t, s, `SELECT COUNT(*) FROM candidates WHERE id=?`, "c-"+st); n != 1 {
			t.Errorf("P1: %s 永不被 GC，got %d", st, n)
		}
	}
}

// P2：ok/noop 完成 Bundle 超过 30 天可删（含 items）；quarantined / failed /
// pending / 保护期内一律不动；items 的证据仍被候选或记忆引用时整个 Bundle 保留。
func TestP2TerminalBundleRetentionAndEvidenceGuard(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	old := now.Add(-31 * day).Unix()
	fresh := now.Add(-1 * day).Unix()

	if _, err := s.db.Exec(`INSERT INTO evidence_sources(id, created_at, updated_at)
		VALUES ('src-p2', ?, ?)`, old, old); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	// 六个窗口：两个完成超龄、一个完成但被引、三个非终态/保护期内。
	bundles := []struct {
		id, status, evID string
		at               int64
	}{
		{"b-ok-old", "ok", "ev-ok-old", old},
		{"b-noop-old", "noop", "ev-noop-old", old},
		{"b-ref-old", "ok", "ev-ref-old", old},
		{"b-quar-old", "quarantined", "ev-quar-old", old},
		{"b-fail-old", "failed", "ev-fail-old", old},
		{"b-pending-old", "pending", "ev-pending-old", old},
		{"b-ok-fresh", "ok", "ev-ok-fresh", fresh},
	}
	for i, b := range bundles {
		if _, err := s.db.Exec(`INSERT INTO evidence_bundles
			(id, source_id, generation, from_seq_exclusive, to_seq_inclusive, evidence_hash,
			 status, created_at, updated_at)
			VALUES (?, 'src-p2', 1, ?, ?, ?, ?, ?, ?)`,
			b.id, i, i+1, "h-"+b.id, b.status, b.at, b.at); err != nil {
			t.Fatalf("insert bundle %s: %v", b.id, err)
		}
		if _, err := s.db.Exec(`INSERT INTO evidence_bundle_items
			(bundle_id, ordinal, evidence_id) VALUES (?, 1, ?)`, b.id, b.evID); err != nil {
			t.Fatalf("insert item %s: %v", b.id, err)
		}
	}

	// 让 b-ref-old 的证据被候选与记忆同时引用：该 Bundle 必须整个保留。
	if _, err := s.db.Exec(`INSERT INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at)
		VALUES ('c-p2', 's', 'p', 'fact', '引用证据的候选', 'h-c-p2', 'pending', 'distill', ?)`, old); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO candidate_evidence
		(candidate_id, evidence_id, created_at) VALUES ('c-p2', 'ev-ref-old', ?)`, old); err != nil {
		t.Fatalf("insert candidate_evidence: %v", err)
	}
	memID := mustAdd(t, s, "引用证据的记忆")
	if _, err := s.db.Exec(`INSERT INTO memory_evidence
		(memory_id, evidence_id, created_at) VALUES (?, 'ev-ref-old', ?)`, memID, old); err != nil {
		t.Fatalf("insert memory_evidence: %v", err)
	}

	got, err := s.GCTerminalBundles(30 * day)
	if err != nil {
		t.Fatalf("GCTerminalBundles: %v", err)
	}
	if got.Bundles != 2 || got.Items != 2 {
		t.Errorf("P2: 应删 2 个完成 Bundle / 2 条 items，got %+v", got)
	}
	for _, id := range []string{"b-ok-old", "b-noop-old"} {
		if n := countQuery(t, s, `SELECT COUNT(*) FROM evidence_bundles WHERE id=?`, id); n != 0 {
			t.Errorf("P2: %s 超龄完成应删除，got %d", id, n)
		}
		if n := countQuery(t, s, `SELECT COUNT(*) FROM evidence_bundle_items WHERE bundle_id=?`, id); n != 0 {
			t.Errorf("P2: %s 的 items 应随父 Bundle 删除，got %d", id, n)
		}
	}
	for _, id := range []string{"b-ref-old", "b-quar-old", "b-fail-old", "b-pending-old", "b-ok-fresh"} {
		if n := countQuery(t, s, `SELECT COUNT(*) FROM evidence_bundles WHERE id=?`, id); n != 1 {
			t.Errorf("P2: %s 应保留，got %d", id, n)
		}
	}
	if n := countQuery(t, s, `SELECT COUNT(*) FROM evidence_bundle_items WHERE bundle_id='b-ref-old'`); n != 1 {
		t.Errorf("P2: 被引用证据的 items 不许删，got %d", n)
	}
}

// P3：决策回收只删「对应候选已被 GC 且超过保留窗口」的孤立行；
// 现存候选的裁决必须全留（否则 eval G9 会红），G9/G12 保绿。
func TestP3DecisionGCKeepsLiveCandidateVerdicts(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	old := now.Add(-100 * day).Unix()

	// 两条现存终态候选 + 各自裁决。
	for _, tc := range []struct{ id, status, action string }{
		{"c-live-a", "accepted", "auto_commit"},
		{"c-live-r", "rejected", "reject"},
	} {
		if _, err := s.db.Exec(`INSERT INTO candidates
			(id, session_id, project, kind, content, content_hash, status, source, created_at)
			VALUES (?, 's', 'p', 'fact', ?, ?, ?, 'distill', ?)`,
			tc.id, "内容 "+tc.id, "h-"+tc.id, tc.status, old); err != nil {
			t.Fatalf("insert candidate %s: %v", tc.id, err)
		}
		if _, err := s.db.Exec(`INSERT INTO commit_decisions
			(id, candidate_id, revision, action, actor, idempotency_key, created_at)
			VALUES (?, ?, 0, ?, 'auto', ?, ?)`,
			"d-"+tc.id, tc.id, tc.action, "key-"+tc.id, old); err != nil {
			t.Fatalf("insert decision %s: %v", tc.id, err)
		}
	}
	// 一条孤立决策：候选已被 GC 删掉、且超过窗口；一条孤立但在保护期内。
	if _, err := s.db.Exec(`INSERT INTO commit_decisions
		(id, candidate_id, revision, action, actor, idempotency_key, created_at)
		VALUES ('d-orphan-old', 'c-gone', 0, 'reject', 'auto', 'key-orphan-old', ?)`, old); err != nil {
		t.Fatalf("insert orphan: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO commit_decisions
		(id, candidate_id, revision, action, actor, idempotency_key, created_at)
		VALUES ('d-orphan-fresh', 'c-gone2', 0, 'reject', 'auto', 'key-orphan-fresh', ?)`,
		now.Add(-1*day).Unix()); err != nil {
		t.Fatalf("insert fresh orphan: %v", err)
	}

	got, err := s.GCCommitDecisions(90 * day)
	if err != nil {
		t.Fatalf("GCCommitDecisions: %v", err)
	}
	if got.Deleted != 1 {
		t.Errorf("P3: 只应删 1 条孤立超窗决策，got %+v", got)
	}
	for _, id := range []string{"d-c-live-a", "d-c-live-r", "d-orphan-fresh"} {
		if n := countQuery(t, s, `SELECT COUNT(*) FROM commit_decisions WHERE id=?`, id); n != 1 {
			t.Errorf("P3: %s 应保留，got %d", id, n)
		}
	}
	if n := countQuery(t, s, `SELECT COUNT(*) FROM commit_decisions WHERE id='d-orphan-old'`); n != 0 {
		t.Errorf("P3: 孤立超窗决策应删除，got %d", n)
	}

	// 现存候选的裁决仍在 ⇒ G9（裁决闭环）/ G12（准出率对账）保绿。
	snap, err := eval.LoadFlowSnapshot(s.db, now.Unix())
	if err != nil {
		t.Fatalf("LoadFlowSnapshot: %v", err)
	}
	rep := eval.CheckFlow(snap, 0)
	for _, c := range rep.Checks {
		if (c.ID == "G9" || c.ID == "G12") && !c.Pass {
			t.Errorf("P3: %s 变红：%s", c.ID, c.Detail)
		}
	}
}

// ---------- 新增分支的覆盖用例（store 覆盖率余量只有 0.1pt，不许留未测分支）----------

// seedTerminalBundle 造一个超龄 ok 完成 Bundle（含一条 item），供默认保留期与
// 写失败回滚用例复用。evidence_bundles 的 UNIQUE 键是 (source,generation,from)，
// 所以每个用例只放一个窗口。
func seedTerminalBundle(t *testing.T, s *Store, id string, at int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO evidence_sources(id, created_at, updated_at)
		VALUES ('src-cov', ?, ?)`, at, at); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO evidence_bundles
		(id, source_id, generation, from_seq_exclusive, to_seq_inclusive, evidence_hash,
		 status, created_at, updated_at)
		VALUES (?, 'src-cov', 1, 0, 1, ?, 'ok', ?, ?)`, id, "h-"+id, at, at); err != nil {
		t.Fatalf("insert bundle: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO evidence_bundle_items(bundle_id, ordinal, evidence_id)
		VALUES (?, 1, ?)`, id, "ev-"+id); err != nil {
		t.Fatalf("insert item: %v", err)
	}
}

// 传 0 走默认 30 天保留期；空集合时 SkippedReferenced 也不许为负。
func TestGCTerminalBundlesDefaultRetention(t *testing.T) {
	s := newTestStore(t)
	old := time.Now().Add(-31 * day).Unix()
	seedTerminalBundle(t, s, "b-default", old)

	got, err := s.GCTerminalBundles(0)
	if err != nil {
		t.Fatalf("GCTerminalBundles(0): %v", err)
	}
	if got.Bundles != 1 || got.Items != 1 || got.SkippedReferenced != 0 {
		t.Fatalf("默认保留期应删 1 个 Bundle / 1 条 item，got %+v", got)
	}
	// 空库再跑一次：零删除、零跳过，幂等。
	got2, err := s.GCTerminalBundles(0)
	if err != nil {
		t.Fatalf("二次调用: %v", err)
	}
	if got2.Bundles != 0 || got2.Items != 0 || got2.SkippedReferenced != 0 {
		t.Fatalf("空集合应零回收，got %+v", got2)
	}
}

// 删 items / 删 Bundle 任一写失败都必须整轮回滚（同事务，不留半删）。
func TestGCTerminalBundlesRollsBackOnWriteFailure(t *testing.T) {
	for _, table := range []string{"evidence_bundle_items", "evidence_bundles"} {
		t.Run(table, func(t *testing.T) {
			s := newTestStore(t)
			old := time.Now().Add(-31 * day).Unix()
			seedTerminalBundle(t, s, "b-fail", old)
			undo, err := testkit.FailWritesOn(s.db, table, "DELETE")
			if err != nil {
				t.Fatalf("FailWritesOn(%s): %v", table, err)
			}
			defer undo()
			if _, err := s.GCTerminalBundles(30 * day); err == nil {
				t.Fatalf("%s 的 DELETE 失败应冒泡", table)
			}
			if n := countQuery(t, s, `SELECT COUNT(*) FROM evidence_bundles WHERE id='b-fail'`); n != 1 {
				t.Errorf("失败后 Bundle 应原样保留，got %d", n)
			}
			if n := countQuery(t, s, `SELECT COUNT(*) FROM evidence_bundle_items WHERE bundle_id='b-fail'`); n != 1 {
				t.Errorf("失败后 items 应原样保留，got %d", n)
			}
		})
	}
}

// 库连接已关时 Begin 失败必须原样上报，不吞错。
func TestGCTerminalBundlesSurfacesBeginFailure(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.GCTerminalBundles(30 * day); err == nil {
		t.Fatal("Begin 失败应上报")
	}
}

// 决策回收：传 0 走默认 90 天；写失败如实冒泡。
func TestGCCommitDecisionsDefaultAndWriteFailure(t *testing.T) {
	s := newTestStore(t)
	old := time.Now().Add(-100 * day).Unix()
	if _, err := s.db.Exec(`INSERT INTO commit_decisions
		(id, candidate_id, revision, action, actor, idempotency_key, created_at)
		VALUES ('d-cov', 'c-gone', 0, 'reject', 'auto', 'key-cov', ?)`, old); err != nil {
		t.Fatalf("insert decision: %v", err)
	}

	undo, err := testkit.FailWritesOn(s.db, "commit_decisions", "DELETE")
	if err != nil {
		t.Fatalf("FailWritesOn: %v", err)
	}
	if _, err := s.GCCommitDecisions(0); err == nil {
		t.Fatal("DELETE 失败应冒泡")
	}
	if n := countQuery(t, s, `SELECT COUNT(*) FROM commit_decisions WHERE id='d-cov'`); n != 1 {
		t.Errorf("失败后决策行应原样保留，got %d", n)
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}

	got, err := s.GCCommitDecisions(0)
	if err != nil {
		t.Fatalf("GCCommitDecisions(0): %v", err)
	}
	if got.Deleted != 1 {
		t.Errorf("默认保留期应删孤立决策，got %+v", got)
	}
}
