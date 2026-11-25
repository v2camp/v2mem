package store

// S4：积压五指标的 store 侧口径（design-data-lifecycle §3.2）。
// 每个年龄都是原始值 now-ts，无数据必须是 nil 而不是 0；MIN 走 sql.NullInt64。

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/testkit"
)

func backlogNow(t *testing.T) int64 {
	t.Helper()
	return testkit.NewManualClock(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)).Now().Unix()
}

func insertBacklogCandidate(t *testing.T, s *Store, id, status string, createdAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO candidates(id, session_id, project, kind, content,
		content_hash, status, source, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		id, "sess-"+id, "p", "fact", "content-"+id, "hash-"+id, status, "distill", createdAt); err != nil {
		t.Fatalf("插入候选 %s: %v", id, err)
	}
}

func insertBacklogSource(t *testing.T, s *Store, id string, generation, lastSeq, cursorSeq int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO evidence_sources(id, kind, locator, generation,
		last_seq, cursor_seq, created_at, updated_at)
		VALUES(?,'hook',?,?,?,?,?,?)`,
		id, "/tmp/"+id, generation, lastSeq, cursorSeq, 1000, 1000); err != nil {
		t.Fatalf("插入证据源 %s: %v", id, err)
	}
}

func insertBacklogArchive(t *testing.T, s *Store, hash, sourceID string, generation, toSeq, createdAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO evidence_archives(archive_hash, source_id, generation,
		from_seq, to_seq, raw_count, byte_size, path, created_at)
		VALUES(?,?,?,1,?,1,1,?,?)`,
		hash, sourceID, generation, toSeq, "/tmp/"+hash+".gz", createdAt); err != nil {
		t.Fatalf("插入归档 %s: %v", hash, err)
	}
}

func insertBacklogBundle(t *testing.T, s *Store, id, sourceID string, generation, fromSeq int64, status string, createdAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO evidence_bundles(id, source_id, generation,
		from_seq_exclusive, to_seq_inclusive, evidence_hash, status, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		id, sourceID, generation, fromSeq, fromSeq+1, "h-"+id, status, createdAt, createdAt); err != nil {
		t.Fatalf("插入 Bundle %s: %v", id, err)
	}
}

// T1：空库——计数全 0、四个年龄与最后维护全 nil。
func TestBacklogEmptyStore(t *testing.T) {
	s := newTestStore(t)
	now := backlogNow(t)

	b, err := s.Backlog(now)
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if b.PendingCount != 0 || b.EvidenceBacklogSources != 0 || b.OpenBundles != 0 ||
		b.CandidatesIsolated != 0 || b.BundlesIsolated != 0 {
		t.Fatalf("空库计数应全 0: %+v", b)
	}
	if b.OldestPendingAge != nil || b.OldestUnconsumedEvidenceAge != nil ||
		b.OldestOpenBundleAge != nil || b.LastMaintainOKAt != nil || b.LastMaintainOKAge != nil {
		t.Fatalf("空库年龄与最后维护应为 nil: %+v", b)
	}

	// 查询失败必须冒泡，不能返回一份全 0 的「健康」读数。
	closed := newTestStore(t)
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := closed.Backlog(now); err == nil {
		t.Fatal("closed store 的 Backlog 必须报错")
	}
}

// T2：3 条 pending，最老取真实最早；全改成未来时间后年龄夹 0。
func TestBacklogOldestPendingAgeAndClamp(t *testing.T) {
	s := newTestStore(t)
	now := backlogNow(t)
	insertBacklogCandidate(t, s, "c-2h", "pending", now-2*3600)
	insertBacklogCandidate(t, s, "c-1h", "pending", now-3600)
	insertBacklogCandidate(t, s, "c-future", "pending", now+600)

	b, err := s.Backlog(now)
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if b.PendingCount != 3 {
		t.Fatalf("pending 应为 3，got %d", b.PendingCount)
	}
	if b.OldestPendingAge == nil || *b.OldestPendingAge != 2*3600 {
		t.Fatalf("最老 pending 年龄应为 7200，got %v", b.OldestPendingAge)
	}

	// 全部推到未来：now-ts 为负，必须夹到 0，而不是报负数。
	if _, err := s.db.Exec(`UPDATE candidates SET created_at = ?`, now+3600); err != nil {
		t.Fatalf("老化 pending: %v", err)
	}
	b, err = s.Backlog(now)
	if err != nil {
		t.Fatalf("Backlog(未来): %v", err)
	}
	if b.OldestPendingAge == nil || *b.OldestPendingAge != 0 {
		t.Fatalf("未来时间应夹 0，got %v", b.OldestPendingAge)
	}
}

