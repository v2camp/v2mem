package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

func TestImportTranscriptObsOnlyAndBoth(t *testing.T) {
	db := testDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	body := strings.Join([]string{
		`{"type":"user","sessionId":"imp","timestamp":"2026-10-01T00:00:00Z","cwd":"/work/demo","message":{"role":"user","content":"决策：冷启动只写观测 api_key=sk-supersecretvalue"}}`,
		`{"type":"assistant","sessionId":"imp","message":{"role":"assistant","content":[{"type":"text","text":"报错：不要把密钥写进库"}]}}`,
		`this is not json`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdImportTranscript([]string{"--db", db, "--file", path, "--into", "obs"}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := st.ListObservations("imp", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 {
		t.Fatalf("应导入 2 条观测，got %+v", obs)
	}
	blob := obs[0].Summary + obs[1].Summary
	if strings.Contains(blob, "sk-supersecretvalue") || !strings.Contains(blob, "[redacted]") {
		t.Fatalf("密钥应被打码，got %s", blob)
	}
	mem, err := st.List(store.ListQuery{Scope: "all", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(mem) != 0 {
		t.Fatal("import 不得写 memories")
	}
	st.Close()

	if err := cmdImportTranscript([]string{"--db", db, "--file", path, "--into", "obs"}); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Open(db)
	obs, _ = st.ListObservations("imp", 10)
	if len(obs) != 2 {
		t.Fatalf("重跑应幂等，got %d", len(obs))
	}
	st.Close()

	if err := cmdImportTranscript([]string{
		"--db", db, "--file", path, "--into", "both", "--cursor-dir", dir,
	}); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Open(db)
	defer st.Close()
	list, err := st.ListCandidates("pending", "imp", 10)
	if err != nil || len(list) == 0 {
		t.Fatalf("both 应有 pending，got %+v %v", list, err)
	}
	hits, err := st.Search(store.SearchQuery{Query: "冷启动", Limit: 5})
	if err != nil || len(hits) != 0 {
		t.Fatalf("accept 前不得检索到，got %+v %v", hits, err)
	}
	mem, err = st.List(store.ListQuery{Scope: "all", Limit: 20})
	if err != nil || len(mem) != 0 {
		t.Fatalf("both 仍不得写 memories，mem=%d err=%v", len(mem), err)
	}
}

// 导入必须把「重复 / 空内容 / since 过滤 / 写失败」分开报。
// 旧实现只有一个 skipped，写库失败和幂等跳过长得一模一样，数据丢失被藏起来。
func TestImportTranscriptSplitsCounters(t *testing.T) {
	db := testDB(t)
	path := filepath.Join(t.TempDir(), "mix.jsonl")
	body := strings.Join([]string{
		`{"type":"user","sessionId":"c1","timestamp":"2020-01-01T00:00:00Z","cwd":"/w","message":{"role":"user","content":"决策：太旧了"}}`,
		`{"type":"user","sessionId":"c1","timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `","cwd":"/w","message":{"role":"user","content":"决策：这条要进库"}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdImportTranscript([]string{"--db", db, "--file", path, "--since", "48h", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONOut(t, out)
	if num(got, "written") != 1 || num(got, "filtered") != 1 || num(got, "duplicate") != 0 || num(got, "failed") != 0 {
		t.Fatalf("首轮应为 written=1 filtered=1，got %v", got)
	}

	out, err = captureStdout(t, func() error {
		return cmdImportTranscript([]string{"--db", db, "--file", path, "--since", "48h", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	got = decodeJSONOut(t, out)
	if num(got, "written") != 0 || num(got, "duplicate") != 1 {
		t.Fatalf("重跑应计 duplicate=1 而不是 written，got %v", got)
	}
}

// 写库失败必须单独计数，不能被并进「跳过」。
func TestImportTranscriptCountsWriteFailures(t *testing.T) {
	db := testDB(t)
	path := filepath.Join(t.TempDir(), "two.jsonl")
	body := strings.Join([]string{
		`{"type":"user","sessionId":"c2","timestamp":"2026-10-01T00:00:00Z","cwd":"/w","message":{"role":"user","content":"决策：第一行"}}`,
		`{"type":"assistant","sessionId":"c2","message":{"role":"assistant","content":[{"type":"text","text":"报错：第二行"}]}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	execSQL(t, db, `CREATE TRIGGER fail_obs BEFORE INSERT ON observations
		BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	out, err := captureStdout(t, func() error {
		return cmdImportTranscript([]string{"--db", db, "--file", path, "--json"})
	})
	if err != nil {
		t.Fatalf("单行写失败不该升级为命令级错误（文件级才算）: %v", err)
	}
	got := decodeJSONOut(t, out)
	if num(got, "failed") != 2 || num(got, "written") != 0 || num(got, "duplicate") != 0 {
		t.Fatalf("两行都写失败应计 failed=2，got %v", got)
	}
}

// 文件级失败必须影响退出码：整份 transcript 没进来是数据丢失。
func TestImportTranscriptFileFailureSetsExitCode(t *testing.T) {
	db := testDB(t)
	err := cmdImportTranscript([]string{"--db", db, "--file", filepath.Join(t.TempDir(), "missing.jsonl")})
	if err == nil {
		t.Fatal("文件打不开时应返回错误")
	}
	if !strings.Contains(err.Error(), "missing.jsonl") {
		t.Fatalf("错误应指明文件，got %v", err)
	}
}

// 同摘要、不同事件身份必须共存：证据身份来自宿主 uuid，不是内容 hash。
func TestImportTranscriptKeepsSameSummaryApart(t *testing.T) {
	db := testDB(t)
	path := filepath.Join(t.TempDir(), "dup.jsonl")
	body := strings.Join([]string{
		`{"type":"user","uuid":"x1","sessionId":"dup","message":{"role":"user","content":"决策：同摘要"}}`,
		`{"type":"user","uuid":"x2","sessionId":"dup","message":{"role":"user","content":"决策：同摘要"}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdImportTranscript([]string{"--db", db, "--file", path}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := transcriptSourceID("claude", "dup")
	for _, uid := range []string{"tx:x1", "tx:x2"} {
		rec, ok, err := st.LookupEvidence(uid, src, 0, 0)
		if err != nil || !ok {
			t.Fatalf("%s 应独立成条: ok=%v err=%v", uid, ok, err)
		}
		if rec.Summary != "决策：同摘要" {
			t.Fatalf("摘要应一致（被内容去重吞并才会失败）: %+v", rec)
		}
	}
	obs, err := st.ListObservations("dup", 10)
	if err != nil || len(obs) != 2 {
		t.Fatalf("同摘要不同身份应共存，got %d err=%v", len(obs), err)
	}
}

// 记忆工具自身与 MCP 管道事件：照常归档，但 ProposalEligible=false。
func TestImportTranscriptPipelineToolIneligible(t *testing.T) {
	db := testDB(t)
	path := filepath.Join(t.TempDir(), "pipe.jsonl")
	body := strings.Join([]string{
		`{"type":"assistant","uuid":"p1","sessionId":"pipe","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_mem","name":"mcp__v2mem__search","input":{"query":"x"}}]}}`,
		`{"type":"user","uuid":"p2","sessionId":"pipe","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_mem","content":"[]"}]}}`,
		`{"type":"assistant","uuid":"p3","sessionId":"pipe","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls"}}]}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdImportTranscript([]string{"--db", db, "--file", path}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := transcriptSourceID("claude", "pipe")
	cases := map[string]bool{
		"tx:tool_use:toolu_mem":    false,
		"tx:tool_result:toolu_mem": false,
		"tx:tool_use:toolu_bash":   true,
	}
	for uid, want := range cases {
		rec, ok, err := st.LookupEvidence(uid, src, 0, 0)
		if err != nil || !ok {
			t.Fatalf("%s 应被归档: ok=%v err=%v", uid, ok, err)
		}
		if rec.ProposalEligible != want {
			t.Errorf("%s ProposalEligible=%v want %v", uid, rec.ProposalEligible, want)
		}
	}
	if obs, err := st.ListObservations("pipe", 10); err != nil || len(obs) != 3 {
		t.Fatalf("管道事件也必须归档，got %d err=%v", len(obs), err)
	}
}
