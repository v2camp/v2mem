package main

// S6c：`mem distill --gaps` 的只读出口与 stats 的洞计数。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

func captureGapFixture(t *testing.T) string {
	t.Helper()
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		uid := "uid-" + string(rune('0'+i))
		if _, _, err := st.CaptureEvidence(store.EvidenceCaptureInput{
			EventUID: uid, SourceID: "src-gap", SourceKind: "hook", Generation: 1,
			OccurredAtNS: 1700000000000000000, SessionID: "sess-1", EventType: "post-tool-use",
			ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
			RawHashConcat: "rh-" + uid, ByteSize: 8, RawCount: 1,
		}); err != nil {
			t.Fatalf("CaptureEvidence(%s): %v", uid, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, db)
	if _, err := raw.Exec(`DELETE FROM observations WHERE event_uid='uid-2'`); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	// 第一窗发 0..1，第二窗记录 [2,2] 并把 cursor 推到 2、继续发 3。
	for i := 0; i < 2; i++ {
		if _, err := st.NextEvidenceBundle(10, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCmdDistillGapsTextAndJSON(t *testing.T) {
	db := captureGapFixture(t)

	out, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--gaps", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatalf("cmdDistill --gaps: %v", err)
	}
	for _, want := range []string{"source=src-gap", "seq=2..2"} {
		if !strings.Contains(out, want) {
			t.Errorf("文本应含 %q，got:\n%s", want, out)
		}
	}

	jsonOut, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--gaps", "--json", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatalf("cmdDistill --gaps --json: %v", err)
	}
	var gaps []struct {
		ID         string `json:"id"`
		SourceID   string `json:"source_id"`
		Generation int    `json:"generation"`
		FromSeq    int    `json:"from_seq"`
		ToSeq      int    `json:"to_seq"`
		DetectedAt int64  `json:"detected_at"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &gaps); err != nil {
		t.Fatalf("解析 gaps JSON: %v\n%s", err, jsonOut)
	}
	if len(gaps) != 1 {
		t.Fatalf("应有一条洞: %+v", gaps)
	}
	g := gaps[0]
	if g.ID == "" || g.SourceID != "src-gap" || g.Generation != 1 ||
		g.FromSeq != 2 || g.ToSeq != 2 || g.DetectedAt == 0 {
		t.Fatalf("洞字段不完整: %+v", g)
	}

	empty := testDB(t)
	emptyOut, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", empty, "--gaps", "--audit-file", "-"})
	})
	if err != nil || !strings.Contains(emptyOut, "没有序号洞") {
		t.Fatalf("空库应明确「没有」: %q err=%v", emptyOut, err)
	}

	// 洞不是 Bundle：回捞与其它运维出口都不适用。
	for _, args := range [][]string{
		{"--db", db, "--gaps", "--requeue", "bnd-x", "--audit-file", "-"},
		{"--db", db, "--gaps", "--quarantined", "--audit-file", "-"},
		{"--db", db, "--gaps", "--session", "sess-1", "--audit-file", "-"},
	} {
		if err := cmdDistill(args); err == nil {
			t.Errorf("互斥参数应报错: %v", args)
		}
	}
}

func TestCmdStatsCountsEvidenceGaps(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawDB(t, db)
	if _, err := raw.Exec(`INSERT INTO evidence_gaps(id, source_id, generation, from_seq, to_seq, detected_at)
		VALUES('gap_test','src-gap',1,2,2,?)`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error { return cmdStats([]string{"--db", db}) })
	if err != nil {
		t.Fatalf("cmdStats: %v", err)
	}
	for _, want := range []string{"洞 1", "序号缺失已跳过，记录在案"} {
		if !strings.Contains(out, want) {
			t.Errorf("文本应含 %q，got:\n%s", want, out)
		}
	}

	jsonOut, err := captureStdout(t, func() error { return cmdStats([]string{"--db", db, "--json"}) })
	if err != nil {
		t.Fatalf("cmdStats --json: %v", err)
	}
	var top struct {
		Backlog struct {
			EvidenceGaps int `json:"evidence_gaps"`
		} `json:"backlog"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &top); err != nil {
		t.Fatalf("解析 stats JSON: %v\n%s", err, jsonOut)
	}
	if top.Backlog.EvidenceGaps != 1 {
		t.Fatalf("JSON evidence_gaps 应为 1: %+v", top.Backlog)
	}
}
