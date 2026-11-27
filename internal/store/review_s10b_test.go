package store

// S10b 复核动作面的 store 层单测（docs/design-data-lifecycle.md §6.3/§6.4）。
//
// CLI 层（cmd/mem）已有端到端 case；这里用 store API 直测分支与不变量，
// 让新增的 ApplyMemoryReview / RollbackModelTag / ReviewStats /
// MarkJudgeInvalidOutput / ModelAutoCommit 在本包内也有覆盖率与回归保护。

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/commit"
	"github.com/v2camp/v2mem/internal/similarity"
)

// seedRollbackTarget 造一条「model-auto 采纳 + 候选 accepted」的回滚标的，
// created_memory=1 与 RollbackModelTag 的命中条件一致。
func seedRollbackTarget(t *testing.T, s *Store, content, modelTag, reviewState string) (memID, candID string) {
	t.Helper()
	res, err := s.Add(AddInput{Content: content, Source: "distill-accept", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	memID = res.ID
	candID = "cand-" + memID[:8]
	if _, err := s.db.Exec(`UPDATE memories SET review_state = ?, reviewed_at = ?, reviewed_by = ?
		WHERE id = ?`, reviewState, 1700000000, "human", memID); err != nil {
		t.Fatalf("preset review_state: %v", err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at, memory_id)
		VALUES (?, 's', 'p', 'fact', ?, ?, 'accepted', 'distill', 1700000000, ?)`,
		candID, content, "h-"+candID, memID); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO commit_decisions
		(id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
		 memory_id, model_tag, created_memory, idempotency_key, created_at)
		VALUES (?,?,0,'','auto_commit','model-auto','[]',?,?,1,?,1700000000)`,
		"dec-"+candID, candID, memID, modelTag, "key-"+candID); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	return memID, candID
}

func reviewActionCount(t *testing.T, s *Store, memoryID, action string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM commit_decisions
		WHERE action = ? AND memory_id = ?`, action, memoryID).Scan(&n); err != nil {
		t.Fatalf("count %s decisions: %v", action, err)
	}
	return n
}

func TestApplyMemoryReviewFourActions(t *testing.T) {
	s := newTestStore(t)
	memID, candID := seedRollbackTarget(t, s, "四动作依次走一遍的记忆", "m@v1", "")

	// confirm：confirmed + human，不改检索可见性。
	rep, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: ReviewActionConfirm})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if rep.Before.ReviewState != "" || rep.After.ReviewState != ReviewStateConfirmed || rep.After.ReviewedBy != "human" {
		t.Fatalf("confirm 前后状态错: %+v -> %+v", rep.Before, rep.After)
	}
	if got := reviewActionCount(t, s, memID, reviewActionAudit); got != 1 {
		t.Fatalf("confirm 应记一条 review_action，got %d", got)
	}

	// correct：正文落库、content_idx 同步、原提案进裁决账；model_tag 沿用产生该记忆的版本。
	rep, err = s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: ReviewActionCorrect,
		Content: "人工修订后的正文"})
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	var content, idx, tag string
	if err := s.db.QueryRow(`SELECT content, content_idx FROM memories WHERE id = ?`, memID).
		Scan(&content, &idx); err != nil {
		t.Fatal(err)
	}
	if content != "人工修订后的正文" || idx != indexText(content) {
		t.Fatalf("correct 应同步正文与 content_idx，got %q / %q", content, idx)
	}
	// 签名必须跟着正文换：否则模糊检索仍用旧文本的 MinHash 比对，修正后反而搜不到。
	var sig []byte
	if err := s.db.QueryRow(`SELECT sig FROM mem_sigs WHERE memory_id = ?`, memID).Scan(&sig); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sig, encodeSig(similarity.Sign("人工修订后的正文"))) {
		t.Fatal("correct 必须同步重算 mem_sigs 签名")
	}
	var reasons string
	if err := s.db.QueryRow(`SELECT reason_codes_json FROM commit_decisions
		WHERE action = ? AND memory_id = ? AND reason_codes_json LIKE '%review_action=correct%'
		ORDER BY created_at DESC, id DESC LIMIT 1`,
		reviewActionAudit, memID).Scan(&reasons); err != nil {
		t.Fatalf("读 review_action: %v", err)
	}
	if !strings.Contains(reasons, "四动作依次走一遍的记忆") || !strings.Contains(reasons, "人工修订后的正文") {
		t.Fatalf("correct 的裁决账应保留 before/after 原文，got %s", reasons)
	}
	if err := s.db.QueryRow(`SELECT model_tag FROM commit_decisions
		WHERE action = ? AND memory_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		reviewActionAudit, memID).Scan(&tag); err != nil {
		t.Fatal(err)
	}
	if tag != "m@v1" {
		t.Fatalf("动作归因应沿用产生该记忆的 model_tag，got %q", tag)
	}

	// withdraw：退出检索、候选回 rejected、原提案候选留 rejected 明确终态。
	rep, err = s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: ReviewActionWithdraw,
		Reason: "事实有误"})
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if rep.After.ReviewState != ReviewStateWithdrawn || len(rep.AffectedCandidates) != 1 ||
		rep.AffectedCandidates[0] != candID {
		t.Fatalf("withdraw 报告错: %+v", rep)
	}
	hits, err := s.Search(SearchQuery{Query: "人工修订后的正文", Scope: "all", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if h.ID == memID {
			t.Fatal("撤回后不得仍出现在检索结果")
		}
	}
	var status, sreason string
	if err := s.db.QueryRow(`SELECT status, status_reason FROM candidates WHERE id = ?`, candID).
		Scan(&status, &sreason); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || sreason != "事实有误" {
		t.Fatalf("候选应恢复 rejected + 人工原因，got %q/%q", status, sreason)
	}
	// 撤回那条候选也留一条人工 reject 终态裁决（G9：rejected 候选必须有匹配裁决）。
	if got := reviewActionCount(t, s, memID, string(commit.ActionReject)); got != 1 {
		t.Fatalf("撤回应为候选写一条 reject 终态裁决，got %d", got)
	}

	// restore：清 review_state/reviewed_*，检索重新可见。
	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: ReviewActionRestore}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var state, by string
	var at int64
	if err := s.db.QueryRow(`SELECT review_state, reviewed_by, reviewed_at FROM memories WHERE id = ?`, memID).
		Scan(&state, &by, &at); err != nil {
		t.Fatal(err)
	}
	if state != "" || by != "" || at != 0 {
		t.Fatalf("restore 应清空审核痕迹，got %q/%q/%d", state, by, at)
	}
}

