package main

// mem review ls（只读四态视图）的命令面断言。
//
// 视图口径完全来自 store.ListReviewQueue：CLI 只做渲染，不在命令层拼 SQL ——
// 否则 MCP 的 review_ls 与 CLI 迟早给出两套「同一状态不同数」的答案。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

// seedReviewFixture 造一套四态都有样本的库：
// pending（没判定）/ failed / quarantined，外加一条 model-auto 写库但未审核的记忆。
func seedReviewFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	st := newJudgeStore(t)

	pending := seedCandidate(t, st, "fact", "模型还没跑过的候选")
	failed := seedCandidate(t, st, "fact", "执行失败等待重试的候选")
	quar := seedCandidate(t, st, "pitfall", "重试超限被隔离的候选")
	if err := st.MarkCandidateFailed(failed.ID, "executor_timeout"); err != nil {
		t.Fatalf("MarkCandidateFailed: %v", err)
	}
	if err := st.MarkCandidateQuarantined(quar.ID, "attempts_exceeded"); err != nil {
		t.Fatalf("MarkCandidateQuarantined: %v", err)
	}
	// 真实候选由 proposal 写入时带 scope；夹具显式补上，免得 scope 列永远是空的。
	if _, err := st.DB().Exec(`UPDATE candidates SET scope = 'project'`); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	// 同一秒插入的 pending 行排序由 id 决定，这里只需要一条未判定的，其余标已判定。
	if _, err := st.DB().Exec(`UPDATE candidates SET created_at = ? WHERE id = ?`,
		time.Now().Add(-72*time.Hour).Unix(), pending.ID); err != nil {
		t.Fatalf("backdate pending: %v", err)
	}

	// summary：模型自动写库、记忆仍未审核。S10b 之前没有生产者，只能夹具造。
	res, err := st.Add(store.AddInput{Content: "模型自动写库等着人复核的记忆", Source: "distill-accept", Project: "demo"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE memories SET review_state = '', reviewed_at = 0, reviewed_by = '' WHERE id = ?`,
		res.ID); err != nil {
		t.Fatalf("reset review_state: %v", err)
	}
	if _, err := st.DB().Exec(`INSERT INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at)
		VALUES ('cand-model','s','demo','fact','模型自动写库等着人复核的记忆','h-model','accepted','distill',?)`,
		time.Now().Add(-2*time.Hour).Unix()); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	if _, err := st.DB().Exec(`INSERT INTO commit_decisions
		(id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
		 memory_id, model_tag, created_memory, idempotency_key, created_at)
		VALUES ('dec-model','cand-model',0,'','auto_commit','model-auto','["low_confidence"]',?,'m@v1',1,'k-model',?)`,
		res.ID, time.Now().Add(-2*time.Hour).Unix()); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	return st, res.ID
}

// review ls 承接原 candidates ls 的候选投影：pending 段必须带 id/kind/status，
// 以及巡检 R8/R9/R15 依赖的 project/content/created_at/reviewable。
func TestReviewLsCoversCandidateProjection(t *testing.T) {
	st := newJudgeStore(t)
	cand := seedCandidate(t, st, "fact", "v2mem 的 MCP 通道与 hook 注入相互独立")

	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"ls", "--state", "all", "--json", "--db", st.Path()})
	})
	if err != nil {
		t.Fatalf("cmdReview ls --state all: %v\n%s", err, out)
	}
	var q store.ReviewQueue
	if err := json.Unmarshal([]byte(out), &q); err != nil {
		t.Fatalf("解析 review ls JSON: %v\n%s", err, out)
	}
	var got *store.ReviewItem
	for i := range q.States[store.ReviewQueuePending] {
		if q.States[store.ReviewQueuePending][i].ID == cand.ID {
			got = &q.States[store.ReviewQueuePending][i]
		}
	}
	if got == nil {
		t.Fatalf("pending 段应含候选 %s，got %+v", cand.ID, q.States[store.ReviewQueuePending])
	}
	if got.Project != cand.Project || got.Content != cand.Content {
		t.Errorf("候选投影缺 project/content：got %q/%q", got.Project, got.Content)
	}
	if got.CreatedAt == 0 {
		t.Error("候选投影缺 created_at（R15 TTL 依赖）")
	}
	if got.Reviewable == nil {
		t.Error("候选投影缺 reviewable（R8 可裁决口径依赖）")
	}
}

func TestReviewLsTextShowsFourStatesAndCounts(t *testing.T) {
	st, _ := seedReviewFixture(t)
	db := st.Path()

	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"ls", "--db", db})
	})
	if err != nil {
		t.Fatalf("cmdReview ls: %v\n%s", err, out)
	}
	for _, want := range []string{
		"复核队列", "pending", "summary", "quarantined", "failed",
		"pending 待判定：1", "summary 未审核摘要：1", "quarantined 隔离：1", "failed 失败：1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("文本输出应含 %q，got:\n%s", want, out)
		}
	}
	// 短 id（8 位）+ kind + scope + 年龄。
	if !strings.Contains(out, "[fact]") || !strings.Contains(out, "project") {
		t.Errorf("列表应带 kind 与 scope，got:\n%s", out)
	}
	if !strings.Contains(out, "3 天") && !strings.Contains(out, "72 小时") {
		t.Errorf("应显示年龄（按插入时间回填 72h），got:\n%s", out)
	}
	// 最新裁决 actor/action 与记忆 id 要能看到：复核「这条是谁写进来的」靠它。
	for _, want := range []string{"model-auto", "auto_commit"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary 段应显示最新裁决 %q，got:\n%s", want, out)
		}
	}
}

func TestReviewLsJSONShape(t *testing.T) {
	st, memID := seedReviewFixture(t)
	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"ls", "--db", st.Path(), "--json"})
	})
	if err != nil {
		t.Fatalf("cmdReview ls --json: %v\n%s", err, out)
	}
	var got struct {
		States map[string][]store.ReviewItem `json:"states"`
		Counts map[string]int                `json:"counts"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json 应可解析: %v\n%s", err, out)
	}
	for _, s := range store.ReviewQueueStates() {
		if _, ok := got.States[s]; !ok {
			t.Errorf("states 缺 %q 键（空态也必须出现，前端才能稳定渲染）", s)
		}
		if _, ok := got.Counts[s]; !ok {
			t.Errorf("counts 缺 %q 键", s)
		}
	}
	if len(got.States[store.ReviewQueuePending]) != 1 ||
		len(got.States[store.ReviewQueueFailed]) != 1 ||
		len(got.States[store.ReviewQueueQuarantined]) != 1 ||
		len(got.States[store.ReviewQueueSummary]) != 1 {
		t.Fatalf("四态各应 1 条，got %+v", got.States)
	}
	if got.Counts[store.ReviewQueueSummary] != 1 {
		t.Errorf("counts.summary = %d, want 1", got.Counts[store.ReviewQueueSummary])
	}
	if it := got.States[store.ReviewQueueSummary][0]; it.MemoryID != memID || it.Actor != "model-auto" {
		t.Errorf("summary 行应带记忆 id 与裁决 actor，got %+v", it)
	}
	if it := got.States[store.ReviewQueueFailed][0]; it.StatusReason != "executor_timeout" || it.Attempt != 1 {
		t.Errorf("failed 行应带 reason/attempt，got %+v", it)
	}
}

