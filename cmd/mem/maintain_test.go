package main

// S8：mem maintain 门面的用例（design-data-lifecycle §3、§10 P2）。
//
// 口径：最老优先连续推进、过期租约接管、连跑幂等、dry-run 只读、
// --force 只清退避（不动 attempt）、出现隔离即停、输出字段与 maintain:last_ok 打点。
// 模型来源一律用 stub 替换（maintainLocalGenerator），既不在单测里起 ollama，
// 也不把「本机恰好装了模型」变成用例结果的一部分。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	return seedMaintainEvidence(t, db, "src-mnt", uid, "sess-mnt", "决策：维护窗口 "+uid, true)
}

// seedMaintainEvidence 是 maintain 夹具的统一入口：hook 侧落一条证据后立刻切窗。
// source/session/summary/eligible 可调，用于端到端夹具构造多源、noop 与失败重试场景。
func seedMaintainEvidence(t *testing.T, db, source, uid, session, summary string, eligible bool) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: source, SourceKind: "hook", Generation: 1,
		OccurredAtNS: 1700000000000000000, SessionID: session, Project: "demo",
		Harness: "workbuddy", EventType: "user-prompt", ParserVersion: "evidence/v1",
		ProposalEligible: eligible, Summary: summary,
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

// ---------- S10 接入：maintain 自动裁决（--model-auto，默认关） ----------

func pendingCandidates(t *testing.T, db, bundleID string) int {
	t.Helper()
	q := `SELECT COUNT(*) FROM candidates WHERE status = 'pending'`
	var args []any
	if bundleID != "" {
		q += ` AND bundle_id = ?`
		args = append(args, bundleID)
	}
	var n int
	if err := rawDB(t, db).QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("统计 pending 候选: %v", err)
	}
	return n
}

func decisionCount(t *testing.T, db, actor, modelTag string) int {
	t.Helper()
	q := `SELECT COUNT(*) FROM commit_decisions WHERE 1=1`
	var args []any
	if actor != "" {
		q += ` AND actor = ?`
		args = append(args, actor)
	}
	if modelTag != "" {
		q += ` AND model_tag = ?`
		args = append(args, modelTag)
	}
	var n int
	if err := rawDB(t, db).QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("统计裁决: %v", err)
	}
	return n
}

