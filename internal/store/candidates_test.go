package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/testkit"
)

// InsertCandidate 的入参校验与「观测不存在」降级：坏输入要直接报错，
// 未知观测只跳过证据展开，不能把候选一起丢掉。
func TestInsertCandidateValidation(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		name string
		in   CandidateInput
	}{
		{"空正文", CandidateInput{SessionID: "s", Kind: "fact", Content: "  "}},
		{"未知 kind", CandidateInput{SessionID: "s", Kind: "rumor", Content: "x"}},
		{"非法 channel", CandidateInput{SessionID: "s", Kind: "fact", Content: "x", Channel: "carrier-pigeon"}},
	}
	for _, tc := range cases {
		if _, err := s.InsertCandidate(tc.in); err == nil {
			t.Errorf("%s 应报错", tc.name)
		}
	}
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "fact",
		Content: "观测失效时仍可落候选", ObsIDs: []string{"ev_missing"},
	})
	if err != nil || !res.Created {
		t.Fatalf("未知观测应忽略而非失败: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM candidate_evidence WHERE candidate_id = ?`,
		res.Candidate.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("未知观测不应展开证据，got %d", n)
	}
}

func TestCandidateNotSearchableUntilAccept(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendObservation(ObsInput{
		SessionID: "s", Event: "stop", Summary: "决策：记忆库放在用户目录",
	}); err != nil {
		t.Fatal(err)
	}
	obs, err := s.ListObservations("s", 10)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "decision",
		Content: "记忆库放在用户目录，不放代码目录", Source: "distill", Salience: 0.6,
		ObsIDs: []string{obs[0].ID},
	})
	if err != nil || !res.Created {
		t.Fatalf("insert: %+v %v", res, err)
	}
	again, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "decision",
		Content: "记忆库放在用户目录，不放代码目录", Source: "distill",
	})
	if err != nil || again.Created || again.Candidate.ID != res.Candidate.ID {
		t.Fatalf("同会话同内容应去重，got %+v %v", again, err)
	}
	hits, err := s.Search(SearchQuery{Query: "用户目录", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("pending 不得进检索，got %+v", hits)
	}

	acc, err := s.AcceptCandidate(res.Candidate.ID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if acc.MemoryID == "" || acc.Candidate.Status != "accepted" {
		t.Fatalf("accept: %+v", acc)
	}
	var source string
	if err := s.db.QueryRow(`SELECT source FROM memories WHERE id=?`, acc.MemoryID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "distill-accept" {
		t.Fatalf("source=%s", source)
	}
	// 记忆沿用候选 id：派生关系由 id 同值表达，不再写 src==dst 的自环边。
	if acc.MemoryID != res.Candidate.ID {
		t.Fatalf("记忆应继承候选 id，got memory=%s candidate=%s", acc.MemoryID, res.Candidate.ID)
	}
	var selfLoops int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE src=dst AND rel='derived_from'`).Scan(&selfLoops); err != nil {
		t.Fatal(err)
	}
	if selfLoops != 0 {
		t.Fatalf("id 继承后不得写自环边，got %d", selfLoops)
	}
	hits, err = s.Search(SearchQuery{Query: "用户目录", Limit: 5})
	if err != nil || len(hits) != 1 {
		t.Fatalf("采纳后应可检索，got %+v %v", hits, err)
	}

	// 删掉已采纳的候选行，记忆还在。
	if _, err := s.db.Exec(`DELETE FROM candidates WHERE id=?`, res.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	hits, err = s.Search(SearchQuery{Query: "用户目录", Limit: 5})
	if err != nil || len(hits) != 1 {
		t.Fatalf("删候选不应删记忆，got %+v %v", hits, err)
	}
}

func TestRejectDoesNotWriteMemory(t *testing.T) {
	s := newTestStore(t)
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "pitfall", Content: "报错：不要把库放进同步盘", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.RejectCandidate(res.Candidate.ID)
	if err != nil || c.Status != "rejected" {
		t.Fatalf("reject: %+v %v", c, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reject 不得写 memories，got %d", n)
	}
	if _, err := s.AcceptCandidate(res.Candidate.ID); err == nil {
		t.Fatal("已拒绝的候选不能再采纳")
	}
}

func TestGCCandidatesLeavesMemories(t *testing.T) {
	s := newTestStore(t)
	pending, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "task", Content: "待办：还剩文档", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "decision", Content: "决定：gc 不级联删记忆", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.AcceptCandidate(accepted.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldPending := time.Now().Add(-40 * 24 * time.Hour).Unix()
	oldAccepted := time.Now().Add(-100 * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE candidates SET created_at=? WHERE id=?`, oldPending, pending.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE candidates SET decided_at=? WHERE id=?`, oldAccepted, accepted.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GCCandidates(14*24*time.Hour, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.Expired != 1 || got.Deleted != 1 || got.DeletedExpired != 0 {
		t.Fatalf("gc: %+v", got)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id=?`, acc.MemoryID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("gc 候选不得删记忆")
	}
	listed, err := s.ListCandidates("expired", "s", 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("过期行应留下，got %+v %v", listed, err)
	}
}

// 过期行也要有出口：从未判过的老候选不参与一致率，留着只会让候选表只增不减。
func TestGCCandidatesDeletesOldExpired(t *testing.T) {
	s := newTestStore(t)
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "task", Content: "很久没人裁决的候选", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(
		`UPDATE candidates SET status='expired', created_at=?, decided_at=? WHERE id=?`,
		old, old, res.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GCCandidates(14*24*time.Hour, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeletedExpired != 1 {
		t.Fatalf("超期 expired 行应删除，got %+v", got)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM candidates WHERE id=?`, res.Candidate.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("超期 expired 行应已删除")
	}
}

// 清边失败必须整轮回滚：半删（边没了候选还在）比不删更糟——
// 读者会以为证据链断了，而实际只是 GC 撞上了写故障。
func TestGCCandidatesRollsBackWhenEdgeCleanupFails(t *testing.T) {
	s := newTestStore(t)
	accepted, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "decision", Content: "决定：清边失败要回滚", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCandidate(accepted.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	// 采纳沿用候选 id，不再写自环边；这里手工补一条旧模型留下的跨 id 派生边，
	// GC 清边失败仍必须整轮回滚。
	otherMem := mustAdd(t, s, "旧模型留下的派生记忆")
	if _, err := s.db.Exec(`INSERT INTO edges(src,dst,rel) VALUES(?,?, 'derived_from')`,
		otherMem, accepted.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE candidates SET decided_at=? WHERE id=?`, old, accepted.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	undo, err := testkit.FailWrites(s.db, "edges")
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	if _, err := s.GCCandidates(14*24*time.Hour, 90*24*time.Hour); err == nil {
		t.Fatal("清边写失败应报错")
	}
	var candLeft, edgeLeft int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM candidates WHERE id=?`, accepted.Candidate.ID).Scan(&candLeft); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE dst=?`, accepted.Candidate.ID).Scan(&edgeLeft); err != nil {
		t.Fatal(err)
	}
	if candLeft != 1 || edgeLeft != 1 {
		t.Fatalf("失败必须整轮回滚：候选剩 %d、边剩 %d（期望 1/1）", candLeft, edgeLeft)
	}
}

// 删候选行失败同样整轮回滚：过期行清理路径先删掉的边不许留下。
func TestGCCandidatesRollsBackWhenCandidateDeleteFails(t *testing.T) {
	s := newTestStore(t)
	memID := mustAdd(t, s, "派生记忆")
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "task", Content: "过期行清理撞写故障", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE candidates SET status='expired', decided_at=? WHERE id=?`,
		old, res.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO edges(src, dst, rel) VALUES (?, ?, 'derived_from')`,
		memID, res.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	undo, err := testkit.FailWrites(s.db, "candidates")
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	if _, err := s.GCCandidates(14*24*time.Hour, 90*24*time.Hour); err == nil {
		t.Fatal("删候选行写失败应报错")
	}
	var candLeft, edgeLeft int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM candidates WHERE id=?`, res.Candidate.ID).Scan(&candLeft); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE dst=?`, res.Candidate.ID).Scan(&edgeLeft); err != nil {
		t.Fatal(err)
	}
	if candLeft != 1 || edgeLeft != 1 {
		t.Fatalf("失败必须整轮回滚：候选剩 %d、边剩 %d（期望 1/1）", candLeft, edgeLeft)
	}
}

// 标过期的写失败直接冒泡：装了写失败注入的表上跑 GC 必须报错，不许静默少删。
func TestGCCandidatesSurfacesWriteFailure(t *testing.T) {
	s := newTestStore(t)
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "task", Content: "标过期撞写故障", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE candidates SET created_at=? WHERE id=?`, old, res.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	undo, err := testkit.FailWrites(s.db, "candidates")
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	if _, err := s.GCCandidates(14*24*time.Hour, 90*24*time.Hour); err == nil {
		t.Fatal("写失败应冒泡")
	}
}

func TestDistillWindowSemantics(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendObservation(ObsInput{SessionID: "s1", Event: "stop", Summary: "决策：甲"}); err != nil {
		t.Fatal(err)
	}
	// 蒸馏游标（distill_runs 的 window_key）是审计 GC 与 R12 判据仍依赖的事实：
	// 同窗口幂等（ErrAlreadyDistilled），失败行不占窗口。
	if err := s.SaveDistillRun(DistillRun{
		SessionID: "s1", Provider: "none", Status: "ok", CandCount: 1, WindowKey: "w1",
	}); err != nil {
		t.Fatal(err)
	}
	done, err := s.DistillWindowDone("s1", "w1")
	if err != nil || !done {
		t.Fatal(done, err)
	}
	if err := s.SaveDistillRun(DistillRun{
		SessionID: "s1", Provider: "none", Status: "ok", WindowKey: "w1",
	}); err != ErrAlreadyDistilled {
		t.Fatalf("重复窗口应冲突，got %v", err)
	}
	if err := s.SaveDistillRun(DistillRun{
		SessionID: "s1", Provider: "none", Status: "fail",
	}); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := s.db.QueryRow(`SELECT window_key FROM distill_runs WHERE status='fail'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "" {
		t.Fatalf("失败行的 window_key 应为空，got %q", key)
	}
}

func TestOldEdgesDstFKIsRelaxed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE edges`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE edges (
		src TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
		dst TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
		rel TEXT NOT NULL,
		valid_from INTEGER,
		valid_to INTEGER,
		PRIMARY KEY (src, dst, rel)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	rows, err := s.db.Query(`PRAGMA foreign_key_list(edges)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatal(err)
		}
		if from == "dst" {
			t.Fatal("旧库的 dst 外键应被拆掉")
		}
	}
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "decision", Content: "决定：旧库也能采纳", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCandidate(res.Candidate.ID); err != nil {
		t.Fatal(err)
	}
}

