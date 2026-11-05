package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/commit"
)

// ---------- T3 提交治理与证据链 ----------

func captureEvidence(t *testing.T, s *Store, uid, session, project, summary string) EvidenceRecord {
	t.Helper()
	rec, _, err := s.CaptureEvidence(EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-" + project, SourceKind: "hook",
		ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
		SessionID: session, Project: project, EventType: "post-tool-use",
		Summary: summary, OccurredAtNS: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("CaptureEvidence(%s): %v", uid, err)
	}
	return rec
}

func attachCandidateEvidence(t *testing.T, s *Store, candidateID, evidenceID, eventUID, archiveHash, role string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO candidate_evidence(
		candidate_id, evidence_id, event_uid, source_id, source_seq, archive_hash,
		evidence_role, created_at
	) VALUES (?,?,?,?,?,?,?,?)`,
		candidateID, evidenceID, eventUID, "src-p", 1, archiveHash, role, time.Now().Unix()); err != nil {
		t.Fatalf("insert candidate_evidence: %v", err)
	}
}

func newCandidate(t *testing.T, s *Store, kind, project, content string) *CandidateResult {
	t.Helper()
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: project, Kind: kind, Content: content, Source: "distill",
	})
	if err != nil {
		t.Fatalf("InsertCandidate: %v", err)
	}
	return res
}

func countMemoryEvidence(t *testing.T, s *Store, memoryID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_evidence WHERE memory_id = ?`, memoryID).Scan(&n); err != nil {
		t.Fatalf("count memory_evidence: %v", err)
	}
	return n
}

func memoryAdoption(t *testing.T, s *Store, memoryID string) (int, int64) {
	t.Helper()
	var n int
	var at *int64
	if err := s.db.QueryRow(`SELECT adopted_count, last_adopted_at FROM memories WHERE id = ?`, memoryID).
		Scan(&n, &at); err != nil {
		t.Fatalf("read adopted_count: %v", err)
	}
	if at == nil {
		return n, 0
	}
	return n, *at
}

