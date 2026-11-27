package store

// 候选证据载体统一的回归测试。
//
// 背景：规则蒸馏路径（InsertCandidate）长期只写 obs_ids_json，proposal 路径才写
// candidate_evidence；采纳侧只读表，于是旧路径候选采纳后 memory_evidence 为空、
// provenance 退化成 preview。三条防线在这里各测一遍：
//   - 落候选时展开 obs_ids（新数据）
//   - 采纳前兜底展开（存量候选）
//   - RepairMemoryEvidence 显式补链（已落库记忆，可重复执行）

import (
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/commit"
)

func TestInsertCandidateDerivesEvidenceFromObs(t *testing.T) {
	s := newTestStore(t)
	ev := captureEvidence(t, s, "uid-derive", "s", "p", "派生证据摘要")
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "fact",
		Content: "记忆库放用户目录", Source: "distill",
		ObsIDs: []string{ev.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	var eid, ah, role string
	if err := s.db.QueryRow(`SELECT evidence_id, archive_hash, evidence_role
		FROM candidate_evidence WHERE candidate_id = ?`, res.Candidate.ID).Scan(&eid, &ah, &role); err != nil {
		t.Fatalf("候选落库应展开 obs_ids 到 candidate_evidence: %v", err)
	}
	if eid != ev.ID || ah != ev.ArchiveHash || role != "direct" {
		t.Fatalf("证据字段应来自观测行，got id=%s hash=%s role=%s", eid, ah, role)
	}
}

func TestCommitAcceptBackfillsLegacyCandidateEvidence(t *testing.T) {
	s := newTestStore(t)
	ev := captureEvidence(t, s, "uid-legacy", "s", "p", "旧路径留下的证据")
	// 模拟旧路径：只写 obs_ids_json，不写 candidate_evidence。
	candID := newID()
	now := time.Now().Unix()
	content := "旧路径候选：采纳后应补出证据链"
	if _, err := s.db.Exec(`INSERT INTO candidates(
		id, session_id, project, kind, content, content_hash, status, source, channel,
		salience, obs_ids_json, created_at, attributed_to
	) VALUES (?,?,?,?,?,?, 'pending', 'distill', 'agent', ?,?,?,?)`,
		candID, "s", "p", "fact", content, HashOf(content), 0.5,
		marshalObsIDs([]string{ev.ID}), now, ""); err != nil {
		t.Fatal(err)
	}
	view, err := s.CommitCandidate(candID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.CommitAccept(candID, view.Revision, commit.ActorHuman)
	if err != nil {
		t.Fatalf("采纳: %v", err)
	}
	if out.MemoryID != candID {
		t.Fatalf("记忆应继承候选 id，got %s want %s", out.MemoryID, candID)
	}
	if n := countMemoryEvidence(t, s, out.MemoryID); n != 1 {
		t.Fatalf("采纳前应兜底补出 1 条 memory_evidence，got %d", n)
	}
	var prov string
	if err := s.db.QueryRow(`SELECT provenance FROM memories WHERE id = ?`, out.MemoryID).Scan(&prov); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prov, "A级") {
		t.Fatalf("provenance 应反映真实证据等级而不是 preview，got %s", prov)
	}
}

func TestRepairAllMemoryEvidenceScansAndReports(t *testing.T) {
	s := newTestStore(t)
	ev := captureEvidence(t, s, "uid-all", "s", "p", "全量扫描的证据")
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "fact", Content: "缺证据的记忆", Source: "distill",
		ObsIDs: []string{ev.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.CommitCandidate(res.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.CommitAccept(res.Candidate.ID, view.Revision, commit.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟历史缺口：只删记忆侧证据，候选侧仍在（补得回来）。
	if _, err := s.db.Exec(`DELETE FROM memory_evidence WHERE memory_id = ?`, out.MemoryID); err != nil {
		t.Fatal(err)
	}
	// 直写记忆没有候选背书，属于「补不了也不该报失败」。
	if _, err := s.Add(AddInput{Content: "没有候选的直写记忆", Project: "p", Kind: "fact"}); err != nil {
		t.Fatal(err)
	}

	probe, err := s.RepairAllMemoryEvidence(true)
	if err != nil {
		t.Fatalf("预演: %v", err)
	}
	if probe.Scanned != 2 || probe.Skipped != 1 || len(probe.Items) != 1 || probe.Items[0].Status != "pending" {
		t.Fatalf("预演应报 1 条待修、1 条跳过，got %+v", probe)
	}
	if n := countMemoryEvidence(t, s, out.MemoryID); n != 0 {
		t.Fatalf("预演不得写库，got %d", n)
	}

	rep, err := s.RepairAllMemoryEvidence(false)
	if err != nil {
		t.Fatalf("执行: %v", err)
	}
	if rep.Repaired != 1 || rep.Skipped != 1 || rep.AddedEvidence != 1 || rep.Failed != 0 {
		t.Fatalf("全量修复结果不对: %+v", rep)
	}
	if n := countMemoryEvidence(t, s, out.MemoryID); n != 1 {
		t.Fatalf("修复后应有 1 条证据，got %d", n)
	}
}

func TestRepairMemoryEvidenceIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ev := captureEvidence(t, s, "uid-repair", "s", "p", "待修复的证据")
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "fact",
		Content: "修复入口应幂等", Source: "distill",
		ObsIDs: []string{ev.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, _ := s.CommitCandidate(res.Candidate.ID)
	out, err := s.CommitAccept(res.Candidate.ID, view.Revision, commit.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	// 回退成历史缺口：证据链清空、provenance 退回 preview。
	if _, err := s.db.Exec(`DELETE FROM memory_evidence WHERE memory_id = ?`, out.MemoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM candidate_evidence WHERE candidate_id = ?`, out.MemoryID); err != nil {
		t.Fatal(err)
	}
	preview := `{"source":"commit","preview":"candidate:` + out.MemoryID[:8] + `"}`
	if _, err := s.db.Exec(`UPDATE memories SET provenance = ? WHERE id = ?`, preview, out.MemoryID); err != nil {
		t.Fatal(err)
	}

	n, err := s.RepairMemoryEvidence(out.MemoryID[:8])
	if err != nil {
		t.Fatalf("修复: %v", err)
	}
	if n != 1 || countMemoryEvidence(t, s, out.MemoryID) != 1 {
		t.Fatalf("应补回 1 条证据，got added=%d evidence=%d", n,
			countMemoryEvidence(t, s, out.MemoryID))
	}
	var prov string
	if err := s.db.QueryRow(`SELECT provenance FROM memories WHERE id = ?`, out.MemoryID).Scan(&prov); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prov, "A级") {
		t.Fatalf("修复后 provenance 应反映证据等级，got %s", prov)
	}

	again, err := s.RepairMemoryEvidence(out.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("重复修复不应再新增证据，got %d", again)
	}
}
