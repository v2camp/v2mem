package main

// P4 Wave 2 / D：judge 的 --apply-reject / --apply-accept。
//
// 默认仍是影子；显式打开后 reject 走 CommitReject(actor=judge)，
// accept 必须过 commit.Matrix{AllowModelAuto:true}，Defer/Conflict 只记裁决。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/v2camp/v2mem/internal/commit"
	"github.com/v2camp/v2mem/internal/store"
)

// seedJudgeApplyCandidate 直接写 proposal 候选：只有它能带 review_class 与证据链，
// 而这两项正是「冲突跳过」与「矩阵门控」的输入。
func seedJudgeApplyCandidate(t *testing.T, st *store.Store, kind, content, reviewClass string, withEvidence bool) store.Candidate {
	t.Helper()
	in := store.ProposalCandidateInput{
		SessionID: "s-apply", Project: "v2mem", Kind: kind, Content: content,
		Scope: "project", ReviewClass: reviewClass,
	}
	if withEvidence {
		in.Evidence = []store.CandidateEvidence{{
			EvidenceID: "ev-" + shortPrefix(content), EventUID: "uid-" + shortPrefix(content),
			SourceID: "src-p", SourceSeq: 1, ArchiveHash: "ah-" + shortPrefix(content),
			EvidenceRole: "direct",
		}}
	}
	res, err := st.InsertProposalCandidate(in)
	if err != nil {
		t.Fatalf("InsertProposalCandidate: %v", err)
	}
	c, err := st.ResolveCandidate(res.ID)
	if err != nil {
		t.Fatalf("ResolveCandidate: %v", err)
	}
	return *c
}

func shortPrefix(s string) string {
	r := []rune(s)
	if len(r) > 12 {
		r = r[:12]
	}
	return string(r)
}

func judgeVerdictsFor(c store.Candidate, verdict, reason string) []judgeVerdict {
	return []judgeVerdict{{ID: c.ID[:8], Verdict: verdict, Reason: reason}}
}

