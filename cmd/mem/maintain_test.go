package main

// S8：mem maintain 门面的用例（design-data-lifecycle §3、§10 P2）。
//
// 口径：最老优先连续推进、过期租约接管、连跑幂等、dry-run 只读、
// --force 只清退避（不动 attempt）、出现隔离即停、输出字段与 maintain:last_ok 打点。
// 模型来源一律用 stub 替换（maintainLocalGenerator），既不在单测里起 ollama，
// 也不把「本机恰好装了模型」变成用例结果的一部分。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/proposal"
	"github.com/v2camp/v2mem/internal/store"
)

// stubMaintainLocal 把 maintain 的本地模型来源换成固定 generator。
func stubMaintainLocal(t *testing.T, gen proposal.Generator) {
	t.Helper()
	old := maintainLocalGenerator
	maintainLocalGenerator = func(distill.ModelConfig, string, time.Duration) proposal.Generator {
		return gen
	}
	t.Cleanup(func() { maintainLocalGenerator = old })
}

// stubRulesFallback 模拟「本机没有可用模型」：链路降级到规则兜底。
func stubRulesFallback(t *testing.T) {
	t.Helper()
	stubMaintainLocal(t, proposal.UnavailableGenerator("local", errors.New("test: 无可用模型")))
}

// seedMaintainBundle 造一条证据并立即切出一个单条窗口，返回 Bundle id。
// 逐条切窗是为了得到多个可独立调度的 Bundle（一次 NextBundle 会把能装的全装走）。
func seedMaintainBundle(t *testing.T, db, uid string) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-mnt", SourceKind: "hook", Generation: 1,
		OccurredAtNS: 1700000000000000000, SessionID: "sess-mnt", Project: "demo",
		Harness: "workbuddy", EventType: "user-prompt", ParserVersion: "evidence/v1",
		ProposalEligible: true, Summary: "决策：维护窗口 " + uid,
		ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
		RawRecordHash: "rh-" + uid, ByteSize: 16, RawCount: 1,
	}); err != nil {
		t.Fatalf("capture %s: %v", uid, err)
	}
	b, err := evidence.New(st).NextBundle(50, 1<<20)
	if err != nil {
		t.Fatalf("NextBundle(%s): %v", uid, err)
	}
	if b == nil {
		t.Fatalf("应切出 Bundle（%s）", uid)
	}
	return b.ID
}

// updateBundle 直接改调度字段：退避/租约/attempt 这些状态靠真实时钟造不精确。
func updateBundle(t *testing.T, db, id, sets string, args ...any) {
	t.Helper()
	if _, err := rawDB(t, db).Exec(`UPDATE evidence_bundles SET `+sets+` WHERE id = ?`,
		append(args, id)...); err != nil {
		t.Fatalf("更新 Bundle %s: %v", id, err)
	}
}

func loadBundle(t *testing.T, db, id string) *store.ProposalBundle {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, err := st.LoadProposalBundle(id)
	if err != nil {
		t.Fatalf("LoadProposalBundle(%s): %v", id, err)
	}
	return b
}

