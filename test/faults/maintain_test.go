//go:build faults

// 故障注入套件（S9b）：maintain 入口的 F7–F12。
//
// 与 S9a 同一条纪律：只跑真实二进制（临时 HOME + 独立库 + 子进程），
// 不 import 产品实现；注入手法复用 internal/testkit。
// 断言口径见 docs/design-eval-flows.md §3.2：
//
//	不丢数据、不静默、重启幂等、退出码/状态如实。
//
// 运行：make test-faults
package faults

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/testkit"
)

// maintainJSON 是 `mem maintain --json` 的稳定字段面（只取用例用到的键）。
type maintainJSON struct {
	DryRun        bool   `json:"dry_run"`
	Forced        int    `json:"forced_retry"`
	Processed     int    `json:"processed"`
	OK            int    `json:"ok"`
	Noop          int    `json:"noop"`
	Fail          int    `json:"fail"`
	Partial       int    `json:"partial"`
	Quarantine    int    `json:"quarantine"`
	Backlog       int    `json:"backlog"`
	WouldGenerate int    `json:"would_generate"`
	Remaining     int    `json:"remaining"`
	Blocked       string `json:"blocked,omitempty"`
	Results       []struct {
		Bundle       string `json:"bundle"`
		Status       string `json:"status"`
		BundleStatus string `json:"bundle_status"`
		Inserted     int    `json:"inserted"`
		Attempt      int    `json:"attempt"`
		Error        string `json:"error"`
	} `json:"results"`
}

func parseMaintainJSON(t *testing.T, r cmdResult) maintainJSON {
	t.Helper()
	var out maintainJSON
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		t.Fatalf("解析 maintain --json: %v\nstdout:\n%s\nstderr:\n%s", err, r.Stdout, r.Stderr)
	}
	return out
}

// seedPendingBundle 造一条可提案证据并切出一个单窗 Bundle，再把它恢复成
// 「可立即调度」的 pending：`--executor none` 建窗后会被记成 backlog（退避 10 分钟），
// 用例要验的是 maintain 的处理路径，所以显式清掉退避与认领痕迹。
func seedPendingBundle(t *testing.T, e *faultEnv, sid, uid string) string {
	t.Helper()
	requireCode(t, e.hookPrompt(sid, "决策：maintain 故障注入 "+uid), 0, "seed 证据")
	requireCode(t, e.run("distill", "--db", e.db, "--proposal", "--executor", "none", "--json"), 0, "建 Bundle")

	db := openDB(t, e.db)
	id := queryStr(t, db, `SELECT id FROM evidence_bundles ORDER BY created_at DESC, id DESC LIMIT 1`)
	if _, err := db.Exec(`UPDATE evidence_bundles
		SET status='pending', attempt=0, not_before=0, lease_until=0, lease_owner='', last_run_id=''
		WHERE id=?`, id); err != nil {
		t.Fatalf("重置 Bundle %s: %v", id, err)
	}
	db.Close()
	return id
}

type bundleState struct {
	Status     string
	Attempt    int
	NotBefore  int64
	LeaseUntil int64
	LeaseOwner string
}

func loadBundleState(t *testing.T, dbPath, id string) bundleState {
	t.Helper()
	db := openDB(t, dbPath)
	defer db.Close()
	var s bundleState
	if err := db.QueryRow(`SELECT status, attempt, not_before, lease_until, lease_owner
		FROM evidence_bundles WHERE id=?`, id).Scan(
		&s.Status, &s.Attempt, &s.NotBefore, &s.LeaseUntil, &s.LeaseOwner); err != nil {
		t.Fatalf("读 Bundle %s: %v", id, err)
	}
	return s
}

func countCandidates(t *testing.T, dbPath string) int {
	t.Helper()
	db := openDB(t, dbPath)
	defer db.Close()
	return queryInt(t, db, `SELECT COUNT(*) FROM candidates`)
}

func countDuplicateCandidates(t *testing.T, dbPath string) int {
	t.Helper()
	db := openDB(t, dbPath)
	defer db.Close()
	return queryInt(t, db, `SELECT COUNT(*) FROM (
		SELECT session_id, content_hash FROM candidates
		GROUP BY session_id, content_hash HAVING COUNT(*) > 1
	)`)
}