func candidateStatus(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var status string
	if err := st.DB().QueryRow(`SELECT status FROM candidates WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("读候选状态: %v", err)
	}
	return status
}

func countJudgeMemories(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("数记忆: %v", err)
	}
	return n
}

// accept 必须过矩阵：fact + 直接 A 级证据 → 未审核记忆。
func TestJudgeApplyAcceptViaMatrix(t *testing.T) {
	st := newJudgeStore(t)
	c := seedJudgeApplyCandidate(t, st, "fact", "judge accept 走矩阵落未审核记忆", "", true)

	r, err := applyJudgments(st, []store.Candidate{c}, judgeVerdictsFor(c, "accept", "durable"),
		"deepseek:flash@judge-v4", false, true)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Accept != 1 || r.AppliedAccept != 1 || r.Skipped != 0 {
		t.Fatalf("accept 应落库一条，got %+v", r)
	}
	if got := candidateStatus(t, st, c.ID); got != "accepted" {
		t.Fatalf("状态应 accepted，got %q", got)
	}
	view, err := st.CommitCandidate(c.ID)
	if err != nil || view.MemoryID == "" {
		t.Fatalf("应回填 memory_id，got %+v %v", view, err)
	}
	var state string
	if err := st.DB().QueryRow(`SELECT review_state FROM memories WHERE id = ?`, view.MemoryID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		t.Fatalf("judge accept 产物应留未审核态，got %q", state)
	}
	ds, err := st.CommitDecisions(c.ID)
	if err != nil || len(ds) != 1 {
		t.Fatalf("应有一条裁决，got %+v %v", ds, err)
	}
	if ds[0].Actor != commit.ActorJudge {
		t.Fatalf("裁决 actor 应为 judge，got %q", ds[0].Actor)
	}
}

// Defer 只记裁决：没有 A 级证据的 fact 不得写记忆，也不得改状态。
func TestJudgeApplyAcceptDeferKeepsPending(t *testing.T) {
	st := newJudgeStore(t)
	c := seedJudgeApplyCandidate(t, st, "fact", "没有证据的候选只记裁决", "", false)

	r, err := applyJudgments(st, []store.Candidate{c}, judgeVerdictsFor(c, "accept", "durable"),
		"deepseek:flash@judge-v4", false, true)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Accept != 1 || r.AppliedAccept != 0 {
		t.Fatalf("Defer 不应计入 applied_accept，got %+v", r)
	}
	if r.Skipped != 1 {
		t.Fatalf("Defer 应计入 skipped，got %+v", r)
	}
	if got := candidateStatus(t, st, c.ID); got != "pending" {
		t.Fatalf("Defer 不得改状态，got %q", got)
	}
	if n := countJudgeMemories(t, st); n != 0 {
		t.Fatalf("Defer 不得写记忆，got %d", n)
	}
	if ds, _ := st.CommitDecisions(c.ID); len(ds) != 1 {
		t.Fatalf("Defer 也应留一条裁决，got %d", len(ds))
	}
}

// Conflict 优先于开关：review_class=conflict 的 accept 不写记忆。
func TestJudgeApplyAcceptConflictSkipsMemory(t *testing.T) {
	st := newJudgeStore(t)
	c := seedJudgeApplyCandidate(t, st, "fact", "冲突类 accept 不得写记忆", "conflict", true)

	r, err := applyJudgments(st, []store.Candidate{c}, judgeVerdictsFor(c, "accept", "durable"),
		"deepseek:flash@judge-v4", false, true)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Accept != 1 || r.AppliedAccept != 0 || r.Skipped != 1 {
		t.Fatalf("冲突类不计 applied_accept，got %+v", r)
	}
	if n := countJudgeMemories(t, st); n != 0 {
		t.Fatalf("冲突类不得写记忆，got %d", n)
	}
	if got := candidateStatus(t, st, c.ID); got != "pending" {
		t.Fatalf("冲突类状态应保持 pending，got %q", got)
	}
}

// 冲突类 reject 跳过：冲突要留给人看，不能被 judge 自动否决掉。
func TestJudgeApplyRejectConflictSkipped(t *testing.T) {
	st := newJudgeStore(t)
	c := seedJudgeApplyCandidate(t, st, "fact", "冲突类 reject 必须跳过", "conflict", true)

	r, err := applyJudgments(st, []store.Candidate{c}, judgeVerdictsFor(c, "reject", "process_narrative"),
		"deepseek:flash@judge-v4", true, false)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Reject != 1 || r.AppliedReject != 0 || r.Skipped != 1 {
		t.Fatalf("冲突类 reject 应跳过，got %+v", r)
	}
	if got := candidateStatus(t, st, c.ID); got != "pending" {
		t.Fatalf("冲突类状态应保持 pending，got %q", got)
	}
	if ds, _ := st.CommitDecisions(c.ID); len(ds) != 0 {
		t.Fatalf("跳过不应产生裁决，got %d", len(ds))
	}
	var verdict string
	if err := st.DB().QueryRow(`SELECT judge_verdict FROM candidates WHERE id = ?`, c.ID).Scan(&verdict); err != nil {
		t.Fatal(err)
	}
	if verdict != "reject" {
		t.Fatalf("判定痕迹仍应落库，got %q", verdict)
	}
}

// reject 落库：actor=judge，理由码 judge_reject + judge:<reason>。
func TestJudgeApplyRejectActorAndReason(t *testing.T) {
	st := newJudgeStore(t)
	c := seedJudgeApplyCandidate(t, st, "fact", "巡检已完成，报告已输出", "", false)

	r, err := applyJudgments(st, []store.Candidate{c}, judgeVerdictsFor(c, "reject", "process_narrative"),
		"deepseek:flash@judge-v4", true, false)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Reject != 1 || r.AppliedReject != 1 || r.Skipped != 0 {
		t.Fatalf("reject 应落库一条，got %+v", r)
	}
	if got := candidateStatus(t, st, c.ID); got != "rejected" {
		t.Fatalf("状态应 rejected，got %q", got)
	}
	ds, err := st.CommitDecisions(c.ID)
	if err != nil || len(ds) != 1 {
		t.Fatalf("应有一条裁决，got %+v %v", ds, err)
	}
	if ds[0].Actor != commit.ActorJudge {
		t.Fatalf("actor 应为 judge，got %q", ds[0].Actor)
	}
	if len(ds[0].ReasonCodes) < 2 || ds[0].ReasonCodes[0] != "judge_reject" ||
		ds[0].ReasonCodes[1] != "judge:process_narrative" {
		t.Fatalf("理由码应为 judge_reject + judge:<reason>，got %+v", ds[0].ReasonCodes)
	}
}

// CAS stale 跳过：拿旧 revision 的批次不得覆盖并发裁决。
func TestJudgeApplySkipsStaleRevision(t *testing.T) {
	st := newJudgeStore(t)
	c := seedJudgeApplyCandidate(t, st, "fact", "并发推进后的旧批次", "", false)
	stale := c
	stale.Revision = c.Revision + 1

	r, err := applyJudgments(st, []store.Candidate{stale}, judgeVerdictsFor(c, "reject", "process_narrative"),
		"deepseek:flash@judge-v4", true, true)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Skipped != 1 || r.AppliedReject != 0 || r.AppliedAccept != 0 {
		t.Fatalf("stale 应跳过，got %+v", r)
	}
	if got := candidateStatus(t, st, c.ID); got != "pending" {
		t.Fatalf("stale 不得改状态，got %q", got)
	}
	var verdict string
	if err := st.DB().QueryRow(`SELECT judge_verdict FROM candidates WHERE id = ?`, c.ID).Scan(&verdict); err != nil {
		t.Fatal(err)
	}
	if verdict != "" {
		t.Fatalf("stale 不得写判定，got %q", verdict)
	}
}

// 重跑幂等：第二次拿同一批（旧 revision）只会 skipped，不重复建记忆。
func TestJudgeApplyRerunIsIdempotent(t *testing.T) {
	st := newJudgeStore(t)
	c := seedJudgeApplyCandidate(t, st, "fact", "重跑不得重复落库", "", true)

	if _, err := applyJudgments(st, []store.Candidate{c}, judgeVerdictsFor(c, "accept", "durable"),
		"deepseek:flash@judge-v4", false, true); err != nil {
		t.Fatalf("首次 applyJudgments: %v", err)
	}
	r, err := applyJudgments(st, []store.Candidate{c}, judgeVerdictsFor(c, "accept", "durable"),
		"deepseek:flash@judge-v4", false, true)
	if err != nil {
		t.Fatalf("重跑 applyJudgments: %v", err)
	}
	if r.AppliedAccept != 0 || r.Skipped != 1 {
		t.Fatalf("重跑应跳过，got %+v", r)
	}
	if n := countJudgeMemories(t, st); n != 1 {
		t.Fatalf("重跑不得新增记忆，got %d", n)
	}
	if ds, _ := st.CommitDecisions(c.ID); len(ds) != 1 {
		t.Fatalf("重跑不得新增裁决，got %d", len(ds))
	}
}

// unsure 即便开了 apply 也不改状态：信息不足不该替人做决定。
func TestJudgeApplyUnsureKeepsPending(t *testing.T) {
	st := newJudgeStore(t)
	c := seedJudgeApplyCandidate(t, st, "fact", "信息不足的候选保持 pending", "", false)

	r, err := applyJudgments(st, []store.Candidate{c}, judgeVerdictsFor(c, "unsure", "unsure"),
		"deepseek:flash@judge-v4", true, true)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Unsure != 1 || r.AppliedAccept != 0 || r.AppliedReject != 0 || r.Skipped != 0 {
		t.Fatalf("unsure 不计落库，got %+v", r)
	}
	if got := candidateStatus(t, st, c.ID); got != "pending" {
		t.Fatalf("unsure 不得改状态，got %q", got)
	}
	if n := countJudgeMemories(t, st); n != 0 {
		t.Fatalf("unsure 不得写记忆，got %d", n)
	}
}

// 完整 CLI 输出：JSON 带 applied_accept / applied_reject / skipped，且状态真的改了。
func TestCmdJudgeApplyOutputAndEffects(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	acceptC := seedJudgeApplyCandidate(t, st, "fact", "CLI 落库的 fact 候选", "", true)
	rejectC := seedJudgeApplyCandidate(t, st, "fact", "巡检完毕，CLI 应否决", "", false)
	st.Close()

	home := t.TempDir()
	bindir := filepath.Join(home, "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\\\n' '[{\"id\":\"%s\",\"verdict\":\"accept\",\"reason\":\"durable\"},{\"id\":\"%s\",\"verdict\":\"reject\",\"reason\":\"process_narrative\"}]'\n",
		acceptC.ID[:8], rejectC.ID[:8])
	if err := os.WriteFile(filepath.Join(bindir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := filepath.Join(home, "model.json")
	if err := os.WriteFile(cfg, []byte(`{"mode":"cli","cli":"claude","model_tag":"fake@v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return cmdJudge([]string{"--config", cfg, "--db", db, "--audit-file", "-", "--json",
			"--apply-reject", "--apply-accept", "--limit", "10"})
	})
	if err != nil {
		t.Fatalf("cmdJudge: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("JSON 输出应可解析: %v（%s）", err, out)
	}
	if got["applied_accept"] != float64(1) || got["applied_reject"] != float64(1) || got["skipped"] != float64(0) {
		t.Fatalf("输出计数异常: %s", out)
	}

	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if s := candidateStatus(t, st2, acceptC.ID); s != "accepted" {
		t.Fatalf("accept 候选应 accepted，got %q", s)
	}
	if s := candidateStatus(t, st2, rejectC.ID); s != "rejected" {
		t.Fatalf("reject 候选应 rejected，got %q", s)
	}
}
