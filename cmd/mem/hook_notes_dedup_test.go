package main

// 回归：notes 去重只能覆盖「本会话开场**实际**注入过」的 rule。
//
// 缺陷原状：notesCoveredPredicate 用「全库所有 rule 的内容」当覆盖集合，
// 而开场注入只有 --limit（默认 3）条 + 字符预算。两者的差集里那些规则
// 开场进不去、每轮又被判 notes_duplicate 跳过 —— 整个库的生命周期里一次
// 都进不了上下文，而审计只表现为「正常去重」。
//
// 两条验收线：
//  ① 被 limit 截掉的规则，后续提问**必须能注入**（本文件的核心断言）；
//  ② 开场已注入的那条，提问时**仍然被跳过**（去重没被整个关掉）。

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// hookStdinWithSession 与 hookStdin 同形，额外带 session_id。
//
// 会话维度是本次修复的判据来源：开场写轨迹与后续检索必须落同一个 session_id，
// 否则「本会话注入了什么」无从对应。生产宿主总会带（Cursor 走 conversation_id 兜底）。
func hookStdinWithSession(event, cwd, prompt, session string) string {
	m := map[string]string{"hook_event_name": event, "cwd": cwd, "session_id": session}
	if prompt != "" {
		m["prompt"] = prompt
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// notesDedupFixture 造 5 条跨工程硬规则，salience 递减 —— 开场必然只装得下前 3 条。
//
// 用 salience 而不是「碰巧的顺序」来保证 cap 外的目标条稳定：sessionStartContext
// 的硬规则段按 List 的 (salience DESC, ...) 排序，前 3 条恒为 rule[0..2]。
const (
	rule1 = "规则1号 提交前必跑 make cover-gate 门禁"
	rule2 = "规则2号 禁止直接把改动 push 到 main 分支"
	rule3 = "规则3号 部署脚本统一放在 scripts 目录"
	rule4 = "规则4号 数据库迁移必须可回滚"  // cap 外
	rule5 = "规则5号 日志里禁止打明文手机号" // cap 外
)

func seedNotesDedupRules(t *testing.T, db string) {
	t.Helper()
	for i, c := range []string{rule1, rule2, rule3, rule4, rule5} {
		sal := fmt.Sprintf("%.2f", 0.95-float64(i)*0.05)
		if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule",
			"--salience", sal, c}); err != nil {
			t.Fatalf("cmdAdd rule %d: %v", i+1, err)
		}
	}
}

// runHookWithSession 跑一次钩子，返回 stdout。审计显式关掉，绝不落真实日志。
func runHookWithSession(t *testing.T, db, proj, event, prompt, session string, extra ...string) string {
	t.Helper()
	t.Setenv("MEM_NO_DISTILL", "1")
	args := append([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"},
		extra...)
	out, err := withStdin(t, hookStdinWithSession(event, proj, prompt, session), func() error {
		return cmdHook(args)
	})
	if err != nil {
		t.Fatalf("cmdHook(%s): %v", event, err)
	}
	return out
}

// latestRetrievalTrace 取最近一条**检索类**轨迹的条目，用于断言「为什么没注入」。
//
// 不能直接取 RecentReadTraces(1)：轨迹按 (created_at DESC, id DESC) 排序，
// 同一秒内写完的开场轨迹与检索轨迹会按随机 id 破平，取到开场那条。
// 这里显式跳过开场类 gate_reason，保证拿到的是 prompt-submit 这一次。
func latestRetrievalTrace(t *testing.T, db string) (store.ReadTrace, []store.ReadTraceItem) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ts, err := st.RecentReadTraces(10)
	if err != nil {
		t.Fatalf("RecentReadTraces: %v", err)
	}
	for _, x := range ts {
		if x.GateReason == "session_start" || x.GateReason == "pre_compact" {
			continue
		}
		tr, items, err := st.GetReadTrace(x.ID)
		if err != nil {
			t.Fatalf("GetReadTrace: %v", err)
		}
		return tr, items
	}
	t.Fatalf("应写出检索类读侧轨迹: %+v", ts)
	return store.ReadTrace{}, nil
}

// 核心验收：--limit 3 装不下的第 4 条规则，提问时必须能注入。
func TestHookNotesDedupDoesNotStarveRuleDroppedByLimit(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	seedNotesDedupRules(t, db)
	const session = "sess-notes-dedup-core"

	open := runHookWithSession(t, db, proj, "SessionStart", "", session, "--limit", "3")
	for _, want := range []string{"规则1号", "规则2号", "规则3号"} {
		if !strings.Contains(open, want) {
			t.Fatalf("前置条件不成立：开场应注入 %s，got:\n%s", want, open)
		}
	}
	if strings.Contains(open, "规则4号") {
		t.Fatalf("前置条件不成立：规则4号 应在 limit 之外，got:\n%s", open)
	}

	out := runHookWithSession(t, db, proj, "UserPromptSubmit", "数据库迁移必须可回滚", session)
	if !strings.Contains(out, "规则4号") {
		t.Fatalf("被 limit 截掉的规则必须能在后续提问注入，got:\n%s", out)
	}

	tr, items := latestRetrievalTrace(t, db)
	if tr.Injected == 0 {
		t.Fatalf("轨迹应记录本次注入: %+v", tr)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	injectedRule4 := false
	for _, it := range items {
		if it.InjectionRank == 0 {
			continue
		}
		d, err := st.MemoryDetailByID(it.MemoryID)
		if err != nil {
			t.Fatalf("MemoryDetailByID: %v", err)
		}
		if strings.Contains(d.Content, "规则4号") {
			injectedRule4 = true
		}
	}
	if !injectedRule4 {
		t.Fatalf("轨迹里规则4号 的 injection_rank 应 > 0: %+v", items)
	}
}

// 反向：开场已注入的那条规则，提问时仍被去重跳过 —— 别把去重整个关掉。
func TestHookNotesDedupStillSkipsRuleInjectedAtSessionStart(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	seedNotesDedupRules(t, db)
	const session = "sess-notes-dedup-reverse"

	open := runHookWithSession(t, db, proj, "SessionStart", "", session, "--limit", "3")
	if !strings.Contains(open, "规则1号") {
		t.Fatalf("前置条件不成立：开场应注入 规则1号，got:\n%s", open)
	}

	out := runHookWithSession(t, db, proj, "UserPromptSubmit", "提交前必跑 make cover-gate 门禁", session)
	if strings.Contains(out, "规则1号") {
		t.Fatalf("开场已注入的规则不应重复注入，got:\n%s", out)
	}

	_, items := latestRetrievalTrace(t, db)
	dropped := 0
	for _, it := range items {
		if it.InjectionRank == 0 && it.DropReason == "notes_duplicate" {
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatalf("应有条目以 notes_duplicate 被丢弃（去重不能整个失效）: %+v", items)
	}
}

// 本会话没有开场注入记录时不去重：没有「已覆盖过」的证据就别拦。
// 这条与「失败开放」同源 —— 宁可多注入，漏注入是功能缺失。
func TestHookNotesDedupFailsOpenWithoutSessionStart(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	seedNotesDedupRules(t, db)

	out := runHookWithSession(t, db, proj, "UserPromptSubmit", "数据库迁移必须可回滚", "sess-never-opened")
	if !strings.Contains(out, "规则4号") {
		t.Fatalf("无开场轨迹时应照常检索注入，got:\n%s", out)
	}
}