// metaValue 读 db_meta；键不存在返回 ("", false)。
func metaValue(t *testing.T, dbPath, key string) (string, bool) {
	t.Helper()
	db := openDB(t, dbPath)
	defer db.Close()
	var v string
	err := db.QueryRow(`SELECT v FROM db_meta WHERE k=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("读 db_meta[%s]: %v", key, err)
	}
	return v, true
}

// waitForFile 是可诊断的同步点：轮询到出现为止，上限内没出现就失败，
// 不用固定 sleep 赌进程进度。
func waitForFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%v 内没等到同步文件 %s", within, path)
}

// installFakeCLISlow 与 installFakeCLI 同为假 claude，但首次调用会先写 marker
// 再阻塞到 release 出现：给 kill -9 用例一个「已认领、正在执行」的显式同步点。
// 第二次调用（release 已存在）立即输出，重启接管不会被拖慢。
func (e *faultEnv) installFakeCLISlow(body string) (marker, release string) {
	e.t.Helper()
	marker = filepath.Join(e.home, "maintain-exec-start")
	release = filepath.Join(e.home, "maintain-exec-release")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo '9.9.9 (faults)'; exit 0; fi\n" +
		"cat >/dev/null 2>&1 || true\n" +
		"if [ ! -f '" + release + "' ]; then\n" +
		"  : > '" + marker + "'\n" +
		"  i=0\n" +
		"  while [ $i -lt 400 ] && [ ! -f '" + release + "' ]; do i=$((i+1)); sleep 0.05; done\n" +
		"fi\n" +
		"cat <<'FAKE_JSON'\n" + body + "\nFAKE_JSON\n"
	path := filepath.Join(e.binDir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		e.t.Fatalf("写慢速假 claude: %v", err)
	}
	cfg := `{"mode":"cli","cli":"claude","model_tag":"faults-1"}`
	if err := os.WriteFile(filepath.Join(e.dataDir(), "model.json"), []byte(cfg), 0o600); err != nil {
		e.t.Fatalf("写 model.json: %v", err)
	}
	return marker, release
}

// F7 部分写：第二条候选落库失败 → maintain 如实报 partial，且不产生半提交候选；
// 撤销故障后重跑按内容去重，最终恰好两条。
func TestFaultMaintainPartialWriteReportsPartialAndRetriesIdempotently(t *testing.T) {
	e := newFaultEnv(t)
	e.installFakeCLI(twoDrafts())
	requireCode(t, e.hookPrompt("s-mnt-partial", "决策：维护部分写要如实且可重试"), 0, "seed 证据")

	db := openDB(t, e.db)
	undo, err := testkit.FailWritesWhere(db, "candidates", "INSERT", "NEW.content LIKE '%候选二%'")
	if err != nil {
		t.Fatalf("FailWritesWhere: %v", err)
	}

	r := e.run("maintain", "--db", e.db, "--json")
	// 逐窗失败是「已处理但结果不佳」，不是命令级中断：退出码仍为 0（S8 口径）。
	requireCode(t, r, 0, "部分写 maintain")
	rep := parseMaintainJSON(t, r)
	if rep.Processed != 1 || rep.Partial != 1 || rep.Fail != 0 {
		t.Fatalf("应如实报 partial=1：%+v", rep)
	}
	if len(rep.Results) != 1 || !strings.Contains(rep.Results[0].Error, "testkit injected") {
		t.Fatalf("逐窗必须带出注入原因：%+v", rep.Results)
	}
	if !strings.Contains(r.Stderr, "testkit injected") {
		t.Fatalf("stderr 必须可诊断落库失败：\n%s", r.Stderr)
	}

	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates`); n != 1 {
		t.Fatalf("部分写只应留下成功那条，got %d", n)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidate_evidence`); n != 1 {
		t.Fatalf("成功候选的证据引用必须与候选同一事务落库，got %d 条引用", n)
	}
	var st bundleState
	if err := db.QueryRow(`SELECT status, attempt, not_before, lease_until, lease_owner
		FROM evidence_bundles LIMIT 1`).Scan(
		&st.Status, &st.Attempt, &st.NotBefore, &st.LeaseUntil, &st.LeaseOwner); err != nil {
		t.Fatalf("读失败窗状态: %v", err)
	}
	if st.Status != "partial" || st.Attempt != 1 {
		t.Fatalf("失败窗应落 partial 且 attempt=1：%+v", st)
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	db.Close()

	// 撤销故障后强制重试退避中的 partial 头：第一条按内容去重、第二条补齐。
	r = e.run("maintain", "--db", e.db, "--force", "--json")
	requireCode(t, r, 0, "撤销后 --force 重跑")
	rep = parseMaintainJSON(t, r)
	if rep.Forced != 1 || rep.Processed != 1 || rep.OK != 1 {
		t.Fatalf("重跑应清退避并完成：%+v", rep)
	}
	if n := countCandidates(t, e.db); n != 2 {
		t.Fatalf("重跑后应恰好 2 条，got %d", n)
	}
	if n := countDuplicateCandidates(t, e.db); n != 0 {
		t.Fatalf("重跑不得产生重复候选：%d 组", n)
	}
	if st := loadBundleState(t, e.db, rep.Results[0].Bundle); st.Status != "ok" {
		t.Fatalf("重跑成功后 Bundle 应为 ok：%+v", st)
	}

	// 第三跑：无新证据，不得再落候选。
	requireCode(t, e.run("maintain", "--db", e.db, "--json"), 0, "第三次 maintain")
	if n := countCandidates(t, e.db); n != 2 {
		t.Fatalf("幂等重跑不得新增候选，got %d", n)
	}
}

// F8 kill -9：在 Executor 执行中强杀 maintain → 租约过期后重启可接管，
// 不重复消费同一窗口，Bundle 不永久卡在 leased。
func TestFaultMaintainKill9LeaseTakenOverWithoutDuplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL 用例只覆盖 unix")
	}
	e := newFaultEnv(t)
	marker, release := e.installFakeCLISlow(twoDrafts())
	requireCode(t, e.hookPrompt("s-mnt-kill", "决策：维护中途被杀后要能接管"), 0, "seed 证据")

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("打开 devnull: %v", err)
	}
	defer devnull.Close()

	cmd := exec.Command(memBin, "maintain", "--db", e.db, "--json")
	cmd.Dir = repoRootPath
	cmd.Env = e.env()
	// 指向真实文件而不是 bytes.Buffer：否则 Wait 会等孙进程（假 claude）关掉
	// 管道才返回，kill 后反而卡住。
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 maintain: %v", err)
	}
	waitForFile(t, marker, 30*time.Second)
	if err := testkit.KillNow(cmd); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatalf("被 SIGKILL 的 maintain 不应成功退出")
	}
	// 放行孤儿假 claude，避免它继续阻塞。
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("写 release: %v", err)
	}

	db := openDB(t, e.db)
	id := queryStr(t, db, `SELECT id FROM evidence_bundles ORDER BY created_at, id LIMIT 1`)
	var st bundleState
	if err := db.QueryRow(`SELECT status, attempt, lease_until, lease_owner
		FROM evidence_bundles WHERE id=?`, id).Scan(&st.Status, &st.Attempt, &st.LeaseUntil, &st.LeaseOwner); err != nil {
		t.Fatalf("读被杀后的 Bundle: %v", err)
	}
	if st.Status != "leased" || st.LeaseOwner == "" || st.LeaseUntil <= time.Now().Unix() {
		t.Fatalf("被杀时应留下有效租约（供过期后接管）：%+v", st)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates`); n != 0 {
		t.Fatalf("执行中被杀不应留下半提交候选，got %d", n)
	}
	// 模拟租约到期：用例不真等 2 分钟，但走的仍是生产判据 (lease_until <= now)。
	if _, err := db.Exec(`UPDATE evidence_bundles SET lease_until=0 WHERE id=?`, id); err != nil {
		t.Fatalf("模拟租约过期: %v", err)
	}
	db.Close()

	requireCode(t, e.run("maintain", "--db", e.db, "--json"), 0, "重启接管")
	db = openDB(t, e.db)
	defer db.Close()
	if err := db.QueryRow(`SELECT status, attempt, lease_until, lease_owner
		FROM evidence_bundles WHERE id=?`, id).Scan(&st.Status, &st.Attempt, &st.LeaseUntil, &st.LeaseOwner); err != nil {
		t.Fatalf("读接管后的 Bundle: %v", err)
	}
	if st.Status != "ok" || st.LeaseOwner != "" || st.LeaseUntil != 0 {
		t.Fatalf("接管后应完成且释放租约（不得永久 leased）：%+v", st)
	}
	if st.Attempt != 2 {
		t.Fatalf("接管应只把 attempt 从 1 加到 2，got %d", st.Attempt)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates`); n != 2 {
		t.Fatalf("接管后应补齐 2 条候选，got %d", n)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM (
		SELECT session_id, content_hash FROM candidates GROUP BY session_id, content_hash HAVING COUNT(*) > 1
	)`); n != 0 {
		t.Fatalf("接管后出现重复候选：%d 组", n)
	}
}

