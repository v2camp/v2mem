package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

// gate 主动跳过的 query 不进入召回，也不计入审计的空注入分母。
func TestHookGateSkipsPleasantryWithoutEmptyAudit(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", name, "检索预算默认走类码表"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	auditPath := t.TempDir() + "/audit.jsonl"

	out, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "你好"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", auditPath})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("客套 query 不应注入，got:\n%s", out)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	tr, items, err := st.GetReadTrace(mustLatestTraceID(t, st))
	if err != nil {
		t.Fatalf("GetReadTrace: %v", err)
	}
	if tr.QueryGate != "skip" || tr.GateReason != "pleasantry" || tr.Outcome != "skipped_gate" {
		t.Fatalf("应记录 gate 跳过: %+v", tr)
	}
	if len(items) != 0 {
		t.Fatalf("gate 跳过不应有候选条目: %+v", items)
	}

	rs := readAudit(t, auditPath)
	if len(rs) != 1 {
		t.Fatalf("应有一条审计: %+v", rs)
	}
	if rs[0].Empty {
		t.Fatal("gate 跳过不是空命中，不能计入 empty 分母")
	}
}

// 只有 broad 单路支持的命中属于弱证据：无其它结构支持时弃答，不凑数。
func TestHookBroadOnlyAbstains(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", name, "记忆库不能放进同步目录"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	out, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "同步目录能不能放"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("仅 broad 单路支持应弃答，got:\n%s", out)
	}

	st, _ := store.Open(db)
	defer st.Close()
	tr, items, err := st.GetReadTrace(mustLatestTraceID(t, st))
	if err != nil {
		t.Fatalf("GetReadTrace: %v", err)
	}
	if tr.Outcome != "abstained_low_quality" || tr.Injected != 0 {
		t.Fatalf("结局应为低质量弃答: %+v", tr)
	}
	if len(items) != 1 || items[0].DropReason != "unsupported_quality" || items[0].Eligible {
		t.Fatalf("应记录不支持原因: %+v", items)
	}
}

// 检索/注入不自动 touch：adopted_count 与 access_count 都保持不变。
func TestHookInjectionDoesNotAutoAdopt(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", name, "检索结果必须带短 id"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if _, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "检索"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	st, _ := store.Open(db)
	defer st.Close()
	hits, _ := st.List(store.ListQuery{Scope: "all", Limit: 10})
	if len(hits) != 1 {
		t.Fatalf("前置条件不成立: %d", len(hits))
	}
	d, err := st.MemoryDetailByID(hits[0].ID)
	if err != nil {
		t.Fatalf("MemoryDetailByID: %v", err)
	}
	if d.AdoptedCount != 0 || d.LastAdopted != nil || d.AccessCount != 0 {
		t.Fatalf("注入不得自动 touch: %+v", d)
	}
}

// SessionStart 的「硬规则」段只取 kind=rule；全局高 salience 的普通记忆
// 仍有自己的注入段，但不得冒充硬规则。
func TestHookSessionStartHardRulesOnlyKindRule(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "rule", "硬规则样本：提交前跑门禁"}); err != nil {
		t.Fatalf("cmdAdd rule: %v", err)
	}
	if err := cmdAdd([]string{"--db", db, "--global", "--kind", "preference", "--salience", "0.95", "全局偏好样本：默认 pnpm"}); err != nil {
		t.Fatalf("cmdAdd preference: %v", err)
	}
	out, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	ruleAt := strings.Index(out, "硬规则（跨工程")
	globalAt := strings.Index(out, "全局记忆")
	prefAt := strings.Index(out, "全局偏好样本")
	if ruleAt < 0 || globalAt < 0 || prefAt < 0 {
		t.Fatalf("三段应都存在:\n%s", out)
	}
	if !(ruleAt < globalAt && globalAt < prefAt) {
		t.Fatalf("硬规则段必须在前、普通全局记忆在后:\n%s", out)
	}
}

// hook p95 < 50ms：真实注入路径（含 store.Open、召回、证据批量、轨迹写入）。
func TestHookReadSupplyP95Under50ms(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	db := testDB(t)
	_, name := hookProjectDir(t, "demo-repo")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 120; i++ {
		content := fmt.Sprintf("第%d条读侧供给基准记忆：部署脚本放在 scripts 目录，构建使用 make %d", i, i)
		if _, err := st.Add(store.AddInput{
			Content: content, Kind: "decision", Project: name, Source: "test", Salience: 0.6,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	st.Close()

	run := func() {
		out := renderHookContext(common{db: db}, harness.EventPromptSubmit, name, "部署脚本 构建", 3, 1200,
			hookMeta{SessionID: "bench", Harness: "claude", Event: "prompt-submit"})
		if out.Text == "" {
			t.Fatal("基准前置条件不成立：应有注入")
		}
	}
	for i := 0; i < 5; i++ {
		run()
	}
	const n = 80
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		run()
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	p95 := ds[int(float64(n)*0.95)-1]
	t.Logf("hook prompt-submit p95=%v（n=%d）", p95, n)
	if p95 >= 50*time.Millisecond {
		t.Fatalf("hook p95=%v，要求 <50ms", p95)
	}
}

func mustLatestTraceID(t *testing.T, st *store.Store) string {
	t.Helper()
	ts, err := st.RecentReadTraces(1)
	if err != nil || len(ts) == 0 {
		t.Fatalf("应写出读侧轨迹: %+v %v", ts, err)
	}
	return ts[0].ID
}

func readAudit(t *testing.T, path string) []audit.Record {
	t.Helper()
	rs, err := audit.Read(path, 0)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	return rs
}

// notes 去重后必须从合格池补满配额，而不是「先截 3 条再过滤、空了也不补」。
func TestHookBackfillsFullQuotaAfterNotesDedup(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	for i := 0; i < 3; i++ {
		if err := cmdAdd([]string{"--db", db, "--project", name, "--kind", "rule",
			fmt.Sprintf("检索规则 %d：提交前必须跑门禁", i)}); err != nil {
			t.Fatalf("cmdAdd rule: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := cmdAdd([]string{"--db", db, "--project", name, "--kind", "fact",
			fmt.Sprintf("检索事实 %d：部署脚本放在 scripts 目录", i)}); err != nil {
			t.Fatalf("cmdAdd fact: %v", err)
		}
	}
	if _, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "检索"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	res := renderHookContext(common{db: db}, harness.EventPromptSubmit, name, "检索", 3, 1200, hookMeta{})
	if len(res.Injected) != 3 {
		t.Fatalf("notes 去重后应补满 3 条，got %d", len(res.Injected))
	}
	for _, h := range res.Injected {
		if h.Kind != "fact" || !strings.Contains(h.Content, "检索事实") {
			t.Fatalf("补位应取事实条目: %+v", h)
		}
	}
}
