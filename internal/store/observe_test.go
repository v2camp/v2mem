package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesObservationSideTables(t *testing.T) {
	s := newTestStore(t)
	for _, table := range []string{"observations", "candidates", "distill_runs"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Errorf("旧库 Open 应幂等建出 %s: %v", table, err)
		}
	}
}

func TestAppendObservationDedupesWithinSession(t *testing.T) {
	s := newTestStore(t)
	in := ObsInput{
		SessionID: "s1", Project: "p", Harness: "cursor", Event: "post-tool-use",
		ToolName: "Shell", Summary: "跑了测试", MetaJSON: `{"source":"hook-observe"}`,
	}
	first, err := s.AppendObservation(in)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !first.Created {
		t.Fatal("首条应新建")
	}
	second, err := s.AppendObservation(in)
	if err != nil {
		t.Fatalf("Append 重复: %v", err)
	}
	if second.Created || second.ID != first.ID {
		t.Fatalf("同一会话相同摘要应去重，got %+v / %+v", first, second)
	}
	other := in
	other.SessionID = "s2"
	third, err := s.AppendObservation(other)
	if err != nil {
		t.Fatalf("Append 他会话: %v", err)
	}
	if !third.Created {
		t.Fatal("不同会话的相同摘要应各留一条")
	}
	rows, err := s.ListObservations("", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有 2 条观测，got %d", len(rows))
	}
}

