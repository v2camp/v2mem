package store

import (
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