func TestApplyMemoryReviewValidation(t *testing.T) {
	s := newTestStore(t)
	memID, _ := seedRollbackTarget(t, s, "校验分支的记忆", "m@v1", "")

	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: " ", Action: ReviewActionConfirm}); err == nil {
		t.Fatal("空 id 必须报错")
	}
	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: "no-such", Action: ReviewActionConfirm}); err == nil {
		t.Fatal("未知 id 必须报错")
	}
	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: "nuke"}); err == nil {
		t.Fatal("闭集外动作必须报错")
	}
	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: ReviewActionCorrect}); err == nil {
		t.Fatal("correct 缺 --content 必须报错")
	}
	// confirm/correct 不接受已撤回的记忆；restore 只接受已撤回。
	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: ReviewActionRestore}); err == nil {
		t.Fatal("对未撤回记忆 restore 必须报错")
	}
	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: ReviewActionWithdraw}); err != nil {
		t.Fatalf("首次 withdraw: %v", err)
	}
	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: ReviewActionWithdraw}); err == nil {
		t.Fatal("重复 withdraw 必须报错")
	}
	for _, a := range []string{ReviewActionConfirm, ReviewActionCorrect} {
		if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: memID, Action: a, Content: "x"}); err == nil {
			t.Fatalf("已撤回记忆上 %s 必须报错（restore 才是逆动作）", a)
		}
	}
	// 合法输入缺省值：Actor/Now 留空时由 store 兜底，不产生空审计行。
	s2mem, _ := seedRollbackTarget(t, s, "缺省值兜底的记忆", "m@v2", "")
	rep, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: s2mem, Action: ReviewActionConfirm})
	if err != nil {
		t.Fatalf("缺省 Actor/Now: %v", err)
	}
	if rep.After.ReviewedBy != "human" || rep.After.ReviewedAt <= 0 {
		t.Fatalf("Actor/Now 缺省应兜底 human/当前时间，got %+v", rep.After)
	}
}

