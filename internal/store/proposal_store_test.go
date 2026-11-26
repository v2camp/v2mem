package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustBundle(t *testing.T, s *Store, prefix string, n int) *EvidenceBundleRecord {
	t.Helper()
	for i := 0; i < n; i++ {
		mustCaptureEvidence(t, s, evidenceInput(prefix+itoa(i), 0))
	}
	b, err := s.NextEvidenceBundle(50, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("应拿到 Bundle")
	}
	return b
}

func TestProposalBundleClaimFinishAndIdempotency(t *testing.T) {
	s := newTestStore(t)
	b := mustBundle(t, s, "uid-a-", 2)

	if head, ok, err := s.NextOpenProposalBundle(); err != nil || !ok || head.ID != b.ID {
		t.Fatalf("新建 Bundle 应是最老的未终结窗口: %+v ok=%v err=%v", head, ok, err)
	}

	claimed, err := s.ClaimProposalBundle(b.ID, "w1", 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != "leased" || claimed.Attempt != 1 || claimed.LeaseOwner != "w1" || claimed.LeaseUntil != 1060 {
		t.Fatalf("认领状态不对: %+v", claimed)
	}
	if len(claimed.EvidenceIDs) != 2 {
		t.Fatalf("认领应带条目 id: %+v", claimed.EvidenceIDs)
	}

	// 租约未过期：第二次认领必须被挡住，且 attempt 不增加。
	if _, err := s.ClaimProposalBundle(b.ID, "w2", 1001, time.Minute); !errors.Is(err, ErrProposalBundleLeased) {
		t.Fatalf("重复认领应被租约挡住: %v", err)
	}

	if err := s.FinishProposalBundle(b.ID, "w2", "ok", 0, "run-x"); !errors.Is(err, ErrProposalBundleLeaseLost) {
		t.Fatalf("非持有者不得完成: %v", err)
	}
	if err := s.FinishProposalBundle(b.ID, "w1", "ok", 0, "run-1"); err != nil {
		t.Fatal(err)
	}
	// 重复完成同一 run 是幂等的。
	if err := s.FinishProposalBundle(b.ID, "w1", "ok", 0, "run-1"); err != nil {
		t.Fatalf("重复完成应幂等: %v", err)
	}
	if err := s.FinishProposalBundle(b.ID, "w1", "ok", 0, "run-2"); !errors.Is(err, ErrProposalBundleLeaseLost) {
		t.Fatalf("已终结窗口不能再完成: %v", err)
	}

	got, err := s.LoadProposalBundle(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || got.LeaseOwner != "" || got.LeaseUntil != 0 || got.LastRunID != "run-1" {
		t.Fatalf("完成后租约应释放: %+v", got)
	}
	if _, ok, err := s.NextOpenProposalBundle(); err != nil || ok {
		t.Fatalf("ok 不应留在待处理队列: ok=%v err=%v", ok, err)
	}
	if next, err := s.NextEvidenceBundle(50, 1<<20); err != nil || next != nil {
		t.Fatalf("cursor 已到末尾，不应再出窗: %+v err=%v", next, err)
	}
	if _, err := s.LoadProposalBundle("bnd-missing"); !errors.Is(err, ErrEvidenceBundleNotFound) {
		t.Fatalf("未知 Bundle 应 NotFound: %v", err)
	}
}

func TestProposalBundleFailRetryRespectsNotBefore(t *testing.T) {
	s := newTestStore(t)
	b := mustBundle(t, s, "uid-b-", 1)
	if _, err := s.ClaimProposalBundle(b.ID, "w1", 1000, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishProposalBundle(b.ID, "w1", "fail", 2000, "run-f"); err != nil {
		t.Fatal(err)
	}
	head, ok, err := s.NextOpenProposalBundle()
	if err != nil || !ok || head.ID != b.ID {
		t.Fatalf("fail 必须留在队列里: %+v ok=%v err=%v", head, ok, err)
	}
	if _, err := s.ClaimProposalBundle(b.ID, "w1", 1500, time.Minute); !errors.Is(err, ErrProposalBundleLeased) {
		t.Fatalf("not_before 未到不得认领: %v", err)
	}
	again, err := s.ClaimProposalBundle(b.ID, "w1", 2000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if again.Attempt != 2 {
		t.Fatalf("重试应累加 attempt，got %d", again.Attempt)
	}
	// 租约过期后可被另一个 worker 接手。
	if err := s.FinishProposalBundle(b.ID, "w1", "partial", 0, "run-p"); err != nil {
		t.Fatal(err)
	}
	third, err := s.ClaimProposalBundle(b.ID, "w2", 2000, time.Minute)
	if err != nil || third.LeaseOwner != "w2" || third.Attempt != 3 {
		t.Fatalf("partial 应可重试: %+v err=%v", third, err)
	}
}

func TestProposalBundleOldestBlocksLaterWindows(t *testing.T) {
	s := newTestStore(t)
	first := mustBundle(t, s, "uid-c-", 1)
	second := mustBundle(t, s, "uid-d-", 1)
	if first.ID == second.ID {
		t.Fatal("两次取窗应是不同 Bundle")
	}
	head, ok, err := s.NextOpenProposalBundle()
	if err != nil || !ok || head.ID != first.ID {
		t.Fatalf("应先返回最老窗口: %+v ok=%v err=%v", head, ok, err)
	}
	// 认领并失败第一窗；第二窗仍不能被越过。
	if _, err := s.ClaimProposalBundle(first.ID, "w", 1000, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishProposalBundle(first.ID, "w", "fail", 9999999999, "run-1"); err != nil {
		t.Fatal(err)
	}
	head, ok, err = s.NextOpenProposalBundle()
	if err != nil || !ok || head.ID != first.ID {
		t.Fatalf("失败窗口应继续挡在第二窗前: %+v ok=%v err=%v", head, ok, err)
	}
}

// S6b：按源公平调度的定点构造——直接写调度字段，避免依赖真实时钟。
// evidence_bundle_items 留空：本组用例只关心「谁的头可运行」，
// 条目内容由 proposal 包的真实流程覆盖。
func insertFairBundle(t *testing.T, s *Store, id, sourceID string, fromSeq int64,
	status string, createdAt, notBefore, leaseUntil int64, leaseOwner string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO evidence_bundles(id, source_id, generation,
		from_seq_exclusive, to_seq_inclusive, evidence_hash, status, attempt,
		not_before, lease_until, lease_owner, created_at, updated_at)
		VALUES(?,?,1,?,?,?,?,0,?,?,?,?,?)`,
		id, sourceID, fromSeq, fromSeq+1, "h-"+id, status,
		notBefore, leaseUntil, leaseOwner, createdAt, createdAt); err != nil {
		t.Fatalf("插入 Bundle %s: %v", id, err)
	}
}

// F1：A 的头在退避（not_before 未来），B 的头可运行 → 返回 B。
//
// 对照组同时钉住：NextOpenProposalBundle 仍是全局最老（A 的头），
// 公平调度是新口径，不是把旧语义改掉。
func TestNextRunnableSkipsSourceWhoseHeadBacksOff(t *testing.T) {
	s := newTestStore(t)
	insertBacklogSource(t, s, "src-a", 1, 10, 0)
	insertBacklogSource(t, s, "src-b", 1, 10, 0)
	insertFairBundle(t, s, "bnd-a1", "src-a", 0, "fail", 100, 5000, 0, "")
	insertFairBundle(t, s, "bnd-b1", "src-b", 0, "pending", 200, 0, 0, "")

	got, ok, err := s.NextRunnableProposalBundle(1000)
	if err != nil {
		t.Fatalf("NextRunnableProposalBundle: %v", err)
	}
	if !ok || got.ID != "bnd-b1" {
		t.Fatalf("退避中的 A 头应整源让位，返回 B: %+v ok=%v", got, ok)
	}
	if len(got.EvidenceIDs) != 0 {
		t.Fatalf("无条目的 Bundle 应返回空证据集: %+v", got.EvidenceIDs)
	}

	head, ok, err := s.NextOpenProposalBundle()
	if err != nil || !ok || head.ID != "bnd-a1" {
		t.Fatalf("NextOpenProposalBundle 必须保持全局最老语义: %+v ok=%v err=%v", head, ok, err)
	}
}

// F2：多个源的头都可运行 → 取 created_at 最老；同秒按 id 升序兜底。
// now<=0 走当前时间分支：这些夹具的时间戳都远早于当下，仍应选中同一条。
func TestNextRunnablePicksOldestRunnableHead(t *testing.T) {
	s := newTestStore(t)
	insertBacklogSource(t, s, "src-a", 1, 10, 0)
	insertBacklogSource(t, s, "src-b", 1, 10, 0)
	insertBacklogSource(t, s, "src-c", 1, 10, 0)
	insertFairBundle(t, s, "bnd-a1", "src-a", 0, "pending", 300, 0, 0, "")
	insertFairBundle(t, s, "bnd-c1", "src-c", 0, "pending", 100, 0, 0, "")
	insertFairBundle(t, s, "bnd-b1", "src-b", 0, "pending", 100, 0, 0, "")

	got, ok, err := s.NextRunnableProposalBundle(1000)
	if err != nil || !ok || got.ID != "bnd-b1" {
		t.Fatalf("应取 created_at 最老、同秒 id 最小者: %+v ok=%v err=%v", got, ok, err)
	}
	got, ok, err = s.NextRunnableProposalBundle(0)
	if err != nil || !ok || got.ID != "bnd-b1" {
		t.Fatalf("now<=0 应取当前时间并返回同一条: %+v ok=%v err=%v", got, ok, err)
	}
}

// F3：A 的头被挡住时，A 的第二窗绝不能顶上来；只有 A 有积压 → 无候选。
func TestNextRunnableNeverSkipsBlockedSourceHead(t *testing.T) {
	s := newTestStore(t)
	insertBacklogSource(t, s, "src-a", 1, 10, 0)
	insertFairBundle(t, s, "bnd-a1", "src-a", 0, "fail", 100, 5000, 0, "")
	insertFairBundle(t, s, "bnd-a2", "src-a", 1, "pending", 200, 0, 0, "")

	got, ok, err := s.NextRunnableProposalBundle(1000)
	if err != nil {
		t.Fatalf("NextRunnableProposalBundle: %v", err)
	}
	if ok {
		t.Fatalf("头被挡住时整源让位，不得返回第二窗: %+v", got)
	}

	// 头恢复可运行后，返回的仍是头而不是更晚的第二窗。
	if _, err := s.db.Exec(`UPDATE evidence_bundles SET not_before=0 WHERE id='bnd-a1'`); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.NextRunnableProposalBundle(1000)
	if err != nil || !ok || got.ID != "bnd-a1" {
		t.Fatalf("头可运行时应先跑头: %+v ok=%v err=%v", got, ok, err)
	}
}

// F4：头被他人租约占住 → 整源让位；租约过期后恢复候选。
func TestNextRunnableYieldsLeasedHeadUntilExpiry(t *testing.T) {
	s := newTestStore(t)
	insertBacklogSource(t, s, "src-a", 1, 10, 0)
	insertBacklogSource(t, s, "src-b", 1, 10, 0)
	insertFairBundle(t, s, "bnd-a1", "src-a", 0, "leased", 100, 0, 2000, "other")
	insertFairBundle(t, s, "bnd-a2", "src-a", 1, "pending", 200, 0, 0, "")
	insertFairBundle(t, s, "bnd-b1", "src-b", 0, "pending", 300, 0, 0, "")

	got, ok, err := s.NextRunnableProposalBundle(1000)
	if err != nil || !ok || got.ID != "bnd-b1" {
		t.Fatalf("租约中的 A 头应整源让位: %+v ok=%v err=%v", got, ok, err)
	}

	// lease_until == now 视为已过期（边界取闭区间）。
	got, ok, err = s.NextRunnableProposalBundle(2000)
	if err != nil || !ok || got.ID != "bnd-a1" {
		t.Fatalf("租约过期后应恢复 A 的头（不是第二窗）: %+v ok=%v err=%v", got, ok, err)
	}

	// owner 为空时，即使 lease_until 在未来也不算占用。
	insertBacklogSource(t, s, "src-c", 1, 10, 0)
	insertFairBundle(t, s, "bnd-c1", "src-c", 0, "pending", 50, 0, 9999, "")
	if got, ok, err := s.NextRunnableProposalBundle(1000); err != nil || !ok || got.ID != "bnd-c1" {
		t.Fatalf("owner 为空的租约不应挡住候选: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestProposalRunSaveAndList(t *testing.T) {
	s := newTestStore(t)
	b := mustBundle(t, s, "uid-b-", 1)
	// 同 session 同一秒连续追账是合法形态（maintain 一次处理多个窗口）：
	// v4 去掉了旧的表级 UNIQUE(session_id, started_at)，写入方不得再篡改时间戳。
	const n = 5
	for i := 0; i < n; i++ {
		rec := ProposalRunRecord{
			SessionID: "sess-1", Provider: "local", Status: "ok", CandCount: 2,
			StartedAt: 100, FinishedAt: 120, BundleID: b.ID, Executor: "local",
			SchemaVersion: 1, GeneratedCount: 3, ValidCount: 2, InvalidCount: 1,
			DuplicateCount: 1, Attempt: i + 1, SourceFingerprint: b.EvidenceHash,
		}
		if i == 1 {
			rec.Status = "partial"
		}
		if err := s.SaveProposalRun(rec); err != nil {
			t.Fatalf("第 %d 条同秒运行账应落库: %v", i+1, err)
		}
	}
	runs, err := s.ProposalRuns(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != n {
		t.Fatalf("应有 %d 条运行账，got %d", n, len(runs))
	}
	// 同一秒的运行账按 (started_at, id) 排序，行间无序：逐条核对列值，不假设首行。
	seen := map[int]bool{}
	for i, r := range runs {
		if r.GeneratedCount != 3 || r.SourceFingerprint == "" {
			t.Fatalf("第 %d 条扩展列未写入: %+v", i+1, r)
		}
		if r.StartedAt != 100 || r.FinishedAt != 120 {
			t.Fatalf("第 %d 条时间戳被篡改: started=%d finished=%d", i+1, r.StartedAt, r.FinishedAt)
		}
		seen[r.Attempt] = true
	}
	for i := 1; i <= n; i++ {
		if !seen[i] {
			t.Fatalf("缺少 attempt=%d 的运行账", i)
		}
	}
}

// Q1：quarantined 是合法完成状态，写完必须离开 open 集合。
// 隔离不是失败重试的变体：它要求人工处置，不能再被队列自动认领。
func TestProposalBundleQuarantineLeavesOpenSet(t *testing.T) {
	s := newTestStore(t)
	b := mustBundle(t, s, "uid-q-", 1)
	if _, err := s.ClaimProposalBundle(b.ID, "w1", 1000, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishProposalBundle(b.ID, "w1", "quarantined", 0, "run-q"); err != nil {
		t.Fatalf("quarantined 应是合法完成状态: %v", err)
	}
	got, err := s.LoadProposalBundle(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "quarantined" || got.NotBefore != 0 || got.LeaseOwner != "" || got.LeaseUntil != 0 {
		t.Fatalf("隔离后应释放租约且不设 not_before: %+v", got)
	}
	// 同一 run 重复完成仍幂等。
	if err := s.FinishProposalBundle(b.ID, "w1", "quarantined", 0, "run-q"); err != nil {
		t.Fatalf("重复完成应幂等: %v", err)
	}
	if _, ok, err := s.NextOpenProposalBundle(); err != nil || ok {
		t.Fatalf("隔离窗口不得留在 open 集合: ok=%v err=%v", ok, err)
	}
	if _, err := s.ClaimProposalBundle(b.ID, "w2", 2000, time.Minute); err == nil {
		t.Fatal("隔离窗口不得再被认领")
	}
}

// Q2：回捞只重置调度字段，绝不动 cursor 与 Bundle 条目。
func TestProposalRequeueResetsSchedulingNotCursor(t *testing.T) {
	s := newTestStore(t)
	b := mustBundle(t, s, "uid-r-", 2)
	if _, err := s.ClaimProposalBundle(b.ID, "w1", 1000, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishProposalBundle(b.ID, "w1", "quarantined", 0, "run-q"); err != nil {
		t.Fatal(err)
	}
	var cursorBefore int
	if err := s.db.QueryRow(`SELECT cursor_seq FROM evidence_sources WHERE id = ?`, b.SourceID).Scan(&cursorBefore); err != nil {
		t.Fatal(err)
	}
	itemsBefore, err := s.proposalBundleEvidenceIDs(b.ID)
	if err != nil || len(itemsBefore) != 2 {
		t.Fatalf("隔离前应有 2 条条目: %+v err=%v", itemsBefore, err)
	}

	got, err := s.RequeueProposalBundle(b.ID, 1234)
	if err != nil {
		t.Fatalf("quarantined 应可回捞: %v", err)
	}
	if got.Status != "pending" || got.Attempt != 0 || got.NotBefore != 0 ||
		got.LeaseUntil != 0 || got.LeaseOwner != "" || got.LastRunID != "" {
		t.Fatalf("回捞应重置调度字段: %+v", got)
	}
	if got.UpdatedAt != 1234 {
		t.Fatalf("updated_at 应取传入 now，got %d", got.UpdatedAt)
	}
	var cursorAfter int
	if err := s.db.QueryRow(`SELECT cursor_seq FROM evidence_sources WHERE id = ?`, b.SourceID).Scan(&cursorAfter); err != nil {
		t.Fatal(err)
	}
	if cursorAfter != cursorBefore {
		t.Fatalf("回捞不得改 cursor: before=%d after=%d", cursorBefore, cursorAfter)
	}
	itemsAfter, err := s.proposalBundleEvidenceIDs(b.ID)
	if err != nil || len(itemsAfter) != 2 {
		t.Fatalf("回捞不得动 Bundle 条目: %+v err=%v", itemsAfter, err)
	}
	// attempt 归零后才不会认领即再次触发隔离。
	claimed, err := s.ClaimProposalBundle(b.ID, "w2", 1234, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Attempt != 1 {
		t.Fatalf("回捞后 attempt 应从 1 重新计数，got %d", claimed.Attempt)
	}

	// 状态不符时必须报出当前状态，便于排障。
	if _, err := s.RequeueProposalBundle(b.ID, 2000); err == nil || !strings.Contains(err.Error(), "leased") {
		t.Fatalf("非隔离项应报错并带当前状态: %v", err)
	}
	if _, err := s.RequeueProposalBundle("bnd-missing", 2000); !errors.Is(err, ErrEvidenceBundleNotFound) {
		t.Fatalf("未知 Bundle 应 NotFound: %v", err)
	}

	// ok 是已消费终态，不允许回捞。
	okBundle := mustBundle(t, s, "uid-s-", 1)
	if _, err := s.ClaimProposalBundle(okBundle.ID, "w1", 2000, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishProposalBundle(okBundle.ID, "w1", "ok", 0, "run-ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequeueProposalBundle(okBundle.ID, 3000); err == nil || !strings.Contains(err.Error(), "ok") {
		t.Fatalf("已消费窗口不得回捞: %v", err)
	}

	// fail 保留为排障口：确认修复后可手工放回队列。
	failBundle := mustBundle(t, s, "uid-t-", 1)
	if _, err := s.RequeueProposalBundle(failBundle.ID, 3000); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending 窗口不该回捞，报错需带当前状态: %v", err)
	}
	if _, err := s.ClaimProposalBundle(failBundle.ID, "w1", 3000, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishProposalBundle(failBundle.ID, "w1", "fail", 9999, "run-f"); err != nil {
		t.Fatal(err)
	}
	back, err := s.RequeueProposalBundle(failBundle.ID, 4000)
	if err != nil {
		t.Fatalf("fail 应可回捞: %v", err)
	}
	if back.Status != "pending" || back.Attempt != 0 || back.NotBefore != 0 {
		t.Fatalf("fail 回捞后应清空退避: %+v", back)
	}
}

// Q3：列表按状态过滤，created_at 升序、同秒按 id 升序。
func TestListProposalBundlesFiltersAndOrders(t *testing.T) {
	s := newTestStore(t)
	insertBacklogSource(t, s, "src-l", 1, 10, 0)
	insertBacklogBundle(t, s, "bnd-c", "src-l", 1, 2, "quarantined", 3000)
	insertBacklogBundle(t, s, "bnd-a", "src-l", 1, 0, "quarantined", 1000)
	insertBacklogBundle(t, s, "bnd-b", "src-l", 1, 1, "ok", 2000)
	insertBacklogBundle(t, s, "bnd-d", "src-l", 1, 3, "quarantined", 1000)

	list, err := s.ListProposalBundles("quarantined", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("应只返回 quarantined，got %+v", list)
	}
	want := []string{"bnd-a", "bnd-d", "bnd-c"}
	for i, id := range want {
		if list[i].ID != id {
			t.Fatalf("顺序应为 created_at ASC,id ASC：位置 %d 期望 %s，got %+v", i, id, list)
		}
	}
	all, err := s.ListProposalBundles("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != "bnd-a" || all[1].ID != "bnd-d" {
		t.Fatalf("空状态应返回全部且受 limit 约束: %+v", all)
	}
}

func TestInsertProposalCandidateAndMergeEvidence(t *testing.T) {
	s := newTestStore(t)
	ev := []CandidateEvidence{
		{EvidenceID: "ev-1", EventUID: "u1", SourceID: "src", SourceSeq: 1, EvidenceRole: "direct"},
		{EvidenceID: "ev-2", EventUID: "u2", SourceID: "src", SourceSeq: 2, EvidenceRole: "direct"},
	}
	in := ProposalCandidateInput{
		SessionID: "s", Project: "p", BundleID: "b", RunID: "r", ProposalIdx: 1,
		Kind: "decision", Scope: "project", Content: "决定：候选落库",
		Rationale: "因为", SubjectKey: "k", TaskState: "done",
		ObsIDs: []string{"ev-1", "ev-2"}, Evidence: ev,
	}
	res, err := s.InsertProposalCandidate(in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.ID == "" {
		t.Fatalf("首次应创建: %+v", res)
	}
	list, err := s.ListCandidates("pending", "s", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("应有一条 pending: %+v err=%v", list, err)
	}
	got := list[0]
	if got.Content != in.Content || got.Hash != HashOf(in.Content) {
		t.Fatalf("候选正文/hash 不对: %+v", got)
	}
	rows, err := s.ListCandidateEvidence(got.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("应有 2 条证据引用: %+v err=%v", rows, err)
	}

	// 同会话同内容：不新增行，只合并新证据。
	in.Evidence = []CandidateEvidence{
		{EvidenceID: "ev-2", EventUID: "u2", SourceID: "src", SourceSeq: 2},
		{EvidenceID: "ev-3", EventUID: "u3", SourceID: "src", SourceSeq: 3},
	}
	in.ObsIDs = []string{"ev-2", "ev-3"}
	res2, err := s.InsertProposalCandidate(in)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created || res2.ID != res.ID {
		t.Fatalf("重复候选不应新增行: %+v", res2)
	}
	if res2.MergedEvidence != 1 {
		t.Fatalf("应只合并 1 条新证据，got %d", res2.MergedEvidence)
	}
	rows, err = s.ListCandidateEvidence(res.ID)
	if err != nil || len(rows) != 3 {
		t.Fatalf("合并后应有 3 条证据: %+v err=%v", rows, err)
	}
	if n := migrationCount(t, s, "candidates"); n != 1 {
		t.Fatalf("不应新增候选行，got %d", n)
	}
	list, err = s.ListCandidates("pending", "s", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list[0].ObsIDs) != 3 {
		t.Fatalf("obs_ids 应合并为 3 条，got %+v", list[0].ObsIDs)
	}
	// 再合并一次同样的证据是幂等的。
	res3, err := s.InsertProposalCandidate(in)
	if err != nil || res3.MergedEvidence != 0 {
		t.Fatalf("重复合并应幂等: %+v err=%v", res3, err)
	}
}

func TestInsertProposalCandidateValidation(t *testing.T) {
	s := newTestStore(t)
	base := ProposalCandidateInput{SessionID: "s", Kind: "decision", Content: "x",
		Evidence: []CandidateEvidence{{EvidenceID: "ev-1"}}}
	cases := []struct {
		name string
		mod  func(*ProposalCandidateInput)
	}{
		{"空内容", func(in *ProposalCandidateInput) { in.Content = "  " }},
		{"坏 kind", func(in *ProposalCandidateInput) { in.Kind = "nope" }},
		{"缺 evidence_id", func(in *ProposalCandidateInput) { in.Evidence = []CandidateEvidence{{}} }},
	}
	for _, tc := range cases {
		in := base
		tc.mod(&in)
		if _, err := s.InsertProposalCandidate(in); err == nil {
			t.Errorf("%s: 应报错", tc.name)
		}
	}
	if n := migrationCount(t, s, "candidates"); n != 0 {
		t.Fatalf("校验失败不应留候选: %d", n)
	}
}

// S8：--force 只清「退避中且可重试」的 not_before，不动 attempt，
// 也不碰终态、已可运行与租约中的窗口。attempt 是「已试过几次」的事实，
// 清零会让超限隔离失效，并把退避中的头伪装成从未试过。
func TestForceRetryHeadsClearsBackoffOnly(t *testing.T) {
	s := newTestStore(t)
	insertBacklogSource(t, s, "src-a", 1, 10, 0)
	insertBacklogSource(t, s, "src-b", 1, 10, 0)
	insertBacklogSource(t, s, "src-c", 1, 10, 0)
	insertBacklogSource(t, s, "src-d", 1, 10, 0)
	insertFairBundle(t, s, "bnd-fail", "src-a", 0, "fail", 100, 900, 0, "")
	insertFairBundle(t, s, "bnd-backlog", "src-a", 1, "backlog", 200, 800, 0, "")
	insertFairBundle(t, s, "bnd-partial", "src-a", 2, "partial", 300, 700, 0, "")
	insertFairBundle(t, s, "bnd-ready", "src-b", 0, "fail", 100, 0, 0, "")
	insertFairBundle(t, s, "bnd-leased", "src-c", 0, "leased", 100, 0, 5000, "w1")
	insertFairBundle(t, s, "bnd-quar", "src-d", 0, "quarantined", 100, 600, 0, "")
	if _, err := s.db.Exec(`UPDATE evidence_bundles SET attempt=4 WHERE id='bnd-fail'`); err != nil {
		t.Fatalf("设置 attempt: %v", err)
	}

	n, err := s.ForceRetryHeads(100)
	if err != nil {
		t.Fatalf("ForceRetryHeads: %v", err)
	}
	if n != 3 {
		t.Fatalf("应清 3 个退避中的可重试窗口（fail/backlog/partial），got %d", n)
	}
	for id, want := range map[string]int64{
		"bnd-fail": 0, "bnd-backlog": 0, "bnd-partial": 0,
		"bnd-ready": 0, "bnd-leased": 0, "bnd-quar": 600,
	} {
		b, err := s.LoadProposalBundle(id)
		if err != nil {
			t.Fatalf("LoadProposalBundle(%s): %v", id, err)
		}
		if b.NotBefore != want {
			t.Fatalf("%s 的 not_before 应为 %d，got %d", id, want, b.NotBefore)
		}
	}
	b, err := s.LoadProposalBundle("bnd-fail")
	if err != nil {
		t.Fatalf("LoadProposalBundle: %v", err)
	}
	if b.Attempt != 4 {
		t.Fatalf("--force 不得改 attempt，got %d", b.Attempt)
	}
	if b.LeaseOwner != "" {
		t.Fatalf("--force 不得制造租约: %+v", b)
	}
	if again, err := s.ForceRetryHeads(100); err != nil || again != 0 {
		t.Fatalf("幂等：没有退避中的窗口时应清 0，got %d err=%v", again, err)
	}
}

// S8：dry-run 的读取面——未终结窗口按 created_at 最老优先，终态不出现，limit 生效。
func TestListOpenProposalBundlesOldestFirst(t *testing.T) {
	s := newTestStore(t)
	insertBacklogSource(t, s, "src-l", 1, 10, 0)
	insertBacklogBundle(t, s, "bnd-new", "src-l", 1, 2, "pending", 3000)
	insertBacklogBundle(t, s, "bnd-old", "src-l", 1, 0, "backlog", 1000)
	insertBacklogBundle(t, s, "bnd-mid", "src-l", 1, 1, "leased", 2000)
	insertBacklogBundle(t, s, "bnd-done", "src-l", 1, 3, "ok", 500)
	insertBacklogBundle(t, s, "bnd-quar", "src-l", 1, 4, "quarantined", 400)

	list, err := s.ListOpenProposalBundles(2)
	if err != nil {
		t.Fatalf("ListOpenProposalBundles: %v", err)
	}
	if len(list) != 2 || list[0].ID != "bnd-old" || list[1].ID != "bnd-mid" {
		t.Fatalf("应按 created_at 最老优先并受 limit 约束: %+v", list)
	}
	all, err := s.ListOpenProposalBundles(0)
	if err != nil {
		t.Fatalf("ListOpenProposalBundles(0): %v", err)
	}
	if len(all) != 3 || all[2].ID != "bnd-new" {
		t.Fatalf("终态（ok/quarantined）不得出现在预演列表: %+v", all)
	}
}
