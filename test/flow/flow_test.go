package flow_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/eval"
	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/proposal"
	"github.com/v2camp/v2mem/internal/store"
)

// 逐跳验收夹具：capture → observations → archive → Bundle → 候选 → 裁决 → 记忆。
// 全部在临时库上跑真实写入路径，不触碰 ~/.v2mem；eval flow 自身只读。

const (
	flowWindow = 15 * 24 * time.Hour
	flowNow    = int64(1_700_000_000)
)

type flowEnv struct {
	st  *store.Store
	ev  *evidence.Engine
	now *int64
}

func newFlowEnv(t *testing.T) *flowEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	now := flowNow
	return &flowEnv{st: st, ev: evidence.New(st), now: &now}
}

func (e *flowEnv) capture(t *testing.T, source, uid, session, summary string, eligible bool) {
	t.Helper()
	_, created, err := e.st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: source, SourceKind: "hook", Generation: 1,
		OccurredAtNS: *e.now * int64(time.Second), SessionID: session,
		Project: "proj", Harness: "workbuddy", EventType: "stop",
		ParserVersion: "flow/v1", ProposalEligible: eligible,
		Summary: summary, ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
		RawRecordHash: "rh-" + uid, ByteSize: 16, RawCount: 1,
	})
	if err != nil {
		t.Fatalf("CaptureEvidence(%s): %v", uid, err)
	}
	if !created {
		t.Fatalf("CaptureEvidence(%s) 应首次写入", uid)
	}
}

func (e *flowEnv) runner(gen proposal.Generator) *proposal.Runner {
	return &proposal.Runner{
		St: e.st, Reader: e.ev,
		Chain: []proposal.Executor{proposal.NewExecutor(e.ev, gen)},
		Owner: "flow", LeaseTTL: time.Minute, RetryDelay: 10 * time.Second,
		BacklogDelay: time.Minute,
		Now:          func() time.Time { return time.Unix(*e.now, 0) },
	}
}

func (e *flowEnv) bundle(t *testing.T) *evidence.Bundle {
	t.Helper()
	b, err := e.ev.NextBundle(50, 1<<20)
	if err != nil || b == nil {
		t.Fatalf("NextBundle: b=%v err=%v", b, err)
	}
	return b
}

func goodGenA() proposal.Generator {
	return proposal.FuncGenerator{Label: "fake-a", Fn: func(context.Context, proposal.GenerateRequest) (string, error) {
		return `{"kind":"decision","content":"用 SQLite 存记忆","evidence_idx":[1]}
{"kind":"preference","content":"中文注释优先","evidence_idx":[2]}
{"kind":"pitfall","content":"WAL 必须开启","evidence_idx":[3]}`, nil
	}}
}