func TestRollbackModelTagFlow(t *testing.T) {
	s := newTestStore(t)
	m1, c1 := seedRollbackTarget(t, s, "回滚标的 m1", "m@v1", "")
	seedRollbackTarget(t, s, "人工确认过 m2", "m@v1", ReviewStateConfirmed)
	seedRollbackTarget(t, s, "人工修正过 m3", "m@v1", ReviewStateCorrected)
	seedRollbackTarget(t, s, "已撤回 m4", "m@v1", ReviewStateWithdrawn)
	m5, _ := seedRollbackTarget(t, s, "别的版本 m5", "m@v2", "")

	// dry-run：报告与真实运行一致，零写入。
	dry, err := s.RollbackModelTag("m@v1", true, 1700001000)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if dry.Matched != 4 || dry.Withdrawn != 1 || dry.Skipped != 2 || dry.Already != 1 || dry.Reopened != 0 {
		t.Fatalf("dry-run 计数错: %+v", dry)
	}
	var state string
	if err := s.db.QueryRow(`SELECT review_state FROM memories WHERE id = ?`, m1).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		t.Fatalf("dry-run 不得写库，m1 state=%q", state)
	}

	rep, err := s.RollbackModelTag("m@v1", false, 1700001000)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rep.Matched != 4 || rep.Withdrawn != 1 || rep.Skipped != 2 || rep.Already != 1 || rep.Reopened != 1 {
		t.Fatalf("回滚计数错: %+v", rep)
	}
	if len(rep.WithdrawnIDs) != 1 || rep.WithdrawnIDs[0] != m1 {
		t.Fatalf("撤回明细错: %v", rep.WithdrawnIDs)
	}
	if err := s.db.QueryRow(`SELECT review_state FROM memories WHERE id = ?`, m5).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		t.Fatalf("别的版本不得被回滚，m5 state=%q", state)
	}
	// 被撤候选：rejected 终态 + reopen 决策（G9 口径）。
	var cstatus string
	if err := s.db.QueryRow(`SELECT status FROM candidates WHERE id = ?`, c1).Scan(&cstatus); err != nil {
		t.Fatal(err)
	}
	if cstatus != "rejected" {
		t.Fatalf("回滚候选应回 rejected，got %q", cstatus)
	}
	if got := reviewActionCount(t, s, m1, string(commit.ActionReopen)); got != 1 {
		t.Fatalf("回滚应记一条 reopen 决策，got %d", got)
	}

	// 幂等：重跑全部计 already，不再动作。
	again, err := s.RollbackModelTag("m@v1", false, 1700002000)
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if again.Withdrawn != 0 || again.Reopened != 0 || again.Already != 2 || again.Matched != 4 {
		t.Fatalf("重跑应幂等: %+v", again)
	}

	// 空 tag 如实报错。
	if _, err := s.RollbackModelTag(" ", false, 0); err == nil {
		t.Fatal("缺 --model-tag 必须报错")
	}
}