func TestReviewLsEmptyDBIsStable(t *testing.T) {
	db := testDB(t)
	if err := cmdReview([]string{"ls", "--db", db}); err != nil {
		t.Fatalf("空库不应报错: %v", err)
	}
	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"ls", "--db", db})
	})
	if err != nil {
		t.Fatalf("空库: %v", err)
	}
	for _, want := range []string{"pending 待判定：0", "summary 未审核摘要：0", "quarantined 隔离：0", "failed 失败：0"} {
		if !strings.Contains(out, want) {
			t.Errorf("空库输出应含 %q，got:\n%s", want, out)
		}
	}
	js, err := captureStdout(t, func() error {
		return cmdReview([]string{"ls", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("空库 --json: %v", err)
	}
	// 四个数组必须是 [] 而不是 null：零行也要稳定。
	for _, s := range store.ReviewQueueStates() {
		if !strings.Contains(js, `"`+s+`":[]`) {
			t.Errorf("空库 JSON 的 %q 应为 []，got:\n%s", s, js)
		}
	}
}

func TestReviewLsStateFilter(t *testing.T) {
	st, _ := seedReviewFixture(t)
	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"ls", "--db", st.Path(), "--state", "failed", "--json"})
	})
	if err != nil {
		t.Fatalf("--state failed: %v\n%s", err, out)
	}
	var got struct {
		States map[string][]store.ReviewItem `json:"states"`
		Counts map[string]int                `json:"counts"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(got.States[store.ReviewQueueFailed]) != 1 {
		t.Fatalf("failed 段应有 1 条，got %+v", got.States[store.ReviewQueueFailed])
	}
	for _, s := range []string{store.ReviewQueuePending, store.ReviewQueueSummary, store.ReviewQueueQuarantined} {
		if len(got.States[s]) != 0 {
			t.Errorf("--state failed 时 %q 段必须为空，got %+v", s, got.States[s])
		}
	}
	// 计数不受 --state 影响：一屏就能看到别处还有多少等人工项。
	if got.Counts[store.ReviewQueuePending] != 1 || got.Counts[store.ReviewQueueQuarantined] != 1 {
		t.Errorf("计数应保持全量，got %+v", got.Counts)
	}
	if err := cmdReview([]string{"ls", "--db", st.Path(), "--state", "bogus"}); err == nil {
		t.Fatal("未知 --state 必须报错")
	} else if !strings.Contains(err.Error(), "pending") || !strings.Contains(err.Error(), "failed") {
		t.Errorf("报错应列出可选值，got %v", err)
	}
}

func TestReviewLsLimit(t *testing.T) {
	st := newJudgeStore(t)
	for _, c := range []string{"第一条待判定", "第二条待判定", "第三条待判定"} {
		seedCandidate(t, st, "fact", c)
	}
	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"ls", "--db", st.Path(), "--limit", "2", "--json"})
	})
	if err != nil {
		t.Fatalf("--limit: %v", err)
	}
	var got struct {
		States map[string][]store.ReviewItem `json:"states"`
		Counts map[string]int                `json:"counts"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(got.States[store.ReviewQueuePending]) != 2 {
		t.Fatalf("--limit 2 应只列 2 条，got %d", len(got.States[store.ReviewQueuePending]))
	}
	if got.Counts[store.ReviewQueuePending] != 3 {
		t.Fatalf("计数不受 limit 截断，got %d", got.Counts[store.ReviewQueuePending])
	}
	if err := cmdReview([]string{"ls", "--db", st.Path(), "--limit", "0"}); err == nil {
		t.Fatal("--limit 0 应报错或回落默认值；当前行为必须显式")
	}
}

