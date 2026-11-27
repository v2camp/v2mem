package store

// P4 Wave 2 / D：Judge 裁决落库。
//
// accept 必须过提交矩阵（AllowModelAuto），只有 auto_commit 才写记忆；Defer/Conflict
// 只记裁决。reject 的理由码按 actor 分流，回滚与标定都依赖它。

import (
	"testing"

	"github.com/v2camp/v2mem/internal/commit"
)

func TestJudgeAutoCommitMatrixAndAttribution(t *testing.T) {
	s := newTestStore(t)
	ev := captureEvidence(t, s, "uid-judge", "s", "p", "A 级直接证据日志")
	c := newCandidate(t, s, "fact", "p", "judge accept 必须过矩阵的候选")
	attachCandidateEvidence(t, s, c.Candidate.ID, ev.ID, ev.EventUID, ev.ArchiveHash, "direct")

	res, err := s.JudgeAutoCommit(c.Candidate.ID, -1, "deepseek:flash@judge-v4")
	if err != nil {
		t.Fatalf("JudgeAutoCommit: %v", err)
	}
	if res.Action != commit.ActionAutoCommit || res.Actor != commit.ActorJudge || !res.CreatedMemory {
		t.Fatalf("judge 自动采纳结果错: %+v", res)
	}
	var actor, tag string
	var created int
	if err := s.db.QueryRow(`SELECT actor, model_tag, created_memory FROM commit_decisions
		WHERE candidate_id = ?`, c.Candidate.ID).Scan(&actor, &tag, &created); err != nil {
		t.Fatal(err)
	}
	if actor != commit.ActorJudge || tag != "deepseek:flash@judge-v4" || created != 1 {
		t.Fatalf("归因错: actor=%q tag=%q created=%d", actor, tag, created)
	}
	var state string
	if err := s.db.QueryRow(`SELECT review_state FROM memories WHERE id = ?`, res.MemoryID).
		Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		t.Fatalf("judge 产物应留未审核态，got %q", state)
	}

	// 重放幂等：同 revision 的重放不新增记忆、不新增裁决。
	res2, err := s.JudgeAutoCommit(c.Candidate.ID, -1, "deepseek:flash@judge-v4")
	if err != nil {
		t.Fatalf("重放: %v", err)
	}
	if res2.CreatedMemory {
		t.Fatal("重放不得重复新建记忆")
	}
	mid := len(mustDecisions(t, s, c.Candidate.ID))
	if _, err := s.JudgeAutoCommit(c.Candidate.ID, -1, "deepseek:flash@judge-v4"); err != nil {
		t.Fatalf("再次重放: %v", err)
	}
	if after := len(mustDecisions(t, s, c.Candidate.ID)); after != mid {
		t.Fatalf("同 revision 重放不得新增裁决: %d -> %d", mid, after)
	}

	// Defer 优先于开关：无证据的 fact 不写记忆，但仍留一条裁决。
	deferC := newCandidate(t, s, "fact", "p", "没有证据的 fact 不得因 judge 落库")
	res3, err := s.JudgeAutoCommit(deferC.Candidate.ID, -1, "deepseek:flash@judge-v4")
	if err != nil {
		t.Fatalf("Defer 路径: %v", err)
	}
	if res3.CreatedMemory || res3.Action != commit.ActionDefer {
		t.Fatalf("Defer 不得写库: %+v", res3)
	}
	if got := len(mustDecisions(t, s, deferC.Candidate.ID)); got != 1 {
		t.Fatalf("Defer 也应留一条裁决，got %d", got)
	}

	// Conflict 优先于一切：review_class=conflict 不写记忆。
	confC := newCandidate(t, s, "fact", "p", "冲突候选不得因 judge 落库")
	if _, err := s.db.Exec(`UPDATE candidates SET review_class = 'conflict' WHERE id = ?`, confC.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	res4, err := s.JudgeAutoCommit(confC.Candidate.ID, -1, "deepseek:flash@judge-v4")
	if err != nil {
		t.Fatalf("Conflict 路径: %v", err)
	}
	if res4.CreatedMemory || res4.Action != commit.ActionConflict {
		t.Fatalf("Conflict 不得写库: %+v", res4)
	}
}

// 理由码按 actor 分流：judge/model-auto/其余各一档，原有 reason 追加在后。
func TestCommitRejectReasonCodeByActor(t *testing.T) {
	cases := []struct {
		actor, reason, wantCode string
	}{
		{commit.ActorJudge, "judge:process_narrative", "judge_reject"},
		{commit.ActorModelAuto, "model:process_narrative", "model_reject"},
		{commit.ActorHuman, "process_narrative", "human_reject"},
		{"", "", "human_reject"},
	}
	for _, tc := range cases {
		s := newTestStore(t)
		c := newCandidate(t, s, "fact", "p", "理由码按 actor 分流的候选 "+tc.actor)
		if _, err := s.CommitReject(c.Candidate.ID, -1, tc.actor, tc.reason); err != nil {
			t.Fatalf("CommitReject(%q): %v", tc.actor, err)
		}
		ds, err := s.CommitDecisions(c.Candidate.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) != 1 {
			t.Fatalf("应有 1 条裁决，got %d", len(ds))
		}
		codes := ds[0].ReasonCodes
		if len(codes) == 0 || codes[0] != tc.wantCode {
			t.Fatalf("actor=%q 理由码应为 %q，got %+v", tc.actor, tc.wantCode, codes)
		}
		if tc.reason != "" && (len(codes) < 2 || codes[1] != tc.reason) {
			t.Fatalf("原有 reason 应追加在后，got %+v", codes)
		}
	}
}

// Candidate 投影必须带 review_class：judge 的冲突守卫靠它，不能回表再查一次。
func TestCandidateProjectionIncludesReviewClass(t *testing.T) {
	s := newTestStore(t)
	res, err := s.InsertProposalCandidate(ProposalCandidateInput{
		SessionID: "s", Project: "p", Kind: "fact", Content: "冲突类候选的投影",
		Scope: "project", ReviewClass: "conflict",
	})
	if err != nil {
		t.Fatalf("InsertProposalCandidate: %v", err)
	}
	c, err := s.ResolveCandidate(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReviewClass != "conflict" {
		t.Fatalf("ResolveCandidate 应投影 review_class，got %q", c.ReviewClass)
	}
	list, err := s.ListUnjudgedCandidates("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ReviewClass != "conflict" {
		t.Fatalf("列表投影应带 review_class，got %+v", list)
	}
}