func buildFlow(t *testing.T) *flowEnv {
	t.Helper()
	e := newFlowEnv(t)
	ctx := context.Background()

	// 正常路径：3 条可提案证据 → Bundle ok → 3 条候选。
	e.capture(t, "srcA", "a1", "sA", "决定：用 SQLite 存记忆", true)
	e.capture(t, "srcA", "a2", "sA", "偏好：中文注释优先", true)
	e.capture(t, "srcA", "a3", "sA", "踩坑：WAL 必须开启", true)
	bA := e.bundle(t)
	out, err := e.runner(goodGenA()).StepBundle(ctx, bA.ID)
	if err != nil || out.BundleStatus != "ok" || out.Inserted != 3 {
		t.Fatalf("正常 Bundle: out=%+v err=%v", out, err)
	}

	// 0 提案水位：不可提案证据 → noop，且不调用模型。
	e.capture(t, "srcN", "n1", "sN", "子会话事件", false)
	bN := e.bundle(t)
	out, err = e.runner(proposal.FuncGenerator{Label: "must-not-run",
		Fn: func(context.Context, proposal.GenerateRequest) (string, error) {
			t.Fatal("noop 窗口不应调用模型")
			return "", nil
		}}).StepBundle(ctx, bN.ID)
	if err != nil || out.BundleStatus != "noop" {
		t.Fatalf("noop Bundle: out=%+v err=%v", out, err)
	}

	// 失败不算 noop：坏输出 → fail，退避后重试成功（attempt=2）。
	e.capture(t, "srcF", "f1", "sF", "决定：失败必须重试", true)
	bF := e.bundle(t)
	out, err = e.runner(proposal.FuncGenerator{Label: "bad",
		Fn: func(context.Context, proposal.GenerateRequest) (string, error) {
			return "不是 JSON", nil
		}}).StepBundle(ctx, bF.ID)
	if err != nil {
		t.Fatalf("坏输出 StepBundle: %v", err)
	}
	if out.BundleStatus != "fail" || out.Attempt != 1 {
		t.Fatalf("坏输出应 fail attempt=1: %+v", out)
	}
	*e.now += 120
	out, err = e.runner(proposal.FuncGenerator{Label: "fake-f",
		Fn: func(context.Context, proposal.GenerateRequest) (string, error) {
			return `{"kind":"decision","content":"失败必须重试","evidence_idx":[1]}`, nil
		}}).StepBundle(ctx, bF.ID)
	if err != nil || out.BundleStatus != "ok" || out.Attempt != 2 {
		t.Fatalf("重试应 ok attempt=2: out=%+v err=%v", out, err)
	}

	// 裁决：3 accept + 1 reject，走真实 commit 路径写记忆与证据链。
	rows, err := e.st.DB().Query(`SELECT id, revision FROM candidates
		WHERE status='pending' ORDER BY content_hash ASC`)
	if err != nil {
		t.Fatalf("查 pending: %v", err)
	}
	type pending struct {
		id  string
		rev int
	}
	var ps []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.rev); err != nil {
			t.Fatalf("scan pending: %v", err)
		}
		ps = append(ps, p)
	}
	rows.Close()
	if len(ps) != 4 {
		t.Fatalf("pending 候选 = %d, want 4", len(ps))
	}
	for i, p := range ps {
		if i == len(ps)-1 {
			if _, err := e.st.CommitReject(p.id, p.rev, "human", ""); err != nil {
				t.Fatalf("reject %s: %v", p.id, err)
			}
			continue
		}
		if _, err := e.st.CommitAccept(p.id, p.rev, "human"); err != nil {
			t.Fatalf("accept %s: %v", p.id, err)
		}
	}
	return e
}

// E2：逐跳正常路径 → 13 项全 PASS。
func TestFlowFixtureAllGreen(t *testing.T) {
	// G7 的隔离阈值与执行器同源；eval 不 import proposal，就在这里钉住同步关系。
	if proposal.MaxAttempts != 5 {
		t.Fatalf("proposal.MaxAttempts = %d，需同步 eval flow 的 flowMaxAttempts", proposal.MaxAttempts)
	}
	e := buildFlow(t)
	snap, err := eval.LoadFlowSnapshot(e.st.DB(), *e.now)
	if err != nil {
		t.Fatalf("LoadFlowSnapshot: %v", err)
	}
	rep := eval.CheckFlow(snap, flowWindow)
	if len(rep.Checks) != 13 || rep.Passed != 13 || rep.Failed != 0 {
		for _, c := range rep.Checks {
			t.Logf("%s pass=%v %s", c.ID, c.Pass, c.Detail)
		}
		t.Fatalf("passed/failed = %d/%d, want 13/0", rep.Passed, rep.Failed)
	}
	for i, c := range rep.Checks {
		want := "G" + itoa(i+1)
		if c.ID != want {
			t.Fatalf("第 %d 项 ID = %s, want %s", i+1, c.ID, want)
		}
	}
	// 4 次人工裁决、0 次自动裁决 → 人工介入率 100%（P1 人工验收阶段的口径）。
	if rep.ManualRate != 1 {
		t.Fatalf("ManualRate = %v, want 1", rep.ManualRate)
	}
}

