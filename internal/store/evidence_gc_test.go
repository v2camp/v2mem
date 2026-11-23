package store

import (
	"testing"
	"time"
)

// seedArchive 写一条带归档的观测：观测与 evidence_archives 行同时落库，
// 与真实捕获链路（engine.capture）的关系一致。
func seedArchive(t *testing.T, s *Store, uid, hash string) {
	t.Helper()
	_, _, err := s.CaptureEvidence(EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-a", SourceKind: "hook",
		OccurredAtNS: time.Now().UnixNano(), SessionID: "sess-1",
		Project: "proj", Harness: "workbuddy", EventType: "user-prompt",
		Channel: "user", ParserVersion: "evidence/v1", ProposalEligible: true,
		Summary:       "摘要 " + uid,
		ArchiveHash:   hash,
		ArchivePath:   "/tmp/" + hash + ".jsonl.gz",
		RawRecordHash: "rh-" + uid, RawHashConcat: "rh-" + uid,
		ByteSize: 128, RawCount: 1,
	})
	if err != nil {
		t.Fatalf("CaptureEvidence(%s): %v", uid, err)
	}
}

// 只有「没有任何观测指向」的归档段才算孤儿：观测还在，段就必须留，
// 否则读侧会显示「有证据」却打不开原始记录。
func TestUnreferencedArchivesOnlyReturnsSegmentsNoObservationPointsAt(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u1", "h1")
	seedArchive(t, s, "u2", "h2")

	if _, err := s.db.Exec(`DELETE FROM observations WHERE archive_hash = 'h2'`); err != nil {
		t.Fatal(err)
	}

	got, err := s.UnreferencedArchives(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("UnreferencedArchives: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("孤儿段 = %d，期望 1（h2）: %+v", len(got), got)
	}
	if got[0].Hash != "h2" || got[0].RawCount != 1 || got[0].ByteSize != 128 {
		t.Fatalf("孤儿段字段不对: %+v", got[0])
	}
}

// cutoff 是写入保护期：刚写入的段不参与回收，避免和正在提交的捕获撞车
// （真实顺序是「先落归档文件，再写账」，中间那一瞬段是孤儿的）。
func TestUnreferencedArchivesRespectsWriteWindow(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u1", "h1")
	if _, err := s.db.Exec(`DELETE FROM observations WHERE archive_hash = 'h1'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnreferencedArchives(0) // 1970 之前创建的段才回收 → 一条都不该有
	if err != nil {
		t.Fatalf("UnreferencedArchives: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("保护期内不该回收，got %+v", got)
	}
}

func TestDeleteArchivedSegmentsRemovesLedgerRows(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u1", "h1")
	seedArchive(t, s, "u2", "h2")

	n, err := s.DeleteArchivedSegments([]string{"h1", "h2", "h404"})
	if err != nil {
		t.Fatalf("DeleteArchivedSegments: %v", err)
	}
	if n != 2 {
		t.Fatalf("删除行数 = %d，期望 2（h404 不存在不算）", n)
	}
	var left int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM evidence_archives`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("账本仍有 %d 行", left)
	}
}

func TestArchiveExistsTellsLedgerFromDiskResidue(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u1", "h1")

	ok, err := s.ArchiveExists("h1")
	if err != nil || !ok {
		t.Fatalf("h1 在账本里: ok=%v err=%v", ok, err)
	}
	ok, err = s.ArchiveExists("disk-only")
	if err != nil || ok {
		t.Fatalf("disk-only 不在账本里: ok=%v err=%v", ok, err)
	}
}

// 证据源是序号账本：只有当它既没有归档、也没有观测时才可删，
// 否则下一次捕获会从 seq=1 重新分配，和留存的观测撞号。
func TestDeleteEmptyEvidenceSourcesKeepsSourcesWithHistory(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u1", "h1")
	seedArchive(t, s, "u2", "h2")

	// 只清掉 h1 的观测与归档：src-a 仍有 h2 的历史，不能被删。
	if _, err := s.db.Exec(`DELETE FROM observations WHERE archive_hash = 'h1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM evidence_archives WHERE archive_hash = 'h1'`); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteEmptyEvidenceSources(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("DeleteEmptyEvidenceSources: %v", err)
	}
	if n != 0 {
		t.Fatalf("有历史的证据源不该被删，got %d", n)
	}

	// 全部清空后才允许删。
	if _, err := s.db.Exec(`DELETE FROM observations`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM evidence_archives`); err != nil {
		t.Fatal(err)
	}
	n, err = s.DeleteEmptyEvidenceSources(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("DeleteEmptyEvidenceSources: %v", err)
	}
	if n != 1 {
		t.Fatalf("空转证据源应被删 1 条，got %d", n)
	}
}
