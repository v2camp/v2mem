package store

// 准入判定的存储侧只有三件事可验证：痕迹可写、pending 可数、按 verdict 可聚合。
// 它们的分母口径直接决定「影子期人工一致率」是否可信，所以断言必须落在库上。

import (
	"errors"
	"testing"
)

// P4 前置：revision CAS。并发自动裁决推进 revision 后，Judge 不得覆盖。
func TestMarkJudgeVerdictCASRejectsStaleRevision(t *testing.T) {
	s := newTestStore(t)
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "fact", Content: "CAS 样本", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Candidate.ID
	if err := s.MarkJudgeVerdictCAS(id, "reject", "m@v1", "process_narrative", 1); !errors.Is(err, ErrJudgeVerdictStale) {
		t.Fatalf("过期 revision 应报 ErrJudgeVerdictStale, got %v", err)
	}
	var verdict string
	if err := s.db.QueryRow(`SELECT judge_verdict FROM candidates WHERE id = ?`, id).Scan(&verdict); err != nil {
		t.Fatal(err)
	}
	if verdict != "" {
		t.Fatalf("CAS 失败不得写入判定，got %q", verdict)
	}
	if err := s.MarkJudgeVerdictCAS(id, "reject", "m@v1", "process_narrative", 0); err != nil {
		t.Fatalf("正确 revision 应成功: %v", err)
	}
}

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
	// 提案理由先落一条：判定理由必须写在独立列，不能覆盖它（design §7 第 5 条）。
	if _, err := s.db.Exec(`UPDATE candidates SET rationale = '提案自己的理由' WHERE id = ?`, id); err != nil {
		t.Fatalf("seed rationale: %v", err)
	}

	if err := s.MarkJudgeVerdict(id, "reject", "ollama@judge-v2", "process_narrative"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	var verdict, model, reason, status, rationale string
	if err := s.db.QueryRow(
		`SELECT judge_verdict, judge_model, judge_reason, status, rationale FROM candidates WHERE id = ?`, id,
	).Scan(&verdict, &model, &reason, &status, &rationale); err != nil {
		t.Fatalf("读回判定痕迹: %v", err)
	}
	if verdict != "reject" || model != "ollama@judge-v2" || reason != "process_narrative" {
		t.Errorf("判定痕迹应原样落库，got verdict=%q model=%q reason=%q", verdict, model, reason)
	}
	if rationale != "提案自己的理由" {
		t.Errorf("判定理由不得覆盖提案 rationale，got %q", rationale)
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
	// 已判定但仍是 pending 的行不能再计入积压：影子期不改 status，
	// 若把它算进去，自动触发会永远 ≥ 阈值、每 24h 空跑一次。
	judged := insert("fact", "判定器看过的候选仍在 pending")
	if err := s.MarkJudgeVerdict(judged, "reject", "ollama@judge-v2", "process_narrative"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	n, err := s.CountPending()
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 1 {
		t.Fatalf("只应数未判定的 pending，got %d", n)
	}
}

// 影子期不改 status：判定过的行必须从下一批里消失，否则每轮重判同一批、后面的永远轮不到。
func TestListUnjudgedCandidatesSkipsJudged(t *testing.T) {
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
	first := insert("第一条未判定候选")
	middle := insert("第二条已判定候选")
	last := insert("第三条未判定候选")
	if err := s.MarkJudgeVerdict(middle, "reject", "ollama@judge-v2", "process_narrative"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}

	got, err := s.ListUnjudgedCandidates("", 10)
	if err != nil {
		t.Fatalf("ListUnjudgedCandidates: %v", err)
	}
	// 同一秒插入时 created_at 相同、按 id 兜底排序，所以只比集合不比顺序。
	ids := map[string]bool{}
	for _, c := range got {
		ids[c.ID] = true
	}
	if len(got) != 2 || !ids[first] || !ids[last] || ids[middle] {
		t.Fatalf("应跳过已判定的中间行，got %+v", got)
	}
	if got, err := s.ListUnjudgedCandidates("other", 10); err != nil || len(got) != 0 {
		t.Fatalf("工程过滤应生效，got %+v %v", got, err)
	}
}

// JudgeStats 的分母只含人工样本：判定器自己落库的（历史 --apply 数据）不算一致率，
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
	// 人工采纳一条；另一条由判定器自己落库否决（历史 --apply 数据）。
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

// ListJudgeRejects 只出「已判 reject、仍 pending」的行：
// accept 的候选一条都不能进来——批量清退只允许否决，不允许授予。
func TestListJudgeRejectsOnlyPendingRejects(t *testing.T) {
	s := newTestStore(t)
	insert := func(project, kind, content string) string {
		t.Helper()
		res, err := s.InsertCandidate(CandidateInput{
			SessionID: "s", Project: project, Kind: kind, Content: content, Source: "distill",
		})
		if err != nil {
			t.Fatalf("InsertCandidate: %v", err)
		}
		return res.Candidate.ID
	}
	first := insert("p1", "fact", "巡检完成，报告已输出")
	accept := insert("p1", "fact", "MCP 与 hook 注入相互独立")
	decided := insert("p1", "fact", "已经被人采纳的候选")
	other := insert("p2", "pitfall", "另一个工程的过程叙述候选")

	if err := s.MarkJudgeVerdict(first, "reject", "m@v1", "process_narrative"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkJudgeVerdict(accept, "accept", "m@v1", "durable"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkJudgeVerdict(decided, "reject", "m@v1", "process_narrative"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkJudgeVerdict(other, "reject", "m@v1", "transient_state"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitAccept(decided, -1, "human"); err != nil {
		t.Fatalf("CommitAccept: %v", err)
	}

	got, err := s.ListJudgeRejects("", 10)
	if err != nil {
		t.Fatalf("ListJudgeRejects: %v", err)
	}
	// created_at 只到秒，同一秒内的先后由 id 决定；这里只断言集合，不断言次序。
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if len(got) != 2 || !ids[first] || !ids[other] {
		t.Fatalf("应只出两条未裁决的 reject，got %+v", got)
	}
	for _, r := range got {
		if r.ID == first && (r.JudgeReason != "process_narrative" || r.Kind != "fact") {
			t.Errorf("理由码与 kind 应带出来，got %+v", r)
		}
	}

	filtered, err := s.ListJudgeRejects("p1", 10)
	if err != nil {
		t.Fatalf("ListJudgeRejects(project): %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != first {
		t.Fatalf("工程过滤应生效，got %+v", filtered)
	}
}
