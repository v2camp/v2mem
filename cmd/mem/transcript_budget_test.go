package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
)

// 宿主格式一行一条，Reader 逐行消费：故「时间预算」在本测试里等价于
// 「处理到第几行」，预算置 0 就能稳定复现截断。
func writeTranscriptFixture(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b,
			`{"type":"user","sessionId":"s-budget","message":{"role":"user","content":"决策：第 %d 条"}}`+"\n", i)
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写 transcript 夹具: %v", err)
	}
	return path
}

func evidenceRowCount(t *testing.T, db string) int {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer st.Close()
	e := evidence.New(st)
	b, err := e.NextBundle(200, 1<<20)
	if err != nil || b == nil {
		t.Fatalf("NextBundle: %v", err)
	}
	refs, err := e.ListEvidence(b.ID)
	if err != nil {
		t.Fatalf("ListEvidence: %v", err)
	}
	return len(refs)
}

// TestTranscriptStopBudgetStopsAndResumes 是 R10 的必守回归：
// 撞预算时**必须**停在已完成的块边界并推进游标（否则下次原地重试、
// 永远跑不完），下一次 Stop 补齐剩余，且全程不重复写行。
func TestTranscriptStopBudgetStopsAndResumes(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mem.db")
	in := hookInput{SessionID: "s-budget", TranscriptPath: writeTranscriptFixture(t, 5)}

	old := transcriptStopBudget
	transcriptStopBudget = 0
	t.Cleanup(func() { transcriptStopBudget = old })

	rows, _ := captureTranscriptFailOpen(db, "claude", "demo", in, false)
	if rows != 1 {
		t.Fatalf("超预算应停在完成的第一条（游标必须前进），got %d", rows)
	}

	transcriptStopBudget = old
	rows, _ = captureTranscriptFailOpen(db, "claude", "demo", in, false)
	if rows != 4 {
		t.Fatalf("续传应补完剩余 4 条，got %d", rows)
	}
	if rows, _ := captureTranscriptFailOpen(db, "claude", "demo", in, false); rows != 0 {
		t.Fatalf("游标已到 EOF，不应再补写，got %d", rows)
	}
	if n := evidenceRowCount(t, db); n != 5 {
		t.Fatalf("去重后应恰好 5 条证据，got %d", n)
	}
}
