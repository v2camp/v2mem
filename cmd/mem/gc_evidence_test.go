package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

// seedArchivedEvent 在库与磁盘上同时造一段证据：真实链路也是「先落文件、再写账」。
// 段内容是合法 gzip JSONL、archive_hash 取压缩字节 sha256（与 writeArchiveRecords
// 同口径）——GCEvidence 的保留集回读校验按这个口径验，假文件会被当成坏段拒删。
func seedArchivedEvent(t *testing.T, st *store.Store, archDir, uid string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	raw := base64.StdEncoding.EncodeToString([]byte("原始字节 " + uid))
	if _, err := fmt.Fprintf(zw, `{"v":1,"seq":1,"event_uid":%q,"raw":%q}`+"\n", uid, raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	body := buf.Bytes()
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(archDir, hash+".jsonl.gz")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("写归档文件: %v", err)
	}
	if _, _, err := st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-a", SourceKind: "hook",
		OccurredAtNS: time.Now().UnixNano(), SessionID: "sess-1",
		Project: "proj", Harness: "workbuddy", EventType: "user-prompt",
		Channel: "user", ParserVersion: "evidence/v1", ProposalEligible: true,
		Summary: "摘要 " + uid, ArchiveHash: hash, ArchivePath: path,
		RawRecordHash: "rh-" + uid, RawHashConcat: "rh-" + uid,
		ByteSize: int64(len(body)), RawCount: 1,
	}); err != nil {
		t.Fatalf("CaptureEvidence(%s): %v", uid, err)
	}
}

// markDispatched 把证据源游标推到已发窗（Bundle 行已按保留期回收的库况）：
// 未发窗的活跃段受引用保护不回收，本组用例验的是「孤儿段照常回收」。
func markDispatched(t *testing.T, db string) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE evidence_sources SET cursor_seq = last_seq`); err != nil {
		t.Fatalf("推进游标: %v", err)
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

// 只回收「没有引用指向」的段：账本行与磁盘文件必须一起消失，
// 否则会留下「账本说没有、文件还在」的残渣。
func TestCmdGCEvidenceReclaimsArchivesAndDiskFiles(t *testing.T) {
	db, archDir := setupArchiveDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	seedArchivedEvent(t, st, archDir, "u1")
	seedArchivedEvent(t, st, archDir, "u2")
	// 先发窗再回收观测：未发窗的观测受引用保护，GCObservations 不会掏它。
	markDispatched(t, db)
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
	seedArchivedEvent(t, st, archDir, "u1")
	seedArchivedEvent(t, st, archDir, "u2")
	st.Close()
	markDispatched(t, db)

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
