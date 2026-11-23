package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

// seedArchivedEvent 在库与磁盘上同时造一段证据：真实链路也是「先落文件、再写账」。
func seedArchivedEvent(t *testing.T, st *store.Store, archDir, uid, hash string) {
	t.Helper()
	path := filepath.Join(archDir, hash+".jsonl.gz")
	if err := os.WriteFile(path, []byte("归档段 "+uid), 0o600); err != nil {
		t.Fatalf("写归档文件: %v", err)
	}
	if _, _, err := st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-a", SourceKind: "hook",
		OccurredAtNS: time.Now().UnixNano(), SessionID: "sess-1",
		Project: "proj", Harness: "workbuddy", EventType: "user-prompt",
		Channel: "user", ParserVersion: "evidence/v1", ProposalEligible: true,
		Summary: "摘要 " + uid, ArchiveHash: hash, ArchivePath: path,
		RawRecordHash: "rh-" + uid, RawHashConcat: "rh-" + uid,
		ByteSize: 1024, RawCount: 1,
	}); err != nil {
		t.Fatalf("CaptureEvidence(%s): %v", uid, err)
	}
}

func setupArchiveDB(t *testing.T) (db, archDir string) {
	t.Helper()
	db = testDB(t)
	archDir = filepath.Join(filepath.Dir(db), "evidence", "archives")
	if err := os.MkdirAll(archDir, 0o700); err != nil {
		t.Fatalf("建归档目录: %v", err)
	}
	return db, archDir
}

// 只回收「没有观测指向」的段：账本行与磁盘文件必须一起消失，
// 否则会留下「账本说没有、文件还在」的残渣。
func TestCmdGCEvidenceReclaimsArchivesAndDiskFiles(t *testing.T) {
	db, archDir := setupArchiveDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	seedArchivedEvent(t, st, archDir, "u1", strings.Repeat("1", 64))
	seedArchivedEvent(t, st, archDir, "u2", strings.Repeat("2", 64))
	// 只留一条观测，另一条的归档成为孤儿。
	if _, err := st.GCObservations(0, 1); err != nil {
		t.Fatalf("GCObservations: %v", err)
	}
	st.Close()

	// 保护期按秒比较，等一秒才进得了回收窗口。
	time.Sleep(1100 * time.Millisecond)

	out, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--evidence", "--evidence-grace", "1ms"})
	})
	if err != nil {
		t.Fatalf("cmdGC --evidence: %v", err)
	}
	if !strings.Contains(out, "已回收归档段 1") {
		t.Fatalf("应报回收 1 段，输出: %q", out)
	}
	entries, err := os.ReadDir(archDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("归档目录应只剩 1 个段，got %d", len(entries))
	}
}

// --obs 与 --evidence 合跑的顺序是「先观测、后归档」：被删观测对应的段
// 这一轮才成为孤儿。反过来跑就要等下一次，等于多跑一遍。
func TestCmdGCObsThenEvidenceInOneRun(t *testing.T) {
	db, archDir := setupArchiveDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	seedArchivedEvent(t, st, archDir, "u1", strings.Repeat("1", 64))
	seedArchivedEvent(t, st, archDir, "u2", strings.Repeat("2", 64))
	st.Close()

	time.Sleep(1100 * time.Millisecond)

	out, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--obs", "--max-age", "1ms", "--evidence", "--evidence-grace", "1ms"})
	})
	if err != nil {
		t.Fatalf("cmdGC --obs --evidence: %v", err)
	}
	if !strings.Contains(out, "已回收观测 2") {
		t.Fatalf("应先回收 2 条观测，输出: %q", out)
	}
	if !strings.Contains(out, "已回收归档段 2") {
		t.Fatalf("同一轮应回收 2 段归档，输出: %q", out)
	}
	if !strings.Contains(out, "空转证据源 1") {
		t.Fatalf("无归档无观测的证据源应被回收，输出: %q", out)
	}
	entries, err := os.ReadDir(archDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("归档目录应被清空，还剩 %d 个文件", len(entries))
	}
}
