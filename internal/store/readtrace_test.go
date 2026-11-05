package store

import (
	"strings"
	"testing"
	"time"
)

func seedMemory(t *testing.T, s *Store, content, kind, project string) string {
	t.Helper()
	res, err := s.Add(AddInput{Content: content, Kind: kind, Project: project, Source: "test"})
	if err != nil {
		t.Fatalf("Add(%q): %v", content, err)
	}
	return res.ID
}

func writeTraceForTest(t *testing.T, s *Store, id string, memoryIDs ...string) string {
	t.Helper()
	var items []ReadTraceItem
	for i, mid := range memoryIDs {
		items = append(items, ReadTraceItem{
			Ordinal: i + 1, MemoryID: mid, MatchClass: "precise", Eligible: true,
			InjectionRank: i + 1, Provenance: "p+b",
		})
	}
	got, err := s.WriteReadTrace(ReadTrace{
		ID: id, Project: "demo", QueryGate: "run", RawQuery: "q",
		TargetLimit: 3, Overfetch: 24, RawHits: len(memoryIDs),
		Qualified: len(memoryIDs), Injected: len(memoryIDs), Outcome: "injected",
	}, items)
	if err != nil {
		t.Fatalf("WriteReadTrace: %v", err)
	}
	return got
}

func TestReadTraceWriteAndRead(t *testing.T) {
	s := newTestStore(t)
	a := seedMemory(t, s, "轨迹条目甲", "decision", "demo")
	b := seedMemory(t, s, "轨迹条目乙", "pitfall", "demo")

	id, err := s.WriteReadTrace(ReadTrace{
		Project: "demo", Harness: "claude", QueryGate: "run", GateReason: "ok",
		RawQuery: "轨迹", FocusQuery: "轨迹", Scope: "current",
		TargetLimit: 3, BudgetChars: 1200, Overfetch: 24,
		RawHits: 2, Qualified: 1, Injected: 1, Outcome: "injected", ElapsedMS: 7,
	}, []ReadTraceItem{
		{MemoryID: a, MatchClass: "precise+broad", Eligible: true, InjectionRank: 1},
		{MemoryID: b, MatchClass: "precise", Eligible: false, DropReason: "scope_quota"},
	})
	if err != nil {
		t.Fatalf("WriteReadTrace: %v", err)
	}
	if len(id) != 32 {
		t.Fatalf("默认 trace id 应为 32 位十六进制，got %q", id)
	}

	got, items, err := s.GetReadTrace(id)
	if err != nil {
		t.Fatalf("GetReadTrace: %v", err)
	}
	if got.Outcome != "injected" || got.RawHits != 2 || got.Qualified != 1 || got.Injected != 1 {
		t.Fatalf("轨迹计数未落库: %+v", got)
	}
	if len(items) != 2 {
		t.Fatalf("条目数 = %d, want 2", len(items))
	}
	if items[0].Ordinal != 1 || items[1].Ordinal != 2 {
		t.Fatalf("ordinal 应按顺序补齐: %+v", items)
	}
	if items[0].SignalsJSON != "{}" {
		t.Fatalf("空 signals 应写成 {}，got %q", items[0].SignalsJSON)
	}
	if items[1].DropReason != "scope_quota" || items[1].Eligible {
		t.Fatalf("丢弃原因/合格位未落库: %+v", items[1])
	}

	if _, _, err := s.GetReadTrace("missing"); err == nil {
		t.Fatal("缺失轨迹应报错")
	}
}

func TestResolveReadTracePrefixAndAmbiguity(t *testing.T) {
	s := newTestStore(t)
	a := seedMemory(t, s, "前缀甲", "fact", "demo")
	writeTraceForTest(t, s, "trace-one1", a)
	writeTraceForTest(t, s, "trace-two2", a)

	got, err := s.ResolveReadTrace("trace-one1")
	if err != nil || got != "trace-one1" {
		t.Fatalf("完整 id 应精确命中，got %q %v", got, err)
	}
	got, err = s.ResolveReadTrace("trace-one")
	if err != nil || got != "trace-one1" {
		t.Fatalf("唯一前缀应解析，got %q %v", got, err)
	}
	if _, err := s.ResolveReadTrace("trace-"); err == nil || !strings.Contains(err.Error(), "歧义") {
		t.Fatalf("歧义前缀应报错，got %v", err)
	}
	if _, err := s.ResolveReadTrace(""); err == nil {
		t.Fatal("空 trace id 应报错")
	}
	if _, err := s.ResolveReadTrace("nope"); err == nil {
		t.Fatal("未知 trace 应报错")
	}
}