// E3：人工植入单点破坏 → 恰好对应 G 项 FAIL，退出码（cmd 层）为 1。
func TestFlowSinglePointBreakage(t *testing.T) {
	cases := []struct {
		name   string
		inject func(t *testing.T, e *flowEnv)
		want   string
	}{
		{"observation_without_archive", func(t *testing.T, e *flowEnv) {
			// 新增一条没有归档账的观测：G2 应红，G3 的既有归档计数不受影响。
			if _, err := e.st.DB().Exec(`INSERT INTO observations(id, session_id, event, content_hash, created_at,
				source_id, source_generation, source_seq, event_uid, archive_hash)
				VALUES('inj-o','sA','stop','inj-h',?,'srcA',1,4,'inj-u','')`, *e.now); err != nil {
				t.Fatalf("注入观测: %v", err)
			}
		}, "G2"},
		{"fail_bundle_without_retry", func(t *testing.T, e *flowEnv) {
			// 一个 attempt=0、无 run 账的 fail 窗口：证明 fail 被当成终态静默跳过。
			if _, err := e.st.DB().Exec(`INSERT INTO evidence_bundles(id, source_id, generation,
				from_seq_exclusive, to_seq_inclusive, evidence_hash, status, attempt, last_run_id, created_at, updated_at)
				VALUES('inj-b','srcN',1,1,1,'inj-eh','fail',0,'',?,?)`, *e.now, *e.now); err != nil {
				t.Fatalf("注入 Bundle: %v", err)
			}
		}, "G7"},
		{"expired_candidate_replaces_consumption", func(t *testing.T, e *flowEnv) {
			// 过期候选没有 defer/conflict/reject 裁决：expired 顶替了正常消费。
			if _, err := e.st.DB().Exec(`INSERT INTO candidates(id, session_id, project, kind, content,
				content_hash, status, source, created_at, bundle_id, revision)
				VALUES('inj-c','s-inj','proj','fact','注入','inj-ch','expired','import-proposal',?,'',0)`, *e.now); err != nil {
				t.Fatalf("注入候选: %v", err)
			}
		}, "G9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := buildFlow(t)
			tc.inject(t, e)
			snap, err := eval.LoadFlowSnapshot(e.st.DB(), *e.now)
			if err != nil {
				t.Fatalf("LoadFlowSnapshot: %v", err)
			}
			rep := eval.CheckFlow(snap, flowWindow)
			if rep.Failed != 1 {
				for _, c := range rep.Checks {
					t.Logf("%s pass=%v %s", c.ID, c.Pass, c.Detail)
				}
				t.Fatalf("Failed = %d, want 1", rep.Failed)
			}
			for _, c := range rep.Checks {
				if c.ID == tc.want {
					if c.Pass {
						t.Fatalf("%s 应为唯一失败项，实际通过: %s", tc.want, c.Detail)
					}
					continue
				}
				if !c.Pass {
					t.Fatalf("%s 不应受 %s 影响: %s", c.ID, tc.want, c.Detail)
				}
			}
		})
	}
}

// E4：加载与判定全程只读（业务表行数不变）。
func TestFlowCheckReadOnly(t *testing.T) {
	e := buildFlow(t)
	tables := []string{"observations", "evidence_archives", "evidence_bundles",
		"candidates", "commit_decisions", "memory_evidence", "memories"}
	count := func() map[string]int {
		out := map[string]int{}
		for _, table := range tables {
			var n int
			if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			out[table] = n
		}
		return out
	}
	before := count()
	snap, err := eval.LoadFlowSnapshot(e.st.DB(), *e.now)
	if err != nil {
		t.Fatalf("LoadFlowSnapshot: %v", err)
	}
	_ = eval.CheckFlow(snap, flowWindow)
	after := count()
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s 行数变化: %d → %d", table, n, after[table])
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
