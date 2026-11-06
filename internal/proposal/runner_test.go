package proposal

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
)

type env struct {
	st *store.Store
	ev *evidence.Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &env{st: st, ev: evidence.New(st)}
}

func (e *env) capture(t *testing.T, uid, session, summary string, eligible bool) {
	t.Helper()
	_, created, err := e.st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-t", SourceKind: "hook", Generation: 1,
		OccurredAtNS: 1700000000000000000, SessionID: session, Project: "proj",
		Harness: "workbuddy", EventType: "stop", ParserVersion: "evidence/v1",
		ProposalEligible: eligible, Summary: summary,
		ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
		RawRecordHash: "rh-" + uid, ByteSize: 16, RawCount: 1,
	})
	if err != nil {
		t.Fatalf("CaptureEvidence(%s): %v", uid, err)
	}
	if !created {
		t.Fatalf("CaptureEvidence(%s) 应首次写入", uid)
	}
}

func (e *env) bundle(t *testing.T) *evidence.Bundle {
	t.Helper()
	b, err := e.ev.NextBundle(50, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("应拿到 Bundle")
	}
	return b
}

func fixedNow(v *int64) func() time.Time {
	return func() time.Time { return time.Unix(*v, 0) }
}

func newRunner(e *env, chain []Executor, now *int64) *Runner {
	return &Runner{
		St: e.st, Reader: e.ev, Chain: chain, Owner: "test",
		LeaseTTL: time.Minute, RetryDelay: 10 * time.Second,
		BacklogDelay: time.Minute, Now: fixedNow(now),
	}
}

func gen(label string, fn func(req GenerateRequest, call int) (string, error)) (Generator, *int) {
	calls := 0
	return FuncGenerator{Label: label, Fn: func(_ context.Context, req GenerateRequest) (string, error) {
		calls++
		return fn(req, calls)
	}}, &calls
}

func TestExecutorRepairsOnceAndClassifies(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：用 SQLite", true)
	b := e.bundle(t)

	valid := `{"kind":"decision","scope":"project","content":"用 SQLite","evidence_idx":[1]}`
	g, calls := gen("fake", func(_ GenerateRequest, call int) (string, error) {
		if call == 1 {
			return "抱歉，我无法输出 JSON", nil
		}
		return valid, nil
	})
	res, err := NewExecutor(e.ev, g).Run(b.ID)
	if err != nil {
		t.Fatalf("修复后应成功: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("坏 JSON 只允许一次修复，calls=%d", *calls)
	}
	if res.Status != StatusOK || res.Valid != 1 || res.Executor != "fake" {
		t.Fatalf("结果不对: %+v", res)
	}

	// 第二次仍坏：判 invalid，不能悄悄当成 empty。
	g2, calls2 := gen("fake", func(GenerateRequest, int) (string, error) { return "{", nil })
	res, err = NewExecutor(e.ev, g2).Run(b.ID)
	if !errors.Is(err, ErrBadOutput) {
		t.Fatalf("应报坏输出: %v", err)
	}
	if res.Status != StatusInvalid || *calls2 != 2 {
		t.Fatalf("坏 JSON 应为 invalid: %+v calls=%d", res, *calls2)
	}
}

func TestExecutorNoEligibleIsNoop(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "子会话事件", false)
	b := e.bundle(t)
	g, calls := gen("fake", func(GenerateRequest, int) (string, error) {
		t.Error("不可提案窗口不应调用模型")
		return "", nil
	})
	res, err := NewExecutor(e.ev, g).Run(b.ID)
	if err != nil || res.Status != StatusNoop || *calls != 0 {
		t.Fatalf("应为 noop 且不调模型: %+v err=%v calls=%d", res, err, *calls)
	}
}

func TestExecutorRejectsOutOfBundleEvidence(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	b := e.bundle(t)
	g, _ := gen("fake", func(GenerateRequest, int) (string, error) {
		return `{"kind":"decision","content":"甲","evidence_idx":[9]}`, nil
	})
	res, err := NewExecutor(e.ev, g).Run(b.ID)
	if err == nil || res.Status != StatusInvalid || res.Invalid != 1 {
		t.Fatalf("越界引用应整条判无效: %+v err=%v", res, err)
	}
}