func metaValue(t *testing.T, db, key string) (string, bool) {
	t.Helper()
	var v string
	err := rawDB(t, db).QueryRow(`SELECT v FROM db_meta WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("读取 meta %s: %v", key, err)
	}
	return v, true
}

// 默认路径：不带 --model-auto 时只产候选，不写记忆、不写裁决、报告字段为零值。
func TestMaintainDefaultDoesNotAdjudicate(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db, "mnt-noauto")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMaintain: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if got["adjudicate_scope"] != "" {
		t.Fatalf("默认不应声明裁决范围，got %v", got["adjudicate_scope"])
	}
	for _, key := range []string{"would_adjudicate", "adjudicated", "committed", "deferred", "conflict", "adjudication_errors"} {
		if num(got, key) != 0 {
			t.Fatalf("默认 %s 应为 0，got %v（%v）", key, num(got, key), got)
		}
	}
	if n := pendingCandidates(t, db, ""); n == 0 {
		t.Fatal("默认应只产候选、保持 pending")
	}
	if n := decisionCount(t, db, "", ""); n != 0 {
		t.Fatalf("默认不得写 commit_decisions，got %d", n)
	}
	if n := countAllMemories(t, db); n != 0 {
		t.Fatalf("默认不得落记忆，got %d", n)
	}
}

// 开启后：本轮候选被自动裁决，actor=model-auto、tag 落裁决账，pending 清零。
func TestMaintainModelAutoCommitsThisRun(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	bundle := seedMaintainBundle(t, db, "mnt-auto")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--model-auto", "--model-tag", "test@v1", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMaintain --model-auto: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if got["adjudicate_scope"] != "this-run" {
		t.Fatalf("默认裁决范围应为 this-run，got %v", got["adjudicate_scope"])
	}
	would, adjudicated, committed := num(got, "would_adjudicate"), num(got, "adjudicated"), num(got, "committed")
	if would < 1 || adjudicated != would || committed < 1 || num(got, "adjudication_errors") != 0 {
		t.Fatalf("应裁决本轮候选且无错误：%v\n%s", got, out)
	}
	if n := pendingCandidates(t, db, bundle); n != 0 {
		t.Fatalf("本轮候选应全部离开 pending，剩 %d", n)
	}
	if n := countAllMemories(t, db); n < 1 {
		t.Fatal("自动裁决应落记忆")
	}
	if n := decisionCount(t, db, "model-auto", "test@v1"); n != int(committed) {
		t.Fatalf("裁决账 actor/tag 不符：decisions=%d committed=%v", n, committed)
	}
}

// --model-tag 缺省取 model.json 的 model_tag（不写空 tag）。
func TestMaintainModelAutoUsesConfigTag(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db, "mnt-cfgtag")

	cfg := filepath.Join(os.Getenv("HOME"), ".v2mem", "model.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"mode":"cli","cli":"claude","model_tag":"cfg@v2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(cfg) })

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--model-auto", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMaintain --model-auto: %v\n%s", err, out)
	}
	if n := decisionCount(t, db, "model-auto", "cfg@v2"); n < 1 {
		t.Fatalf("应使用 model.json 的 model_tag，got decisions=%d (%s)", n, out)
	}
}

// 缺 tag：--model-auto 必须报错退出，不写空 tag。
func TestMaintainModelAutoRequiresTag(t *testing.T) {
	db := testDB(t)
	_, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--model-auto", "--json"})
	})
	if err == nil || !strings.Contains(err.Error(), "model tag") {
		t.Fatalf("缺 model tag 必须报错，got %v", err)
	}
}

// this-run：只裁决本轮 Bundle，历史 pending 原样保留。
func TestMaintainAdjudicateScopeThisRunKeepsHistory(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	oldBundle := seedMaintainBundle(t, db, "mnt-old")
	if _, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	}); err != nil {
		t.Fatal(err)
	}
	if n := pendingCandidates(t, db, oldBundle); n == 0 {
		t.Fatal("第一轮应留下历史 pending")
	}
	newBundle := seedMaintainBundle(t, db, "mnt-new")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--model-auto", "--model-tag", "test@v1", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMaintain: %v\n%s", err, out)
	}
	if n := pendingCandidates(t, db, oldBundle); n == 0 {
		t.Fatal("this-run 不得裁决历史 pending")
	}
	if n := pendingCandidates(t, db, newBundle); n != 0 {
		t.Fatalf("本轮候选应被裁决，剩 %d", n)
	}
}

// all：裁决当前全部 pending（灰度/排障用）。
func TestMaintainAdjudicateScopeAllCoversHistory(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	oldBundle := seedMaintainBundle(t, db, "mnt-old-all")
	if _, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	}); err != nil {
		t.Fatal(err)
	}
	newBundle := seedMaintainBundle(t, db, "mnt-new-all")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--model-auto", "--model-tag", "test@v1",
			"--adjudicate-scope", "all", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMaintain: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if got["adjudicate_scope"] != "all" {
		t.Fatalf("scope 应如实报告 all，got %v", got["adjudicate_scope"])
	}
	if n := pendingCandidates(t, db, oldBundle); n != 0 {
		t.Fatalf("all 应覆盖历史 pending，剩 %d", n)
	}
	if n := pendingCandidates(t, db, newBundle); n != 0 {
		t.Fatalf("all 应覆盖本轮 pending，剩 %d", n)
	}
}

// dry-run：只统计 would_adjudicate，零 decision、零记忆、候选不动、不打成功水位。
func TestMaintainModelAutoDryRunCountsOnly(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db, "mnt-adj-dry")
	if _, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	}); err != nil {
		t.Fatal(err)
	}
	before, hadOK := readLastOK(t, db)
	pendingBefore := pendingCandidates(t, db, "")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--dry-run", "--model-auto", "--model-tag", "test@v1",
			"--adjudicate-scope", "all", "--json"})
	})
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if got["dry_run"] != true || num(got, "would_adjudicate") < 1 {
		t.Fatalf("dry-run 应报告 would_adjudicate>0：%v\n%s", got, out)
	}
	for _, key := range []string{"adjudicated", "committed", "deferred", "conflict", "adjudication_errors"} {
		if num(got, key) != 0 {
			t.Fatalf("dry-run %s 应为 0，got %v", key, num(got, key))
		}
	}
	if n := decisionCount(t, db, "", ""); n != 0 {
		t.Fatalf("dry-run 不得写裁决，got %d", n)
	}
	if n := countAllMemories(t, db); n != 0 {
		t.Fatalf("dry-run 不得落记忆，got %d", n)
	}
	if n := pendingCandidates(t, db, ""); n != pendingBefore {
		t.Fatalf("dry-run 不得改候选：前 %d 后 %d", pendingBefore, n)
	}
	if at, ok := readLastOK(t, db); ok != hadOK || at != before {
		t.Fatalf("dry-run 不得打 maintain:last_ok：前 %d/%v 后 %d/%v", before, hadOK, at, ok)
	}
}

// 单条裁决失败不阻断其余：成功的保留，错误聚合后非零退出并点亮维护失败。
func TestMaintainAdjudicationErrorDoesNotBlockOthers(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	bad := seedMaintainBundle(t, db, "mnt-adj-bad")
	good := seedMaintainBundle(t, db, "mnt-adj-good")
	if _, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	}); err != nil {
		t.Fatal(err)
	}
	// 非法 channel 会让 upsertAcceptedMemory 在落库阶段失败（矩阵仍判 auto_commit）。
	if _, err := rawDB(t, db).Exec(`UPDATE candidates SET channel = 'bogus' WHERE bundle_id = ?`, bad); err != nil {
		t.Fatalf("制造失败候选: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--model-auto", "--model-tag", "test@v1",
			"--adjudicate-scope", "all", "--json"})
	})
	if err == nil {
		t.Fatalf("裁决错误必须非零退出\n%s", out)
	}
	got := decodeJSONOut(t, out)
	if num(got, "adjudication_errors") != 1 || num(got, "committed") != 1 {
		t.Fatalf("应 1 错 1 成：%v\n%s", got, out)
	}
	if n := pendingCandidates(t, db, bad); n != 1 {
		t.Fatalf("失败候选应保持 pending，剩 %d", n)
	}
	if n := pendingCandidates(t, db, good); n != 0 {
		t.Fatalf("成功候选不得被失败阻断，pending=%d", n)
	}
	if _, ok := metaValue(t, db, store.MetaMaintainLastFailAt); !ok {
		t.Fatal("裁决失败应点亮 maintain:last_fail_at")
	}
}

// 端到端：hook 造证据 → maintain --model-auto → 记忆 → eval flow 13/13。
func TestMaintainModelAutoEndToEndFlow(t *testing.T) {
	db := testDB(t)
	seedMaintainEvidence(t, db, "src-ok", "ok-1", "sess-ok", "决策：用 SQLite 存记忆", true)
	seedMaintainEvidence(t, db, "src-ok", "ok-2", "sess-ok", "偏好：中文注释优先", true)
	seedMaintainEvidence(t, db, "src-ok", "ok-3", "sess-ok", "踩坑：WAL 必须开启", true)
	seedMaintainEvidence(t, db, "src-noop", "noop-1", "sess-noop", "子会话事件", false)
	failBundle := seedMaintainEvidence(t, db, "src-fail", "fail-1", "sess-fail", "决策：失败必须重试", true)

	failing := true
	stubMaintainLocal(t, proposal.FuncGenerator{
		Label: "e2e",
		Fn: func(_ context.Context, req proposal.GenerateRequest) (string, error) {
			if req.BundleID == failBundle && failing {
				return "不是 JSON", nil
			}
			return fmt.Sprintf(`{"kind":"decision","content":"E2E 事实 %s","evidence_idx":[1]}`, req.BundleID), nil
		},
	})

	// 第一轮：正常窗 ok、0 提案窗 noop、失败窗 fail（不落记忆）。
	if _, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	}); err != nil {
		t.Fatalf("第一轮 maintain: %v", err)
	}
	if n := countAllMemories(t, db); n != 0 {
		t.Fatalf("未开 model-auto 不得落记忆，got %d", n)
	}

	// 第二轮：清退避后重试失败窗，并只裁决本轮（失败窗）候选。
	updateBundle(t, db, failBundle, "not_before = 0")
	failing = false
	if _, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--model-auto", "--model-tag", "e2e@v1", "--json"})
	}); err != nil {
		t.Fatalf("第二轮 maintain: %v", err)
	}

	// 第三轮：all 兜底，把历史 pending 全部裁决完。
	if _, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--model-auto", "--model-tag", "e2e@v1",
			"--adjudicate-scope", "all", "--json"})
	}); err != nil {
		t.Fatalf("第三轮 maintain: %v", err)
	}
	if n := pendingCandidates(t, db, ""); n != 0 {
		t.Fatalf("全部候选应离场，剩 %d 条 pending", n)
	}

	out, err := captureStdout(t, func() error {
		return cmdEvalFlow([]string{"--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("eval flow 应全绿: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if num(got, "passed") != 13 || num(got, "failed") != 0 {
		for _, c := range got["checks"].([]any) {
			t.Logf("%v", c)
		}
		t.Fatalf("eval flow 应 13/13，got %v/%v\n%s", num(got, "passed"), num(got, "failed"), out)
	}
	if num(got, "manual_rate") != 0 {
		t.Fatalf("全自动裁决下人工介入率应为 0，got %v", num(got, "manual_rate"))
	}
}

// ---------- S15：maintain --stage 全阶段（proposal/adjudicate/consolidate/gc） ----------

// stageNames 按报告顺序取出 stages[].name。
func stageNames(got map[string]any) []string {
	raw, _ := got["stages"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		name, _ := m["name"].(string)
		out = append(out, name)
	}
	return out
}

// stageEntry 取单个阶段账目；不存在返回 nil。
func stageEntry(got map[string]any, name string) map[string]any {
	raw, _ := got["stages"].([]any)
	for _, v := range raw {
		if m, ok := v.(map[string]any); ok && m["name"] == name {
			return m
		}
	}
	return nil
}

func stageCounts(t *testing.T, got map[string]any, name string) map[string]any {
	t.Helper()
	entry := stageEntry(got, name)
	if entry == nil {
		t.Fatalf("报告缺少阶段 %q：%v", name, got["stages"])
	}
	counts, _ := entry["counts"].(map[string]any)
	if counts == nil {
		t.Fatalf("阶段 %q 缺少 counts：%v", name, entry)
	}
	return counts
}

func TestMaintainStageRejectsUnknownValue(t *testing.T) {
	db := testDB(t)
	_, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "bogus"})
	})
	if err == nil || !strings.Contains(err.Error(), "--stage") {
		t.Fatalf("未知 --stage 应报用法错误，got %v", err)
	}
}

func TestMaintainStageAdjudicateRequiresModelAuto(t *testing.T) {
	db := testDB(t)
	_, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "adjudicate"})
	})
	if err == nil || !strings.Contains(err.Error(), "--model-auto") {
		t.Fatalf("--stage=adjudicate 必须要求 --model-auto，got %v", err)
	}
}

func TestMaintainStageAdjudicateRequiresTag(t *testing.T) {
	db := testDB(t)
	_, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "adjudicate", "--model-auto"})
	})
	if err == nil || !strings.Contains(err.Error(), "model tag") {
		t.Fatalf("--stage=adjudicate 必须要求可归因 tag，got %v", err)
	}
}

// 单 stage consolidate：只做归并，不跑 proposal。
func TestMaintainStageConsolidateOnly(t *testing.T) {
	db := testDB(t)
	bundle := seedMaintainBundle(t, db, "mnt-stage-cons")
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "--salience", "0.9", cliSimBase}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "--salience", "0.3", cliSimVariant}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "consolidate", "--json"})
	})
	if err != nil {
		t.Fatalf("maintain --stage=consolidate: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if names := stageNames(got); len(names) != 1 || names[0] != "consolidate" {
		t.Fatalf("只应跑 consolidate，got %v\n%s", names, out)
	}
	if n := num(stageCounts(t, got, "consolidate"), "superseded"); n != 1 {
		t.Fatalf("应取代 1 条近重复，got %v\n%s", n, out)
	}
	if num(got, "processed") != 0 {
		t.Fatalf("不含 proposal 时不得处理 Bundle：%v", got)
	}
	if b := loadBundle(t, db, bundle); b.Status != "pending" || b.Attempt != 0 {
		t.Fatalf("consolidate 不得跑 proposal：%+v", b)
	}
}

// 单 stage gc：dry-run 零写（不建备份、不删）。
func TestMaintainStageGCDryRunWritesNothing(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "待保留的记忆"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	backupDir := filepath.Join(filepath.Dir(db), "backup")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "gc", "--dry-run", "--json"})
	})
	if err != nil {
		t.Fatalf("maintain --stage=gc --dry-run: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if names := stageNames(got); len(names) != 1 || names[0] != "gc" {
		t.Fatalf("只应跑 gc，got %v\n%s", names, out)
	}
	if entry := stageEntry(got, "gc"); entry["status"] != "ok" {
		t.Fatalf("gc 阶段状态应为 ok：%v", entry)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不得创建备份目录（%v）", err)
	}
	if n := countAllMemories(t, db); n != 1 {
		t.Fatalf("dry-run 不得删记忆，got %d", n)
	}
}

// 单 stage gc：非 dry-run 仍先备份再回收（保留 §8.1 顺序）。
func TestMaintainStageGCBacksUpBeforeMutating(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "待保留的记忆"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "gc", "--json"})
	})
	if err != nil {
		t.Fatalf("maintain --stage=gc: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if entry := stageEntry(got, "gc"); entry == nil || entry["status"] != "ok" {
		t.Fatalf("gc 阶段应成功：%v", got["stages"])
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(db), "backup"))
	if err != nil {
		t.Fatalf("读备份目录: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("非 dry-run 的 gc 阶段必须先做库备份")
	}
}

// --stage=all + --model-auto：固定顺序 proposal→adjudicate→consolidate→gc。
func TestMaintainStageAllOrderWithModelAuto(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db, "mnt-stage-all")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "all", "--model-auto",
			"--model-tag", "test@v1", "--json"})
	})
	if err != nil {
		t.Fatalf("maintain --stage=all: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	want := []string{"proposal", "adjudicate", "consolidate", "gc"}
	if names := stageNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("阶段顺序应为 %v，got %v\n%s", want, names, out)
	}
	if n := num(stageCounts(t, got, "adjudicate"), "committed"); n < 1 {
		t.Fatalf("all + model-auto 应自动裁决：%v\n%s", n, out)
	}
	if got["adjudicate_scope"] != "this-run" {
		t.Fatalf("默认裁决范围应保持 this-run，got %v", got["adjudicate_scope"])
	}
}

// --stage=all 不给 --model-auto：跳过 adjudicate，其余三阶段照跑。
func TestMaintainStageAllWithoutModelAutoSkipsAdjudicate(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db, "mnt-stage-all-noauto")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "all", "--json"})
	})
	if err != nil {
		t.Fatalf("maintain --stage=all: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	want := []string{"proposal", "consolidate", "gc"}
	if names := stageNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("阶段顺序应为 %v，got %v\n%s", want, names, out)
	}
	if got["adjudicate_scope"] != "" {
		t.Fatalf("未开 model-auto 不得声明裁决范围，got %v", got["adjudicate_scope"])
	}
	if n := decisionCount(t, db, "", ""); n != 0 {
		t.Fatalf("未开 model-auto 不得写裁决，got %d", n)
	}
}

// 逗号多阶段按固定顺序执行，与书写顺序无关。
func TestMaintainStageCommaUsesFixedOrder(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "固定顺序的记忆"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "gc,proposal", "--json"})
	})
	if err != nil {
		t.Fatalf("maintain --stage=gc,proposal: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	want := []string{"proposal", "gc"}
	if names := stageNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("阶段顺序应为 %v，got %v\n%s", want, names, out)
	}
}

// 某 stage 出错即停止：consolidate 备份失败后不得继续跑 gc。
func TestMaintainStageErrorStopsLaterStages(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "--project", "p1", "触发备份失败的记忆"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	// 用普通文件占住 backup 路径：MkdirAll 必然失败，且不依赖权限位（root 也可复现）。
	if err := os.WriteFile(filepath.Join(filepath.Dir(db), "backup"), []byte("x"), 0o600); err != nil {
		t.Fatalf("占位 backup: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "consolidate,gc", "--json"})
	})
	if err == nil {
		t.Fatalf("备份失败应非零退出\n%s", out)
	}
	got := decodeJSONOut(t, out)
	names := stageNames(got)
	if len(names) != 1 || names[0] != "consolidate" {
		t.Fatalf("consolidate 失败后不得执行 gc，got %v\n%s", names, out)
	}
	if entry := stageEntry(got, "consolidate"); entry["status"] != "error" || entry["error"] == "" {
		t.Fatalf("失败阶段应带 status=error 与原因：%v", entry)
	}
}

// --stage=all --dry-run：全阶段零写（无候选/记忆/裁决/备份，Bundle 不动）。
func TestMaintainStageAllDryRunWritesNothing(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	bundle := seedMaintainBundle(t, db, "mnt-stage-dry")
	beforePending := pendingCandidates(t, db, "")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "all", "--dry-run",
			"--model-auto", "--model-tag", "test@v1", "--json"})
	})
	if err != nil {
		t.Fatalf("maintain --stage=all --dry-run: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	want := []string{"proposal", "adjudicate", "consolidate", "gc"}
	if names := stageNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("阶段顺序应为 %v，got %v\n%s", want, names, out)
	}
	if entry := stageEntry(got, "consolidate"); entry["status"] != "skipped" {
		t.Fatalf("consolidate 无预览模式，dry-run 应标 skipped：%v", entry)
	}
	if n := pendingCandidates(t, db, ""); n != beforePending {
		t.Fatalf("dry-run 不得产生候选：前 %d 后 %d", beforePending, n)
	}
	if n := countAllMemories(t, db); n != 0 {
		t.Fatalf("dry-run 不得落记忆，got %d", n)
	}
	if n := decisionCount(t, db, "", ""); n != 0 {
		t.Fatalf("dry-run 不得写裁决，got %d", n)
	}
	if b := loadBundle(t, db, bundle); b.Status != "pending" || b.Attempt != 0 {
		t.Fatalf("dry-run 不得认领/推进 Bundle：%+v", b)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(db), "backup")); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不得创建备份目录（%v）", err)
	}
	if _, ok := readLastOK(t, db); ok {
		t.Fatal("dry-run 不得写 maintain:last_ok")
	}
}

// 单独 adjudicate：不跑 proposal，按 --adjudicate-scope=all 裁决历史 pending。
func TestMaintainStageAdjudicateOnly(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db, "mnt-stage-adj")
	if _, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	}); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--stage", "adjudicate", "--model-auto",
			"--model-tag", "test@v1", "--adjudicate-scope", "all", "--json"})
	})
	if err != nil {
		t.Fatalf("maintain --stage=adjudicate: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if names := stageNames(got); len(names) != 1 || names[0] != "adjudicate" {
		t.Fatalf("只应跑 adjudicate，got %v\n%s", names, out)
	}
	if n := num(stageCounts(t, got, "adjudicate"), "committed"); n < 1 {
		t.Fatalf("应裁决历史 pending：%v\n%s", n, out)
	}
	if num(got, "processed") != 0 {
		t.Fatalf("adjudicate 单跑不得处理 Bundle：%v", got)
	}
}

// 默认（无 --stage）行为不变：只跑 proposal，报告 stages 也只有它。
func TestMaintainDefaultStageIsProposalOnly(t *testing.T) {
	db := testDB(t)
	stubRulesFallback(t)
	seedMaintainBundle(t, db, "mnt-stage-default")

	out, err := captureStdout(t, func() error {
		return cmdMaintain([]string{"--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMaintain: %v\n%s", err, out)
	}
	got := decodeJSONOut(t, out)
	if names := stageNames(got); len(names) != 1 || names[0] != "proposal" {
		t.Fatalf("默认只应跑 proposal，got %v\n%s", names, out)
	}
	if num(got, "ok") != 1 {
		t.Fatalf("默认行为应保持不变：%v", got)
	}
}