// F9 只读：库目录不可写 → 非 0 退出、错误可诊断、库内状态不变。
//
// 基线（S8）只在成功路径写 maintain:last_ok，尚无 maintain:last_fail_at；
// 按 S9b 约定本片不改产品，故这里只守「如实失败 + 无脏状态」，
// last_fail_at 的缺口留给 S11。
func TestFaultMaintainReadOnlyDataDirExitsNonZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 语义在 Windows 不适用")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	e := newFaultEnv(t)
	e.installFakeCLI(twoDrafts())
	id := seedPendingBundle(t, e, "s-mnt-ro", "ro")

	db := openDB(t, e.db)
	flushAndClose(t, db)

	restore, err := testkit.ReadOnlyDir(e.dataDir())
	if err != nil {
		t.Fatalf("ReadOnlyDir: %v", err)
	}
	r := e.run("maintain", "--db", e.db, "--json")
	if err := restore(); err != nil {
		t.Fatalf("恢复目录权限: %v", err)
	}
	if r.Code == 0 {
		t.Fatalf("只读目录下 maintain 应非零退出\nstdout:\n%s", r.Stdout)
	}
	if strings.TrimSpace(r.Stderr) == "" {
		t.Fatalf("只读失败必须有可诊断的 stderr")
	}
	if strings.Contains(r.Stderr, "panic") {
		t.Fatalf("不得 panic:\n%s", r.Stderr)
	}

	if st := loadBundleState(t, e.db, id); st.Status != "pending" || st.Attempt != 0 || st.LeaseOwner != "" {
		t.Fatalf("只读失败不得留下认领/推进痕迹：%+v", st)
	}
	if n := countCandidates(t, e.db); n != 0 {
		t.Fatalf("只读失败不得落候选，got %d", n)
	}
	if _, ok := metaValue(t, e.db, "maintain:last_ok"); ok {
		t.Fatalf("失败路径不得写 maintain:last_ok")
	}
}

