package main

// S11：统一触达面（design-data-lifecycle §3.1）——「问题找人」的单行注入。
//
// 本文件只放集成用例：用真实库 + 真实钩子入口，断言 stdout 里那一行
// 与人看到的样子一致。纯组装/预算逻辑的逐字断言在 touch_unit_test.go。
//
// 数据口径（与 S4 backlog 同源）：
//   - 隔离 x = candidates_isolated + bundles_isolated
//   - 未审核最老 y 天 = 全局 pending 的最老年龄，向上取整
//   - 模型不可用 = 最近一条 proposal distill 审计是 backlog 且无 provider
//   - 维护失败 = db_meta maintain:last_fail_at 在 24h 内且晚于 last_ok

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/store"
)

// ensureTouchDB 先让 store 建好 schema，直插才有表可写。
func ensureTouchDB(t *testing.T, db string) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close()
}

// insertTouchCandidate 直插候选：缺口在 created_at/status，走 CLI 造不出来。
func insertTouchCandidate(t *testing.T, db, id, project, status string, createdAt int64) {
	t.Helper()
	ensureTouchDB(t, db)
	raw := rawDB(t, db)
	if _, err := raw.Exec(`INSERT INTO candidates(id, session_id, project, kind, content,
		content_hash, status, source, created_at)
		VALUES(?, ?, ?, 'fact', ?, ?, ?, 'distill', ?)`,
		id, "s-"+id, project, "触达面候选 "+id, "h-"+id, status, createdAt); err != nil {
		t.Fatalf("插入候选 %s: %v", id, err)
	}
}

// insertTouchQuarantinedBundle 造一个隔离 Bundle（要求先有证据源）。
func insertTouchQuarantinedBundle(t *testing.T, db, id string, createdAt int64) {
	t.Helper()
	ensureTouchDB(t, db)
	raw := rawDB(t, db)
	if _, err := raw.Exec(`INSERT INTO evidence_sources(id, kind, locator, generation,
		last_seq, cursor_seq, created_at, updated_at)
		VALUES(?, 'hook', '/tmp/s11', 1, 0, 0, ?, ?)`, "src-"+id, createdAt, createdAt); err != nil {
		t.Fatalf("插入证据源 %s: %v", id, err)
	}
	if _, err := raw.Exec(`INSERT INTO evidence_bundles(id, source_id, generation,
		from_seq_exclusive, to_seq_inclusive, evidence_hash, status, created_at, updated_at)
		VALUES(?, ?, 1, 0, 0, ?, 'quarantined', ?, ?)`,
		id, "src-"+id, "eh-"+id, createdAt, createdAt); err != nil {
		t.Fatalf("插入隔离 Bundle %s: %v", id, err)
	}
}

func setTouchMeta(t *testing.T, db, key, val string) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := st.MetaSet(key, val); err != nil {
		t.Fatalf("MetaSet(%s): %v", key, err)
	}
}

// runTouchSessionStart 跑真实钩子入口，审计固定落到 auditFile（"-" 即关闭）。
func runTouchSessionStart(t *testing.T, db, proj, auditFile string) string {
	t.Helper()
	t.Setenv("MEM_NO_AUTOMATION", "1") // 只测注入行，不拉起子进程
	out, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", auditFile})
	})
	if err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	return out
}

// touchLineOf 取唯一一行触达面；多于一行说明「不新增第二条注入」被破坏。
func touchLineOf(t *testing.T, out string) string {
	t.Helper()
	var found []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "待人工") {
			found = append(found, ln)
		}
	}
	if len(found) > 1 {
		t.Fatalf("触达面必须只有一行，got %d:\n%s", len(found), out)
	}
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

func writeProposalAuditFixture(t *testing.T, path, mode, provider string) {
	t.Helper()
	if err := audit.Append(path, audit.Record{
		TS: time.Now().Unix(), Event: "distill", Source: "proposal",
		Mode: mode, Provider: provider,
	}); err != nil {
		t.Fatalf("写审计: %v", err)
	}
}

// 无任何待人工项时零输出：既不能出现触达行，也不能留一行空行。
func TestSessionStartTouchSilentWhenNothingPending(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "quiet-repo")
	out := runTouchSessionStart(t, db, proj, "-")

	if line := touchLineOf(t, out); line != "" {
		t.Fatalf("无待人工项时不应注入触达行，got %q", line)
	}
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			t.Fatalf("无待人工项时不得输出空行，got:\n%q", out)
		}
	}
}

