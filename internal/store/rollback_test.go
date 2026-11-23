package store

import (
	"errors"
	"testing"

	"github.com/v2camp/v2mem/internal/commit"
)

// 回滚是批量落库能成立的前提：没有它，mem batch 就是不可逆写库。
// 这组用例锁三件事：记忆真的被删、候选真的退回 pending、裁决历史留下 reopen。

func rollbackCandidate(t *testing.T, s *Store, content, kind string) string {
	t.Helper()
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: kind, Content: content, Source: "distill",
	})
	if err != nil {
		t.Fatalf("InsertCandidate: %v", err)
	}
	return res.Candidate.ID
}

func countMemories(t *testing.T, s *Store, id string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("数记忆: %v", err)
	}
	return n
}

func candidateRevision(t *testing.T, s *Store, id string) int {
	t.Helper()
	var rev int
	if err := s.db.QueryRow(`SELECT revision FROM candidates WHERE id = ?`, id).Scan(&rev); err != nil {
		t.Fatalf("读 revision: %v", err)
	}
	return rev
}

func TestRollbackCandidateDeletesCreatedMemory(t *testing.T) {
	s := newTestStore(t)
	id := rollbackCandidate(t, s, "批量落库必须带得走的回滚凭证", "fact")

	acc, err := s.CommitAccept(id, -1, commit.ActorHumanReviewedLLM)
	if err != nil {
		t.Fatalf("CommitAccept: %v", err)
	}
	if acc.MemoryID == "" {
		t.Fatal("采纳应产出 memory_id")
	}
	if n := countMemories(t, s, acc.MemoryID); n != 1 {
		t.Fatalf("采纳后应有一条记忆，got %d", n)
	}

	rb, err := s.RollbackCandidate(id, commit.ActorHumanReviewedLLM, true, candidateRevision(t, s, id))
	if err != nil {
		t.Fatalf("RollbackCandidate: %v", err)
	}
	if rb.Candidate.Status != "pending" {
		t.Errorf("回滚后应退回 pending，got %q", rb.Candidate.Status)
	}
	if rb.Candidate.MemoryID != "" {
		t.Errorf("回滚后 memory_id 应清空，got %q", rb.Candidate.MemoryID)
	}
	if n := countMemories(t, s, acc.MemoryID); n != 0 {
		t.Errorf("created_memory 的回滚必须删掉记忆，got %d 行", n)
	}

	dec, err := s.CommitDecisions(id)
	if err != nil {
		t.Fatalf("CommitDecisions: %v", err)
	}
	last := dec[len(dec)-1]
	if last.Action != commit.ActionReopen || last.Actor != commit.ActorHumanReviewedLLM {
		t.Errorf("应留下 reopen 裁决，got action=%q actor=%q", last.Action, last.Actor)
	}
}

// 命中「相同知识覆盖」的重放不能删别人的记忆：deleteMemory 只能由 created_memory 决定。
func TestRollbackCandidateKeepsMemoryWhenNotCreated(t *testing.T) {
	s := newTestStore(t)
	id := rollbackCandidate(t, s, "重放命中的既有记忆不属于本次批量", "fact")

	acc, err := s.CommitAccept(id, -1, commit.ActorHumanReviewedLLM)
	if err != nil {
		t.Fatalf("CommitAccept: %v", err)
	}
	if _, err := s.RollbackCandidate(id, commit.ActorHumanReviewedLLM, false, candidateRevision(t, s, id)); err != nil {
		t.Fatalf("RollbackCandidate: %v", err)
	}
	if n := countMemories(t, s, acc.MemoryID); n != 1 {
		t.Errorf("deleteMemory=false 时不得删记忆，got %d 行", n)
	}
}

func TestRollbackCandidateReopensRejected(t *testing.T) {
	s := newTestStore(t)
	id := rollbackCandidate(t, s, "巡检已完成，报告已输出", "pitfall")

	if _, err := s.CommitReject(id, -1, commit.ActorHumanReviewedLLM, "process_narrative"); err != nil {
		t.Fatalf("CommitReject: %v", err)
	}
	rb, err := s.RollbackCandidate(id, commit.ActorHumanReviewedLLM, true, candidateRevision(t, s, id))
	if err != nil {
		t.Fatalf("RollbackCandidate: %v", err)
	}
	if rb.Candidate.Status != "pending" {
		t.Errorf("被否决的候选回滚后应回到 pending，got %q", rb.Candidate.Status)
	}
}

// 归档之后又被裁决过（revision 变了）就不该再按旧 plan 撤，
// 否则回滚一份旧归档会抹掉人后来的决定。
func TestRollbackCandidateRejectsStaleRevision(t *testing.T) {
	s := newTestStore(t)
	id := rollbackCandidate(t, s, "归档之后又被裁决过的候选", "fact")

	if _, err := s.CommitAccept(id, -1, commit.ActorHumanReviewedLLM); err != nil {
		t.Fatalf("CommitAccept: %v", err)
	}
	if _, err := s.RollbackCandidate(id, commit.ActorHumanReviewedLLM, true, -1); err != nil {
		t.Fatalf("先回滚一次: %v", err)
	}
	stale := candidateRevision(t, s, id)
	if _, err := s.CommitAccept(id, -1, commit.ActorHumanReviewedLLM); err != nil {
		t.Fatalf("再采纳: %v", err)
	}
	if _, err := s.RollbackCandidate(id, commit.ActorHumanReviewedLLM, true, stale); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("旧归档应撞 revision 冲突，got %v", err)
	}
}

func TestRollbackCandidateRejectsPending(t *testing.T) {
	s := newTestStore(t)
	id := rollbackCandidate(t, s, "还没裁决过的候选没有可撤的动作", "fact")
	if _, err := s.RollbackCandidate(id, commit.ActorHumanReviewedLLM, true, -1); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("pending 候选回滚应报 ErrCandidateClosed，got %v", err)
	}
}

// 一致率的分母只认 actor=human：人批准 AI 方案的批量落库是模型的回声，
// 算进人工样本会让「≥95% 才转正」立刻达标。
func TestJudgeStatsDoesNotCountReviewedBatchAsHuman(t *testing.T) {
	s := newTestStore(t)
	human := rollbackCandidate(t, s, "人自己看出来的事实", "fact")
	batch := rollbackCandidate(t, s, "人批准 AI 方案后落库的事实", "fact")

	if err := s.MarkJudgeVerdict(human, "accept", "m@judge-v2", "durable"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	if err := s.MarkJudgeVerdict(batch, "accept", "m@judge-v2", "durable"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	if _, err := s.CommitAccept(human, -1, commit.ActorHuman); err != nil {
		t.Fatalf("CommitAccept human: %v", err)
	}
	if _, err := s.CommitAccept(batch, -1, commit.ActorHumanReviewedLLM); err != nil {
		t.Fatalf("CommitAccept batch: %v", err)
	}

	stats, err := s.JudgeStats()
	if err != nil {
		t.Fatalf("JudgeStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("应只有 accept 一组，got %+v", stats)
	}
	got := stats[0]
	if got.Accepted != 1 {
		t.Errorf("人工样本只应算 actor=human 的一条，got %+v", got)
	}
	if got.SelfRun != 1 {
		t.Errorf("batch 样本应归入 self_run，got %+v", got)
	}
}
