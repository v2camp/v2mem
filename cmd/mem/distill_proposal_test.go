package main

import (
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
)

func seedEvidenceBundle(t *testing.T, db string, n int) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < n; i++ {
		uid := "cmd-uid-" + string(rune('a'+i))
		if _, _, err := st.CaptureEvidence(store.EvidenceCaptureInput{
			EventUID: uid, SourceID: "src-cmd", SourceKind: "hook", Generation: 1,
			OccurredAtNS: 1700000000000000000, SessionID: "sess-cmd", Project: "demo",
			Harness: "workbuddy", EventType: "stop", ParserVersion: "evidence/v1",
			ProposalEligible: true, Summary: "决定：CLI 也要走 Bundle",
			ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
			RawRecordHash: "rh-" + uid, ByteSize: 16, RawCount: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ev := evidence.New(st)
	b, err := ev.NextBundle(50, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("应生成 Bundle")
	}
	return b.ID
}

// 无 provider 时 CLI 必须报 backlog；不能把「没跑」显示成「没有事实」。
func TestDistillProposalNoProviderIsBacklog(t *testing.T) {
	db := testDB(t)
	bundleID := seedEvidenceBundle(t, db, 1)

	out, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--proposal", "--executor", "none",
			"--json", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONOut(t, out)
	if got["status"] != "backlog" || num(got, "inserted") != 0 || num(got, "bundles") != 1 {
		t.Fatalf("无 provider 应为 backlog: %v\n%s", got, out)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, err := st.LoadProposalBundle(bundleID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "backlog" || b.Attempt != 1 {
		t.Fatalf("Bundle 应记 backlog/attempt=1: %+v", b)
	}
	if list, _ := st.ListCandidates("", "sess-cmd", 10); len(list) != 0 {
		t.Fatalf("backlog 不得落候选: %+v", list)
	}
	// backlog 不是失败：--strict 也不该非零退出。
	if err := cmdDistill([]string{"--db", db, "--proposal", "--executor", "none",
		"--strict", "--audit-file", "-"}); err != nil {
		t.Fatalf("backlog 不应让 --strict 失败: %v", err)
	}
}

func TestDistillProposalExplicitBundle(t *testing.T) {
	db := testDB(t)
	bundleID := seedEvidenceBundle(t, db, 2)
	out, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--bundle", bundleID, "--executor", "none",
			"--json", "--audit-file", "-"})
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONOut(t, out)
	if got["status"] != "backlog" || num(got, "bundles") != 1 {
		t.Fatalf("显式 bundle 应只处理这一窗: %v", got)
	}
	if !strings.Contains(out, bundleID) {
		t.Fatalf("结果应带 bundle id，got %s", out)
	}
}

func TestDistillProposalArgumentErrors(t *testing.T) {
	db := testDB(t)
	if err := cmdDistill([]string{"--db", db, "--proposal", "--dry-run"}); err == nil ||
		!strings.Contains(err.Error(), "--bundle") {
		t.Fatalf("dry-run 无 bundle 应拒绝: %v", err)
	}
	if err := cmdDistill([]string{"--db", db, "--proposal", "--executor", "nope"}); err == nil {
		t.Fatal("未知 executor 应报错")
	}
	if err := cmdDistill([]string{"--db", db, "--bundle", "bnd-missing", "--executor", "none"}); err == nil {
		t.Fatal("未知 Bundle 应报错")
	}
	// 有新证据时才走到 executor；空库直接说明没有窗口。
	empty, err := captureStdout(t, func() error {
		return cmdDistill([]string{"--db", db, "--proposal", "--executor", "none", "--audit-file", "-"})
	})
	if err != nil || !strings.Contains(empty, "没有待处理的证据窗口") {
		t.Fatalf("空库应说明无窗口，got %q %v", empty, err)
	}
}