func countBundles(t *testing.T, db string) int {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, err := st.ListProposalBundles("", 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func countCandidates(t *testing.T, db string) int {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, err := st.ListCandidates("", "sess-mnt", 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

// M1：catch-up 按最老优先连续推进，预算耗尽即停，列表计数与 remaining 正确。
func TestMaintainCatchUpOldestFirstWithinBudget(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	ids := []string{
		seedMaintainBundle(t, db, "mnt-a"),
		seedMaintainBundle(t, db, "mnt-b"),
		seedMaintainBundle(t, db, "mnt-c"),
	}
	base := time.Now().Unix() - 3600
	for i, id := range ids {
		// created_at 拉开 60s：调度顺序必须由年龄决定，不是由 id/插入顺序。
		updateBundle(t, db, id, "created_at = ?", base+int64(i*60))
	}

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--catch-up", "--max-bundles", "2", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMaintain: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if num(got, "processed") != 2 || num(got, "ok") != 2 || num(got, "remaining") != 1 {
		t.Fatalf("预算 2 应处理最老两窗并剩 1：%v\n%s", got, out)
	}
	if b := loadBundle(t, db, ids[0]); b.Status != "ok" {
		t.Fatalf("最老窗口应先完成: %+v", b)
	}
	if b := loadBundle(t, db, ids[1]); b.Status != "ok" {
		t.Fatalf("第二老窗口应在预算内完成: %+v", b)
	}
	if b := loadBundle(t, db, ids[2]); b.Status != "pending" {
		t.Fatalf("预算耗尽后最新窗口必须原样保留: %+v", b)
	}
}

// M2：进程被杀留下的 leased（租约已过期）由 maintain 接管并完成。
func TestMaintainTakesOverExpiredLease(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	id := seedMaintainBundle(t, db, "mnt-lease")
	now := time.Now().Unix()
	updateBundle(t, db, id, "status='leased', lease_owner='dead-proc', lease_until=?, attempt=1", now-60)

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMaintain: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if num(got, "processed") != 1 || num(got, "ok") != 1 || num(got, "remaining") != 0 {
		t.Fatalf("过期租约应被接管并完成：%v\n%s", got, out)
	}
	b := loadBundle(t, db, id)
	if b.Status != "ok" || b.LeaseOwner != "" || b.LeaseUntil != 0 {
		t.Fatalf("完成后应释放租约: %+v", b)
	}
	if b.Attempt != 2 {
		t.Fatalf("接管应累加 attempt（1→2），got %d", b.Attempt)
	}
}

// M3：幂等——连跑两次，第二次零新候选、零新 Bundle；无新证据时直接 skipped。
func TestMaintainIdempotentSecondRun(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db, "mnt-idem")

	first, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("第一次 maintain: %v\n%s", err, first)
	}
	if num(decodeJSONOut(t, first), "ok") != 1 {
		t.Fatalf("第一次应完成 1 窗: %s", first)
	}
	cands, bundles := countCandidates(t, db), countBundles(t, db)
	if cands == 0 {
		t.Fatal("第一次应产出候选（规则兜底）")
	}

	second, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("第二次 maintain: %v\n%s", err, second)
	}
	got := decodeJSONOut(t, second)
	if num(got, "processed") != 0 || num(got, "ok") != 0 || num(got, "remaining") != 0 {
		t.Fatalf("第二次应零处理：%v\n%s", got, second)
	}
	if countCandidates(t, db) != cands {
		t.Fatalf("第二次不得新增候选：前 %d 后 %d", cands, countCandidates(t, db))
	}
	if countBundles(t, db) != bundles {
		t.Fatalf("第二次不得新增 Bundle：前 %d 后 %d", bundles, countBundles(t, db))
	}
}

// M4：--dry-run 只读——不认领、不落候选、不推进 Bundle、不打 maintain:last_ok。
func TestMaintainDryRunWritesNothing(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	id := seedMaintainBundle(t, db, "mnt-dry")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--dry-run", "--json"})
	})
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if got["dry_run"] != true || num(got, "processed") != 1 || num(got, "would_generate") != 1 {
		t.Fatalf("dry-run 应报告将生成：%v\n%s", got, out)
	}
	if num(got, "remaining") != 1 {
		t.Fatalf("dry-run 不得消耗窗口，remaining 应仍为 1：%v", got)
	}
	b := loadBundle(t, db, id)
	if b.Status != "pending" || b.Attempt != 0 || b.LeaseOwner != "" || b.LeaseUntil != 0 {
		t.Fatalf("dry-run 不得认领/改状态: %+v", b)
	}
	if n := countCandidates(t, db); n != 0 {
		t.Fatalf("dry-run 不得落候选，got %d", n)
	}
	if _, ok := readLastOK(t, db); ok {
		t.Fatal("dry-run 不得写 maintain:last_ok")
	}
}

// M5：--force 把退避中的 fail 头清零 not_before，本轮立即重试；attempt 不被它改。
func TestMaintainForceRetriesBackoffHead(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	id := seedMaintainBundle(t, db, "mnt-force")
	now := time.Now().Unix()
	updateBundle(t, db, id, "status='fail', attempt=3, not_before=?", now+3600)

	// 对照组：不 force 时只能让位，不能处理。
	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("不退避的 maintain: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if num(got, "processed") != 0 || num(got, "backlog") != 1 {
		t.Fatalf("退避中的头应整轮让位：%v\n%s", got, out)
	}
	if b := loadBundle(t, db, id); b.Attempt != 3 || b.NotBefore != now+3600 {
		t.Fatalf("让位不得改 attempt/not_before: %+v", b)
	}

	out, err = captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--force", "--json"})
	})
	if err != nil {
		t.Fatalf("--force maintain: %v\n%s", err, out)
	}
	got = decodeJSONOut(t, out)
	if num(got, "forced_retry") != 1 || num(got, "processed") != 1 || num(got, "ok") != 1 {
		t.Fatalf("--force 应清退避并本轮重试：%v\n%s", got, out)
	}
	b := loadBundle(t, db, id)
	if b.Status != "ok" {
		t.Fatalf("--force 后应完成: %+v", b)
	}
	if b.Attempt != 4 {
		t.Fatalf("claim 只 +1（3→4），--force 本身不改 attempt: %d", b.Attempt)
	}
}