func TestFailDistillDoesNotClaimWindow(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveDistillRun(DistillRun{
		SessionID: "s", Provider: "openai", Status: "fail", Error: "timeout", WindowKey: "should-clear",
	}); err != nil {
		t.Fatal(err)
	}
	done, err := s.DistillWindowDone("s", "should-clear")
	if err != nil || done {
		t.Fatalf("失败不应占窗口，done=%v err=%v", done, err)
	}
	var key string
	if err := s.db.QueryRow(`SELECT window_key FROM distill_runs`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "" {
		t.Fatalf("失败行的 window_key 应为空，got %q", key)
	}
}

func TestCandidateResolveAndAcceptEdges(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.InsertCandidate(CandidateInput{Kind: "unknown-kind", Content: "不该进"}); err == nil {
		t.Fatal("非法 kind 应失败")
	}
	if _, err := s.InsertCandidate(CandidateInput{Kind: "decision", Content: "  "}); err == nil {
		t.Fatal("空内容应失败")
	}
	a, err := s.InsertCandidate(CandidateInput{SessionID: "s", Kind: "decision", Content: "决定甲"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.InsertCandidate(CandidateInput{SessionID: "s", Kind: "task", Content: "待办乙还剩一步"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveCandidate(""); err == nil {
		t.Fatal("空 id 应失败")
	}
	if _, err := s.ResolveCandidate("nope"); err == nil {
		t.Fatal("不存在应失败")
	}
	if _, err := s.db.Exec(`INSERT INTO candidates(
		id, session_id, project, kind, content, content_hash, status, source, salience, obs_ids_json, created_at
	) VALUES ('abc100','s','','decision','歧义一',?, 'pending','distill',0.5,'[]',1)`, HashOf("歧义一")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO candidates(
		id, session_id, project, kind, content, content_hash, status, source, salience, obs_ids_json, created_at
	) VALUES ('abc200','s','','task','歧义二',?, 'pending','distill',0.5,'[]',2)`, HashOf("歧义二")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveCandidate("abc"); err == nil {
		t.Fatal("歧义前缀应失败")
	}
	if _, err := s.GCCandidates(0, 0); err != nil {
		t.Fatal(err)
	}
	prefix := a.Candidate.ID[:1]
	if prefix == b.Candidate.ID[:1] {
		if _, err := s.ResolveCandidate(prefix); err == nil {
			t.Fatal("歧义前缀应失败")
		}
	}
	first, err := s.AcceptCandidate(a.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.AcceptCandidate(a.Candidate.ID)
	if err != nil || !again.Already || again.MemoryID != first.MemoryID {
		t.Fatalf("重复采纳应幂等，got %+v %v", again, err)
	}
	if _, err := s.RejectCandidate(a.Candidate.ID); err == nil {
		t.Fatal("已采纳不能拒绝")
	}
	if _, err := s.RejectCandidate(b.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	rej, err := s.RejectCandidate(b.Candidate.ID)
	if err != nil || rej.Status != "rejected" {
		t.Fatalf("重复拒绝应幂等，got %+v %v", rej, err)
	}
	done, err := s.DistillWindowDone("", "x")
	if err != nil || done {
		t.Fatal(done, err)
	}
	if err := s.SaveDistillRun(DistillRun{SessionID: "s", Provider: "none", Status: "ok", WindowKey: "w-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDistillRun(DistillRun{
		SessionID: "s", Provider: "none", Status: "ok", WindowKey: "w-b",
		StartedAt: time.Now().Unix(), FinishedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchUnaffectedByCandidateVolume(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Add(AddInput{Content: "只有记忆里的苹果", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err := s.InsertCandidate(CandidateInput{
			SessionID: "s",
			Kind:      "decision",
			Content:   "候选香蕉词 " + strings.Repeat("甲", i+1),
			Source:    "distill",
		}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.Search(SearchQuery{Query: "香蕉词", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("候选不得被检索，got %+v", hits)
	}
}

// 采纳必须在单事务里完成：任一步失败都不能留下半状态。
// 这里让最后一步「写 mem_sigs 签名」失败（前面的写记忆、回填候选都已执行），
// 断言整笔回滚；去掉故障后重试必须一次成功，且不产生重复记忆或自环边。
func TestAcceptRollsBackOnMidFailureAndIsRepairable(t *testing.T) {
	s := newTestStore(t)
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "decision",
		Content: "决定：采纳要么全成要么全不成", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`CREATE TRIGGER fail_sigs BEFORE INSERT ON mem_sigs
		 BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCandidate(res.Candidate.ID); err == nil {
		t.Fatal("写签名失败时采纳必须报错")
	}
	var memories, edges int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if memories != 0 || edges != 0 {
		t.Fatalf("失败必须整笔回滚，memories=%d edges=%d", memories, edges)
	}
	c, err := s.candidateByID(res.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "pending" || c.MemoryID != "" {
		t.Fatalf("回滚后候选应仍是 pending 且无 memory_id，got %+v", c)
	}

	if _, err := s.db.Exec(`DROP TRIGGER fail_sigs`); err != nil {
		t.Fatal(err)
	}
	acc, err := s.AcceptCandidate(res.Candidate.ID)
	if err != nil {
		t.Fatalf("故障排除后应能修复: %v", err)
	}
	if acc.Candidate.Status != "accepted" || acc.MemoryID == "" {
		t.Fatalf("修复后应采纳成功，got %+v", acc)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if memories != 1 || acc.MemoryID != res.Candidate.ID {
		t.Fatalf("应恰好 1 条记忆且 id 继承候选，memories=%d memory=%s candidate=%s",
			memories, acc.MemoryID, res.Candidate.ID)
	}
	var selfLoops int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE src=dst`).Scan(&selfLoops); err != nil {
		t.Fatal(err)
	}
	if selfLoops != 0 {
		t.Fatalf("不得写自环边，got %d", selfLoops)
	}
	// 悬空边：每条 derived_from 的 src 都必须能在 memories 里找到。
	var dangling int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM edges e LEFT JOIN memories m ON m.id = e.src
		  WHERE e.rel='derived_from' AND m.id IS NULL`).Scan(&dangling); err != nil {
		t.Fatal(err)
	}
	if dangling != 0 {
		t.Fatalf("不得有悬空边，got %d", dangling)
	}
}

// 已采纳的候选再次调用必须幂等：不新建记忆、不重复计数、不写自环边。
// 旧实现靠补 derived_from 边修复丢失的派生关系；记忆继承候选 id 后关系由
// id 同值表达，已没有可补的边，重放职责收敛为补证据链与裁决账
// （由 TestCommitAcceptReplayOnlyRepairsLinks 覆盖）。
func TestAcceptReplayIsIdempotentWithoutSelfLoop(t *testing.T) {
	s := newTestStore(t)
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "pitfall", Content: "报错：重放不得改变身份", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.AcceptCandidate(res.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.AcceptCandidate(res.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Already || again.MemoryID != first.MemoryID {
		t.Fatalf("重复采纳应幂等，got %+v", again)
	}
	if first.MemoryID != res.Candidate.ID {
		t.Fatalf("记忆 id 应等于候选 id，got %s want %s", first.MemoryID, res.Candidate.ID)
	}
	var memories, selfLoops int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE src=dst`).Scan(&selfLoops); err != nil {
		t.Fatal(err)
	}
	if memories != 1 || selfLoops != 0 {
		t.Fatalf("重放不得新建记忆或写自环边，memories=%d selfLoops=%d", memories, selfLoops)
	}
}

// 唯一冲突必须靠驱动错误码识别，而不是错误文案。
// 用例同时验证「真实冲突被认出来」和「仅仅文案像 UNIQUE 的 error 不被误判」。
func TestUniqueViolationDetectedByErrorCode(t *testing.T) {
	s := newTestStore(t)
	ins := func() error {
		_, err := s.db.Exec(`INSERT INTO candidates(
			id, session_id, project, kind, content, content_hash, status, source, salience, obs_ids_json, created_at
		) VALUES (?, 's', '', 'decision', '同一条', ?, 'pending', 'distill', 0.5, '[]', 1)`,
			newID(), HashOf("同一条"))
		return err
	}
	if err := ins(); err != nil {
		t.Fatal(err)
	}
	err := ins()
	if err == nil {
		t.Fatal("同会话同内容应触发唯一约束")
	}
	if !isUniqueViolation(err) {
		t.Fatalf("应识别为唯一冲突，got %v", err)
	}
	if isUniqueViolation(errors.New("UNIQUE constraint failed: candidates.session_id")) {
		t.Fatal("不应再凭文案判重")
	}
	if isUniqueViolation(nil) {
		t.Fatal("nil 不应判为唯一冲突")
	}
}

// rejected 不占唯一键：拒绝过的内容允许重新进入 pending。
func TestInsertCandidateAfterRejectIsNew(t *testing.T) {
	s := newTestStore(t)
	first, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "task", Content: "待办：拒绝后还能再来", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RejectCandidate(first.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	again, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "task", Content: "待办：拒绝后还能再来", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Created || again.Candidate.ID == first.Candidate.ID {
		t.Fatalf("拒绝后应允许重新插入，got %+v", again)
	}
}

// empty 结果也要占住窗口：否则空会话每次启动都被重新蒸馏。
func TestEmptyRunClaimsWindow(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveDistillRun(DistillRun{
		SessionID: "s", Provider: "none", Status: "empty", WindowKey: "w-empty",
	}); err != nil {
		t.Fatal(err)
	}
	done, err := s.DistillWindowDone("s", "w-empty")
	if err != nil || !done {
		t.Fatalf("empty 应占窗口，done=%v err=%v", done, err)
	}
	if err := s.SaveDistillRun(DistillRun{
		SessionID: "s", Provider: "none", Status: "fail", WindowKey: "w-fail",
	}); err != nil {
		t.Fatal(err)
	}
	done, err = s.DistillWindowDone("s", "w-fail")
	if err != nil || done {
		t.Fatalf("fail 不应占窗口，done=%v err=%v", done, err)
	}
}