// T3：源数按 last_seq>cursor_seq；证据年龄只认「源代际匹配且 to_seq>cursor_seq」的归档。
func TestBacklogEvidenceBacklogSourcesAndOldest(t *testing.T) {
	s := newTestStore(t)
	now := backlogNow(t)
	// A：活跃源，代际 1，cursor 停在 2；归档到 5，未消费。
	insertBacklogSource(t, s, "src-active", 1, 5, 2)
	insertBacklogArchive(t, s, "a-active", "src-active", 1, 5, now-3*3600)
	// B：跨代际源——归档属于上一代（gen1），不能算进未消费年龄。
	insertBacklogSource(t, s, "src-regenerated", 2, 5, 0)
	insertBacklogArchive(t, s, "a-stale-gen", "src-regenerated", 1, 5, now-10*3600)
	// C：已消费源，归档再老也不算。
	insertBacklogSource(t, s, "src-consumed", 1, 5, 5)
	insertBacklogArchive(t, s, "a-consumed", "src-consumed", 1, 5, now-20*3600)

	b, err := s.Backlog(now)
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if b.EvidenceBacklogSources != 2 {
		t.Fatalf("未消费源应为 2（A 与跨代际 B），got %d", b.EvidenceBacklogSources)
	}
	if b.OldestUnconsumedEvidenceAge == nil || *b.OldestUnconsumedEvidenceAge != 3*3600 {
		t.Fatalf("最老未消费证据年龄应为 10800（只认代际匹配的 A），got %v", b.OldestUnconsumedEvidenceAge)
	}

	// 推进 cursor 到 last_seq：源数归 0，年龄归 nil。
	if _, err := s.db.Exec(`UPDATE evidence_sources SET cursor_seq = last_seq`); err != nil {
		t.Fatalf("推进 cursor: %v", err)
	}
	b, err = s.Backlog(now)
	if err != nil {
		t.Fatalf("Backlog(推进后): %v", err)
	}
	if b.EvidenceBacklogSources != 0 {
		t.Fatalf("cursor 追平后未消费源应为 0，got %d", b.EvidenceBacklogSources)
	}
	if b.OldestUnconsumedEvidenceAge != nil {
		t.Fatalf("cursor 追平后证据年龄应为 nil，got %v", b.OldestUnconsumedEvidenceAge)
	}
}

// T4：5 个 open 状态全部计入；更老的 ok/noop/quarantined 不进 open 集。
func TestBacklogOpenBundlesCountsOpenStatusesOnly(t *testing.T) {
	s := newTestStore(t)
	now := backlogNow(t)
	insertBacklogSource(t, s, "src-b", 1, 100, 0)

	open := []string{"pending", "leased", "fail", "partial", "backlog"}
	for i, status := range open {
		insertBacklogBundle(t, s, "b-open-"+status, "src-b", 1, int64(i), status, now-int64(5-i)*3600)
	}
	// 更老的终结态：如果查询漏了 status 过滤，最老年龄会变成 100h。
	insertBacklogBundle(t, s, "b-ok", "src-b", 1, 10, "ok", now-100*3600)
	insertBacklogBundle(t, s, "b-noop", "src-b", 1, 11, "noop", now-90*3600)
	insertBacklogBundle(t, s, "b-quarantined", "src-b", 1, 12, "quarantined", now-80*3600)

	b, err := s.Backlog(now)
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if b.OpenBundles != 5 {
		t.Fatalf("open Bundle 应为 5，got %d", b.OpenBundles)
	}
	if b.OldestOpenBundleAge == nil || *b.OldestOpenBundleAge != 5*3600 {
		t.Fatalf("最老 open Bundle 年龄应取 open 集的 5h，got %v", b.OldestOpenBundleAge)
	}
}

// T5：隔离量 = candidates(quarantined|failed) 与 bundles(quarantined)。
func TestBacklogIsolatedCounts(t *testing.T) {
	s := newTestStore(t)
	now := backlogNow(t)
	insertBacklogCandidate(t, s, "c-q", "quarantined", now-3600)
	insertBacklogCandidate(t, s, "c-f", "failed", now-7200)
	insertBacklogCandidate(t, s, "c-p", "pending", now-1800)
	insertBacklogCandidate(t, s, "c-r", "rejected", now-900)

	insertBacklogSource(t, s, "src-i", 1, 5, 0)
	insertBacklogBundle(t, s, "b-q", "src-i", 1, 0, "quarantined", now-3600)
	insertBacklogBundle(t, s, "b-ok", "src-i", 1, 1, "ok", now-3600)

	b, err := s.Backlog(now)
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if b.CandidatesIsolated != 2 {
		t.Fatalf("隔离候选应为 2（quarantined+failed），got %d", b.CandidatesIsolated)
	}
	if b.BundlesIsolated != 1 {
		t.Fatalf("隔离 Bundle 应为 1，got %d", b.BundlesIsolated)
	}
}

// T6：maintain:last_ok 正常值精确；坏值必须报错且带键名，不能当成「从未维护」。
func TestBacklogLastMaintainOK(t *testing.T) {
	s := newTestStore(t)
	now := backlogNow(t)
	okAt := now - 1234
	if err := s.MetaSet(MetaMaintainLastOK, strconv.FormatInt(okAt, 10)); err != nil {
		t.Fatalf("MetaSet: %v", err)
	}

	b, err := s.Backlog(now)
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if b.LastMaintainOKAt == nil || *b.LastMaintainOKAt != okAt {
		t.Fatalf("最后维护时间应为 %d，got %v", okAt, b.LastMaintainOKAt)
	}
	if b.LastMaintainOKAge == nil || *b.LastMaintainOKAge != 1234 {
		t.Fatalf("最后维护年龄应为 1234，got %v", b.LastMaintainOKAge)
	}

	if err := s.MetaSet(MetaMaintainLastOK, "bad"); err != nil {
		t.Fatalf("MetaSet(bad): %v", err)
	}
	if _, err := s.Backlog(now); err == nil || !strings.Contains(err.Error(), MetaMaintainLastOK) {
		t.Fatalf("坏值应报错且含键名 %q，got %v", MetaMaintainLastOK, err)
	}

	// 元数据表读失败也必须报错：把「读不到」伪装成「从未维护」会掩盖库损坏。
	if _, err := s.db.Exec(`DROP TABLE db_meta`); err != nil {
		t.Fatalf("DROP db_meta: %v", err)
	}
	if _, err := s.Backlog(now); err == nil || !strings.Contains(err.Error(), MetaMaintainLastOK) {
		t.Fatalf("MetaGet 失败应报错且含键名 %q，got %v", MetaMaintainLastOK, err)
	}
}