func TestReviewStatsByModelTag(t *testing.T) {
	s := newTestStore(t)
	m1, _ := seedRollbackTarget(t, s, "stats 确认样本", "m@v1", ReviewStateConfirmed)
	seedRollbackTarget(t, s, "stats 修正样本", "m@v1", ReviewStateCorrected)
	seedRollbackTarget(t, s, "stats 撤回样本", "m@v1", ReviewStateWithdrawn)
	seedRollbackTarget(t, s, "stats 未审核样本", "m@v1", "")
	seedRollbackTarget(t, s, "stats 别的版本", "m@v2", ReviewStateConfirmed)
	// 误拒回捞：restore 动作计入 recovered。
	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: m1, Action: ReviewActionWithdraw}); err != nil {
		t.Fatalf("withdraw m1: %v", err)
	}
	if _, err := s.ApplyMemoryReview(MemoryReviewInput{MemoryID: m1, Action: ReviewActionRestore}); err != nil {
		t.Fatalf("restore m1: %v", err)
	}

	rows, err := s.ReviewStats()
	if err != nil {
		t.Fatalf("ReviewStats: %v", err)
	}
	byTag := map[string]ReviewStatsRow{}
	for _, r := range rows {
		byTag[r.ModelTag] = r
	}
	v1, ok := byTag["m@v1"]
	if !ok {
		t.Fatalf("应按 model_tag 分组，got %v", rows)
	}
	if v1.Samples != 4 || v1.Confirmed != 0 || v1.Corrected != 1 || v1.Withdrawn != 1 || v1.Recovered != 1 {
		t.Fatalf("m@v1 统计错: %+v", v1)
	}
	if v1.WithdrawRate <= 0 || v1.CorrectRate <= 0 {
		t.Fatalf("比率应大于 0: %+v", v1)
	}
	if v2, ok := byTag["m@v2"]; !ok || v2.Samples != 1 || v2.Confirmed != 1 {
		t.Fatalf("m@v2 统计错: %+v", byTag["m@v2"])
	}
}

func TestMarkJudgeInvalidOutputRetriesThenQuarantines(t *testing.T) {
	s := newTestStore(t)
	c := newCandidate(t, s, "fact", "p", "非法输出要留痕的候选")
	id := c.Candidate.ID

	// 第 1、2 次 failed（attempt 递增），第 3 次达上限转 quarantined。
	for i, want := range []string{"failed", "failed", "quarantined"} {
		got, err := s.MarkJudgeInvalidOutput(id, fmt.Sprintf("bad output #%d", i+1))
		if err != nil {
			t.Fatalf("第 %d 次 MarkJudgeInvalidOutput: %v", i+1, err)
		}
		if got != want {
			t.Fatalf("第 %d 次应转 %s，got %s", i+1, want, got)
		}
	}
	var attempt int
	var reason string
	if err := s.db.QueryRow(`SELECT attempt, status_reason FROM candidates WHERE id = ?`, id).
		Scan(&attempt, &reason); err != nil {
		t.Fatal(err)
	}
	if attempt < 3 || !strings.Contains(reason, "bad output") {
		t.Fatalf("attempt/status_reason 应留痕，got %d/%q", attempt, reason)
	}
	// quarantined 是终态：不得被提交/拒绝，也不能再次留痕。
	if _, err := s.CommitAccept(id, -1, "human"); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("quarantined 不得被 CommitAccept 接受，got %v", err)
	}
	if _, err := s.CommitReject(id, -1, "human", ""); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("quarantined 不得被 CommitReject 接受，got %v", err)
	}
	if _, err := s.MarkJudgeInvalidOutput(id, "again"); err == nil {
		t.Fatal("终态候选不得再次留痕")
	}
	if _, err := s.MarkJudgeInvalidOutput(" ", "x"); err == nil {
		t.Fatal("空 id 必须报错")
	}
}