// 只有隔离：候选 + Bundle 两个计数合并成一个「隔离 x」。
func TestSessionStartTouchIsolationOnly(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "iso-repo")
	now := time.Now().Unix()
	insertTouchCandidate(t, db, "c-q", "iso-repo", "quarantined", now-3600)
	insertTouchCandidate(t, db, "c-f", "iso-repo", "failed", now-7200)
	insertTouchQuarantinedBundle(t, db, "b-q", now-3600)

	out := runTouchSessionStart(t, db, proj, "-")
	if got, want := touchLineOf(t, out), "待人工 1 项（隔离 3）"; got != want {
		t.Fatalf("触达行 = %q, want %q", got, want)
	}
}

// 只有未审核：年龄向上取整（2 天 1 小时 → 3 天），并沿用既有 [待裁决] 指引。
func TestSessionStartTouchUnreviewedOnlyMergesPendingHint(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "slow-review")
	now := time.Now().Unix()
	insertTouchCandidate(t, db, "c-p", name, "pending", now-(2*24*3600+3600))

	out := runTouchSessionStart(t, db, proj, "-")
	line := touchLineOf(t, out)
	if !strings.HasPrefix(line, "待人工 1 项（未审核最老 3 天）") {
		t.Fatalf("触达行应以未审核项开头，got %q", line)
	}
	if !strings.Contains(line, "[待裁决]") || !strings.Contains(line, "本工程 1 条") {
		t.Fatalf("pending 计数必须合并进同一条触达行，got %q", line)
	}
	if n := strings.Count(out, "[待裁决]"); n != 1 {
		t.Fatalf("[待裁决] 只应出现一次（不新增第二条注入），got %d", n)
	}
}

// 只有模型不可用：最近一条 proposal distill 审计是 backlog 且没有 provider。
func TestSessionStartTouchModelUnavailableOnly(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "no-provider")
	auditFile := filepath.Join(t.TempDir(), "audit.jsonl")
	writeProposalAuditFixture(t, auditFile, "backlog", "")

	out := runTouchSessionStart(t, db, proj, auditFile)
	if got, want := touchLineOf(t, out), "待人工 1 项（模型不可用）"; got != want {
		t.Fatalf("触达行 = %q, want %q", got, want)
	}
}

// 后续 distill 恢复后不再点亮：只看「最近一次」。
func TestSessionStartTouchModelSignalClearsAfterSuccess(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "model-recovered")
	auditFile := filepath.Join(t.TempDir(), "audit.jsonl")
	writeProposalAuditFixture(t, auditFile, "backlog", "")
	writeProposalAuditFixture(t, auditFile, "ok", "rules")

	if line := touchLineOf(t, runTouchSessionStart(t, db, proj, auditFile)); line != "" {
		t.Fatalf("最近一次 distill 已成功，不应再报模型不可用，got %q", line)
	}
}

// 只有维护失败：24h 内点亮。
func TestSessionStartTouchMaintainFailureOnly(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "maint-fail")
	setTouchMeta(t, db, "maintain:last_fail_at", strconv.FormatInt(time.Now().Unix(), 10))
	setTouchMeta(t, db, "maintain:last_fail_reason", "读取模型配置失败")

	out := runTouchSessionStart(t, db, proj, "-")
	if got, want := touchLineOf(t, out), "待人工 1 项（维护失败）"; got != want {
		t.Fatalf("触达行 = %q, want %q", got, want)
	}
}

// 成功维护后（last_ok 追上）24h 内不再点亮；超过 24h 的旧失败也不再点亮。
func TestSessionStartTouchMaintainFailureClears(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "maint-recovered")
	now := time.Now().Unix()

	setTouchMeta(t, db, "maintain:last_fail_at", strconv.FormatInt(now-3600, 10))
	setTouchMeta(t, db, "maintain:last_ok", strconv.FormatInt(now, 10))
	if line := touchLineOf(t, runTouchSessionStart(t, db, proj, "-")); strings.Contains(line, "维护失败") {
		t.Fatalf("last_ok 已恢复后不应点亮维护失败，got %q", line)
	}

	setTouchMeta(t, db, "maintain:last_ok", strconv.FormatInt(now-48*3600, 10))
	setTouchMeta(t, db, "maintain:last_fail_at", strconv.FormatInt(now-25*3600, 10))
	if line := touchLineOf(t, runTouchSessionStart(t, db, proj, "-")); strings.Contains(line, "维护失败") {
		t.Fatalf("超过 24h 的旧失败不应点亮，got %q", line)
	}
}

