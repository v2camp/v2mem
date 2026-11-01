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
		SessionID: "sess", Project: "demo", Event: "post-tool-use",
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
	var spawned []string
	prev := startDistillProc
	startDistillProc = func(db, session string) error {
		spawned = append(spawned, session)
		return nil
	}
	t.Cleanup(func() { startDistillProc = prev })
	if _, err := withStdin(t, raw, func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatal(err)
	}
	if len(spawned) != 1 || spawned[0] != "s-stop" {
		t.Fatalf("应拉起一次 distill，got %+v", spawned)
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

func TestHookSkipsDistillWithoutTranscript(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	raw := `{"hook_event_name":"Stop","cwd":"` + proj + `","session_id":"s-no","status":"completed"}`
	t.Setenv("MEM_DISTILL_SPAWN", "1")
	called := false
	prev := startDistillProc
	startDistillProc = func(string, string) error { called = true; return nil }
	t.Cleanup(func() { startDistillProc = prev })
	if _, err := withStdin(t, raw, func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("无 transcript_path 不应拉起 distill")
	}
}

func TestSessionStartDistillPendingDoesNotWriteMemories(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendObservation(store.ObsInput{
		SessionID: "pend", Project: "demo-repo", Event: "post-tool-use",
		Summary: "决策：SessionStart 只产候选",
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatal(err)
	}
	if countAllMemories(t, db) != 0 {
		t.Fatal("SessionStart 蒸馏不得写 memories")
	}
	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, err := st.ListCandidates("pending", "pend", 10)
	if err != nil || len(list) == 0 {
		t.Fatalf("应有 pending，got %+v %v", list, err)
	}
	hits, err := st.Search(store.SearchQuery{Query: "SessionStart", Limit: 5})
	if err != nil || len(hits) != 0 {
		t.Fatalf("采纳前 search 不得命中，got %+v %v", hits, err)
	}
	// 再来一次应去重，不新增候选。
	n := len(list)
	if _, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatal(err)
	}
	list, err = st.ListCandidates("pending", "pend", 10)
	if err != nil || len(list) != n {
		t.Fatalf("重复 SessionStart 不应再插候选，got %d want %d err %v", len(list), n, err)
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
		SessionID: "sess", Event: "post-tool-use", Summary: "决定：dry-run 不落库",
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
	if err := defaultStartDistill(testDB(t), "s"); err == nil {
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
		{SessionID: "s-ok", Event: "post-tool-use", Summary: "决策：JSON 要能看出成功"},
		{SessionID: "s-empty", Event: "stop", Summary: "stop status=completed"},
		{SessionID: "s-fail", Event: "post-tool-use", Summary: "决策：模型不可用要能看出来"},
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
		{SessionID: "s", Event: "post-tool-use", Summary: "决策：甲条要进库"},
		{SessionID: "s", Event: "post-tool-use", Summary: "报错：乙条会写失败"},
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
