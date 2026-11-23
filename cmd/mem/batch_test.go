package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/commit"
)

// 批量准出的三段式（docs/design-admission.md）：
// AI 只出方案 → 人 --dry-run 过目 → 执行一次、actor 记 human-reviewed-llm、归档可回滚。
// 这组用例锁住每段都不可省：少了 dry-run 就没有「人看过」，少了归档就没有回滚凭证。

func writeBatchPlan(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plan.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("写方案: %v", err)
	}
	return p
}

func planLine(id, action, reason string) string {
	if reason == "" {
		return `{"id":"` + id + `","action":"` + action + `"}`
	}
	return `{"id":"` + id + `","action":"` + action + `","reason":"` + reason + `"}`
}

func batchArchivePath(t *testing.T, db string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(filepath.Dir(db), "plans", "*.jsonl"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("应恰好有一份归档，got %v", files)
	}
	return files[0]
}

func TestBatchDryRunWritesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := newJudgeStore(t)
	db := st.Path()
	keep := seedCandidate(t, st, "fact", "v2mem 的 MCP 通道与 hook 注入相互独立")
	drop := seedCandidate(t, st, "pitfall", "巡检完成，报告已输出")

	plan := writeBatchPlan(t,
		planLine(keep.ID[:8], "accept", ""),
		planLine(drop.ID[:8], "reject", "process_narrative"))

	out, err := captureStdout(t, func() error {
		return cmdBatch([]string{"--plan", plan, "--db", db, "--dry-run"})
	})
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry-run：未写库") || !strings.Contains(out, "方案 2 条") {
		t.Errorf("dry-run 输出应说明规模与未写库，got:\n%s", out)
	}
	for _, id := range []string{keep.ID, drop.ID} {
		c, err := st.ResolveCandidate(id)
		if err != nil {
			t.Fatalf("ResolveCandidate: %v", err)
		}
		if c.Status != "pending" {
			t.Errorf("dry-run 不得改状态，%s got %q", id[:8], c.Status)
		}
	}
	if files, _ := filepath.Glob(filepath.Join(filepath.Dir(db), "plans", "*.jsonl")); len(files) != 0 {
		t.Errorf("dry-run 不得写归档，got %v", files)
	}
}

func TestBatchAppliesBothActionsAndArchives(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := newJudgeStore(t)
	db := st.Path()
	keep := seedCandidate(t, st, "fact", "v2mem 的 MCP 通道与 hook 注入相互独立")
	drop := seedCandidate(t, st, "pitfall", "巡检完成，报告已输出")

	plan := writeBatchPlan(t,
		planLine(keep.ID[:8], "accept", ""),
		planLine(drop.ID[:8], "reject", "process_narrative"))

	out, err := captureStdout(t, func() error {
		return cmdBatch([]string{"--plan", plan, "--db", db})
	})
	if err != nil {
		t.Fatalf("batch: %v\n%s", err, out)
	}
	if !strings.Contains(out, "已采纳 1 条 / 已拒绝 1 条") || !strings.Contains(out, "回滚：mem batch --rollback") {
		t.Errorf("输出应给出计数与回滚命令，got:\n%s", out)
	}

	got, err := st.ResolveCandidate(keep.ID)
	if err != nil {
		t.Fatalf("ResolveCandidate: %v", err)
	}
	if got.Status != "accepted" {
		t.Errorf("accept 项应落库，got %q", got.Status)
	}
	rejected, err := st.ResolveCandidate(drop.ID)
	if err != nil {
		t.Fatalf("ResolveCandidate: %v", err)
	}
	if rejected.Status != "rejected" {
		t.Errorf("reject 项应否决，got %q", rejected.Status)
	}

	// actor 必须与 human 分开：这是「人批准 AI 方案」能被识别出来的唯一标记。
	dec, err := st.CommitDecisions(keep.ID)
	if err != nil {
		t.Fatalf("CommitDecisions: %v", err)
	}
	if last := dec[len(dec)-1]; last.Actor != commit.ActorHumanReviewedLLM {
		t.Errorf("批量落库的 actor 应为 %q，got %q", commit.ActorHumanReviewedLLM, last.Actor)
	}
	if stats, err := st.Stats(); err != nil || stats.Live != 1 {
		t.Errorf("应只新建一条记忆，got %+v %v", stats, err)
	}

	recs, err := readBatchArchive(batchArchivePath(t, db))
	if err != nil {
		t.Fatalf("readBatchArchive: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("归档应有 2 行，got %d", len(recs))
	}
	var accepted batchRunRecord
	for _, r := range recs {
		if r.Action == "accept" {
			accepted = r
		}
	}
	if !accepted.OK || !accepted.CreatedMemory || accepted.MemoryID == "" {
		t.Errorf("accept 行应记下 memory_id 与 created_memory，got %+v", accepted)
	}
}

func TestReadBatchPlanValidation(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"未知动作", `{"id":"ab","action":"defer"}`, "action 只能是"},
		{"未知理由码", `{"id":"ab","action":"reject","reason":"looks_bad"}`, "未知理由码"},
		{"未知字段", `{"id":"ab","action":"accept","confidence":0.9}`, "不是合法方案行"},
		{"缺 id", `{"action":"accept"}`, "缺少 id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readBatchPlan(writeBatchPlan(t, tc.line))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报 %q，got %v", tc.want, err)
			}
		})
	}
	t.Run("重复 id", func(t *testing.T) {
		_, err := readBatchPlan(writeBatchPlan(t,
			planLine("ab", "accept", ""), planLine("ab", "reject", "process_narrative")))
		if err == nil || !strings.Contains(err.Error(), "重复的 id") {
			t.Fatalf("应报重复 id，got %v", err)
		}
	})
	t.Run("空方案", func(t *testing.T) {
		_, err := readBatchPlan(writeBatchPlan(t))
		if err == nil || !strings.Contains(err.Error(), "方案为空") {
			t.Fatalf("应报方案为空，got %v", err)
		}
	})
}