// 四类同时出现：一条行按 隔离 → 未审核 → 模型 → 维护 固定顺序列出，N 为项数。
func TestSessionStartTouchAllSignalsInOneLine(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "all-signals")
	now := time.Now().Unix()
	insertTouchCandidate(t, db, "c-q", name, "quarantined", now-3600)
	insertTouchQuarantinedBundle(t, db, "b-q", now-3600)
	insertTouchCandidate(t, db, "c-p", name, "pending", now-(2*24*3600+3600))
	setTouchMeta(t, db, "maintain:last_fail_at", strconv.FormatInt(now, 10))
	auditFile := filepath.Join(t.TempDir(), "audit.jsonl")
	writeProposalAuditFixture(t, auditFile, "backlog", "")

	line := touchLineOf(t, runTouchSessionStart(t, db, proj, auditFile))
	want := "待人工 4 项（隔离 2 ｜ 未审核最老 3 天 ｜ 模型不可用 ｜ 维护失败）"
	if !strings.HasPrefix(line, want) {
		t.Fatalf("触达行 = %q, want 前缀 %q", line, want)
	}
	if n := len([]rune(line)); n > defaultHookBudget {
		t.Fatalf("触达行超出默认预算：%d > %d", n, defaultHookBudget)
	}
}

// 注入不得阻塞：库都打不开时钩子仍返回 nil，且不输出触达行。
func TestSessionStartTouchFailsOpen(t *testing.T) {
	db := filepath.Join(t.TempDir(), "not-a-db")
	if err := os.MkdirAll(db, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, _ := hookProjectDir(t, "broken-db")
	out := runTouchSessionStart(t, db, proj, "-")
	if line := touchLineOf(t, out); line != "" {
		t.Fatalf("库不可用时应零输出，got %q", line)
	}
}

// 维护失败 → 写 last_fail_at + 原因，下一次 SessionStart 点亮「维护失败」；
// 修好后再成功维护一次，last_ok 追上，同一窗口内立即熄灭。
//
// 故障注入方式：写一份非法 model.json，让 maintain 在**开库之后**失败
// （读取模型配置失败），这样失败打点路径与真实故障一致。
func TestMaintainFailureLightsTouchLineAndRecovers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	db := testDB(t)
	proj, _ := hookProjectDir(t, "maint-fail-e2e")

	cfgDir := filepath.Join(home, ".v2mem")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(cfgDir, "model.json")
	if err := os.WriteFile(cfg, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := captureStdout(t, func() error { return cmdMaintain([]string{"--db", db}) }); err == nil {
		t.Fatal("非法 model.json 时 maintain 应失败")
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	failAt, ok, err := st.MetaGet(store.MetaMaintainLastFailAt)
	if err != nil || !ok || failAt == "" {
		st.Close()
		t.Fatalf("失败后应写 %s，got %q ok=%v err=%v", store.MetaMaintainLastFailAt, failAt, ok, err)
	}
	reason, ok, err := st.MetaGet(store.MetaMaintainLastFailReason)
	st.Close()
	if err != nil || !ok || strings.TrimSpace(reason) == "" {
		t.Fatalf("失败后应写原因，got %q ok=%v err=%v", reason, ok, err)
	}
	if strings.Contains(reason, "\n") {
		t.Fatalf("原因必须是单行，got %q", reason)
	}

	line := touchLineOf(t, runTouchSessionStart(t, db, proj, "-"))
	if !strings.Contains(line, "维护失败") {
		t.Fatalf("失败后触达行应点亮「维护失败」，got %q", line)
	}

	// 修好配置并成功维护一次 → last_ok 追上 last_fail，触达行熄灭。
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return cmdMaintain([]string{"--db", db}) }); err != nil {
		t.Fatalf("恢复后 maintain 应成功: %v", err)
	}
	if line := touchLineOf(t, runTouchSessionStart(t, db, proj, "-")); strings.Contains(line, "维护失败") {
		t.Fatalf("成功维护后不应再点亮，got %q", line)
	}
}
