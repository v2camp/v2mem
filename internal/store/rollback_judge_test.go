package store

// Wave 2 / E：Judge reject 按 model-tag 回滚的 store 层用例。
//
// 锁四件事：judge 驳回的候选能退回 pending 并留 reopen；人工之后处理过的跳过；
// dry-run 零写入；重跑幂等。原有 created_memory=1 的记忆回滚在
// review_s10b_test.go / rollback_test.go 已有回归保护。

import (
	"testing"

	"github.com/v2camp/v2mem/internal/commit"
)

// seedJudgeReject 造一条「judge 驳回」的候选：status=rejected + 一条
// action=reject/actor=judge/model_tag 的裁决账（P4-D 的落库形态）。
func seedJudgeReject(t *testing.T, s *Store, content, modelTag string, decidedAt int64) string {
	t.Helper()
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "fact", Content: content, Source: "distill",
	})
	if err != nil {
		t.Fatalf("InsertCandidate: %v", err)
	}
	id := res.Candidate.ID
	if _, err := s.db.Exec(`UPDATE candidates
		SET status = 'rejected', status_reason = 'judge_reject', decided_at = ? WHERE id = ?`,
		decidedAt, id); err != nil {
		t.Fatalf("mark rejected: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO commit_decisions
		(id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
		 memory_id, model_tag, created_memory, idempotency_key, created_at)
		VALUES (?,?,0,'','reject','judge','["judge_reject"]','',?,0,?,?)`,
		"dec-"+id, id, modelTag, "key-"+id, decidedAt); err != nil {
		t.Fatalf("insert judge reject: %v", err)
	}
	return id
}

// seedHumanAfter 在候选上追加一条 actor=human 的裁决，模拟人工已接手。
func seedHumanAfter(t *testing.T, s *Store, candID, action string, at int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO commit_decisions
		(id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
		 memory_id, model_tag, created_memory, idempotency_key, created_at)
		VALUES (?,?,0,'',?,?,'["human"]','','',0,?,?)`,
		"dec-human-"+candID, candID, action, commit.ActorHuman, "key-human-"+candID, at); err != nil {
		t.Fatalf("insert human decision: %v", err)
	}
}

func candidateStatus(t *testing.T, s *Store, id string) string {
	t.Helper()
	var status string
	if err := s.db.QueryRow(`SELECT status FROM candidates WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("读候选状态: %v", err)
	}
	return status
}

func reopenCount(t *testing.T, s *Store, candID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM commit_decisions
		WHERE candidate_id = ? AND action = ? AND actor = ?`,
		candID, string(commit.ActionReopen), commit.ActorHuman).Scan(&n); err != nil {
		t.Fatalf("count reopen: %v", err)
	}
	return n
}

func TestRollbackModelTagReopensJudgeReject(t *testing.T) {
	s := newTestStore(t)
	rejected := seedJudgeReject(t, s, "巡检已完成，报告已输出", "judge@prompt-v4", 1700000100)
	otherTag := seedJudgeReject(t, s, "别的版本驳回的候选", "judge@prompt-v3", 1700000100)

	// dry-run：报告计划动作，零写入。
	dry, err := s.RollbackModelTag("judge@prompt-v4", true, 1700001000)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if dry.JudgeReopened != 1 || dry.JudgeSkipped != 0 {
		t.Fatalf("dry-run 计数错: %+v", dry)
	}
	if got := candidateStatus(t, s, rejected); got != "rejected" {
		t.Fatalf("dry-run 不得改状态，got %q", got)
	}
	if got := reopenCount(t, s, rejected); got != 0 {
		t.Fatalf("dry-run 不得写 reopen，got %d", got)
	}

	// 真实回滚：候选回 pending + 一条 human reopen 决策。
	rep, err := s.RollbackModelTag("judge@prompt-v4", false, 1700001000)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rep.JudgeReopened != 1 || rep.JudgeSkipped != 0 {
		t.Fatalf("回滚计数错: %+v", rep)
	}
	if len(rep.JudgeReopenedIDs) != 1 || rep.JudgeReopenedIDs[0] != rejected {
		t.Fatalf("回滚明细错: %v", rep.JudgeReopenedIDs)
	}
	if got := candidateStatus(t, s, rejected); got != "pending" {
		t.Fatalf("judge 驳回候选应回 pending，got %q", got)
	}
	if got := reopenCount(t, s, rejected); got != 1 {
		t.Fatalf("回滚应记一条 reopen，got %d", got)
	}

	// 别的版本不受影响。
	if got := candidateStatus(t, s, otherTag); got != "rejected" {
		t.Fatalf("别的 model_tag 不得被回滚，got %q", got)
	}

	// 重跑幂等：已回 pending 的候选不再重复计数。
	again, err := s.RollbackModelTag("judge@prompt-v4", false, 1700002000)
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if again.JudgeReopened != 0 || again.JudgeSkipped != 0 {
		t.Fatalf("重跑不得重复动作: %+v", again)
	}
	if got := reopenCount(t, s, rejected); got != 1 {
		t.Fatalf("重跑不得再写 reopen，got %d", got)
	}
}

func TestRollbackModelTagJudgeRejectSkipsHumanHandled(t *testing.T) {
	s := newTestStore(t)
	touched := seedJudgeReject(t, s, "judge 驳回了但人工后来又看过", "judge@prompt-v4", 1700000100)
	seedHumanAfter(t, s, touched, string(commit.ActionReject), 1700000200)
	clean := seedJudgeReject(t, s, "judge 驳回后无人接手", "judge@prompt-v4", 1700000100)

	rep, err := s.RollbackModelTag("judge@prompt-v4", false, 1700001000)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rep.JudgeReopened != 1 || rep.JudgeSkipped != 1 {
		t.Fatalf("应 1 回滚 1 跳过，got %+v", rep)
	}
	if len(rep.JudgeSkippedIDs) != 1 || rep.JudgeSkippedIDs[0] != touched {
		t.Fatalf("跳过明细错: %v", rep.JudgeSkippedIDs)
	}
	if got := candidateStatus(t, s, touched); got != "rejected" {
		t.Fatalf("人工接手过的候选不得被回滚，got %q", got)
	}
	if got := candidateStatus(t, s, clean); got != "pending" {
		t.Fatalf("无人接手的候选应回 pending，got %q", got)
	}
}

// 原有 created_memory=1 的记忆回滚与新增 judge 回滚必须在同一次调用里并存，
// 且旧计数口径不变。
func TestRollbackModelTagKeepsMemoryRollbackAlongsideJudge(t *testing.T) {
	s := newTestStore(t)
	memID, _ := seedRollbackTarget(t, s, "按版本撤回的自动记忆", "judge@prompt-v4", "")
	candID := seedJudgeReject(t, s, "同版本被 judge 驳回的候选", "judge@prompt-v4", 1700000100)

	rep, err := s.RollbackModelTag("judge@prompt-v4", false, 1700001000)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rep.Withdrawn != 1 || rep.WithdrawnIDs[0] != memID {
		t.Fatalf("记忆回滚口径不得回归: %+v", rep)
	}
	if rep.JudgeReopened != 1 || rep.JudgeReopenedIDs[0] != candID {
		t.Fatalf("judge 回滚应同时生效: %+v", rep)
	}
}
