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