func TestSearchIgnoresObservations(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Add(AddInput{Content: "只有记忆里有的苹果", Project: "p"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.AppendObservation(ObsInput{
		SessionID: "s", Event: "post-tool-use", Summary: "观测里的香蕉词不应被检索到",
	}); err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}
	hits, err := s.Search(SearchQuery{Query: "香蕉词", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("mem search 不应命中 observations，got %+v", hits)
	}
	hits, err = s.Search(SearchQuery{Query: "苹果", Limit: 5})
	if err != nil {
		t.Fatalf("Search 记忆: %v", err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Content, "苹果") {
		t.Fatalf("记忆检索应不受观测行影响，got %+v", hits)
	}
}

func TestGCObservationsDoesNotTouchMemories(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Add(AddInput{Content: "永久记忆还在"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "旧观测"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE observations SET created_at = ?`, time.Now().Add(-40*24*time.Hour).Unix()); err != nil {
		t.Fatalf("回拨时间: %v", err)
	}
	res, err := s.GCObservations(30*24*time.Hour, 50000)
	if err != nil {
		t.Fatalf("GCObservations: %v", err)
	}
	if res.Deleted != 1 || res.Kept != 0 {
		t.Fatalf("应删掉过期观测，got %+v", res)
	}
	var memories int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memories); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if memories != 1 {
		t.Fatalf("回收观测不得级联删记忆，got %d", memories)
	}
}

func TestGCObservationsCapsCount(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		if _, err := s.AppendObservation(ObsInput{
			SessionID: "s", Event: "post-tool-use", Summary: string(rune('a' + i)),
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	res, err := s.GCObservations(30*24*time.Hour, 2)
	if err != nil {
		t.Fatalf("GCObservations: %v", err)
	}
	if res.Kept != 2 || res.Deleted != 3 {
		t.Fatalf("条数上限应留下最新 2 条，got %+v", res)
	}
}

func TestObservationLayerIsSearchable(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Add(AddInput{Content: "已定：刷新令牌放在 refresh.ts", Project: "pay", Kind: "decision"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.AppendObservation(ObsInput{
		SessionID: "s", Project: "pay", Event: "post-tool-use", ToolName: "Edit",
		Summary: "Edit src/auth/refresh.ts 把过期时间改成 15 分钟",
	}); err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}
	if _, err := s.AppendObservation(ObsInput{
		SessionID: "other", Project: "other", Event: "post-tool-use", ToolName: "Edit",
		Summary: "Edit src/auth/refresh.ts 另一个工程的痕迹",
	}); err != nil {
		t.Fatalf("AppendObservation other: %v", err)
	}

	def, err := s.Search(SearchQuery{Query: "15 分钟", Limit: 5})
	if err != nil {
		t.Fatalf("默认检索: %v", err)
	}
	if len(def) != 0 {
		t.Fatalf("默认检索不应命中痕迹，got %+v", def)
	}

	obs, err := s.Search(SearchQuery{Query: "refresh.ts", Layer: "observation", Limit: 5})
	if err != nil {
		t.Fatalf("痕迹检索: %v", err)
	}
	if len(obs) != 2 || obs[0].Layer != "observation" {
		t.Fatalf("痕迹层应命中两条并标层，got %+v", obs)
	}

	scoped, err := s.Search(SearchQuery{Query: "refresh.ts", Layer: "recent", Scope: "current", Project: "pay", Limit: 5})
	if err != nil {
		t.Fatalf("按工程检索痕迹: %v", err)
	}
	if len(scoped) != 1 || !strings.Contains(scoped[0].Content, "15 分钟") {
		t.Fatalf("current 应只留本工程痕迹，got %+v", scoped)
	}

	both, err := s.Search(SearchQuery{Query: "refresh", Layer: "both", Project: "pay", Limit: 5})
	if err != nil {
		t.Fatalf("两层检索: %v", err)
	}
	if len(both) < 2 || both[0].Layer != "memory" || both[1].Layer != "observation" {
		t.Fatalf("both 应先记忆后痕迹，got %+v", both)
	}

	if _, err := s.Search(SearchQuery{Query: "refresh", Layer: "observation", Kind: "decision"}); err == nil {
		t.Fatal("痕迹层带 kind 应报错")
	}
	if _, err := s.Search(SearchQuery{Query: "refresh", Layer: "nope"}); err == nil {
		t.Fatal("未知 layer 应报错")
	}
}

func TestObservationSearchSurvivesGC(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "停在账单路由"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE observations SET created_at = ?`, time.Now().Add(-40*24*time.Hour).Unix()); err != nil {
		t.Fatalf("回拨: %v", err)
	}
	if _, err := s.GCObservations(30*24*time.Hour, 50000); err != nil {
		t.Fatalf("GC: %v", err)
	}
	hits, err := s.Search(SearchQuery{Query: "账单路由", Layer: "observation", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("删掉的痕迹不应再被检索，got %+v", hits)
	}
}

func TestObservationIndexBackfillsOldRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("Open raw: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE observations (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		project TEXT NOT NULL DEFAULT '',
		harness TEXT NOT NULL DEFAULT '',
		event TEXT NOT NULL,
		tool_name TEXT NOT NULL DEFAULT '',
		summary TEXT NOT NULL DEFAULT '',
		raw_ref TEXT NOT NULL DEFAULT '',
		content_hash TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		meta_json TEXT NOT NULL DEFAULT '{}'
	)`); err != nil {
		t.Fatalf("建旧表: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO observations (
		id, session_id, event, summary, content_hash, created_at
	) VALUES ('obs-old', 's', 'post-tool-use', '旧库里的香蕉词', 'h', 1)`); err != nil {
		t.Fatalf("插入旧行: %v", err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	hits, err := s.Search(SearchQuery{Query: "香蕉词", Layer: "observation", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Content, "香蕉词") {
		t.Fatalf("旧库痕迹应在打开后可检索，got %+v", hits)
	}
}

// GCObservations 引用保护扩集（红线口径 archiveRefGuard 第 4/5 条同源）：
// 被 candidate_evidence 引用（evidence_id 与 event_uid 双口径）的观测行不删——
// 删掉它，候选的证据链就在发窗/裁决时断掉。无引用的老观测照常回收。
func TestGCObservationsProtectsCandidateEvidence(t *testing.T) {
	s := newTestStore(t)
	byID := seedArchive(t, s, "u-c1", "h-c1")
	byUID := seedArchive(t, s, "u-c2", "h-c2")
	if _, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "无引用旧观测"}); err != nil {
		t.Fatal(err)
	}
	// 已发窗：把「candidate_evidence 引用」这一个口径单独拎出来验。
	if _, err := s.db.Exec(`UPDATE evidence_sources SET cursor_seq = last_seq`); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE observations SET created_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	cand, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "decision", Content: "决定：候选证据链钉住观测行", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	// evidence_id 与 event_uid 两条口径各钉一条：后者模拟同步来的链接
	// （evidence_id 是远端 id 对不上本机行，只有 event_uid 可解析）。
	if _, err := s.db.Exec(`INSERT INTO candidate_evidence(
		candidate_id, evidence_id, event_uid, source_id, source_seq, archive_hash,
		raw_record_hash, evidence_role, quote_hash, created_at
	) VALUES (?, ?, ?, '', 0, '', '', 'direct', '', ?)`,
		cand.Candidate.ID, byID.ID, byID.EventUID, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO candidate_evidence(
		candidate_id, evidence_id, event_uid, source_id, source_seq, archive_hash,
		raw_record_hash, evidence_role, quote_hash, created_at
	) VALUES (?, 'ev-remote', ?, '', 0, '', '', 'direct', '', ?)`,
		cand.Candidate.ID, byUID.EventUID, old); err != nil {
		t.Fatal(err)
	}

	res, err := s.GCObservations(24*time.Hour, 50000)
	if err != nil {
		t.Fatalf("GCObservations: %v", err)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE id IN (?, ?)`,
		byID.ID, byUID.ID).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 2 || res.Deleted != 1 {
		t.Fatalf("被 candidate_evidence 引用的 2 条必须留下、无引用旧观测删 1 条，kept=%d %+v", kept, res)
	}
}

// 被 evidence_bundle_items 任一行引用的观测行不删：Bundle 的证据身份要能回放，
// 观测行没了 item 就只剩一个指不回来的 id。无引用的老观测照常回收。
func TestGCObservationsProtectsBundleItemEvidence(t *testing.T) {
	s := newTestStore(t)
	byID := seedArchive(t, s, "u-b1", "h-b1")
	byUID := seedArchive(t, s, "u-b2", "h-b2")
	if _, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "无引用旧观测"}); err != nil {
		t.Fatal(err)
	}
	// 真实发窗：窗口条目（evidence_id + event_uid）钉住两条观测，cursor 推到 to_seq。
	b, err := s.NextEvidenceBundle(0, 0)
	if err != nil || b == nil {
		t.Fatalf("NextEvidenceBundle: %+v %v", b, err)
	}
	// 把 byUID 的条目换成只有 event_uid 对得上的形态（跨设备传播后 evidence_id 是远端 id）。
	if _, err := s.db.Exec(`UPDATE evidence_bundle_items SET evidence_id = 'ev-remote'
		WHERE bundle_id = ? AND event_uid = ?`, b.ID, byUID.EventUID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE observations SET created_at = ?`, old); err != nil {
		t.Fatal(err)
	}

	res, err := s.GCObservations(24*time.Hour, 50000)
	if err != nil {
		t.Fatalf("GCObservations: %v", err)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE id IN (?, ?)`,
		byID.ID, byUID.ID).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 2 || res.Deleted != 1 {
		t.Fatalf("被 bundle item 引用（id/uid 双口径）的 2 条必须留下、无引用旧观测删 1 条，kept=%d %+v", kept, res)
	}
}

// 尚未发窗（所在源 seq ∈ (cursor_seq, last_seq]）的观测行不删：
// 它们还在证据队列里，先掏掉会在发窗时制造序号洞、丢摘要层。
// 年龄与条数两条删除路径都必须遵守；无引用的老观测照常回收。
func TestGCObservationsProtectsUnbundledRange(t *testing.T) {
	s := newTestStore(t)
	seedArchive(t, s, "u-a", "h-a")
	seedArchive(t, s, "u-b", "h-b")
	old := time.Now().Add(-48 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE observations SET created_at = ?`, old); err != nil {
		t.Fatal(err)
	}

	// maxKeep=1 让年龄与条数两条删除路径都跑：未发窗的 2 条一条都不许动。
	res, err := s.GCObservations(24*time.Hour, 1)
	if err != nil {
		t.Fatalf("GCObservations: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 || res.Deleted != 0 {
		t.Fatalf("未发窗观测（age+cap 两路径）一条都不该删，剩 %d %+v", n, res)
	}
}
