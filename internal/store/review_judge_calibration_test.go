package store

// P4 Wave 1 / C：按 candidates.judge_model 分组的 Judge 标定统计。
//
// 与 ReviewStats（按 commit_decisions.model_tag 统计记忆后审核）并列：
// 这一组回答「判定器自己判了多少、否了多少、落库多少、被重开多少」，
// 分组键取 judge_model（带 prompt 版本），阈值仍只观察。

import (
	"fmt"
	"testing"
)

// seedJudgeCandidate 造一条已判定的候选，可带若干条裁决账。
func seedJudgeCandidate(t *testing.T, s *Store, n int, judgeModel, verdict string, decisions ...struct{ action, actor string }) string {
	t.Helper()
	id := fmt.Sprintf("jcand-%03d", n)
	if _, err := s.db.Exec(`INSERT INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at,
		 judge_verdict, judge_model)
		VALUES (?, 's', 'p', 'fact', ?, ?, 'pending', 'distill', 1700000000, ?, ?)`,
		id, "judge 标定样本 "+id, "h-"+id, verdict, judgeModel); err != nil {
		t.Fatalf("insert candidate %s: %v", id, err)
	}
	for i, d := range decisions {
		if _, err := s.db.Exec(`INSERT INTO commit_decisions
			(id, candidate_id, action, actor, idempotency_key, created_at)
			VALUES (?,?,?,?,?,1700000000)`,
			fmt.Sprintf("jdec-%s-%d", id, i), id, d.action, d.actor, fmt.Sprintf("jkey-%s-%d", id, i)); err != nil {
			t.Fatalf("insert decision %s: %v", id, err)
		}
	}
	return id
}

func TestJudgeCalibrationStatsByJudgeModel(t *testing.T) {
	s := newTestStore(t)
	v4 := "ollama:qwen2.5@judge-v4"
	v3 := "ollama:llama3@judge-v3"
	type dec = struct{ action, actor string }

	// v4：3 条判定（2 accept/1 reject），其中 1 条 judge 采纳落库、1 条人工采纳不算 applied。
	seedJudgeCandidate(t, s, 1, v4, "accept", dec{"auto_commit", "judge"})
	seedJudgeCandidate(t, s, 2, v4, "accept", dec{"human_accept", "human"})
	// v4：reject 后被 reopen，计 1 次重开；judge 的 reject 裁决也算 applied。
	seedJudgeCandidate(t, s, 3, v4, "reject", dec{"reject", "judge"}, dec{"reopen", "human"})
	// v3：另一套提示词，独立成组。
	seedJudgeCandidate(t, s, 4, v3, "accept")
	// 未被判定过的候选（judge_model 为空）不进任何组。
	if _, err := s.db.Exec(`INSERT INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at)
		VALUES ('jcand-empty','s','p','fact','未判定样本','h-empty','pending','distill',1700000000)`); err != nil {
		t.Fatalf("insert empty candidate: %v", err)
	}

	rows, err := s.JudgeCalibrationStats()
	if err != nil {
		t.Fatalf("JudgeCalibrationStats: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应按 judge_model 分 2 组，got %+v", rows)
	}
	// 排序稳定：judge_model 字典序。
	if rows[0].JudgeModel != v3 || rows[1].JudgeModel != v4 {
		t.Fatalf("应按 judge_model 升序，got %+v", rows)
	}
	if r := rows[0]; r.Verdicts != 1 || r.Reject != 0 || r.Applied != 0 || r.Reopened != 0 {
		t.Fatalf("v3 统计错: %+v", r)
	}
	v := rows[1]
	if v.Verdicts != 3 || v.Reject != 1 || v.Applied != 2 || v.Reopened != 1 {
		t.Fatalf("v4 统计错: %+v", v)
	}
}

func TestJudgeCalibrationStatsEmpty(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.JudgeCalibrationStats()
	if err != nil {
		t.Fatalf("JudgeCalibrationStats: %v", err)
	}
	if rows == nil {
		t.Fatal("空样本应返回空切片而不是 nil（JSON 应为 []）")
	}
	if len(rows) != 0 {
		t.Fatalf("空样本不应有行，got %+v", rows)
	}
}

func TestJudgeCalibrationStatsQueryError(t *testing.T) {
	s := newTestStore(t)
	s.Close()
	if _, err := s.JudgeCalibrationStats(); err == nil {
		t.Fatal("库已关闭时应返回错误，不得静默当成空样本")
	}
}
