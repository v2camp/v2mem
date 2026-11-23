package store

// 准入判定的存储侧只有三件事可验证：痕迹可写、pending 可数、按 verdict 可聚合。
// 它们的分母口径直接决定「影子期人工一致率」是否可信，所以断言必须落在库上。

import "testing"

// MarkJudgeVerdict 只写痕迹，不动 status：影子期零风险的全部依据在此。
// 覆盖「重复判定覆盖上一次」也在这里——同一候选换模型重判时，verdict 与 model 必须同源。
func TestMarkJudgeVerdictKeepsStatus(t *testing.T) {
	s := newTestStore(t)
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "fact",
		Content: "v2mem 的 MCP 通道与 hook 注入相互独立", Source: "distill",
	})
	if err != nil {
		t.Fatalf("InsertCandidate: %v", err)
	}
	id := res.Candidate.ID

	if err := s.MarkJudgeVerdict(id, "reject", "ollama@judge-v2", "process_narrative"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	var verdict, model, reason, status string
	if err := s.db.QueryRow(
		`SELECT judge_verdict, judge_model, rationale, status FROM candidates WHERE id = ?`, id,
	).Scan(&verdict, &model, &reason, &status); err != nil {
		t.Fatalf("读回判定痕迹: %v", err)
	}
	if verdict != "reject" || model != "ollama@judge-v2" || reason != "process_narrative" {
		t.Errorf("判定痕迹应原样落库，got verdict=%q model=%q reason=%q", verdict, model, reason)
	}
	if status != "pending" {
		t.Fatalf("MarkJudgeVerdict 不得改状态，got %q", status)
	}

	// 重判：verdict 与 model 一起覆盖，否则「谁判的」与「判成什么」分属两个模型。
	if err := s.MarkJudgeVerdict(id, "unsure", "deepseek:flash@judge-v2", "unsure"); err != nil {
		t.Fatalf("MarkJudgeVerdict 重判: %v", err)
	}
	if err := s.db.QueryRow(
		`SELECT judge_verdict, judge_model FROM candidates WHERE id = ?`, id,
	).Scan(&verdict, &model); err != nil {
		t.Fatalf("重判后读回: %v", err)
	}
	if verdict != "unsure" || model != "deepseek:flash@judge-v2" {
		t.Errorf("重判应同时覆盖 verdict/model，got verdict=%q model=%q", verdict, model)
	}
}

// CountPending 用全库口径：触发条件必须与 judge 的实际处理范围一致，
// 否则「某工程 30 条却永不触发」取决于别的工程有没有先到阈值。
func TestCountPendingIgnoresDecided(t *testing.T) {
	s := newTestStore(t)
	insert := func(kind, content string) string {
		t.Helper()
		res, err := s.InsertCandidate(CandidateInput{
			SessionID: "s", Project: "p", Kind: kind, Content: content, Source: "distill",
		})
		if err != nil {
			t.Fatalf("InsertCandidate: %v", err)
		}
		return res.Candidate.ID
	}
	insert("fact", "MCP 与 hook 注入相互独立")
	rejected := insert("fact", "巡检已完成，报告已输出")
	if _, err := s.CommitReject(rejected, -1, "human", "过程叙述"); err != nil {
		t.Fatalf("CommitReject: %v", err)
	}
	n, err := s.CountPending()
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 1 {
		t.Fatalf("只应数 pending，got %d", n)
	}
}

// JudgeStats 的分母只含人工样本：判定器自己 --apply 落下的不算一致率，
// 否则「≥95% 才转正」会被自己的产出喂饱，等于没有门槛。
func TestJudgeStatsSeparatesSelfRunFromHuman(t *testing.T) {
	s := newTestStore(t)
	insert := func(content string) string {
		t.Helper()
		res, err := s.InsertCandidate(CandidateInput{
			SessionID: "s", Project: "p", Kind: "fact", Content: content, Source: "distill",
		})
		if err != nil {
			t.Fatalf("InsertCandidate: %v", err)
		}
		return res.Candidate.ID
	}
	human := insert("MCP 与 hook 注入相互独立")
	selfRun := insert("巡检已完成，报告已输出")
	undecided := insert("候选表量级在千条以内，不为它加索引")

	for _, c := range []struct{ id, verdict, reason string }{
		{human, "accept", "durable"},
		{selfRun, "reject", "process_narrative"},
		{undecided, "unsure", "unsure"},
	} {
		if err := s.MarkJudgeVerdict(c.id, c.verdict, "ollama@judge-v2", c.reason); err != nil {
			t.Fatalf("MarkJudgeVerdict: %v", err)
		}
	}
	// 人工采纳一条；另一条由判定器自己 --apply 否决。
	if _, err := s.CommitAccept(human, -1, "human"); err != nil {
		t.Fatalf("CommitAccept: %v", err)
	}
	if _, err := s.CommitReject(selfRun, -1, "judge", "judge:process_narrative"); err != nil {
		t.Fatalf("CommitReject: %v", err)
	}

	stats, err := s.JudgeStats()
	if err != nil {
		t.Fatalf("JudgeStats: %v", err)
	}
	byVerdict := map[string]JudgeStat{}
	for _, st := range stats {
		byVerdict[st.Verdict] = st
	}
	acc, ok := byVerdict["accept"]
	if !ok {
		t.Fatalf("缺 accept 分组，got %+v", stats)
	}
	if acc.Accepted != 1 || acc.SelfRun != 0 || acc.Pending != 0 {
		t.Errorf("人工采纳应记 accepted=1 self_run=0，got %+v", acc)
	}
	rej, ok := byVerdict["reject"]
	if !ok {
		t.Fatalf("缺 reject 分组，got %+v", stats)
	}
	if rej.Accepted != 0 || rej.Rejected != 0 || rej.SelfRun != 1 {
		t.Errorf("判定器自裁不得进人工分母，got %+v", rej)
	}
	uns, ok := byVerdict["unsure"]
	if !ok {
		t.Fatalf("缺 unsure 分组，got %+v", stats)
	}
	if uns.Pending != 1 || uns.Total != 1 {
		t.Errorf("未裁决的应落在 pending，got %+v", uns)
	}
}