func TestCommitAcceptWritesEvidenceProvenanceAndDecision(t *testing.T) {
	s := newTestStore(t)
	ev := captureEvidence(t, s, "uid-1", "s", "p", "构建失败：缺少 go.sum")
	res := newCandidate(t, s, "fact", "p", "记忆库必须放用户目录")
	attachCandidateEvidence(t, s, res.Candidate.ID, ev.ID, ev.EventUID, ev.ArchiveHash, "direct")

	view, err := s.CommitCandidate(res.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.CommitAccept(res.Candidate.ID, view.Revision, commit.ActorHuman)
	if err != nil {
		t.Fatalf("CommitAccept: %v", err)
	}
	if !out.Committed || out.Already || out.MemoryID == "" {
		t.Fatalf("首次采纳结果异常: %+v", out)
	}
	if out.EvidenceCount != 1 || out.Action != commit.ActionHumanAccept {
		t.Fatalf("动作/证据数不对: %+v", out)
	}
	if n := countMemoryEvidence(t, s, out.MemoryID); n != 1 {
		t.Fatalf("应写 1 条 memory_evidence，got %d", n)
	}
	var grade, uid, prov string
	if err := s.db.QueryRow(`SELECT grade, event_uid FROM memory_evidence WHERE memory_id = ?`, out.MemoryID).
		Scan(&grade, &uid); err != nil {
		t.Fatal(err)
	}
	if grade != string(commit.GradeA) || uid != ev.EventUID {
		t.Fatalf("等级/身份应来自真实观测，got grade=%s uid=%s", grade, uid)
	}
	if err := s.db.QueryRow(`SELECT provenance FROM memories WHERE id = ?`, out.MemoryID).Scan(&prov); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(prov) == "" || !strings.Contains(prov, "A级") {
		t.Fatalf("provenance 必须非空且反映证据等级，got %q", prov)
	}
	if n, _ := memoryAdoption(t, s, out.MemoryID); n != 1 {
		t.Fatalf("首次采纳 adopted_count 应为 1，got %d", n)
	}
	links, err := s.EvidenceLinks(out.MemoryID)
	if err != nil || len(links) != 1 || links[0].Grade != commit.GradeA {
		t.Fatalf("EvidenceLinks 应读到 1 条 A 级，got %+v %v", links, err)
	}
	decisions, err := s.CommitDecisions(res.Candidate.ID)
	if err != nil || len(decisions) != 1 || decisions[0].Action != commit.ActionHumanAccept {
		t.Fatalf("应记录 1 条裁决，got %+v %v", decisions, err)
	}
	if decisions[0].IdempotencyKey == "" || decisions[0].MemoryID != out.MemoryID {
		t.Fatalf("裁决缺少幂等键/记忆 id: %+v", decisions[0])
	}
}

func TestCommitAcceptReplayOnlyRepairsLinks(t *testing.T) {
	s := newTestStore(t)
	ev1 := captureEvidence(t, s, "uid-r1", "s", "p", "证据一")
	ev2 := captureEvidence(t, s, "uid-r2", "s", "p", "证据二")
	res := newCandidate(t, s, "fact", "p", "重放不重复计数")
	attachCandidateEvidence(t, s, res.Candidate.ID, ev1.ID, ev1.EventUID, ev1.ArchiveHash, "direct")
	view, _ := s.CommitCandidate(res.Candidate.ID)
	first, err := s.CommitAccept(res.Candidate.ID, view.Revision, commit.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM memory_evidence WHERE memory_id = ?`, first.MemoryID); err != nil {
		t.Fatal(err)
	}
	// 重放前补一条新证据：重放必须把缺失的引用补回来，但不重复计采纳。
	attachCandidateEvidence(t, s, res.Candidate.ID, ev2.ID, ev2.EventUID, ev2.ArchiveHash, "context")

	view, err = s.CommitCandidate(res.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CommitAccept(res.Candidate.ID, view.Revision, commit.ActorHuman)
	if err != nil {
		t.Fatalf("重放: %v", err)
	}
	if !again.Already || again.MemoryID != first.MemoryID {
		t.Fatalf("重放应幂等复用同一条记忆，got %+v", again)
	}
	if n := countMemoryEvidence(t, s, first.MemoryID); n != 2 {
		t.Fatalf("重放应补回并新增证据，got %d", n)
	}
	if n, _ := memoryAdoption(t, s, first.MemoryID); n != 1 {
		t.Fatalf("重放不得增加 adopted_count，got %d", n)
	}
	var nMemories int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&nMemories); err != nil {
		t.Fatal(err)
	}
	if nMemories != 1 {
		t.Fatalf("重放不得新建记忆，got %d", nMemories)
	}
}

func TestCommitAcceptLinksExistingMemoryByHashProject(t *testing.T) {
	s := newTestStore(t)
	existing, err := s.Add(AddInput{Content: "同一事实只留一行", Project: "p", Kind: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	ev := captureEvidence(t, s, "uid-link", "s", "p", "已有记忆的证据")
	res := newCandidate(t, s, "fact", "p", "同一事实只留一行")
	attachCandidateEvidence(t, s, res.Candidate.ID, ev.ID, ev.EventUID, ev.ArchiveHash, "direct")

	view, _ := s.CommitCandidate(res.Candidate.ID)
	out, err := s.CommitAccept(res.Candidate.ID, view.Revision, commit.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	if out.MemoryID != existing.ID || out.CreatedMemory {
		t.Fatalf("同 (content_hash,project) 应链接已有记忆，got %+v want %s", out, existing.ID)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("不得新建第二条记忆，got %d", n)
	}
	if countMemoryEvidence(t, s, existing.ID) != 1 {
		t.Fatal("证据必须挂到已有记忆上")
	}
	if n, _ := memoryAdoption(t, s, existing.ID); n != 1 {
		t.Fatalf("首次采纳已有记忆应计一次，got %d", n)
	}
}

func TestCommitAcceptConcurrentRevisionCASHasSingleWinner(t *testing.T) {
	s := newTestStore(t)
	res := newCandidate(t, s, "fact", "p", "并发只能有一个赢家")
	view, err := s.CommitCandidate(res.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]*CommitResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.CommitAccept(res.Candidate.ID, view.Revision, commit.ActorHuman)
		}(i)
	}
	wg.Wait()

	winners, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		switch {
		case errs[i] == nil && results[i] != nil && results[i].Committed && !results[i].Already:
			winners++
		case errors.Is(errs[i], ErrRevisionConflict):
			conflicts++
		default:
			t.Fatalf("并发结果既不是赢家也不是 revision 冲突: res=%+v err=%v", results[i], errs[i])
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("应恰好 1 赢 1 冲突，got winners=%d conflicts=%d", winners, conflicts)
	}
	var nMemories int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&nMemories); err != nil {
		t.Fatal(err)
	}
	if nMemories != 1 {
		t.Fatalf("并发只能建 1 条记忆，got %d", nMemories)
	}
	var memoryID string
	if err := s.db.QueryRow(`SELECT memory_id FROM candidates WHERE id = ?`, res.Candidate.ID).Scan(&memoryID); err != nil {
		t.Fatal(err)
	}
	if n, _ := memoryAdoption(t, s, memoryID); n != 1 {
		t.Fatalf("并发下 adopted_count 也只能加一次，got %d", n)
	}
}

func TestCommitRejectCASAndIdempotentReplay(t *testing.T) {
	s := newTestStore(t)
	res := newCandidate(t, s, "pitfall", "p", "拒绝也要 CAS")
	view, _ := s.CommitCandidate(res.Candidate.ID)
	out, err := s.CommitReject(res.Candidate.ID, view.Revision, commit.ActorHuman, "noise")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Committed || out.Candidate.Status != "rejected" {
		t.Fatalf("拒绝结果异常: %+v", out)
	}
	after, err := s.CommitCandidate(res.Candidate.ID)
	if err != nil || after.Revision != view.Revision+1 {
		t.Fatalf("拒绝应推进 revision，got %+v %v", after, err)
	}
	var memories int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if memories != 0 {
		t.Fatalf("拒绝不得写 memories，got %d", memories)
	}

	// 过期 revision 必须被挡住。
	if _, err := s.CommitReject(res.Candidate.ID, view.Revision, commit.ActorHuman, ""); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("过期 revision 应冲突，got %v", err)
	}
	// 当前 revision 重放幂等。
	cur, _ := s.CommitCandidate(res.Candidate.ID)
	again, err := s.CommitReject(res.Candidate.ID, cur.Revision, commit.ActorHuman, "")
	if err != nil || !again.Already || again.Candidate.Status != "rejected" {
		t.Fatalf("重复拒绝应幂等，got %+v %v", again, err)
	}
	decisions, err := s.CommitDecisions(res.Candidate.ID)
	if err != nil || len(decisions) == 0 {
		t.Fatalf("拒绝应记录裁决，got %+v %v", decisions, err)
	}
	if decisions[0].Action != commit.ActionReject {
		t.Fatalf("裁决动作应为 reject，got %s", decisions[0].Action)
	}
}

func TestCommitAcceptRejectsNonPendingCandidate(t *testing.T) {
	s := newTestStore(t)
	res := newCandidate(t, s, "task", "p", "拒绝后不能采纳")
	if _, err := s.RejectCandidate(res.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.CommitCandidate(res.Candidate.ID)
	if _, err := s.CommitAccept(res.Candidate.ID, cur.Revision, commit.ActorHuman); !errors.Is(err, ErrCandidateClosed) {
		t.Fatalf("已裁决候选应拒绝采纳，got %v", err)
	}
}

func TestCommitAutoPolicyMatrixInStore(t *testing.T) {
	s := newTestStore(t)
	ev := captureEvidence(t, s, "uid-auto", "s", "p", "构建日志：ok")
	fact := newCandidate(t, s, "fact", "p", "自动提交需要 A 级直接证据")
	attachCandidateEvidence(t, s, fact.Candidate.ID, ev.ID, ev.EventUID, ev.ArchiveHash, "direct")
	view, _ := s.CommitCandidate(fact.Candidate.ID)
	out, err := s.AutoCommitCandidate(fact.Candidate.ID, view.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != commit.ActionAutoCommit || !out.Committed || out.Actor != commit.ActorAuto {
		t.Fatalf("fact + A 级应自动提交，got %+v", out)
	}

	// 人工类型：只记裁决，不写记忆。
	dec := newCandidate(t, s, "decision", "p", "这类判断只能人来定")
	view, _ = s.CommitCandidate(dec.Candidate.ID)
	out, err = s.AutoCommitCandidate(dec.Candidate.ID, view.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != commit.ActionHumanAccept || out.Committed {
		t.Fatalf("decision 不得自动提交，got %+v", out)
	}
	var memories int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if memories != 1 {
		t.Fatalf("只有 fact 应落记忆，got %d", memories)
	}
	decisions, err := s.CommitDecisions(dec.Candidate.ID)
	if err != nil || len(decisions) != 1 || decisions[0].Action != commit.ActionHumanAccept {
		t.Fatalf("人工类型应留下裁决记录，got %+v %v", decisions, err)
	}

	// pitfall 缺 resolution：defer。
	pit := newCandidate(t, s, "pitfall", "p", "报错：只有错误没有解法")
	attachCandidateEvidence(t, s, pit.Candidate.ID, ev.ID, ev.EventUID, ev.ArchiveHash, "error")
	view, _ = s.CommitCandidate(pit.Candidate.ID)
	out, err = s.AutoCommitCandidate(pit.Candidate.ID, view.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != commit.ActionDefer {
		t.Fatalf("pitfall 缺 resolution 应 defer，got %+v", out)
	}
}

func TestCommitEvidenceLinksBatch(t *testing.T) {
	s := newTestStore(t)
	ev1 := captureEvidence(t, s, "uid-b1", "s", "p", "批量一")
	ev2 := captureEvidence(t, s, "uid-b2", "s", "p", "批量二")
	a := newCandidate(t, s, "fact", "p", "批量记忆甲")
	attachCandidateEvidence(t, s, a.Candidate.ID, ev1.ID, ev1.EventUID, ev1.ArchiveHash, "direct")
	b := newCandidate(t, s, "fact", "p", "批量记忆乙")
	attachCandidateEvidence(t, s, b.Candidate.ID, ev2.ID, ev2.EventUID, ev2.ArchiveHash, "direct")
	va, _ := s.CommitCandidate(a.Candidate.ID)
	ra, err := s.CommitAccept(a.Candidate.ID, va.Revision, commit.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	vb, _ := s.CommitCandidate(b.Candidate.ID)
	rb, err := s.CommitAccept(b.Candidate.ID, vb.Revision, commit.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.EvidenceLinksBatch([]string{ra.MemoryID, rb.MemoryID, "missing", ra.MemoryID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("每个请求 id 都应有键，got %d", len(got))
	}
	if len(got[ra.MemoryID]) != 1 || len(got[rb.MemoryID]) != 1 {
		t.Fatalf("批量应各取到 1 条，got %+v", got)
	}
	if links, ok := got["missing"]; !ok || len(links) != 0 {
		t.Fatalf("无证据的记忆应返回空切片而非缺键，got %+v", got)
	}
	empty, err := s.EvidenceLinksBatch(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空输入应返回空 map，got %+v %v", empty, err)
	}
	if links, err := s.EvidenceLinks(""); err != nil || links != nil {
		t.Fatalf("空 memory id 应返回 nil，got %+v %v", links, err)
	}
}

func TestGCObservationsKeepsEvidenceReferenced(t *testing.T) {
	s := newTestStore(t)
	kept := captureEvidence(t, s, "uid-keep", "s", "p", "被记忆引用的观测")
	gone := captureEvidence(t, s, "uid-gone", "s", "p", "无引用的观测")
	old := time.Now().Add(-48 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE observations SET created_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	mem, err := s.Add(AddInput{Content: "有证据的记忆", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO memory_evidence(
		memory_id, evidence_id, event_uid, archive_hash, raw_locator, grade, seq, occurred_at_ns, created_at
	) VALUES (?,?,?,?,?,?,?,?,?)`,
		mem.ID, kept.ID, kept.EventUID, kept.ArchiveHash, kept.RawLocator, "A", kept.Seq, kept.OccurredAtNS, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	res, err := s.GCObservations(24*time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	var keptN, goneN int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE id = ?`, kept.ID).Scan(&keptN); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE id = ?`, gone.ID).Scan(&goneN); err != nil {
		t.Fatal(err)
	}
	if keptN != 1 {
		t.Fatalf("被证据引用的观测不得删除（age+keep 两条路径都要保护），result=%+v", res)
	}
	if goneN != 0 {
		t.Fatal("无引用的过期观测应被删除")
	}
	if res.Kept != 1 {
		t.Fatalf("Kept 应反映真实剩余条数，got %+v", res)
	}

	// 仅按 event_uid 引用（跨设备传播形态）也必须保护。
	s2 := newTestStore(t)
	byUID := captureEvidence(t, s2, "uid-by-uid", "s2", "p", "只按 event_uid 引用")
	if _, err := s2.db.Exec(`UPDATE observations SET created_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	mem2, err := s2.Add(AddInput{Content: "跨设备记忆", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.db.Exec(`INSERT INTO memory_evidence(
		memory_id, evidence_id, event_uid, archive_hash, raw_locator, grade, seq, occurred_at_ns, created_at
	) VALUES (?,?,?,?,?,?,?,?,?)`,
		mem2.ID, "remote-id", byUID.EventUID, byUID.ArchiveHash, "", "B", 1, 0, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.GCObservations(24*time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE id = ?`, byUID.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("按 event_uid 引用的观测也必须保留")
	}
}

func TestSyncPropagatesProvenanceAndEvidence(t *testing.T) {
	remote := newTestStore(t)
	ev := captureEvidence(t, remote, "uid-sync", "s", "p", "跨设备证据")
	mem, err := remote.Add(AddInput{
		Content: "跨设备传播的证据链", Project: "p",
		Provenance: &Provenance{File: "AGENTS.md", Line: 3, Source: "asset"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.db.Exec(`INSERT INTO memory_evidence(
		memory_id, evidence_id, event_uid, archive_hash, raw_locator, grade, seq, occurred_at_ns, created_at
	) VALUES (?,?,?,?,?,?,?,?,?)`,
		mem.ID, ev.ID, ev.EventUID, ev.ArchiveHash, ev.RawLocator, "A", ev.Seq, ev.OccurredAtNS, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// legacy 链接（无 event_uid）不得被导出，避免接收端伪造证据。
	if _, err := remote.db.Exec(`INSERT INTO memory_evidence(
		memory_id, evidence_id, event_uid, archive_hash, raw_locator, grade, seq, occurred_at_ns, created_at
	) VALUES (?,?,?,?,?,?,?,?,?)`,
		mem.ID, "legacy-ev", "", "", "", "C", 0, 0, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	recs, err := remote.Export()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("应导出 1 条，got %d", len(recs))
	}
	if len(recs[0].Evidence) != 1 || recs[0].Evidence[0].EventUID != ev.EventUID {
		t.Fatalf("只应导出带 event_uid 的证据，got %+v", recs[0].Evidence)
	}
	if recs[0].Provenance == nil || !strings.Contains(*recs[0].Provenance, "AGENTS.md") {
		t.Fatalf("导出应携带 provenance，got %v", recs[0].Provenance)
	}

	local := newTestStore(t)
	stats, err := local.Import(recs)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Inserted != 1 || stats.EvidenceLinked != 1 || stats.EvidenceSkipped != 0 {
		t.Fatalf("导入统计异常: %+v", stats)
	}
	var localID, uid string
	if err := local.db.QueryRow(`SELECT memory_id, event_uid FROM memory_evidence`).Scan(&localID, &uid); err != nil {
		t.Fatal(err)
	}
	if uid != ev.EventUID {
		t.Fatalf("证据身份应随记录到达，got %s", uid)
	}
	links, err := local.EvidenceLinks(localID)
	if err != nil || len(links) != 1 || links[0].Grade != commit.GradeA {
		t.Fatalf("导入后应能读到证据链，got %+v %v", links, err)
	}
	var prov *string
	if err := local.db.QueryRow(`SELECT provenance FROM memories WHERE id = ?`, localID).Scan(&prov); err != nil {
		t.Fatal(err)
	}
	if prov == nil || !strings.Contains(*prov, "AGENTS.md") {
		t.Fatalf("导入应传播 provenance，got %v", prov)
	}

	// SyncMerge 路径：provenance 与证据链同样要传播。
	syncLocal := newTestStore(t)
	syncStats, err := syncLocal.SyncMerge(recs)
	if err != nil {
		t.Fatal(err)
	}
	if syncStats.Inserted != 1 || syncStats.EvidenceLinked != 1 {
		t.Fatalf("sync 统计异常: %+v", syncStats)
	}
	var syncID string
	if err := syncLocal.db.QueryRow(`SELECT id FROM memories`).Scan(&syncID); err != nil {
		t.Fatal(err)
	}
	if links, err := syncLocal.EvidenceLinks(syncID); err != nil || len(links) != 1 {
		t.Fatalf("sync 后应能读到证据链，got %+v %v", links, err)
	}
	if err := syncLocal.db.QueryRow(`SELECT provenance FROM memories WHERE id = ?`, syncID).Scan(&prov); err != nil {
		t.Fatal(err)
	}
	if prov == nil || !strings.Contains(*prov, "AGENTS.md") {
		t.Fatalf("sync 应传播 provenance，got %v", prov)
	}

	// 手工构造 legacy 引用：导入必须跳过，不写 memory_evidence。
	legacy := newTestStore(t)
	rec := recs[0]
	rec.Evidence = []EvidenceLink{{EvidenceID: "ghost", EventUID: "", ArchiveHash: "", Grade: "A"}}
	legacyStats, err := legacy.Import([]ExportRecord{rec})
	if err != nil {
		t.Fatal(err)
	}
	if legacyStats.EvidenceSkipped != 1 || legacyStats.EvidenceLinked != 0 {
		t.Fatalf("legacy 证据必须跳过，got %+v", legacyStats)
	}
	var n int
	if err := legacy.db.QueryRow(`SELECT COUNT(*) FROM memory_evidence`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("不得为 legacy 引用写证据链，got %d", n)
	}
}

func TestSyncMergeEvidenceKeepsLocalID(t *testing.T) {
	remote := newTestStore(t)
	ev := captureEvidence(t, remote, "uid-local-id", "s", "p", "定位本地 id")
	mem, err := remote.Add(AddInput{Content: "同步时解析本地证据 id", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.db.Exec(`INSERT INTO memory_evidence(
		memory_id, evidence_id, event_uid, archive_hash, raw_locator, grade, seq, occurred_at_ns, created_at
	) VALUES (?,?,?,?,?,?,?,?,?)`,
		mem.ID, "remote-local-id", ev.EventUID, ev.ArchiveHash, "", "A", ev.Seq, ev.OccurredAtNS, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	recs, err := remote.Export()
	if err != nil {
		t.Fatal(err)
	}

	local := newTestStore(t)
	localEv := captureEvidence(t, local, ev.EventUID, "s", "p", "同 event_uid 的本地观测")
	stats, err := local.SyncMerge(recs)
	if err != nil {
		t.Fatal(err)
	}
	if stats.EvidenceLinked != 1 {
		t.Fatalf("应链接 1 条证据，got %+v", stats)
	}
	var gotID string
	if err := local.db.QueryRow(`SELECT evidence_id FROM memory_evidence`).Scan(&gotID); err != nil {
		t.Fatal(err)
	}
	if gotID != localEv.ID {
		t.Fatalf("应按 event_uid 解析成本地 evidence id，got %s want %s", gotID, localEv.ID)
	}
}

func TestCommitDecisionIdempotencyKeyUnique(t *testing.T) {
	s := newTestStore(t)
	res := newCandidate(t, s, "task", "p", "同一 revision 同一动作只落一条裁决")
	view, _ := s.CommitCandidate(res.Candidate.ID)
	// 直接在同一 revision 上重复写裁决：INSERT OR IGNORE 保证唯一键收敛。
	for i := 0; i < 3; i++ {
		ctx := t.Context()
		if err := recordDecision(ctx, s.db, res.Candidate.ID, view.Revision,
			"hash", commit.ActionDefer, commit.ActorAuto, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM commit_decisions WHERE candidate_id = ?`, res.Candidate.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("幂等键应去重，got %d 条裁决", n)
	}
}