// 动作面（withdraw/restore/rollback）留 S10b：现在给出明确的下一步，而不是未知子命令。
func TestReviewRequiresLsSubcommand(t *testing.T) {
	err := cmdReview(nil)
	if err == nil {
		t.Fatal("mem review 无子命令应报错")
	}
	if !strings.Contains(err.Error(), "mem review ls") {
		t.Errorf("报错应给出可抄命令 mem review ls，got %v", err)
	}
	// S10b 起 withdraw/restore/rollback/stats 已落地，未知子命令仍要指向只读入口。
	err = cmdReview([]string{"bogus", "x"})
	if err == nil {
		t.Fatal("未知子命令应报错而不是静默成功")
	}
	if !strings.Contains(err.Error(), "mem review ls") {
		t.Errorf("未知子命令应指向只读入口，got %v", err)
	}
}

// 命令注册护栏：新子命令必须同时出现在 knownSubcommands 与 usage。
func TestReviewCommandIsRegistered(t *testing.T) {
	known := false
	for _, s := range knownSubcommands {
		if s == "review" {
			known = true
		}
	}
	if !known {
		t.Fatal("knownSubcommands 缺 review")
	}
	if !strings.Contains(usageText, "mem review ls") {
		t.Fatal("usage 文本缺 mem review ls")
	}
}

func TestReviewDBFlagDefaultsToHome(t *testing.T) {
	// 用空 HOME 隔离默认库路径：不给 --db 时必须落到 <HOME>/.v2mem/mem.db，而不是当前目录。
	t.Setenv("HOME", t.TempDir())
	if err := cmdReview([]string{"ls"}); err != nil {
		t.Fatalf("默认库路径: %v", err)
	}
}