// 一条不可执行就整体不执行：人看到的方案与实际写库的方案必须是同一份。
func TestBatchRefusesPartialExecution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := newJudgeStore(t)
	db := st.Path()
	decided := seedCandidate(t, st, "fact", "已经被人裁决过的候选")
	pending := seedCandidate(t, st, "fact", "还没裁决、方案里也带着的候选")
	if _, err := st.CommitAccept(decided.ID, -1, commit.ActorHuman); err != nil {
		t.Fatalf("先采纳一条: %v", err)
	}

	plan := writeBatchPlan(t,
		planLine(pending.ID[:8], "accept", ""),
		planLine(decided.ID[:8], "accept", ""))
	if _, err := captureStdout(t, func() error {
		return cmdBatch([]string{"--plan", plan, "--db", db, "--dry-run"})
	}); err == nil {
		t.Fatal("dry-run 遇到不可执行项应非零，让人在这里就改方案")
	}
	if _, err := captureStdout(t, func() error {
		return cmdBatch([]string{"--plan", plan, "--db", db})
	}); err == nil {
		t.Fatal("含不可执行项时应整体拒绝")
	}
	got, err := st.ResolveCandidate(pending.ID)
	if err != nil {
		t.Fatalf("ResolveCandidate: %v", err)
	}
	if got.Status != "pending" {
		t.Errorf("整体拒绝时不得落库任何一条，got %q", got.Status)
	}
}

func TestBatchRollbackReopensAndDeletes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := newJudgeStore(t)
	db := st.Path()
	keep := seedCandidate(t, st, "fact", "v2mem 的 MCP 通道与 hook 注入相互独立")
	drop := seedCandidate(t, st, "pitfall", "巡检完成，报告已输出")

	plan := writeBatchPlan(t,
		planLine(keep.ID[:8], "accept", ""),
		planLine(drop.ID[:8], "reject", "process_narrative"))
	if _, err := captureStdout(t, func() error {
		return cmdBatch([]string{"--plan", plan, "--db", db})
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	archive := batchArchivePath(t, db)

	out, err := captureStdout(t, func() error {
		return cmdBatch([]string{"--rollback", archive, "--db", db})
	})
	if err != nil {
		t.Fatalf("rollback: %v\n%s", err, out)
	}
	if !strings.Contains(out, "退回 pending 2 条") {
		t.Errorf("回滚输出应报告退回条数，got:\n%s", out)
	}
	for _, id := range []string{keep.ID, drop.ID} {
		c, err := st.ResolveCandidate(id)
		if err != nil {
			t.Fatalf("ResolveCandidate: %v", err)
		}
		if c.Status != "pending" {
			t.Errorf("回滚后应回到 pending，%s got %q", id[:8], c.Status)
		}
	}
	if stats, err := st.Stats(); err != nil || stats.Live != 0 {
		t.Errorf("回滚应删掉这次新建的记忆，got %+v %v", stats, err)
	}
	dec, err := st.CommitDecisions(keep.ID)
	if err != nil {
		t.Fatalf("CommitDecisions: %v", err)
	}
	if last := dec[len(dec)-1]; last.Action != commit.ActionReopen {
		t.Errorf("应留下 reopen 裁决，got %q", last.Action)
	}
}

// 归档之后又被裁决过的候选，不按旧归档撤。
func TestBatchRollbackSkipsCandidatesDecidedAfterArchive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := newJudgeStore(t)
	db := st.Path()
	c := seedCandidate(t, st, "fact", "归档之后又被人重新裁决过的候选")

	plan := writeBatchPlan(t, planLine(c.ID[:8], "accept", ""))
	if _, err := captureStdout(t, func() error {
		return cmdBatch([]string{"--plan", plan, "--db", db})
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	archive := batchArchivePath(t, db)

	// 模拟归档之后又被人回滚 + 重新采纳：revision 前进了两格。
	if _, err := st.RollbackCandidate(c.ID, commit.ActorHumanReviewedLLM, true, -1); err != nil {
		t.Fatalf("中间回滚: %v", err)
	}
	if _, err := st.CommitAccept(c.ID, -1, commit.ActorHuman); err != nil {
		t.Fatalf("重新采纳: %v", err)
	}

	if _, err := captureStdout(t, func() error {
		return cmdBatch([]string{"--rollback", archive, "--db", db})
	}); err == nil {
		t.Fatal("旧归档撞 revision 应报错")
	}
	got, err := st.ResolveCandidate(c.ID)
	if err != nil {
		t.Fatalf("ResolveCandidate: %v", err)
	}
	if got.Status != "accepted" {
		t.Errorf("旧归档不得抹掉后来的裁决，got %q", got.Status)
	}
}

func TestBatchFlagUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := cmdBatch([]string{}); err == nil || !strings.Contains(err.Error(), "用法: mem batch") {
		t.Fatalf("缺参数应给用法，got %v", err)
	}
	if err := cmdBatch([]string{"--plan", "a", "--rollback", "b"}); err == nil {
		t.Fatal("--plan 与 --rollback 互斥")
	}
	if err := cmdBatch([]string{"--rollback", "b", "--dry-run"}); err == nil {
		t.Fatal("--dry-run 只用于 --plan")
	}
}