func TestRunnerPersistsCandidatesAndEvidence(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	e.capture(t, "u2", "s1", "踩坑：乙", true)
	b := e.bundle(t)
	var now int64 = 1000

	g, _ := gen("fake", func(GenerateRequest, int) (string, error) {
		return "{\"kind\":\"decision\",\"content\":\"决定：甲\",\"evidence_idx\":[1]}\n" +
			"{\"kind\":\"pitfall\",\"content\":\"踩坑：乙\",\"evidence_idx\":[2]}", nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, g)}, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusOK || out.BundleStatus != "ok" || out.Inserted != 2 || out.Generated != 2 {
		t.Fatalf("运行结果不对: %+v", out)
	}
	list, err := e.st.ListCandidates("pending", "s1", 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("应落 2 条 pending: %+v err=%v", list, err)
	}
	for _, c := range list {
		rows, err := e.st.ListCandidateEvidence(c.ID)
		if err != nil || len(rows) != 1 {
			t.Fatalf("候选 %s 应有 1 条证据: %+v err=%v", c.ID, rows, err)
		}
		found := false
		for _, id := range b.EvidenceIDs {
			if rows[0].EvidenceID == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("证据 %s 不属于本 Bundle: %+v", rows[0].EvidenceID, b.EvidenceIDs)
		}
		if rows[0].EventUID == "" || rows[0].SourceID != "src-t" || rows[0].SourceSeq == 0 {
			t.Fatalf("证据身份不完整: %+v", rows[0])
		}
	}
	runs, err := e.st.ProposalRuns(b.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("应有一条运行账: %+v err=%v", runs, err)
	}
	if runs[0].Status != "ok" || runs[0].GeneratedCount != 2 || runs[0].Attempt != 1 ||
		runs[0].SourceFingerprint == "" {
		t.Fatalf("运行账字段不对: %+v", runs[0])
	}
	got, err := e.st.LoadProposalBundle(b.ID)
	if err != nil || got.Status != "ok" || got.LeaseOwner != "" {
		t.Fatalf("Bundle 应已消费: %+v err=%v", got, err)
	}
	// 没有新证据：下一次是 skipped，不是空跑。
	next, err := r.Step(context.Background())
	if err != nil || next.Status != "skipped" {
		t.Fatalf("应无窗口可跑: %+v err=%v", next, err)
	}
}

func TestRunnerDuplicateMergesEvidenceWithoutNewRow(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：同一条", true)
	b1 := e.bundle(t)
	var now int64 = 1000
	g, _ := gen("fake", func(GenerateRequest, int) (string, error) {
		return `{"kind":"decision","content":"决定：同一条","evidence_idx":[1]}`, nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, g)}, &now)
	if out, err := r.Step(context.Background()); err != nil || out.Inserted != 1 {
		t.Fatalf("第一窗: %+v err=%v", out, err)
	}

	e.capture(t, "u2", "s1", "决定：同一条（新证据）", true)
	_ = e.bundle(t)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusOK || out.Inserted != 0 || out.Duplicate != 1 || out.Merged != 1 {
		t.Fatalf("重复候选应合并证据而非新增: %+v", out)
	}
	list, err := e.st.ListCandidates("pending", "s1", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("候选行不应增加: %+v err=%v", list, err)
	}
	rows, err := e.st.ListCandidateEvidence(list[0].ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("证据应合并为 2 条: %+v err=%v", rows, err)
	}
	_ = b1
}