// F10 SQLITE_BUSY：另一条连接持写锁 → 不 panic、错误可见、状态不变；解锁后可继续。
func TestFaultMaintainBusyReportsWithoutDirtyState(t *testing.T) {
	e := newFaultEnv(t)
	e.installFakeCLI(twoDrafts())
	id := seedPendingBundle(t, e, "s-mnt-busy", "busy")

	release, err := testkit.HoldWriteLock(e.db)
	if err != nil {
		t.Fatalf("HoldWriteLock: %v", err)
	}
	r := e.run("maintain", "--db", e.db, "--json")
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	if r.Code == 0 {
		t.Fatalf("写锁被占时 maintain 应非零退出\nstdout:\n%s", r.Stdout)
	}
	low := strings.ToLower(r.Stderr)
	if !strings.Contains(low, "busy") && !strings.Contains(low, "locked") {
		t.Fatalf("stderr 应说明库被占用：\n%s", r.Stderr)
	}
	if strings.Contains(r.Stderr, "panic") {
		t.Fatalf("不得 panic:\n%s", r.Stderr)
	}
	if st := loadBundleState(t, e.db, id); st.Status != "pending" || st.Attempt != 0 || st.LeaseOwner != "" {
		t.Fatalf("BUSY 失败不得改 Bundle 状态：%+v", st)
	}
	if n := countCandidates(t, e.db); n != 0 {
		t.Fatalf("BUSY 失败不得落候选，got %d", n)
	}

	requireCode(t, e.run("maintain", "--db", e.db, "--json"), 0, "解锁后 maintain")
	if st := loadBundleState(t, e.db, id); st.Status != "ok" {
		t.Fatalf("解锁后应能继续完成：%+v", st)
	}
	if n := countCandidates(t, e.db); n != 2 {
		t.Fatalf("解锁后应产出 2 条候选，got %d", n)
	}
}

