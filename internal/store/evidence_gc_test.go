package store

import (
	"testing"
	"time"
)

// seedArchive 写一条带归档的观测：观测与 evidence_archives 行同时落库，
// 与真实捕获链路（engine.capture）的关系一致。返回记录以便用例引用观测 id。
func seedArchive(t *testing.T, s *Store, uid, hash string) EvidenceRecord {
	t.Helper()
	rec, _, err := s.CaptureEvidence(EvidenceCaptureInput{
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
	return rec
}

// 只有「没有任何引用指向」的归档段才算孤儿：观测还在，段就必须留，
// 否则读侧会显示「有证据」却打不开原始记录。
// 夹具把序号推到已发窗（活跃序号范围为空）：未发窗的活跃段另有保护
// （TestUnreferencedArchivesProtectsUnbundledActiveRange），不在本用例口径内。
func TestUnreferencedArchivesOnlyReturnsSegmentsNoObservationPointsAt(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u1", "h1")
	seedArchive(t, s, "u2", "h2")

	if _, err := s.db.Exec(`DELETE FROM observations WHERE archive_hash = 'h2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE evidence_sources SET cursor_seq = last_seq`); err != nil {
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

// 引用保护扩集口径（docs/design-data-lifecycle §8.1）：memory_evidence.archive_hash
// 是最终记忆的证据链锚点。观测行被回收（或本机只有同步来的记忆证据链）后，
// 归档仍被它引用就绝不能进回收集合；无任何引用的段照常回收。
func TestUnreferencedArchivesProtectsMemoryEvidence(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u-mem", "h-mem")
	seedArchive(t, s, "u-free", "h-free")
	if _, err := s.db.Exec(`DELETE FROM observations`); err != nil {
		t.Fatal(err)
	}
	// 已发窗完毕（活跃序号范围为空）：把「memory_evidence 引用」这一个口径单独拎出来验。
	if _, err := s.db.Exec(`UPDATE evidence_sources SET cursor_seq = last_seq`); err != nil {
		t.Fatal(err)
	}
	memID := mustAdd(t, s, "被证据链钉住的记忆")
	if _, err := s.db.Exec(`INSERT INTO memory_evidence(
		memory_id, evidence_id, event_uid, archive_hash, raw_locator, grade, seq, occurred_at_ns, created_at
	) VALUES (?, 'ev-x', '', 'h-mem', '', 'C', 0, 0, ?)`, memID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	got, err := s.UnreferencedArchives(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("UnreferencedArchives: %v", err)
	}
	if len(got) != 1 || got[0].Hash != "h-free" {
		t.Fatalf("memory_evidence 引用的 h-mem 必须保护、无引用的 h-free 照常回收，got %+v", got)
	}
}

// 引用保护扩集口径：candidate_evidence.archive_hash 指向的归档不许回收。
// 候选可能长期 pending（甚至终态行还没到保留期），它的证据链不能被 GC 挖断——
// GCObservations 不看 candidate_evidence，观测被回收后归档只剩这一条引用。
func TestUnreferencedArchivesProtectsCandidateEvidence(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u-cand", "h-cand")
	if _, err := s.db.Exec(`DELETE FROM observations`); err != nil {
		t.Fatal(err)
	}
	// 已发窗完毕：只剩 candidate_evidence 一个引用方，验证的就是这一条口径。
	if _, err := s.db.Exec(`UPDATE evidence_sources SET cursor_seq = last_seq`); err != nil {
		t.Fatal(err)
	}
	cand, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "decision", Content: "决定：候选证据链必须保留", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO candidate_evidence(
		candidate_id, evidence_id, event_uid, source_id, source_seq, archive_hash,
		raw_record_hash, evidence_role, quote_hash, created_at
	) VALUES (?, 'ev-x', 'u-cand', 'src-a', 1, 'h-cand', '', 'direct', '', ?)`,
		cand.Candidate.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	got, err := s.UnreferencedArchives(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("UnreferencedArchives: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("candidate_evidence 引用的 h-cand 不该出现在集合里，got %+v", got)
	}
}

// 引用保护扩集口径：未 Bundle 化的活跃序号范围（per-source seq ∈ (cursor_seq, last_seq]）
// 对应的归档不许回收。观测行可能已被 GCObservations 按年龄回收，但这些序号还没发过
// Bundle——段是这些事件仅存的原始字节，删了就是永久丢证据（设计 §9「活跃范围可能被 GC」）。
func TestUnreferencedArchivesProtectsUnbundledActiveRange(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u-act", "h-active") // src-a seq 1，cursor_seq=0 → 未发窗
	if _, err := s.db.Exec(`DELETE FROM observations`); err != nil {
		t.Fatal(err)
	}

	got, err := s.UnreferencedArchives(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("UnreferencedArchives: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("未发窗活跃段 h-active 不该出现在集合里，got %+v", got)
	}
}

// 引用保护扩集口径：被 evidence_bundle_items 引用的观测对应归档不许回收。
// 观测行被回收后 item 仍钉着证据身份，读侧要能回放 Bundle 的原始字节；
// 完成 Bundle 的保留回收是后续切片的事，本切片宁可多保护。
func TestUnreferencedArchivesProtectsBundleItemEvidence(t *testing.T) {
	s := newTestStore(t)
	rec := seedArchive(t, s, "u-bun", "h-bun")
	// 真实发窗：NextEvidenceBundle 写 bundle + item 并把 cursor 推到 to_seq。
	b, err := s.NextEvidenceBundle(0, 0)
	if err != nil || b == nil {
		t.Fatalf("NextEvidenceBundle: %+v %v", b, err)
	}
	if _, err := s.db.Exec(`DELETE FROM observations`); err != nil {
		t.Fatal(err)
	}

	got, err := s.UnreferencedArchives(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("UnreferencedArchives: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("bundle item（%s）引用的 h-bun 不该出现在集合里，got %+v", rec.ID, got)
	}
}

// 候选行被 GC 删除时，edges.dst 指向它的行必须同事务清掉：edges.dst 没有外键
// （relaxEdgeDstFK 拆掉了 dst→memories），删了候选就留下指向空处的孤边。
// dst 指向非候选（如观测）的边不在回收范围，不许误伤。
func TestGCCandidatesLeavesNoOrphanEdges(t *testing.T) {
	s := newTestStore(t)
	cand, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "decision", Content: "决定：GC 要清 edges 对端", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.AcceptCandidate(cand.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "旁证"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO edges(src, dst, rel) VALUES (?, ?, 'derived_from')`,
		acc.MemoryID, obs.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE candidates SET decided_at=? WHERE id=?`, old, cand.Candidate.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GCCandidates(14*24*time.Hour, 90*24*time.Hour); err != nil {
		t.Fatalf("GCCandidates: %v", err)
	}
	var orphan int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE dst=?`, cand.Candidate.ID).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	if orphan != 0 {
		t.Fatalf("dst 指向被删候选的边应一并删除，剩 %d 条孤边", orphan)
	}
	var obsEdges int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE dst=?`, obs.ID).Scan(&obsEdges); err != nil {
		t.Fatal(err)
	}
	if obsEdges != 1 {
		t.Fatalf("dst 指向观测的边不该被清，got %d", obsEdges)
	}
}

// ReferencedArchives 是 UnreferencedArchives 的引用侧补集（不带写入保护期）：
// GCEvidence 删除前拿它做保留集回读校验，两侧必须互补成全量段。
func TestReferencedArchivesComplementsUnreferencedSet(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u-mem", "h-mem")
	seedArchive(t, s, "u-free", "h-free")
	if _, err := s.db.Exec(`DELETE FROM observations`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE evidence_sources SET cursor_seq = last_seq`); err != nil {
		t.Fatal(err)
	}
	memID := mustAdd(t, s, "钉住 h-mem")
	if _, err := s.db.Exec(`INSERT INTO memory_evidence(
		memory_id, evidence_id, event_uid, archive_hash, raw_locator, grade, seq, occurred_at_ns, created_at
	) VALUES (?, 'ev-x', '', 'h-mem', '', 'C', 0, 0, ?)`, memID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	kept, err := s.ReferencedArchives()
	if err != nil {
		t.Fatalf("ReferencedArchives: %v", err)
	}
	if len(kept) != 1 || kept[0].Hash != "h-mem" {
		t.Fatalf("保留集应恰为被引用的 h-mem，got %+v", kept)
	}
	gone, err := s.UnreferencedArchives(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("UnreferencedArchives: %v", err)
	}
	if len(gone) != 1 || gone[0].Hash != "h-free" {
		t.Fatalf("补集应恰为无引用的 h-free，got %+v", gone)
	}
}

// 引用反查的查询失败必须冒泡，绝不许当成「没有引用」放行删除。
func TestArchiveQueriesSurfaceDBFailure(t *testing.T) {
	s := newTestStore(t)
	s.Close()
	if _, err := s.UnreferencedArchives(1); err == nil {
		t.Fatal("UnreferencedArchives 查询失败应报错")
	}
	if _, err := s.ReferencedArchives(); err == nil {
		t.Fatal("ReferencedArchives 查询失败应报错")
	}
}