func TestRunnerPartialDoesNotConsumeWindow(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	e.capture(t, "u2", "s1", "决定：乙", true)
	b := e.bundle(t)
	var now int64 = 1000
	g, _ := gen("fake", func(GenerateRequest, int) (string, error) {
		return "{\"kind\":\"decision\",\"content\":\"甲\",\"evidence_idx\":[1]}\n" +
			"{\"kind\":\"nope\",\"content\":\"乙\",\"evidence_idx\":[2]}", nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, g)}, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusPartial || out.BundleStatus != "partial" || out.Inserted != 1 || out.Invalid != 1 {
		t.Fatalf("应为 partial 且保留有效候选: %+v", out)
	}
	if out.NotBefore <= 1000 {
		t.Fatalf("partial 应设置 not_before: %d", out.NotBefore)
	}
	head, ok, err := e.st.NextOpenProposalBundle()
	if err != nil || !ok || head.ID != b.ID {
		t.Fatalf("partial 不得推进连续水位: %+v ok=%v err=%v", head, ok, err)
	}
	// 退避期内：不再跑，也不取下一窗。
	blocked, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Status != StatusBacklog || blocked.Blocked == "" {
		t.Fatalf("退避期内应 blocked: %+v", blocked)
	}
	// 退避结束后重试同一窗；重复候选合并而不是新增。
	now = out.NotBefore + 1
	again, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again.Attempt != 2 || again.Duplicate != 1 {
		t.Fatalf("重试应复用同一窗并合并重复: %+v", again)
	}
	list, err := e.st.ListCandidates("pending", "s1", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("不应因重试多出候选: %+v err=%v", list, err)
	}
}