// F11 dry-run × 全表写故障：不认领、不写库、不写 last_ok/last_fail。
// 触发器装在 candidates 与 evidence_bundles 上——dry-run 若偷偷落库或改 Bundle，
// 会直接以注入错误暴露，而不是靠「跑完再看」。
func TestFaultMaintainDryRunWritesNothingUnderWriteFault(t *testing.T) {
	e := newFaultEnv(t)
	e.installFakeCLI(twoDrafts())
	id := seedPendingBundle(t, e, "s-mnt-dry", "dry")

	db := openDB(t, e.db)
	runsBefore := queryInt(t, db, `SELECT COUNT(*) FROM distill_runs`)
	undoCand, err := testkit.FailWrites(db, "candidates")
	if err != nil {
		t.Fatalf("FailWrites(candidates): %v", err)
	}
	defer undoCand()
	undoBundle, err := testkit.FailWrites(db, "evidence_bundles")
	if err != nil {
		t.Fatalf("FailWrites(evidence_bundles): %v", err)
	}
	defer undoBundle()

	r := e.run("maintain", "--db", e.db, "--dry-run", "--json")
	requireCode(t, r, 0, "dry-run")
	rep := parseMaintainJSON(t, r)
	if !rep.DryRun || rep.Processed != 1 || rep.WouldGenerate != 1 || rep.Remaining != 1 {
		t.Fatalf("dry-run 应报告将生成且不消费窗口：%+v", rep)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates`); n != 0 {
		t.Fatalf("dry-run 不得写候选，got %d", n)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM distill_runs`); n != runsBefore {
		t.Fatalf("dry-run 不得写运行账：before=%d after=%d", runsBefore, n)
	}
	if st := loadBundleState(t, e.db, id); st.Status != "pending" || st.Attempt != 0 || st.LeaseOwner != "" || st.LeaseUntil != 0 {
		t.Fatalf("dry-run 不得认领/改状态：%+v", st)
	}
	if _, ok := metaValue(t, e.db, "maintain:last_ok"); ok {
		t.Fatalf("dry-run 不得写 maintain:last_ok")
	}
	if _, ok := metaValue(t, e.db, "maintain:last_fail_at"); ok {
		t.Fatalf("dry-run 不得写 maintain:last_fail_at")
	}
}

// F12 --force + 退避中的 failed 头 + 候选写故障：
// 失败如实进账（fail + 逐窗 error），attempt 只由 claim 加 1，
// 失败后重新进入退避；再次不 --force 时不得再涨 attempt。
func TestFaultMaintainForceUnderWriteFaultKeepsAttemptHonest(t *testing.T) {
	e := newFaultEnv(t)
	e.installFakeCLI(twoDrafts())
	id := seedPendingBundle(t, e, "s-mnt-force", "force")

	db := openDB(t, e.db)
	now := time.Now().Unix()
	if _, err := db.Exec(`UPDATE evidence_bundles
		SET status='fail', attempt=2, not_before=? WHERE id=?`, now+3600, id); err != nil {
		t.Fatalf("造退避中的 failed 头: %v", err)
	}
	undo, err := testkit.FailWrites(db, "candidates")
	if err != nil {
		t.Fatalf("FailWrites: %v", err)
	}

	r := e.run("maintain", "--db", e.db, "--force", "--json")
	requireCode(t, r, 0, "--force maintain")
	rep := parseMaintainJSON(t, r)
	if rep.Forced != 1 || rep.Processed != 1 || rep.Fail != 1 {
		t.Fatalf("--force 应清退避并如实报 fail：%+v", rep)
	}
	if len(rep.Results) != 1 || !strings.Contains(rep.Results[0].Error, "testkit injected") {
		t.Fatalf("失败原因必须逐窗可见：%+v", rep.Results)
	}
	if !strings.Contains(r.Stderr, "testkit injected") {
		t.Fatalf("stderr 必须可诊断：\n%s", r.Stderr)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates`); n != 0 {
		t.Fatalf("全部落库失败时不得有候选，got %d", n)
	}
	st := loadBundleState(t, e.db, id)
	if st.Attempt != 3 {
		t.Fatalf("--force 不改 attempt，claim 只 +1（2→3），got %d", st.Attempt)
	}
	if st.Status != "fail" || st.NotBefore <= time.Now().Unix() {
		t.Fatalf("失败后应重新进入退避，状态仍为 fail：%+v", st)
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	db.Close()

	// 故障撤销但不 --force：退避中的头只能让位，attempt 不得再变。
	r = e.run("maintain", "--db", e.db, "--json")
	requireCode(t, r, 0, "退避中 maintain")
	rep = parseMaintainJSON(t, r)
	if rep.Processed != 0 || rep.Backlog != 1 {
		t.Fatalf("退避中的头应整轮让位：%+v", rep)
	}
	if got := loadBundleState(t, e.db, id); got.Attempt != 3 || got.Status != "fail" {
		t.Fatalf("让位不得改 attempt/status：%+v", got)
	}
	if n := countCandidates(t, e.db); n != 0 {
		t.Fatalf("让位不得落候选，got %d", n)
	}
}
