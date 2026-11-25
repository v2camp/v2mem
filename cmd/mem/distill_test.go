package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/proposal"
	"github.com/v2camp/v2mem/internal/store"
)

func countAllMemories(t *testing.T, db string) int {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hits, err := st.List(store.ListQuery{Scope: "all", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return len(hits)
}

func TestDistillNoneThenAcceptAndReject(t *testing.T) {
	db := testDB(t)
	ap := t.TempDir() + "/audit.jsonl"
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendObservation(store.ObsInput{
		SessionID: "sess", Project: "demo", Event: "user-prompt",
		Summary: "决策：记忆库放在用户目录，不放代码目录",
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	memBefore := countAllMemories(t, db)

	if err := cmdDistill([]string{"--db", db, "--session", "sess", "--provider", "none", "--audit-file", ap}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "用户目录"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "用户目录") {
		t.Fatalf("蒸馏结果不得被 search 命中，got %s", out)
	}
	listed, err := captureStdout(t, func() error {
		return cmdCandidates([]string{"ls", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed, `"status":"pending"`) || !strings.Contains(listed, "用户目录") {
		t.Fatalf("应有 pending 候选，got %s", listed)
	}

	if countAllMemories(t, db) != memBefore {
		t.Fatalf("distill 不得写 memories，before=%d after=%d", memBefore, countAllMemories(t, db))
	}

	accepted, err := captureStdout(t, func() error {
		return cmdAccept([]string{"--db", db, "--all-pending", "--session", "sess", "--audit-file", ap})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(accepted, "source=distill-accept") {
		t.Fatalf("采纳输出应带 source，got %s", accepted)
	}
	out, err = captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "用户目录"})
	})
	if err != nil || !strings.Contains(out, "用户目录") {
		t.Fatalf("采纳后应可检索，got %q %v", out, err)
	}
	rs, err := audit.Read(ap, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawDistill, sawAccept bool
	for _, r := range rs {
		if r.Event == "distill" && r.Provider == "none" && r.Mode == "ok" {
			sawDistill = true
		}
		if r.Event == "accept" && r.Source == "distill-accept" {
			sawAccept = true
		}
	}
	if !sawDistill || !sawAccept {
		t.Fatalf("审计缺 distill/accept，got %+v", rs)
	}
}

func TestDistillStrictAndFailOpen(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendObservation(store.ObsInput{
		SessionID: "sess", Event: "stop", Summary: "决策：超时也不能写记忆",
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	t.Setenv("OPENAI_API_KEY", "")
	if err := cmdDistill([]string{"--db", db, "--session", "sess", "--provider", "openai", "--audit-file", "-"}); err != nil {
		t.Fatalf("缺 key 默认应 exit 0 语义（返回 nil），got %v", err)
	}
	if err := cmdDistill([]string{"--db", db, "--session", "sess", "--provider", "openai", "--strict", "--audit-file", "-"}); err == nil {
		t.Fatal("--strict 在失败时应返回错误")
	}
	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, err := st.ListCandidates("", "sess", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("失败不得写候选，got %+v", list)
	}
	if countAllMemories(t, db) != 0 {
		t.Fatal("失败不得写 memories")
	}
}

func TestRejectKeepsCandidateOutOfSearch(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Kind: "pitfall", Content: "报错：拒绝后不该被搜到", Source: "distill",
	})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdReject([]string{"--db", db, "--audit-file", "-", res.Candidate.ID[:8]}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "拒绝后不该被搜到"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "拒绝后") {
		t.Fatalf("拒绝的候选不得进检索，got %s", out)
	}
}

func TestHookStopDoesNotInsertMemories(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	raw := `{"hook_event_name":"Stop","cwd":"` + proj + `","session_id":"s-stop","status":"completed","transcript_path":"/tmp/t.jsonl"}`
	t.Setenv("MEM_DISTILL_SPAWN", "1")
	var spawned int
	prev := startDistillProc
	startDistillProc = func(db string) error {
		spawned++
		return nil
	}
	t.Cleanup(func() { startDistillProc = prev })
	if _, err := withStdin(t, raw, func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatal(err)
	}
	if spawned != 1 {
		t.Fatalf("应拉起一次 proposal，got %d", spawned)
	}
	if countAllMemories(t, db) != 0 {
		t.Fatal("Stop 钩子不得写 memories")
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	obs, err := st.ListObservations("s-stop", 10)
	if err != nil || len(obs) != 1 {
		t.Fatalf("应有观测，got %+v %v", obs, err)
	}
}

// TestHookSpawnsProposalWithoutTranscript：单轨后 Stop 只要求 session_id 非空。
// proposal 从证据 Bundle 读输入，不依赖 transcript_path，因此缺它也必须拉起。
func TestHookSpawnsProposalWithoutTranscript(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	raw := `{"hook_event_name":"Stop","cwd":"` + proj + `","session_id":"s-no","status":"completed"}`
	t.Setenv("MEM_DISTILL_SPAWN", "1")
	called := false
	prev := startDistillProc
	startDistillProc = func(string) error { called = true; return nil }
	t.Cleanup(func() { startDistillProc = prev })
	if _, err := withStdin(t, raw, func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("proposal 不读 transcript_path，缺它也必须拉起")
	}
}

// TestSessionStartSpawnsProposalWithoutWritingMemories：单轨后 SessionStart 只拉起
// proposal 子进程，不再内联跑 distillOne——因此既不平白写 candidates，也不碰 memories。
// 子进程是否拉起只受 MEM_NO_DISTILL 关断，与 MEM_DISTILL_SPAWN 无关。
func TestSessionStartSpawnsProposalWithoutWritingMemories(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
	t.Setenv("MEM_NO_DISTILL", "")
	var calls int
	prev := startDistillProc
	startDistillProc = func(string) error { calls++; return nil }
	t.Cleanup(func() { startDistillProc = prev })
	if _, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("SessionStart 应拉起 1 次 proposal 子进程，got %d", calls)
	}
	if countAllMemories(t, db) != 0 {
		t.Fatal("SessionStart 蒸馏不得写 memories")
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, err := st.ListCandidates("pending", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("SessionStart 不再内联 distillOne，不应写候选，got %d", len(list))
	}
	if hits, err := st.Search(store.SearchQuery{Query: "SessionStart", Limit: 5}); err != nil || len(hits) != 0 {
		t.Fatalf("采纳前 search 不得命中，got %+v %v", hits, err)
	}
}

func TestActivityObservedWithoutAccept(t *testing.T) {
	dir := t.TempDir()
	ap := dir + "/audit.jsonl"
	now := timeNowUnix()
	for _, r := range []audit.Record{
		{TS: now, Event: "obs-append", SessionID: "s"},
		{TS: now, Event: "distill", Mode: "ok", Provider: "none", Count: 1, Source: "distill"},
		{TS: now, Event: "manual-add", Source: "distill-pending", Mode: "create"},
	} {
		if err := audit.Append(ap, r); err != nil {
			t.Fatal(err)
		}
	}
	out, err := captureStdout(t, func() error {
		return cmdEvalActivity([]string{"--audit-file", ap, "--since", "1h"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "有观测无采纳") {
		t.Fatalf("应判有观测无采纳，got %s", out)
	}
	if strings.Contains(out, "写侧 1") {
		t.Fatalf("distill-pending 不应计入写侧，got %s", out)
	}
}

func timeNowUnix() int64 { return time.Now().Unix() }

func TestCmdGCCandidatesAndTextList(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Kind: "task", Content: "待办：gc 候选", Source: "distill",
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	out, err := captureStdout(t, func() error {
		return cmdCandidates([]string{"ls", "--db", db})
	})
	if err != nil || !strings.Contains(out, "待办") {
		t.Fatalf("文本列表应打出候选，got %q %v", out, err)
	}
	if err := cmdGC([]string{"--db", db, "--candidates"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdGC([]string{"--db", db, "--candidates", "--obs"}); err == nil {
		t.Fatal("两个开关不能同时用")
	}
}

func TestDistillDryRunPrintsAndSkipsEmpty(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendObservation(store.ObsInput{
		SessionID: "sess", Event: "user-prompt", Summary: "决定：dry-run 不落库",
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	out, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--session", "sess", "--provider", "none", "--dry-run", "--audit-file", "-"})
	})
	if err != nil || !strings.Contains(out, "dry-run 不落库") {
		t.Fatalf("dry-run 应打印候选，got %q %v", out, err)
	}
	if countAllMemories(t, db) != 0 {
		t.Fatal("dry-run 不得写记忆")
	}
	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	list, err := st.ListCandidates("", "sess", 10)
	st.Close()
	if err != nil || len(list) != 0 {
		t.Fatalf("dry-run 不得写候选，got %+v %v", list, err)
	}
	emptyDB := testDB(t)
	empty, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", emptyDB, "--pending", "--provider", "none", "--audit-file", "-"})
	})
	if err != nil || !strings.Contains(empty, "没有需要蒸馏") {
		t.Fatalf("无待蒸馏会话应说明，got %q %v", empty, err)
	}
	none, err := captureStdout(t, func() error {
		return cmdCandidates([]string{"ls", "--db", emptyDB})
	})
	if err != nil || !strings.Contains(none, "没有候选") {
		t.Fatalf("空候选列表，got %q %v", none, err)
	}
}

func TestDefaultStartDistillRefusesTestBinary(t *testing.T) {
	if err := defaultStartDistill(testDB(t)); err == nil {
		t.Fatal("测试二进制不应真的拉起 distill")
	}
}

func TestImportTranscriptDirSinceAndDryRun(t *testing.T) {
	db := testDB(t)
	dir := t.TempDir()
	sub := dir + "/proj"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","sessionId":"old","timestamp":"2020-01-01T00:00:00Z","cwd":"/work/demo","message":{"role":"user","content":"决策：太旧"}}` + "\n" +
		`{"type":"user","sessionId":"new","timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `","cwd":"/work/demo","message":{"role":"user","content":"决定：够新"}}` + "\n"
	if err := os.WriteFile(sub+"/a.jsonl", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdImportTranscript([]string{"--db", db, "--claude-dir", dir, "--since", "48h", "--dry-run", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"observations":1`) {
		t.Fatalf("dry-run 应只数到新行，got %s", out)
	}
	if countAllMemories(t, db) != 0 {
		t.Fatal("dry-run 不得写库")
	}
	if err := cmdImportTranscript([]string{"--db", db, "--into", "nope"}); err == nil {
		t.Fatal("非法 --into 应失败")
	}
}

// ---------- P0：错误语义与写入成功计数 ----------

// execSQL 直接对库执行 DDL。命令层测试需要制造「写库失败」，
// 而 store 既没有、也不该暴露这种口子，所以测试自己开连接。
func execSQL(t *testing.T, db, stmt string) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

// decodeJSONOut 解析命令的 --json 输出。
func decodeJSONOut(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("JSON 输出不可解析: %v\n%s", err, out)
	}
	return m
}

func num(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return -1
}

// --json 必须能区分 ok / empty / fail 三种结局。
// 旧实现只输出 {"candidates":N}，失败与「合法 0 条」长得一模一样。
func TestDistillJSONDistinguishesTriState(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []store.ObsInput{
		{SessionID: "s-ok", Event: "user-prompt", Summary: "决策：JSON 要能看出成功"},
		{SessionID: "s-empty", Event: "stop", Summary: "stop status=completed"},
		{SessionID: "s-fail", Event: "user-prompt", Summary: "决策：模型不可用要能看出来"},
	} {
		if _, err := st.AppendObservation(o); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--session", "s-ok", "--provider", "none", "--json", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONOut(t, out)
	if got["status"] != "ok" || num(got, "inserted") != 1 || num(got, "failed") != 0 {
		t.Fatalf("成功应为 ok/inserted=1，got %v", got)
	}

	out, err = captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--session", "s-empty", "--provider", "none", "--json", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatal(err)
	}
	got = decodeJSONOut(t, out)
	if got["status"] != "empty" {
		t.Fatalf("合法 0 条应为 empty，got %v", got)
	}
	if num(got, "inserted") != 0 || num(got, "failed") != 0 || num(got, "failed_sessions") != 0 {
		t.Fatalf("empty 不应计失败，got %v", got)
	}

	// 缺 key ⇒ 模型不可用 ⇒ fail，且与 empty 明确不同。
	t.Setenv("OPENAI_API_KEY", "")
	out, err = captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--session", "s-fail", "--provider", "openai", "--json", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatalf("默认 fail-open，不该以 error 返回: %v", err)
	}
	got = decodeJSONOut(t, out)
	if got["status"] != "fail" || num(got, "failed_sessions") != 1 {
		t.Fatalf("模型不可用应为 fail，got %v", got)
	}
	if err := cmdDistill([]string{"--db", db, "--session", "s-fail", "--provider", "openai", "--strict", "--audit-file", "-"}); err == nil {
		t.Fatal("--strict 下 fail 必须非零退出")
	}
}

// 部分落库时必须同时报 inserted 与 failed。
// 只报一个数都会说谎：报了 inserted 会掩盖丢数据，报 failed 会让人以为一条没成。
func TestDistillPartialInsertReportsBothCounts(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []store.ObsInput{
		{SessionID: "s", Event: "user-prompt", Summary: "决策：甲条要进库"},
		{SessionID: "s", Event: "user-prompt", Summary: "报错：乙条会写失败"},
	} {
		if _, err := st.AppendObservation(o); err != nil {
			t.Fatal(err)
		}
	}
	// 只让含「乙」的候选插入失败，制造「一条成一条败」。
	execSQL(t, db, `CREATE TRIGGER fail_cand BEFORE INSERT ON candidates
		WHEN NEW.content LIKE '%乙%' BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--session", "s", "--provider", "none", "--json", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatalf("默认 fail-open，不该以 error 返回: %v", err)
	}
	got := decodeJSONOut(t, out)
	if got["status"] != "partial" {
		t.Fatalf("部分落库应为 partial，got %v", got)
	}
	if num(got, "inserted") != 1 || num(got, "failed") != 1 {
		t.Fatalf("必须同时报 inserted=1 failed=1，got %v", got)
	}
	if err := cmdDistill([]string{"--db", db, "--session", "s", "--provider", "none", "--strict", "--audit-file", "-"}); err == nil {
		t.Fatal("--strict 下部分失败必须非零退出（沉默的成功就是谎报）")
	}

	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, err := st.ListCandidates("pending", "s", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !strings.Contains(list[0].Content, "甲") {
		t.Fatalf("失败的候选不应落库，成功的应在，got %+v", list)
	}
	// 部分失败不能占窗口，否则丢掉的那条永远补不回来。
	done, err := st.DistillWindowDone("s", distill.WindowKey(observationIDs(t, st, "s")))
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("partial 不得占住蒸馏窗口")
	}
}

// observationIDs 取会话观测 id（按时间正序，与 distillOne 一致），用于算窗口键。
func observationIDs(t *testing.T, st *store.Store, session string) []string {
	t.Helper()
	obs, err := st.ObservationsForDistill(session, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(obs))
	for i, o := range obs {
		ids[i] = o.ID
	}
	return ids
}

// --all-pending 中途失败时，已采纳的结果不能被丢掉。
// 旧实现第一条失败就 return，但前面几条其实已经落库（审计也写了），
// 于是审计数 > 用户看到数。
func TestAcceptAllPendingKeepsSuccessesOnFailure(t *testing.T) {
	db := testDB(t)
	ap := filepath.Join(t.TempDir(), "audit.jsonl")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ kind, content string }{
		{"decision", "决定：坏的一条"},
		{"decision", "决定：好的一条"},
		{"task", "待办：好的另一条"},
	} {
		if _, err := st.InsertCandidate(store.CandidateInput{
			SessionID: "s", Kind: c.kind, Content: c.content, Source: "distill",
		}); err != nil {
			t.Fatal(err)
		}
	}
	execSQL(t, db, `CREATE TRIGGER fail_mem BEFORE INSERT ON memories
		WHEN NEW.content LIKE '%坏%' BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdAccept([]string{"--db", db, "--all-pending", "--session", "s", "--audit-file", ap})
	})
	if err == nil {
		t.Fatal("有一个候选采纳失败时应返回错误")
	}
	if n := strings.Count(out, "已采纳"); n != 2 {
		t.Fatalf("已成功的 2 条必须照常打印，got %d:\n%s", n, out)
	}
	if countAllMemories(t, db) != 2 {
		t.Fatalf("已成功的候选必须留在库里，got %d", countAllMemories(t, db))
	}
	rs, err := audit.Read(ap, 0)
	if err != nil {
		t.Fatal(err)
	}
	accepts := 0
	for _, r := range rs {
		if r.Event == "accept" {
			accepts++
		}
	}
	// 审计数与用户看到的一致：不许出现「审计 3 条、只打印 0 条」。
	if accepts != 2 || accepts != strings.Count(out, "已采纳") {
		t.Fatalf("审计=%d 打印=%d，应都等于 2", accepts, strings.Count(out, "已采纳"))
	}

	// 剩下的失败项重试仍失败，但 --json 要把失败单独列出来。
	out, err = captureStdout(t, func() error {
		return cmdAccept([]string{"--db", db, "--all-pending", "--session", "s", "--json", "--audit-file", "-"})
	})
	if err == nil {
		t.Fatal("仍有失败时应返回错误")
	}
	got := decodeJSONOut(t, out)
	if num(got, "inserted") != 0 || num(got, "failed_count") != 1 {
		t.Fatalf("JSON 应区分成功与失败，got %v", got)
	}
	if failed, ok := got["failed"].([]any); !ok || len(failed) != 1 {
		t.Fatalf("failed 应列出失败项，got %v", got["failed"])
	}
}

// model.json 写坏了必须 fail-closed：读不出来就当 api/auto 处理，
// 等于用户配的 CLI 被静默换成外发路径。这条只能靠断言守住。
func TestCmdDistillRejectsBadModelConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".v2mem"), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(home, ".v2mem", "model.json")
	if err := os.WriteFile(bad, []byte(`{"mode":"bogus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "m.db")
	err := cmdDistill([]string{"--session", "x", "--db", db, "--audit-file", "-"})
	if err == nil {
		t.Fatal("配置文件非法时应返回错误，不得退回 api/auto")
	}
	if !strings.Contains(err.Error(), "model.json") {
		t.Errorf("错误应指出配置文件：%v", err)
	}
	// 配置在读库之前校验：没有库文件就说明没有走到 store.Open，更不可能外发。
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Errorf("应在打开数据库前失败，db 却被创建了: %v", statErr)
	}
}

// proposalOptions 必须把 model.json 的 mode 语义带进 proposal 的 local/byok 两条来源：
// mode=off 走规则路径（provider=none），否则 `mem distill --proposal` 仍会去连 ollama/openai。
func TestProposalOptionsOffIsNone(t *testing.T) {
	if got := proposalOptions(distill.ModelConfig{Mode: "off"}, "ollama", "m", time.Second); got.Provider != "none" {
		t.Errorf("mode=off 应映射 provider=none，got %+v", got)
	}
	cli := distill.ModelConfig{Mode: "cli", CLI: "claude", ModelTag: "claude@1+tpl1"}
	if got := proposalOptions(cli, "ollama", "m", time.Second); got.CLI != "claude" || got.Timeout != time.Second {
		t.Errorf("mode=cli 应填 CLI 与 timeout，got %+v", got)
	}
	if got := proposalOptions(distill.ModelConfig{Mode: "api", Provider: "deepseek"}, "ollama", "m", time.Second); got.Provider != "ollama" || got.Model != "m" {
		t.Errorf("mode=api 应保持原来的 local 硬编码 provider，got %+v", got)
	}
}

// ---------- S6a: Bundle 隔离查看与回捞 ----------

// quarantineForTest 把一个已生成的 Bundle 走完认领→隔离，模拟 runner 超限后的库状态。
func quarantineForTest(t *testing.T, db, bundleID string) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.ClaimProposalBundle(bundleID, "test", time.Now().Unix(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishProposalBundle(bundleID, "test", "quarantined", 0, "run-q"); err != nil {
		t.Fatal(err)
	}
}

// seedExtraBundle 用唯一 uid 追加一段证据并取窗，供需要第二个 Bundle 的用例使用。
func seedExtraBundle(t *testing.T, db, uid string) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-cmd", SourceKind: "hook", Generation: 1,
		OccurredAtNS: 1700000000000000000, SessionID: "sess-cmd", Project: "demo",
		Harness: "workbuddy", EventType: "stop", ParserVersion: "evidence/v1",
		ProposalEligible: true, Summary: "决定：第二条隔离窗口",
		ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
		RawRecordHash: "rh-" + uid, ByteSize: 16, RawCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	b, err := evidence.New(st).NextBundle(50, 1<<20)
	if err != nil || b == nil {
		t.Fatalf("应生成第二个 Bundle: %+v err=%v", b, err)
	}
	return b.ID
}

// Q7：--quarantined 文本/JSON、--requeue 重置与错误透传。
func TestDistillQuarantinedListAndRequeue(t *testing.T) {
	db := testDB(t)
	ap := filepath.Join(t.TempDir(), "audit.jsonl")

	// 空库先验证「没有」也说得清楚。
	empty, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--quarantined", "--audit-file", "-"})
	})
	if err != nil || !strings.Contains(empty, "没有隔离") {
		t.Fatalf("空库应说明没有隔离 Bundle，got %q %v", empty, err)
	}

	bundleID := seedEvidenceBundle(t, db, 1)
	quarantineForTest(t, db, bundleID)

	text, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--quarantined", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"bundle " + shortID(bundleID), "source=src-cmd", "seq=0..1", "attempt=1", "updated=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("文本应含 %q，got %s", want, text)
		}
	}

	jsonOut, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--quarantined", "--json", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &list); err != nil {
		t.Fatalf("JSON 不可解析: %v\n%s", err, jsonOut)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条隔离记录，got %+v", list)
	}
	got := list[0]
	if got["id"] != bundleID || got["source_id"] != "src-cmd" || num(got, "generation") != 1 ||
		num(got, "from_seq_exclusive") != 0 || num(got, "to_seq_inclusive") != 1 ||
		num(got, "attempt") != 1 || num(got, "updated_at") <= 0 {
		t.Fatalf("隔离 JSON 字段不对: %+v", got)
	}

	out, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--requeue", bundleID, "--audit-file", ap})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已重新入队") || !strings.Contains(out, "attempt 归零") {
		t.Fatalf("回捞提示不对: %s", out)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	back, err := st.LoadProposalBundle(bundleID)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if back.Status != "pending" || back.Attempt != 0 || back.NotBefore != 0 {
		t.Fatalf("回捞后应为 pending/attempt=0: %+v", back)
	}
	rs, err := audit.Read(ap, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawRequeue bool
	for _, r := range rs {
		if r.Event == "bundle-requeue" {
			sawRequeue = true
		}
	}
	if !sawRequeue {
		t.Fatalf("回捞必须写 bundle-requeue 审计，got %+v", rs)
	}

	// 非隔离项报错并透传当前状态。
	if err := cmdDistill([]string{"--db", db, "--requeue", bundleID, "--audit-file", "-"}); err == nil ||
		!strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending 不应可回捞: %v", err)
	}
	if err := cmdDistill([]string{"--db", db, "--requeue", "bnd-missing", "--audit-file", "-"}); err == nil {
		t.Fatal("未知 Bundle 应报错")
	}

	// 回捞 JSON 分支。
	b2 := seedExtraBundle(t, db, "cmd-uid-z")
	quarantineForTest(t, db, b2)
	rj, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--requeue", b2, "--json", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeJSONOut(t, rj); got["status"] != "pending" || got["id"] != b2 {
		t.Fatalf("回捞 JSON 不对: %v", got)
	}

	// 互斥：隔离查看/回捞必须单独使用，不要求 --session 等必填项。
	for _, args := range [][]string{
		{"--db", db, "--quarantined", "--proposal"},
		{"--db", db, "--requeue", bundleID, "--bundle", bundleID},
		{"--db", db, "--quarantined", "--requeue", bundleID},
	} {
		if err := cmdDistill(append(args, "--audit-file", "-")); err == nil || !strings.Contains(err.Error(), "单独") {
			t.Fatalf("%v 应被互斥校验拒绝: %v", args, err)
		}
	}
}

// 隔离发生时由 CLI 写 bundle-quarantine 审计；runner 是纯逻辑，不依赖 audit 包。
func TestProposalQuarantineAuditRecord(t *testing.T) {
	ap := filepath.Join(t.TempDir(), "audit.jsonl")
	writeProposalAudit(ap, &proposal.Outcome{
		BundleID: "bnd-q", SessionID: "sess-q", BundleStatus: "quarantined",
		Status: proposal.StatusQuarantined,
	})
	rs, err := audit.Read(ap, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawQuarantine, sawDistill bool
	for _, r := range rs {
		if r.Event == "bundle-quarantine" {
			sawQuarantine = true
		}
		if r.Event == "distill" {
			sawDistill = true
		}
	}
	if !sawQuarantine || !sawDistill {
		t.Fatalf("隔离应写 bundle-quarantine 且照旧写 distill，got %+v", rs)
	}
}