// M6：出现新隔离即停止并如实汇报，不越过去继续跑后面的窗口。
func TestMaintainStopsOnQuarantine(t *testing.T) {
	db := testDB(t)
	stubMaintainLocal(t, proposal.FuncGenerator{
		Label: "test-bad",
		Fn: func(context.Context, proposal.GenerateRequest) (string, error) {
			return "这不是 JSONL", nil
		},
	})
	bad := seedMaintainBundle(t, db, "mnt-bad")
	next := seedMaintainBundle(t, db, "mnt-next")
	// attempt 已达上限：本轮再失败就进隔离。
	updateBundle(t, db, bad, "status='fail', attempt=?", proposal.MaxAttempts)
	updateBundle(t, db, next, "created_at = ?", time.Now().Unix()+60)

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("隔离不应让命令失败（应如实汇报）：%v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if num(got, "quarantine") != 1 || num(got, "processed") != 1 {
		t.Fatalf("应只处理到隔离点：%v\n%s", got, out)
	}
	if b := loadBundle(t, db, bad); b.Status != "quarantined" {
		t.Fatalf("坏窗口应进隔离: %+v", b)
	}
	if b := loadBundle(t, db, next); b.Status != "pending" {
		t.Fatalf("隔离后不得继续处理后面的窗口: %+v", b)
	}
	results, _ := got["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("结果应只列已处理的那一步：%v", got["results"])
	}
	step, _ := results[0].(map[string]any)
	if msg, _ := step["error"].(string); msg == "" {
		t.Fatalf("隔离必须带出原因，不能吞错误：%v", step)
	}
}

// M7：文本/JSON 字段与 remaining 正确；成功返回后写 maintain:last_ok。
func TestMaintainReportAndLastOK(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db, "mnt-report")

	text, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db})
	})
	if err != nil {
		t.Fatalf("文本 maintain: %v\n%s", err, text)
	}
	for _, want := range []string{"已维护 处理 1", "ok 1", "提交 0 拒绝 0", "剩余 0"} {
		if !strings.Contains(text, want) {
			t.Fatalf("文本应含 %q，got:\n%s", want, text)
		}
	}
	at, ok := readLastOK(t, db)
	if !ok {
		t.Fatal("成功后必须写 maintain:last_ok")
	}
	if d := time.Since(time.Unix(at, 0)); d < -5*time.Second || d > time.Minute {
		t.Fatalf("last_ok 应≈now，got %v（差 %v）", at, d)
	}

	// JSON 字段面固定，供巡检 / 自动化读取。
	db2 := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db2, "mnt-json")
	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db2, "--json"})
	})
	if err != nil {
		t.Fatalf("JSON maintain: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	for key, want := range map[string]float64{
		"processed": 1, "ok": 1, "noop": 0, "fail": 0, "partial": 0,
		"quarantine": 0, "backlog": 0, "remaining": 0,
		"committed": 0, "rejected": 0, "forced_retry": 0,
	} {
		if num(got, key) != want {
			t.Fatalf("JSON 字段 %q 应为 %v，got %v（%v）", key, want, num(got, key), got)
		}
	}
	if got["dry_run"] != false || got["catch_up"] != false {
		t.Fatalf("dry_run/catch_up 应如实反映 flag：%v", got)
	}
	if _, ok := got["results"]; !ok {
		t.Fatalf("JSON 应带 results：%v", got)
	}
}

func TestMaintainDryRunRejectsForce(t *testing.T) {
	if err := cmdMaintain([]string{"--db", testDB(t), "--dry-run", "--force"}); err == nil {
		t.Fatal("--dry-run 与 --force 互斥：前者只读，后者写 not_before")
	}
}