func TestRunnerInvalidAndFailKeepWindow(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	b := e.bundle(t)
	var now int64 = 1000

	bad, calls := gen("fake", func(GenerateRequest, int) (string, error) { return "not json", nil })
	r := newRunner(e, []Executor{NewExecutor(e.ev, bad)}, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusInvalid || out.BundleStatus != "fail" || *calls != 2 {
		t.Fatalf("坏 JSON 应为 invalid/fail: %+v calls=%d", out, *calls)
	}
	if list, _ := e.st.ListCandidates("", "s1", 10); len(list) != 0 {
		t.Fatalf("invalid 不应落候选: %+v", list)
	}
	if head, ok, _ := e.st.NextOpenProposalBundle(); !ok || head.ID != b.ID {
		t.Fatalf("invalid 不得消费窗口: %+v ok=%v", head, ok)
	}
	runs, _ := e.st.ProposalRuns(b.ID)
	if len(runs) != 1 || runs[0].Status != StatusInvalid {
		t.Fatalf("运行账应记 invalid: %+v", runs)
	}

	// 非 unavailable 的 provider 错误 → fail，同样不消费。
	now = out.NotBefore + 1
	boom, _ := gen("broken", func(GenerateRequest, int) (string, error) { return "", errors.New("boom") })
	r2 := newRunner(e, []Executor{NewExecutor(e.ev, boom)}, &now)
	out, err = r2.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusFail || out.BundleStatus != "fail" || !strings.Contains(out.Error, "boom") {
		t.Fatalf("provider 失败应为 fail: %+v", out)
	}
}

func TestRunnerNoProviderIsBacklogNotNoop(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	b := e.bundle(t)
	var now int64 = 1000
	r := newRunner(e, nil, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusBacklog || out.BundleStatus != "backlog" {
		t.Fatalf("无 provider 必须是 backlog，不能伪装成 empty: %+v", out)
	}
	if out.Status == StatusNoop {
		t.Fatal("backlog 被写成了 noop")
	}
	if out.NotBefore <= 1000 {
		t.Fatalf("backlog 应设置 not_before: %d", out.NotBefore)
	}
	if list, _ := e.st.ListCandidates("", "s1", 10); len(list) != 0 {
		t.Fatalf("backlog 不应落候选: %+v", list)
	}
	head, ok, _ := e.st.NextOpenProposalBundle()
	if !ok || head.ID != b.ID {
		t.Fatalf("backlog 不得消费窗口: %+v", head)
	}
}

func TestRunnerHostUnavailableFallsBackToLocal(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	e.bundle(t)
	var now int64 = 1000
	host, hostCalls := gen("host", func(GenerateRequest, int) (string, error) {
		return "", ErrHostUnavailable
	})
	local, localCalls := gen("local", func(GenerateRequest, int) (string, error) {
		return `{"kind":"decision","content":"甲","evidence_idx":[1]}`, nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, host), NewExecutor(e.ev, local)}, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusOK || out.Executor != "local" || *hostCalls != 1 || *localCalls != 1 {
		t.Fatalf("Host 不可用应降级 Local: %+v host=%d local=%d", out, *hostCalls, *localCalls)
	}
	if out.Inserted != 1 {
		t.Fatalf("Local 应产出候选: %+v", out)
	}
}

func TestRunnerDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	b := e.bundle(t)
	var now int64 = 1000
	g, _ := gen("fake", func(GenerateRequest, int) (string, error) {
		return `{"kind":"decision","content":"甲","evidence_idx":[1]}`, nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, g)}, &now)
	out, err := r.Peek(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusWouldGenerate || out.Inserted != 1 || out.Valid != 1 {
		t.Fatalf("dry-run 应只报告 would_generate: %+v", out)
	}
	got, _ := e.st.LoadProposalBundle(b.ID)
	if got.Status != "pending" || got.LeaseOwner != "" || got.Attempt != 0 {
		t.Fatalf("dry-run 不得改 Bundle 状态: %+v", got)
	}
	if list, _ := e.st.ListCandidates("", "s1", 10); len(list) != 0 {
		t.Fatalf("dry-run 不得落候选: %+v", list)
	}
	if runs, _ := e.st.ProposalRuns(b.ID); len(runs) != 0 {
		t.Fatalf("dry-run 不得写运行账: %+v", runs)
	}
}

func TestRunnerLeaseBlocksSecondWorker(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	b := e.bundle(t)
	if _, err := e.st.ClaimProposalBundle(b.ID, "other", 1000, time.Minute); err != nil {
		t.Fatal(err)
	}
	var now int64 = 1000
	g, _ := gen("fake", func(GenerateRequest, int) (string, error) {
		t.Error("租约被占用时不应执行")
		return "", nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, g)}, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusBacklog || out.Blocked == "" {
		t.Fatalf("应被租约挡住: %+v", out)
	}
}

func TestRunnerPersistFailureBecomesPartial(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	e.capture(t, "u2", "s1", "决定：乙", true)
	b := e.bundle(t)
	execSQL(t, e.st.Path(), `CREATE TRIGGER fail_cand BEFORE INSERT ON candidates
		WHEN NEW.content LIKE '%坏%' BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	var now int64 = 1000
	g, _ := gen("fake", func(GenerateRequest, int) (string, error) {
		return "{\"kind\":\"decision\",\"content\":\"坏的一条\",\"evidence_idx\":[1]}\n" +
			"{\"kind\":\"decision\",\"content\":\"好的一条\",\"evidence_idx\":[2]}", nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, g)}, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusPartial || out.BundleStatus != "partial" || out.Inserted != 1 {
		t.Fatalf("一条落库失败应为 partial: %+v", out)
	}
	if out.Error == "" {
		t.Fatal("partial 必须带上失败原因")
	}
	if list, _ := e.st.ListCandidates("pending", "s1", 10); len(list) != 1 {
		t.Fatalf("成功的候选必须留下: %+v", list)
	}
	if head, ok, _ := e.st.NextOpenProposalBundle(); !ok || head.ID != b.ID {
		t.Fatalf("partial 不得消费窗口: %+v", head)
	}
}

func TestRunnerPeekRejectsConsumedBundle(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	b := e.bundle(t)
	var now int64 = 1000
	g, _ := gen("fake", func(GenerateRequest, int) (string, error) {
		return `{"kind":"decision","content":"甲","evidence_idx":[1]}`, nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, g)}, &now)
	if _, err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := r.Peek(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusNoop || out.Blocked == "" {
		t.Fatalf("已消费窗口 dry-run 应直接 noop: %+v", out)
	}
}

func TestRunnerRequiresStoreAndReader(t *testing.T) {
	if _, err := (&Runner{}).Step(context.Background()); err == nil {
		t.Fatal("缺 Store/Reader 应报错")
	}
	if _, err := (&Runner{}).Peek(context.Background(), "x"); err == nil {
		t.Fatal("缺 Store/Reader 应报错")
	}
	if _, err := (&Runner{}).StepBundle(context.Background(), "x"); err == nil {
		t.Fatal("缺 Store/Reader 应报错")
	}
}

func TestRunnerClosedStoreReturnsError(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	_ = e.bundle(t)
	var now int64 = 1000
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(e, nil, &now)
	if _, err := r.Step(context.Background()); err == nil {
		t.Fatal("库关闭后应返回错误")
	}
}

func execSQL(t *testing.T, path, stmt string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerStepBundleExplicit(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	b := e.bundle(t)
	var now int64 = 1000
	g, _ := gen("fake", func(GenerateRequest, int) (string, error) {
		return `{"kind":"decision","content":"甲","evidence_idx":[1]}`, nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, g)}, &now)
	out, err := r.StepBundle(context.Background(), b.ID)
	if err != nil || out.Status != StatusOK || out.Inserted != 1 {
		t.Fatalf("显式 bundle 应可跑: %+v err=%v", out, err)
	}
	if _, err := r.StepBundle(context.Background(), "bnd-missing"); err == nil {
		t.Fatal("不存在的 bundle 应报错")
	}
}

func TestRunnerEvidenceMismatchFails(t *testing.T) {
	e := newEnv(t)
	e.capture(t, "u1", "s1", "决定：甲", true)
	e.capture(t, "u2", "s1", "决定：乙", true)
	e.bundle(t)
	execSQL(t, e.st.Path(), `DELETE FROM observations WHERE event_uid='u1'`)
	var now int64 = 1000
	r := newRunner(e, nil, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusFail || out.BundleStatus != "fail" || !strings.Contains(out.Error, "不一致") {
		t.Fatalf("条目数不一致应 fail: %+v", out)
	}
}

func TestRunnerDefaultsAndClassify(t *testing.T) {
	r := &Runner{}
	if !r.now().IsZero() == false {
		t.Fatal("默认 now 应为当前时间")
	}
	if r.retryDelay(0) != time.Minute {
		t.Fatalf("默认退避应为 1m，got %v", r.retryDelay(0))
	}
	if r.retryDelay(99) != 8*time.Minute {
		t.Fatalf("退避应封顶 8 倍，got %v", r.retryDelay(99))
	}
	if r.backlogDelay() != 10*time.Minute {
		t.Fatalf("默认 backlog 退避应为 10m，got %v", r.backlogDelay())
	}
	if r.owner() != "proposal" {
		t.Fatalf("默认 owner 不对: %s", r.owner())
	}
	cases := []struct {
		exec     string
		inserted int
		errs     int
		status   string
		bundle   string
	}{
		{StatusNoop, 0, 0, StatusNoop, "noop"},
		{StatusOK, 1, 0, StatusOK, "ok"},
		{StatusOK, 0, 1, StatusFail, "fail"},
		{StatusOK, 1, 1, StatusPartial, "partial"},
		{StatusPartial, 1, 0, StatusPartial, "partial"},
		{StatusPartial, 0, 1, StatusFail, "fail"},
		{StatusInvalid, 0, 0, StatusInvalid, "fail"},
		{StatusBacklog, 0, 0, StatusBacklog, "backlog"},
		{"weird", 0, 0, StatusFail, "fail"},
	}
	for _, tc := range cases {
		status, bundle := classifyOutcome(tc.exec, tc.inserted, tc.errs)
		if status != tc.status || bundle != tc.bundle {
			t.Errorf("%s: got %s/%s want %s/%s", tc.exec, status, bundle, tc.status, tc.bundle)
		}
	}
}

func TestPeekUnknownBundle(t *testing.T) {
	e := newEnv(t)
	var now int64 = 1000
	r := newRunner(e, nil, &now)
	if _, err := r.Peek(context.Background(), "bnd-missing"); err == nil {
		t.Fatal("未知 bundle 应报错")
	}
}

func TestGeneratorsAndExecutorNames(t *testing.T) {
	if _, err := PromptGenerator("x", nil).Generate(context.Background(), GenerateRequest{}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("nil 函数应报 no provider: %v", err)
	}
	if _, err := UnavailableGenerator("h", ErrHostUnavailable).Generate(context.Background(), GenerateRequest{}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("不可用 generator 应可降级: %v", err)
	}
	if _, err := (FuncGenerator{Label: "x"}).Generate(context.Background(), GenerateRequest{}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("未实现 generator 应报 no provider: %v", err)
	}
	// 自定义 Executor 没有 name 方法 → unknown，不应 panic。
	if names := ExecutorNames([]Executor{fakeExecutor{}}); len(names) != 1 || names[0] != "unknown" {
		t.Fatalf("未知 executor 名字不对: %+v", names)
	}
	// reader/gen 缺失时 Run 直接报 no provider。
	if _, err := NewExecutor(nil, FuncGenerator{Label: "x"}).Run("b"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("缺 reader 应报 no provider: %v", err)
	}
	if _, err := NewExecutor(FakeReader{}, nil).(*modelExecutor).Run("b"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("缺 generator 应报 no provider: %v", err)
	}
}

type fakeExecutor struct{}

func (fakeExecutor) Run(string) (Result, error) { return Result{}, nil }

type FakeReader struct{}

func (FakeReader) NextBundle(int, int64) (*evidence.Bundle, error) { return nil, nil }
func (FakeReader) ListEvidence(string) ([]evidence.Ref, error)     { return nil, nil }

// refsReader 让 executor 层测试能注入带 Fidelity 的证据引用。
// 真实 Engine 的 toRef 在本分支不填 Fidelity（W0 冻结、T2 尚未合入），
// 故用假 reader 直接覆盖准入矩阵，不依赖 store。
type refsReader struct{ refs []evidence.Ref }

func (r refsReader) NextBundle(int, int64) (*evidence.Bundle, error) { return nil, nil }
func (r refsReader) ListEvidence(string) ([]evidence.Ref, error)     { return r.refs, nil }

func TestExecutorNoEligibleIsNoopByFidelity(t *testing.T) {
	// 总开关为 true，但 Fidelity 全为 pointer/none → 全不可提案 → noop 且不调模型。
	refs := []evidence.Ref{
		{ProposalEligible: true, Fidelity: evidence.FidelityPointer, Summary: "p", EventType: "stop", SessionID: "s"},
		{ProposalEligible: true, Fidelity: evidence.FidelityNone, Summary: "n", EventType: "stop", SessionID: "s"},
	}
	g, calls := gen("fake", func(GenerateRequest, int) (string, error) {
		t.Error("全不可提案窗口不应调用模型")
		return "", nil
	})
	res, err := NewExecutor(refsReader{refs}, g).Run("b")
	if err != nil || res.Status != StatusNoop || *calls != 0 {
		t.Fatalf("Fidelity 全不可提案应 noop 且不调模型: %+v err=%v calls=%d", res, err, *calls)
	}
}

func TestExecutorEmptyFidelityCompatible(t *testing.T) {
	// 空 Fidelity（旧行/未声明）按 full 兼容：仍可提案、正常生成。
	refs := []evidence.Ref{
		{ProposalEligible: true, Summary: "旧证据", EventType: "stop", SessionID: "s"},
	}
	g, calls := gen("fake", func(GenerateRequest, int) (string, error) {
		return `{"kind":"decision","content":"旧证据","evidence_idx":[1]}`, nil
	})
	res, err := NewExecutor(refsReader{refs}, g).Run("b")
	if err != nil || res.Status != StatusOK || res.Valid != 1 || *calls != 1 {
		t.Fatalf("空 Fidelity 应兼容并可提案: %+v err=%v calls=%d", res, err, *calls)
	}
}

func TestExecutorFidelityFullSummaryProceed(t *testing.T) {
	for _, fid := range []evidence.Fidelity{evidence.FidelityFull, evidence.FidelitySummary} {
		refs := []evidence.Ref{
			{ProposalEligible: true, Fidelity: fid, Summary: "s", EventType: "stop", SessionID: "s"},
		}
		g, calls := gen("fake", func(GenerateRequest, int) (string, error) {
			return `{"kind":"decision","content":"x","evidence_idx":[1]}`, nil
		})
		res, err := NewExecutor(refsReader{refs}, g).Run("b")
		if err != nil || res.Status != StatusOK || *calls != 1 {
			t.Fatalf("Fidelity=%s 应可提案: %+v err=%v calls=%d", fid, res, err, *calls)
		}
	}
}
