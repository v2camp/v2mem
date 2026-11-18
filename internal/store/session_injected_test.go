package store

import "testing"

// writeOpeningTrace 造一条开场类读侧轨迹：前 rank 条真注入，其余按 limit 丢弃。
func writeOpeningTrace(t *testing.T, s *Store, session, reason string, injected []string, dropped ...string) {
	t.Helper()
	var items []ReadTraceItem
	for i, id := range injected {
		items = append(items, ReadTraceItem{
			Ordinal: i + 1, MemoryID: id, MatchClass: "hard_rule",
			Eligible: true, InjectionRank: i + 1, Provenance: "p",
		})
	}
	for i, id := range dropped {
		items = append(items, ReadTraceItem{
			Ordinal: len(injected) + i + 1, MemoryID: id, MatchClass: "hard_rule",
			Eligible: true, DropReason: "limit",
		})
	}
	if _, err := s.WriteReadTrace(ReadTrace{
		SessionID: session, Project: "demo", Harness: "claude", QueryGate: "skip",
		GateReason: reason, Scope: "current", TargetLimit: 3, BudgetChars: 1200,
		RawHits: len(items), Qualified: len(items), Injected: len(injected),
		Outcome: "injected",
	}, items); err != nil {
		t.Fatalf("WriteReadTrace(%s): %v", reason, err)
	}
}

// 覆盖集合必须只含「开场 + 真注入 + kind=rule」的交集。
//
// 反例就是本方法要修的缺陷：开场把全部 rule 当覆盖集合，而它自己只注入得下
// limit 条 —— 差集里的规则永远进不来上下文。
func TestSessionInjectedRuleIDsOnlyCountsOpeningInjected(t *testing.T) {
	s := newTestStore(t)
	r1 := seedMemory(t, s, "规则一：提交前必跑门禁", "rule", "")
	r2 := seedMemory(t, s, "规则二：禁止直推 main", "rule", "")
	r3 := seedMemory(t, s, "规则三：迁移必须可回滚", "rule", "")
	r4 := seedMemory(t, s, "规则四：日志禁打明文密码", "rule", "") // 开场被 limit 截掉
	d1 := seedMemory(t, s, "决策：部署脚本放 scripts", "decision", "")

	// 开场：r1/r2/r3 注入，r4 被 limit 丢弃，d1 虽注入但不是 rule。
	if _, err := s.WriteReadTrace(ReadTrace{
		SessionID: "sess-1", Project: "demo", Harness: "claude", QueryGate: "skip",
		GateReason: "session_start", Scope: "current", TargetLimit: 3, RawHits: 4,
		Qualified: 4, Injected: 4, Outcome: "injected",
	}, []ReadTraceItem{
		{Ordinal: 1, MemoryID: r1, MatchClass: "hard_rule", Eligible: true, InjectionRank: 1},
		{Ordinal: 2, MemoryID: r2, MatchClass: "hard_rule", Eligible: true, InjectionRank: 2},
		{Ordinal: 3, MemoryID: r3, MatchClass: "hard_rule", Eligible: true, InjectionRank: 3},
		{Ordinal: 4, MemoryID: r4, MatchClass: "hard_rule", Eligible: true, DropReason: "limit"},
		{Ordinal: 5, MemoryID: d1, MatchClass: "global", Eligible: true, InjectionRank: 4},
	}); err != nil {
		t.Fatalf("WriteReadTrace: %v", err)
	}

	// 检索侧：同会话的 prompt-submit 注入过 r4（另有 rank=0 的条目），
	// 它不得因此被算作「开场覆盖过」。
	if _, err := s.WriteReadTrace(ReadTrace{
		SessionID: "sess-1", Project: "demo", Harness: "claude", QueryGate: "run",
		GateReason: "ok", RawQuery: "日志", RawHits: 2, Qualified: 1, Injected: 1,
		Outcome: "injected",
	}, []ReadTraceItem{
		{Ordinal: 1, MemoryID: r4, MatchClass: "precise", Eligible: true, InjectionRank: 1},
		{Ordinal: 2, MemoryID: r2, MatchClass: "broad", DropReason: "notes_duplicate"},
	}); err != nil {
		t.Fatalf("WriteReadTrace: %v", err)
	}

	// 别的会话开场注入过的规则，不能算到本会话头上。
	writeOpeningTrace(t, s, "sess-2", "pre_compact", []string{r4})

	got, err := s.SessionInjectedRuleIDs("sess-1")
	if err != nil {
		t.Fatalf("SessionInjectedRuleIDs: %v", err)
	}
	want := map[string]bool{r1: true, r2: true, r3: true}
	if len(got) != len(want) {
		t.Fatalf("应只返回开场真注入的 3 条 rule，got %d: %v", len(got), got)
	}
	for id, ok := range want {
		if !ok || !got[id] {
			t.Errorf("缺少开场注入的 rule %s", id)
		}
	}
	if got[r4] {
		t.Error("被 limit 截掉、只在 prompt-submit 注入过的 rule 不得算作开场覆盖")
	}
	if got[d1] {
		t.Error("非 rule 记忆不得进入覆盖集合")
	}
}

// pre_compact 与 session_start 同属开场类事件，都必须被认。
func TestSessionInjectedRuleIDsCoversPreCompact(t *testing.T) {
	s := newTestStore(t)
	r1 := seedMemory(t, s, "压缩前保留的红线规则", "rule", "")
	writeOpeningTrace(t, s, "sess-pc", "pre_compact", []string{r1})
	got, err := s.SessionInjectedRuleIDs("sess-pc")
	if err != nil {
		t.Fatalf("SessionInjectedRuleIDs: %v", err)
	}
	if !got[r1] {
		t.Fatalf("pre_compact 注入的规则也应计入开场覆盖，got %v", got)
	}
}

// 空会话标识 ∕ 未知会话：返回空集合且不报错 —— 调用方据此退化为不去重。
func TestSessionInjectedRuleIDsEmptySessionIsEmptyNotError(t *testing.T) {
	s := newTestStore(t)
	r1 := seedMemory(t, s, "任意硬规则", "rule", "")
	writeOpeningTrace(t, s, "sess-x", "session_start", []string{r1})

	for _, sid := range []string{"", "   ", "sess-unknown"} {
		got, err := s.SessionInjectedRuleIDs(sid)
		if err != nil {
			t.Fatalf("SessionInjectedRuleIDs(%q) 不应报错: %v", sid, err)
		}
		if got == nil {
			t.Fatalf("SessionInjectedRuleIDs(%q) 应返回空集合而非 nil", sid)
		}
		if len(got) != 0 {
			t.Fatalf("SessionInjectedRuleIDs(%q) 应为空，got %v", sid, got)
		}
	}
}
