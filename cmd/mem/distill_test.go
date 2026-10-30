package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
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
