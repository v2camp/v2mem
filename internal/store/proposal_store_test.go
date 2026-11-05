package store

import (
	"errors"
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

func TestProposalRunSaveAndList(t *testing.T) {
	s := newTestStore(t)
	b := mustBundle(t, s, "uid-b-", 1)
	rec := ProposalRunRecord{
		SessionID: "sess-1", Provider: "local", Status: "ok", CandCount: 2,
		StartedAt: 100, FinishedAt: 120, BundleID: b.ID, Executor: "local",
		SchemaVersion: 1, GeneratedCount: 3, ValidCount: 2, InvalidCount: 1,
		DuplicateCount: 1, Attempt: 2, SourceFingerprint: b.EvidenceHash,
	}
	if err := s.SaveProposalRun(rec); err != nil {
		t.Fatal(err)
	}
	// 同 session + 同一秒：唯一约束冲突要自增 started_at 而不是丢账。
	rec2 := rec
	rec2.Status = "partial"
	if err := s.SaveProposalRun(rec2); err != nil {
		t.Fatalf("同秒运行账应重试: %v", err)
	}
	runs, err := s.ProposalRuns(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("应有 2 条运行账，got %d", len(runs))
	}
	if runs[0].GeneratedCount != 3 || runs[0].Attempt != 2 || runs[0].SourceFingerprint == "" {
		t.Fatalf("扩展列未写入: %+v", runs[0])
	}
	if runs[1].StartedAt == runs[0].StartedAt {
		t.Fatalf("冲突时应自增 started_at: %d vs %d", runs[0].StartedAt, runs[1].StartedAt)
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
