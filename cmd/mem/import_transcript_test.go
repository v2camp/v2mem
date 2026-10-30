package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