func TestModelAutoCommitMatrixAndAttribution(t *testing.T) {
	s := newTestStore(t)
	ev := captureEvidence(t, s, "uid-modelauto", "s", "p", "A 级直接证据日志")
	c := newCandidate(t, s, "decision", "p", "model-auto 可自动采纳的 decision 候选")
	attachCandidateEvidence(t, s, c.Candidate.ID, ev.ID, ev.EventUID, ev.ArchiveHash, "direct")

	// manualKind + A 级证据：开关打开后自动 accept，actor=model-auto、model_tag 落账。
	res, err := s.ModelAutoCommit(c.Candidate.ID, -1, "deepseek:chat@v7")
	if err != nil {
		t.Fatalf("ModelAutoCommit: %v", err)
	}
	if res.Action != commit.ActionAutoCommit || res.Actor != commit.ActorModelAuto || !res.CreatedMemory {
		t.Fatalf("自动采纳结果错: %+v", res)
	}
	var actor, tag string
	var created int
	if err := s.db.QueryRow(`SELECT actor, model_tag, created_memory FROM commit_decisions
		WHERE candidate_id = ?`, c.Candidate.ID).Scan(&actor, &tag, &created); err != nil {
		t.Fatal(err)
	}
	if actor != commit.ActorModelAuto || tag != "deepseek:chat@v7" || created != 1 {
		t.Fatalf("归因错: actor=%q tag=%q created=%d", actor, tag, created)
	}
	var state string
	if err := s.db.QueryRow(`SELECT review_state FROM memories WHERE id = ?`, res.MemoryID).
		Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		t.Fatalf("model-auto 产物应留未审核态，got %q", state)
	}

	// 重放幂等：首次 accept 把 revision 推进 1（幂等键含 revision），同 revision 的
	// 重放不再新增记忆、裁决数稳定。
	res2, err := s.ModelAutoCommit(c.Candidate.ID, -1, "deepseek:chat@v7")
	if err != nil {
		t.Fatalf("重放: %v", err)
	}
	if res2.CreatedMemory {
		t.Fatal("重放不得重复新建记忆")
	}
	mid := len(mustDecisions(t, s, c.Candidate.ID))
	if _, err := s.ModelAutoCommit(c.Candidate.ID, -1, "deepseek:chat@v7"); err != nil {
		t.Fatalf("再次重放: %v", err)
	}
	if after := len(mustDecisions(t, s, c.Candidate.ID)); after != mid {
		t.Fatalf("同 revision 重放不得新增裁决: %d -> %d", mid, after)
	}

	// Defer 优先于开关：fact 无证据不得写库，但仍留一条带 model_tag 的裁决。
	deferC := newCandidate(t, s, "fact", "p", "没有证据的 fact 不得因开关落库")
	res3, err := s.ModelAutoCommit(deferC.Candidate.ID, -1, "deepseek:chat@v7")
	if err != nil {
		t.Fatalf("Defer 路径: %v", err)
	}
	if res3.CreatedMemory || res3.Action != commit.ActionDefer {
		t.Fatalf("Defer 不得写库: %+v", res3)
	}
	if got := len(mustDecisions(t, s, deferC.Candidate.ID)); got != 1 {
		t.Fatalf("Defer 也应留一条裁决，got %d", got)
	}

	// Conflict 优先于开关：scope=conflict 直接判冲突，不写记忆。
	confC := newCandidate(t, s, "fact", "p", "冲突候选不得因开关落库")
	if _, err := s.db.Exec(`UPDATE candidates SET scope = 'conflict' WHERE id = ?`, confC.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	res4, err := s.ModelAutoCommit(confC.Candidate.ID, -1, "deepseek:chat@v7")
	if err != nil {
		t.Fatalf("Conflict 路径: %v", err)
	}
	if res4.CreatedMemory || res4.Action != commit.ActionConflict {
		t.Fatalf("Conflict 不得写库: %+v", res4)
	}
}