func TestRecentReadTraces(t *testing.T) {
	s := newTestStore(t)
	a := seedMemory(t, s, "最近轨迹记忆", "fact", "demo")
	writeTraceForTest(t, s, "recent-1", a)
	writeTraceForTest(t, s, "recent-2", a)

	got, err := s.RecentReadTraces(1)
	if err != nil {
		t.Fatalf("RecentReadTraces: %v", err)
	}
	if len(got) != 1 || got[0].ID == "" || got[0].Outcome != "injected" {
		t.Fatalf("最近轨迹不符: %+v", got)
	}
	if _, err := s.RecentReadTraces(0); err != nil {
		t.Fatalf("limit=0 应回落到默认: %v", err)
	}
}

func TestContentHashesBatch(t *testing.T) {
	s := newTestStore(t)
	a := seedMemory(t, s, "哈希甲", "fact", "demo")
	b := seedMemory(t, s, "哈希乙", "fact", "demo")

	got, err := s.ContentHashes([]string{a, a, "", b, "missing"})
	if err != nil {
		t.Fatalf("ContentHashes: %v", err)
	}
	if len(got) != 2 || got[a] == "" || got[b] == "" {
		t.Fatalf("批量哈希应只含存在的两条，got %+v", got)
	}
	empty, err := s.ContentHashes(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空输入应返回空 map，got %+v %v", empty, err)
	}
}

func TestAdoptedSubjectKeysAndAdopt(t *testing.T) {
	s := newTestStore(t)
	a := seedMemory(t, s, "被采纳的决策", "decision", "demo")
	seedMemory(t, s, "没被采纳的坑", "pitfall", "demo")

	if _, _, err := s.Adopt(a[:8]); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	keys, err := s.AdoptedSubjectKeys()
	if err != nil {
		t.Fatalf("AdoptedSubjectKeys: %v", err)
	}
	if !keys["decision|demo"] || keys["pitfall|demo"] {
		t.Fatalf("只应标记被采纳的 subject，got %+v", keys)
	}
	d, err := s.MemoryDetailByID(a)
	if err != nil {
		t.Fatalf("MemoryDetailByID: %v", err)
	}
	if d.AdoptedCount != 1 || d.LastAdopted == nil || d.AccessCount != 1 {
		t.Fatalf("Adopt 应累加采纳并保留命中字段: %+v", d)
	}
	if _, _, err := s.Adopt("ffffffff"); err == nil {
		t.Fatal("未知 id 应报错")
	}
}

func TestApplyReadFeedbackOnlyAdoptedUpdatesCounters(t *testing.T) {
	s := newTestStore(t)
	a := seedMemory(t, s, "反馈记忆甲", "decision", "demo")
	b := seedMemory(t, s, "反馈记忆乙", "pitfall", "demo")
	traceID := writeTraceForTest(t, s, "", a, b)

	res, err := s.ApplyReadFeedback(traceID[:8], a[:8], "adopted", "答案直接可用")
	if err != nil {
		t.Fatalf("ApplyReadFeedback adopted: %v", err)
	}
	if res.Feedback != "adopted" || res.AdoptedCount != 1 || res.Reason == "" {
		t.Fatalf("adopted 结果不符: %+v", res)
	}
	d, _ := s.MemoryDetailByID(a)
	if d.AdoptedCount != 1 || d.LastAdopted == nil {
		t.Fatalf("adopted 应更新采纳字段: %+v", d)
	}

	// 同值重复提交幂等，不重复计数。
	again, err := s.ApplyReadFeedback(traceID, a, "adopted", "再次确认")
	if err != nil {
		t.Fatalf("重复 adopted 应幂等: %v", err)
	}
	if again.AdoptedCount != 1 {
		t.Fatalf("重复 adopted 不应累加，got %d", again.AdoptedCount)
	}

	// negative 只改轨迹，不动采纳计数。
	neg, err := s.ApplyReadFeedback(traceID, b, "negative", "与问题无关")
	if err != nil {
		t.Fatalf("ApplyReadFeedback negative: %v", err)
	}
	if neg.AdoptedCount != 0 {
		t.Fatalf("negative 不应更新 adopted_count，got %d", neg.AdoptedCount)
	}
	db, _ := s.MemoryDetailByID(b)
	if db.AdoptedCount != 0 || db.LastAdopted != nil {
		t.Fatalf("negative 不应写 memories.adopted_*: %+v", db)
	}

	// 已 adopted 的条目不能直接改判。
	if _, err := s.ApplyReadFeedback(traceID, a, "negative", "改判"); err == nil {
		t.Fatal("已 adopted 直接改判应被拒绝")
	}
	if _, err := s.ApplyReadFeedback(traceID, a, "unknown", ""); err == nil {
		t.Fatal("未知反馈应报错")
	}
	if _, err := s.ApplyReadFeedback("missing", a, "adopted", ""); err == nil {
		t.Fatal("未知 trace 应报错")
	}
	if _, err := s.ApplyReadFeedback(traceID, "missing", "adopted", ""); err == nil {
		t.Fatal("轨迹里不存在的记忆应报错")
	}
}

