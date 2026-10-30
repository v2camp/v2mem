package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到 go.mod")
		}
		dir = parent
	}
}

func TestSandboxRecallIgnoresPersonalDB(t *testing.T) {
	home := os.Getenv("HOME")
	personal := filepath.Join(home, ".v2mem", "mem.db")
	_ = os.Remove(personal)
	root := moduleRoot(t)
	dir := filepath.Join(root, "eval", "sandbox")
	receipts := t.TempDir()
	out, err := captureStdout(t, func() error {
		return cmdEvalRecall([]string{"--sandbox", dir, "--k", "5", "--json", "--receipts", receipts})
	})
	if err != nil {
		t.Fatalf("sandbox recall: %v\n%s", err, out)
	}
	if _, err := os.Stat(personal); err == nil {
		t.Fatal("沙箱评测不应创建个人 mem.db")
	}
	for _, want := range []string{`"recall_at_k":1`, `"strict_all_at_k":1`, `"empty_rate":0`} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出应含 %s，got %s", want, out)
		}
	}
	ev, err := os.ReadFile(filepath.Join(receipts, "evidence.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ev), "临时记忆库") || strings.Contains(string(ev), `"content"`) {
		t.Fatalf("收据含正文: %s", ev)
	}
	man, err := os.ReadFile(filepath.Join(receipts, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(man), `"release_gate": true`) || !strings.Contains(string(man), "开发评测") {
		t.Fatalf("manifest: %s", man)
	}
	if _, err := os.Stat(filepath.Join(receipts, "scores.ndjson")); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxRejectsPersonalDBFlag(t *testing.T) {
	root := moduleRoot(t)
	err := cmdEvalRecall([]string{"--sandbox", filepath.Join(root, "eval", "sandbox"), "--db", testDB(t)})
	if err == nil || !strings.Contains(err.Error(), "不接受 --db") {
		t.Fatalf("应拒绝 --db，got %v", err)
	}
	err = cmdEvalRecall([]string{"--sandbox", filepath.Join(root, "eval", "sandbox"), "--gold", "x"})
	if err == nil || !strings.Contains(err.Error(), "不可同时") {
		t.Fatalf("应拒绝与 --gold 同用，got %v", err)
	}
}

func TestSandboxSeedDoesNotLeakObservationIntoMemorySearch(t *testing.T) {
	root := moduleRoot(t)
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := seedSandbox(st, filepath.Join(root, "eval", "sandbox", "seed.jsonl")); err != nil {
		t.Fatal(err)
	}
	mem, err := st.Search(store.SearchQuery{Query: "青柠标记", Scope: "all", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(mem) != 0 {
		t.Fatalf("默认检索不应返回合成观测: %+v", mem)
	}
	obs, err := st.Search(store.SearchQuery{Query: "青柠标记", Scope: "all", Layer: "observation", Limit: 5})
	if err != nil || len(obs) != 1 {
		t.Fatalf("观测层应能找到合成会话: %+v %v", obs, err)
	}
	listed, err := st.ListObservations("sandbox-sess-1", 5)
	if err != nil || len(listed) != 1 || listed[0].Channel != store.ChannelTool || listed[0].EventID != "sandbox-evt-1" {
		t.Fatalf("合成观测通道/事件号: %+v %v", listed, err)
	}
}

func TestAddAndSearchShareScrub(t *testing.T) {
	db := testDB(t)
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := cmdAdd([]string{"--db", db, "--project", "scrub", "--no-audit", "记住 api_key=supersecret 的位置"}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hits, err := st.List(store.ListQuery{Project: "scrub", Scope: "all", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || strings.Contains(hits[0].Content, "supersecret") || !strings.Contains(hits[0].Content, "[redacted]") {
		t.Fatalf("add 应打码: %+v", hits)
	}
	recs, err := st.Export()
	if err != nil {
		t.Fatal(err)
	}
	if recs[0].Channel != store.ChannelUser {
		t.Fatalf("CLI add 默认 channel=user，got %s", recs[0].Channel)
	}
	st.Close()

	if err := cmdAdd([]string{"--db", db, "--project", "scrub", "--no-scrub", "--no-audit", "--channel", "agent", "另一条 api_key=keepme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--project", "scrub", "--no-audit", "keepme"})
	}); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hits, err = st.List(store.ListQuery{Project: "scrub", Scope: "all", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, h := range hits {
		if strings.Contains(h.Content, "keepme") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("--no-scrub 应留下密钥: %+v", hits)
	}

	if _, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--audit-file", auditPath, "--no-fuzzy", "api_key=supersecret 位置"})
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "supersecret") || !strings.Contains(string(b), "[redacted]") {
		t.Fatalf("审计 query 应打码: %s", b)
	}
}

func TestMCPAddScrubsAndDefaultsAgent(t *testing.T) {
	s, _ := newMCPServer(t)
	mcpCall(t, s, "add", map[string]any{"content": "mcp api_key=supersecret 事实", "project": "mcpbox"})
	recs, err := s.st.Export()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Channel != store.ChannelAgent || strings.Contains(recs[0].Content, "supersecret") {
		t.Fatalf("MCP add 应 channel=agent 且打码: %+v", recs)
	}
	mcpCall(t, s, "search", map[string]any{"query": "token=abcdefg 事实", "project": "mcpbox"})
	b, err := os.ReadFile(s.auditFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "abcdefg") {
		t.Fatalf("MCP 审计 query 应打码: %s", b)
	}
}