func TestReviewSummaryEligiblePureFunction(t *testing.T) {
	conflictish := "mem-conflict"
	if !reviewSummaryEligible(conflictish, nil, "conflict", "fact", "project") {
		t.Fatal("action=conflict 必进摘要")
	}
	if !reviewSummaryEligible(conflictish, []string{"Conflict"}, "accept", "fact", "project") {
		t.Fatal("理由码冲突（大小写不敏感）必进摘要")
	}
	if !reviewSummaryEligible(conflictish, []string{"SELF_SCORE_LOW"}, "accept", "fact", "project") {
		t.Fatal("低置信理由码必进摘要")
	}
	for _, kind := range []string{"decision", "preference", "rule"} {
		if !highImpactKindOrScope(kind, "") {
			t.Errorf("%s 是高影响 kind", kind)
		}
	}
	for _, scope := range []string{"global", "cross_project"} {
		if !highImpactKindOrScope("fact", scope) {
			t.Errorf("%s 是高影响 scope", scope)
		}
	}
	if highImpactKindOrScope("fact", "project") {
		t.Error("普通 fact/project 不是高影响")
	}
	// 确定性抽样：同 id 恒定，且 256 个取值里命中约 10%。
	hits := 0
	for i := 0; i < 256; i++ {
		id := fmt.Sprintf("sample-%d", i)
		want := sampleHighImpact(id)
		if want != sampleHighImpact(id) {
			t.Fatalf("抽样必须确定性: %s", id)
		}
		if want {
			hits++
		}
	}
	if hits < 15 || hits > 40 {
		t.Fatalf("抽样比例应约 10%%，got %d/256", hits)
	}
	// 普通产物（非高影响、无标记）默认信任不进摘要。
	if reviewSummaryEligible("mem-plain", []string{"auto_eligible"}, "accept", "fact", "project") {
		t.Fatal("普通产物不该进摘要")
	}
	if truncateForAudit(strings.Repeat("长", 300)) == strings.Repeat("长", 300) {
		t.Fatal("超长正文必须截断")
	}
	if got := truncateForAudit("短文本"); got != "短文本" {
		t.Fatalf("短正文原样保留，got %q", got)
	}
}

// 采纳写入的 scope 必须真的影响摘要：global 记忆按 sha256(id)[0]%10==0 抽样，
// 命中的进 summary，未命中的不进。这条把「写 scope」与「复核摘要」两端钉在一起。
func TestReviewSummaryIncludesSampledGlobalMemory(t *testing.T) {
	s := newTestStore(t)
	var hitID, missID string
	for i := 0; i < 200 && (hitID == "" || missID == ""); i++ {
		res, err := s.Add(AddInput{
			Content: fmt.Sprintf("scope 抽样记忆 %d", i), Project: "p", Kind: "fact",
			Source: "distill-accept",
		})
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		switch {
		case sampleHighImpact(res.ID) && hitID == "":
			hitID = res.ID
		case !sampleHighImpact(res.ID) && missID == "":
			missID = res.ID
		}
	}
	if hitID == "" || missID == "" {
		t.Fatalf("未能造出命中/未命中两条记忆: hit=%q miss=%q", hitID, missID)
	}
	for _, id := range []string{hitID, missID} {
		if _, err := s.db.Exec(`UPDATE memories SET scope = 'global' WHERE id = ?`, id); err != nil {
			t.Fatalf("set scope: %v", err)
		}
		if _, err := s.db.Exec(`INSERT INTO commit_decisions
			(id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
			 memory_id, model_tag, created_memory, idempotency_key, created_at)
			VALUES (?,?,0,'','auto_commit','model-auto','[]',?,'m@v1',1,?,1700000000)`,
			"dec-"+id, "cand-"+id, id, "key-"+id); err != nil {
			t.Fatalf("insert decision: %v", err)
		}
	}

	q, err := s.ListReviewQueue(ReviewQueueSummary, 50)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	got := map[string]bool{}
	for _, it := range q.States[ReviewQueueSummary] {
		got[it.ID] = true
	}
	if !got[hitID] {
		t.Fatal("hash(id)%10==0 的 global 记忆必须进 summary")
	}
	if got[missID] {
		t.Fatal("未命中抽样的 global 记忆不应进 summary")
	}
	if q.Counts[ReviewQueueSummary] != 1 {
		t.Fatalf("summary 计数应与列表同源，got %d", q.Counts[ReviewQueueSummary])
	}
}