func TestApplyReadFeedbackRejectsAmbiguousMemoryPrefix(t *testing.T) {
	s := newTestStore(t)
	// 轨迹条目直接写入同前缀的两个 id：歧义判定发生在解析记忆之前，
	// 不需要两条真实记忆参与（也避免 UPDATE 主键触发外键约束）。
	traceID := writeTraceForTest(t, s, "", "dupabcdef1", "dupabcdef2")
	if _, err := s.ApplyReadFeedback(traceID, "dupabcdef", "adopted", ""); err == nil ||
		!strings.Contains(err.Error(), "歧义") {
		t.Fatalf("歧义前缀应报错，got %v", err)
	}
}

func TestApplyReadFeedbackFailsWhenMemoryDeleted(t *testing.T) {
	s := newTestStore(t)
	a := seedMemory(t, s, "会消失的记忆", "fact", "demo")
	traceID := writeTraceForTest(t, s, "", a)
	if _, err := s.db.Exec(`DELETE FROM memories WHERE id = ?`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReadFeedback(traceID, a, "adopted", ""); err == nil {
		t.Fatal("记忆已删除时反馈应报错")
	}
}

func TestMemoryDetailByIDCarriesProvenanceAndExpiry(t *testing.T) {
	s := newTestStore(t)
	res, err := s.Add(AddInput{
		Content: "带溯源与过期的记忆", Kind: "decision", Project: "demo", Source: "test",
		TTL:        time.Hour,
		Provenance: &Provenance{File: "docs/plan.md", Line: 12, Source: "asset", Preview: "计划片段"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	d, err := s.MemoryDetailByID(res.ID[:8])
	if err != nil {
		t.Fatalf("MemoryDetailByID: %v", err)
	}
	if d.Content != "带溯源与过期的记忆" || d.ExpiresAt == nil {
		t.Fatalf("详情缺内容或过期时间: %+v", d)
	}
	if d.Provenance == nil || d.Provenance.File != "docs/plan.md" || d.Provenance.Line != 12 {
		t.Fatalf("溯源未解析: %+v", d.Provenance)
	}
	if _, err := s.MemoryDetailByID("ffffffff"); err == nil {
		t.Fatal("未知记忆应报错")
	}
}

func TestTracesForMemory(t *testing.T) {
	s := newTestStore(t)
	a := seedMemory(t, s, "轨迹反查记忆", "fact", "demo")
	writeTraceForTest(t, s, "trace-lookup-1", a)
	writeTraceForTest(t, s, "trace-lookup-2", a)

	got, err := s.TracesForMemory(a, 1)
	if err != nil {
		t.Fatalf("TracesForMemory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("limit=1 应只返回一条，got %d", len(got))
	}
	if got[0].TraceID == "" || got[0].InjectionRank != 1 {
		t.Fatalf("轨迹内容不完整: %+v", got[0])
	}
	empty, err := s.TracesForMemory("", 5)
	if err != nil || empty != nil {
		t.Fatalf("空 memory id 应返回 nil，got %+v %v", empty, err)
	}
}
