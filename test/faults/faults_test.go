//go:build faults

// 故障注入套件（S9a）：六类故障 × hook / gc / distill 三个既有入口。
//
// 这里只跑真实二进制：临时 HOME + 独立库 + 子进程，不 import 产品实现
// （除了共享注入助手 internal/testkit）。断言口径见 docs/design-eval-flows.md
// §3.2 故障矩阵；maintain 入口的同类注入留 S9b。
//
// 运行：make test-faults
package faults

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/v2camp/v2mem/internal/testkit"
	_ "modernc.org/sqlite"
)

var memBin, repoRootPath string

func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Getwd:", err)
		os.Exit(1)
	}
	repoRootPath = filepath.Dir(filepath.Dir(wd)) // test/faults → 仓库根

	dir, err := os.MkdirTemp("", "v2mem-faults-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "MkdirTemp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	memBin = filepath.Join(dir, "mem")
	build := exec.Command("go", "build", "-o", memBin, "./cmd/mem")
	build.Dir = repoRootPath
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "构建 mem 失败: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// ---------- 隔离环境与命令执行 ----------

type faultEnv struct {
	t      *testing.T
	home   string
	db     string
	binDir string
}

func newFaultEnv(t *testing.T) *faultEnv {
	t.Helper()
	home := t.TempDir()
	e := &faultEnv{
		t:      t,
		home:   home,
		db:     filepath.Join(home, ".v2mem", "mem.db"),
		binDir: filepath.Join(home, "bin"),
	}
	if err := os.MkdirAll(e.binDir, 0o755); err != nil {
		t.Fatalf("建 bin 目录: %v", err)
	}
	e.mustRun("stats")
	return e
}

func (e *faultEnv) env() []string {
	var out []string
	for _, kv := range os.Environ() {
		key := strings.SplitN(kv, "=", 2)[0]
		if key == "HOME" || key == "PATH" || strings.HasPrefix(key, "MEM_") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"HOME="+e.home,
		"PATH="+e.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		// 子落盘/后续蒸馏都由用例显式驱动：唯一总闸拦掉全部自动 spawn
		//（S12 已退役 MEM_NO_DISTILL / MEM_NO_COLDSTART / MEM_DISTILL_SPAWN 散闸）。
		"MEM_NO_AUTOMATION=1",
	)
	return out
}

type cmdResult struct {
	Stdout string
	Stderr string
	Code   int
}

func (e *faultEnv) runStdin(stdin string, args ...string) cmdResult {
	e.t.Helper()
	cmd := exec.Command(memBin, args...)
	cmd.Dir = repoRootPath
	cmd.Env = e.env()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			e.t.Fatalf("执行 mem %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return cmdResult{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
}

func (e *faultEnv) run(args ...string) cmdResult { return e.runStdin("", args...) }

func (e *faultEnv) mustRun(args ...string) cmdResult {
	e.t.Helper()
	r := e.run(args...)
	if r.Code != 0 {
		e.t.Fatalf("mem %v 退出码 %d\nstdout:\n%s\nstderr:\n%s", args, r.Code, r.Stdout, r.Stderr)
	}
	return r
}

func (e *faultEnv) dataDir() string   { return filepath.Dir(e.db) }
func (e *faultEnv) spoolRoot() string { return filepath.Join(e.dataDir(), "spool") }

// hookPrompt 走 UserPromptSubmit 写一条 user-prompt 证据（proposal 可用）。
func (e *faultEnv) hookPrompt(session, prompt string) cmdResult {
	e.t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      session,
		"prompt":          prompt,
		"cwd":             repoRootPath,
	})
	return e.runStdin(string(payload),
		"hook", "--db", e.db, "--harness", "claude",
		"--dedup-window", "0", "--audit", "-", "--no-scrub")
}

// installFakeCLI 在 PATH 前置一个假的 claude，让 model.json 的 cli 模式
// 走确定性输出：不联网、不依赖本机装了什么模型。
func (e *faultEnv) installFakeCLI(body string) {
	e.t.Helper()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo '9.9.9 (faults)'; exit 0; fi\n" +
		"cat >/dev/null 2>&1 || true\n" +
		"cat <<'FAKE_JSON'\n" + body + "\nFAKE_JSON\n"
	path := filepath.Join(e.binDir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		e.t.Fatalf("写假 claude: %v", err)
	}
	cfg := `{"mode":"cli","cli":"claude","model_tag":"faults-1"}`
	if err := os.WriteFile(filepath.Join(e.dataDir(), "model.json"), []byte(cfg), 0o600); err != nil {
		e.t.Fatalf("写 model.json: %v", err)
	}
}

// ---------- 库与文件断言 ----------

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开库 %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查询失败 %q: %v", query, err)
	}
	return n
}

// flushAndClose 在改目录权限前把 WAL 合并回主库并切回 DELETE 日志模式，
// 避免残留的 -wal/-shm（仍可写）让「只读目录」用例失效。
func flushAndClose(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatalf("切回 DELETE 日志: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关库: %v", err)
	}
}

func queryStr(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("查询失败 %q: %v", query, err)
	}
	return s
}

func countSpoolSegments(t *testing.T, dir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "seg-*.jsonl*"))
	if err != nil {
		t.Fatalf("glob spool: %v", err)
	}
	return len(matches)
}

func requireCode(t *testing.T, r cmdResult, want int, label string) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("%s 退出码=%d，want %d\nstdout:\n%s\nstderr:\n%s", label, r.Code, want, r.Stdout, r.Stderr)
	}
}

// ---------- hook ----------

// F1 部分写：观测落库被触发器拦住 → hook 必须 fail-open（退出 0）、
// 数据进 spool；撤销故障后 SessionStart 重放一次即补回，重放两次不重复。
func TestFaultHookPartialWriteSpoolsAndReplays(t *testing.T) {
	e := newFaultEnv(t)
	db := openDB(t, e.db)
	undo, err := testkit.FailWrites(db, "observations")
	if err != nil {
		t.Fatalf("FailWrites: %v", err)
	}

	r := e.hookPrompt("s-partial", "决策：部分写故障下证据必须进 spool")
	requireCode(t, r, 0, "hook 部分写")
	if !strings.Contains(r.Stderr, "spool") {
		t.Fatalf("stderr 应说明已落 spool:\n%s", r.Stderr)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM observations WHERE session_id='s-partial'`); n != 0 {
		t.Fatalf("写失败时不应有半提交，got %d 行", n)
	}
	if n := countSpoolSegments(t, filepath.Join(e.spoolRoot(), "s-partial")); n == 0 {
		t.Fatalf("写失败后必须留下 spool 段")
	}

	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	db.Close()

	start := `{"hook_event_name":"SessionStart","session_id":"s-partial","cwd":"` + repoRootPath + `"}`
	requireCode(t, e.runStdin(start, "hook", "--db", e.db, "--harness", "claude", "--audit", "-"), 0, "重放")
	db = openDB(t, e.db)
	if n := queryInt(t, db, `SELECT COUNT(*) FROM observations WHERE session_id='s-partial'`); n != 1 {
		t.Fatalf("重放后应恰好 1 条，got %d", n)
	}
	if n := countSpoolSegments(t, filepath.Join(e.spoolRoot(), "s-partial")); n != 0 {
		t.Fatalf("重放成功后 spool 段应删除，剩 %d", n)
	}
	db.Close()

	// 第二次重放：同一条 spool 不能变成第二条证据。
	requireCode(t, e.runStdin(start, "hook", "--db", e.db, "--harness", "claude", "--audit", "-"), 0, "二次重放")
	db = openDB(t, e.db)
	if n := queryInt(t, db, `SELECT COUNT(*) FROM observations WHERE session_id='s-partial'`); n != 1 {
		t.Fatalf("二次重放不得重复落库，got %d", n)
	}
}

// F3 只读：归档目录不可写时，hook 仍退出 0，但必须把事件转存 spool；
// 目录恢复后重放补回。验证的是「不静默丢数据」，不是退出码。
func TestFaultHookReadOnlyArchiveFallsBackToSpool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 语义在 Windows 不适用")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	e := newFaultEnv(t)
	archiveDir := filepath.Join(e.dataDir(), "evidence", "archives")
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		t.Fatalf("建归档目录: %v", err)
	}
	restore, err := testkit.ReadOnlyDir(archiveDir)
	if err != nil {
		t.Fatalf("ReadOnlyDir: %v", err)
	}
	defer restore()

	r := e.hookPrompt("s-ro-archive", "决策：归档目录只读时不能丢证据")
	requireCode(t, r, 0, "hook 只读归档")
	if !strings.Contains(r.Stderr, "spool") {
		t.Fatalf("归档写失败必须转 spool，stderr:\n%s", r.Stderr)
	}
	if n := countSpoolSegments(t, filepath.Join(e.spoolRoot(), "s-ro-archive")); n == 0 {
		t.Fatalf("归档只读时应留下 spool 段")
	}

	if err := restore(); err != nil {
		t.Fatalf("恢复目录权限: %v", err)
	}
	start := `{"hook_event_name":"SessionStart","session_id":"s-ro-archive","cwd":"` + repoRootPath + `"}`
	requireCode(t, e.runStdin(start, "hook", "--db", e.db, "--harness", "claude", "--audit", "-"), 0, "恢复后重放")
	db := openDB(t, e.db)
	if n := queryInt(t, db, `SELECT COUNT(*) FROM observations WHERE session_id='s-ro-archive'`); n != 1 {
		t.Fatalf("重放后应补回 1 条，got %d", n)
	}
}

// F5 坏输入：spool 段里是坏 JSON → hook 不崩、退出 0，原文进 dead-letter，
// 正常段不受影响（这里只放坏段，断言坏段被移走且原文保留）。
func TestFaultHookBadSpoolSegmentGoesDeadLetter(t *testing.T) {
	e := newFaultEnv(t)
	sessDir := filepath.Join(e.spoolRoot(), "s-bad")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatalf("建 spool 会话目录: %v", err)
	}
	body := "这不是 JSON\n{\"event_uid\":\"\",\"source_id\":\"\"}\n"
	seg := filepath.Join(sessDir, "seg-0001.jsonl")
	if err := os.WriteFile(seg, []byte(body), 0o600); err != nil {
		t.Fatalf("写坏段: %v", err)
	}

	payload := `{"hook_event_name":"SessionStart","session_id":"s-bad","cwd":"` + repoRootPath + `"}`
	r := e.runStdin(payload, "hook", "--db", e.db, "--harness", "claude", "--audit", "-")
	requireCode(t, r, 0, "坏 spool 段")
	if _, err := os.Stat(seg); !os.IsNotExist(err) {
		t.Fatalf("坏行处理完后原段应删除: %v", err)
	}
	dead := filepath.Join(e.spoolRoot(), "dead-letter", "s-bad-seg-0001-badlines.jsonl")
	raw, err := os.ReadFile(dead)
	if err != nil {
		t.Fatalf("坏行应进 dead-letter: %v", err)
	}
	if !strings.Contains(string(raw), "这不是 JSON") || !strings.Contains(string(raw), "bad_json") {
		t.Fatalf("dead-letter 必须保留原文与原因:\n%s", raw)
	}
	if !strings.Contains(r.Stderr, "dead-letter") {
		t.Fatalf("stderr 应可诊断 dead-letter:\n%s", r.Stderr)
	}
}

// F2 kill -9：Stop 处理 transcript 中途强杀 → 重启重跑后证据不重不缺。
// 同步点用子进程 stderr 的 event=stop 标记，避免固定 sleep 的 flaky。
func TestFaultHookKill9MidCaptureResumesWithoutDuplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL 用例只覆盖 unix")
	}
	e := newFaultEnv(t)
	sid := "s-kill"
	path := filepath.Join(e.home, "transcript.jsonl")
	const lines = 800
	var b strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, `{"role":"user","content":"第%d行 故障注入 transcript","sessionId":"%s"}`+"\n", i, sid)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写 transcript: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "Stop",
		"session_id":      sid,
		"event_id":        "stop-kill-1", // 稳定身份：重跑不会产生第二条 stop marker
		"status":          "completed",
		"transcript_path": path,
		"cwd":             repoRootPath,
	})
	hookArgs := []string{"hook", "--db", e.db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"}

	cmd := exec.Command(memBin, hookArgs...)
	cmd.Dir = repoRootPath
	cmd.Env = e.env()
	cmd.Stdin = strings.NewReader(string(payload))
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 hook: %v", err)
	}
	marker := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stderrPipe)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "event=stop") {
				select {
				case <-marker:
				default:
					close(marker)
				}
			}
		}
		io.Copy(io.Discard, stderrPipe)
	}()
	select {
	case <-marker:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("20s 内没等到 event=stop 标记")
	}
	time.Sleep(30 * time.Millisecond) // 让进程真正进入 transcript 补齐
	if err := testkit.KillNow(cmd); err != nil {
		t.Fatalf("SIGKILL: %v（进程可能已自行结束）", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatalf("被 SIGKILL 的进程不应成功退出")
	}

	// 重启补写：Stop 的 8s 时间预算可能分多轮，最多 5 轮；每轮都必须成功。
	var obs int
	for i := 0; i < 5; i++ {
		r := e.runStdin(string(payload), hookArgs...)
		requireCode(t, r, 0, fmt.Sprintf("第 %d 轮重启补写", i+1))
		db := openDB(t, e.db)
		obs = queryInt(t, db, `SELECT COUNT(*) FROM observations WHERE session_id=?`, sid)
		db.Close()
		if obs >= lines+1 {
			break
		}
	}
	if obs != lines+1 {
		t.Fatalf("重启后应补齐 %d 条（%d 行 + stop），got %d", lines+1, lines, obs)
	}

	db := openDB(t, e.db)
	if n := queryInt(t, db, `SELECT COUNT(*) FROM (
		SELECT source_id, source_seq FROM observations GROUP BY source_id, source_seq HAVING COUNT(*) > 1
	)`); n != 0 {
		t.Fatalf("重启后出现重复 (source_id,seq): %d 组", n)
	}
}

// kill -9 常见的残留形态是 *.jsonl.processing。即使没有真实 kill，
// 重启也必须认领并重放它（不丢、不重复）。
func TestFaultHookProcessingSegmentRecovers(t *testing.T) {
	e := newFaultEnv(t)
	sessDir := filepath.Join(e.spoolRoot(), "s-processing")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatalf("建 spool 目录: %v", err)
	}
	rec := map[string]any{
		"v": 1, "event_uid": "uid-processing", "source_id": "hook:claude:s-processing",
		"generation": 1, "seq": 1, "session_id": "s-processing",
		"event_type": "user-prompt", "proposal_eligible": true,
		"raw": base64.StdEncoding.EncodeToString([]byte("processing 重放")),
	}
	body, _ := json.Marshal(rec)
	seg := filepath.Join(sessDir, "seg-9999.jsonl.processing")
	if err := os.WriteFile(seg, append(body, '\n'), 0o600); err != nil {
		t.Fatalf("写 processing 段: %v", err)
	}
	payload := `{"hook_event_name":"SessionStart","session_id":"s-processing","cwd":"` + repoRootPath + `"}`
	requireCode(t, e.runStdin(payload, "hook", "--db", e.db, "--harness", "claude", "--audit", "-"), 0, "恢复 processing")
	if _, err := os.Stat(seg); !os.IsNotExist(err) {
		t.Fatalf("重放成功后 processing 文件应删除: %v", err)
	}
	db := openDB(t, e.db)
	if n := queryInt(t, db, `SELECT COUNT(*) FROM observations WHERE event_uid='uid-processing'`); n != 1 {
		t.Fatalf("processing 段应重放为 1 条，got %d", n)
	}
}

// F6 超大负载：单条观测按 32000 rune 硬截断，不静默变成空/丢整条。
func TestFaultHookOversizePromptTruncatedAt32000Runes(t *testing.T) {
	e := newFaultEnv(t)
	prompt := strings.Repeat("验", 40000)
	requireCode(t, e.hookPrompt("s-oversize", prompt), 0, "超长 prompt")

	db := openDB(t, e.db)
	archPath := queryStr(t, db, `SELECT a.path FROM evidence_archives a
		JOIN observations o ON o.archive_hash=a.archive_hash
		WHERE o.session_id='s-oversize' LIMIT 1`)
	f, err := os.Open(archPath)
	if err != nil {
		t.Fatalf("打开归档 %s: %v", archPath, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("解 gzip: %v", err)
	}
	defer zr.Close()
	line, err := bufio.NewReader(zr).ReadBytes('\n')
	if err != nil && err != io.EOF {
		t.Fatalf("读归档行: %v", err)
	}
	var rec struct {
		Raw []byte `json:"raw"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("解析归档记录: %v", err)
	}
	runs := []rune(string(rec.Raw))
	// 现行实现是 truncateRunes：前 32000 rune + 一个省略号（32001）。
	// 设计文档写的是「32000 硬截断、不加省略号」，这里放宽到二者之一，
	// 只守「有界且前 32000 不丢」；文档与实现的这一 rune 差记在 S9a 报告里。
	if len(runs) < 32000 || len(runs) > 32001 {
		t.Fatalf("归档 raw 应截断到 32000（允许带省略号的 32001），got %d", len(runs))
	}
	if got := string(runs[:32000]); got != strings.Repeat("验", 32000) {
		t.Fatalf("前 32000 rune 必须保真，got prefix len=%d", utf8.RuneCountInString(got))
	}
	if len(runs) == 32001 && runs[32000] != '…' {
		t.Fatalf("多出的第 32001 个 rune 应为省略号，got %q", runs[32000])
	}
}

// ---------- gc ----------

func seedCandidate(t *testing.T, db *sql.DB, id, status string, createdAt int64, decidedAt int64) {
	t.Helper()
	var decided any
	if decidedAt > 0 {
		decided = decidedAt
	}
	_, err := db.Exec(`INSERT INTO candidates(
		id, session_id, project, kind, content, content_hash, status, source, channel,
		salience, obs_ids_json, created_at, decided_at
	) VALUES (?, 's-gc', 'p', 'decision', ?, ?, ?, 'distill', 'agent', 0.5, '[]', ?, ?)`,
		id, "内容 "+id, "hash-"+id, status, createdAt, decided)
	if err != nil {
		t.Fatalf("插入候选 %s: %v", id, err)
	}
}

func seedEdgeSource(t *testing.T, e *faultEnv) string {
	t.Helper()
	e.mustRun("add", "--db", e.db, "--kind", "decision", "边源记忆：故障注入")
	db := openDB(t, e.db)
	return queryStr(t, db, `SELECT id FROM memories ORDER BY created_at LIMIT 1`)
}

// F1 部分写：gc --candidates 的「标过期 + 删陈旧行」是同一个事务。
// 只让 DELETE 失败时，pending→expired 的 UPDATE 与 edge 清理都必须回滚，
// 不能留下「已过期但没删」的半提交。命令如实非 0。
func TestFaultGCCandidatesPartialWriteRollsBack(t *testing.T) {
	e := newFaultEnv(t)
	now := time.Now()
	db := openDB(t, e.db)
	seedCandidate(t, db, "c-pending-old", "pending", now.Add(-30*24*time.Hour).Unix(), 0)
	seedCandidate(t, db, "c-accepted-old", "accepted", now.Add(-200*24*time.Hour).Unix(), now.Add(-100*24*time.Hour).Unix())
	memID := seedEdgeSource(t, e)
	if _, err := db.Exec(`INSERT INTO edges(src,dst,rel) VALUES (?, 'c-accepted-old', 'derived_from')`, memID); err != nil {
		t.Fatalf("插入 edge: %v", err)
	}

	undo, err := testkit.FailWritesOn(db, "candidates", "DELETE")
	if err != nil {
		t.Fatalf("FailWritesOn: %v", err)
	}
	r := e.run("gc", "--db", e.db, "--candidates", "--no-backup")
	if r.Code == 0 {
		t.Fatalf("gc 部分写应非零退出\nstdout:\n%s", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "testkit injected") {
		t.Fatalf("stderr 应带注入原因:\n%s", r.Stderr)
	}
	if got := queryStr(t, db, `SELECT status FROM candidates WHERE id='c-pending-old'`); got != "pending" {
		t.Fatalf("DELETE 失败时 pending→expired 的 UPDATE 必须回滚，got %q", got)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates WHERE id='c-accepted-old'`); n != 1 {
		t.Fatalf("事务失败后陈旧候选不应被部分删除")
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM edges WHERE dst='c-accepted-old'`); n != 1 {
		t.Fatalf("事务失败后 edge 不应被部分删除")
	}

	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	db.Close()

	// 故障撤销后重跑：同一批数据应被正常回收，且 pending 按保留期标过期。
	requireCode(t, e.run("gc", "--db", e.db, "--candidates", "--no-backup"), 0, "恢复后 gc")
	db = openDB(t, e.db)
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates WHERE id='c-accepted-old'`); n != 0 {
		t.Fatalf("恢复后陈旧 accepted 候选应删除")
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM edges WHERE dst='c-accepted-old'`); n != 0 {
		t.Fatalf("恢复后 edge 应清理")
	}
	if got := queryStr(t, db, `SELECT status FROM candidates WHERE id='c-pending-old'`); got != "expired" {
		t.Fatalf("恢复后超期 pending 应标 expired，got %q", got)
	}
}

// F4 库被占用：另一个连接持写锁时，gc 拿不到锁即如实报错（busy/locked），
// 不 panic、不改任何状态。是否等满 busy_timeout 由 SQLite 的锁类型决定，
// 用例只守「错误可见 + 状态不变」。
func TestFaultGCBusyReportsWithoutDirtyState(t *testing.T) {
	e := newFaultEnv(t)
	db := openDB(t, e.db)
	seedCandidate(t, db, "c-busy", "pending", time.Now().Unix(), 0)
	db.Close()

	release, err := testkit.HoldWriteLock(e.db)
	if err != nil {
		t.Fatalf("HoldWriteLock: %v", err)
	}
	r := e.run("gc", "--db", e.db, "--candidates", "--no-backup")
	if r.Code == 0 {
		t.Fatalf("写锁被占时应非零退出\nstdout:\n%s", r.Stdout)
	}
	low := strings.ToLower(r.Stderr)
	if !strings.Contains(low, "busy") && !strings.Contains(low, "locked") {
		t.Fatalf("stderr 应说明库被占用:\n%s", r.Stderr)
	}
	if strings.Contains(r.Stderr, "panic") {
		t.Fatalf("不得 panic:\n%s", r.Stderr)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	db = openDB(t, e.db)
	if got := queryStr(t, db, `SELECT status FROM candidates WHERE id='c-busy'`); got != "pending" {
		t.Fatalf("BUSY 失败不得改状态，got %q", got)
	}
}

// F3 只读：库目录不可写 → gc 非 0 退出，错误可诊断。
func TestFaultGCReadOnlyDirExitsNonZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 语义在 Windows 不适用")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	e := newFaultEnv(t)
	db := openDB(t, e.db)
	seedCandidate(t, db, "c-ro", "pending", time.Now().Unix(), 0)
	flushAndClose(t, db)

	restore, err := testkit.ReadOnlyDir(e.dataDir())
	if err != nil {
		t.Fatalf("ReadOnlyDir: %v", err)
	}
	defer restore()
	r := e.run("gc", "--db", e.db, "--candidates", "--no-backup")
	if r.Code == 0 {
		t.Fatalf("只读目录下 gc 应非零退出\nstdout:\n%s", r.Stdout)
	}
	if strings.TrimSpace(r.Stderr) == "" {
		t.Fatalf("只读失败必须有可诊断的 stderr")
	}
	if strings.Contains(r.Stderr, "panic") {
		t.Fatalf("不得 panic:\n%s", r.Stderr)
	}
}

// ---------- distill ----------

type distillResult struct {
	Status   string `json:"status"`
	Inserted int    `json:"inserted"`
	Results  []struct {
		Status       string `json:"status"`
		BundleStatus string `json:"bundle_status"`
		Error        string `json:"error"`
	} `json:"results"`
}

func parseDistill(t *testing.T, r cmdResult) distillResult {
	t.Helper()
	var out distillResult
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		t.Fatalf("解析 distill --json: %v\nstdout:\n%s\nstderr:\n%s", err, r.Stdout, r.Stderr)
	}
	return out
}

func twoDrafts() string {
	return strings.Join([]string{
		`{"kind":"decision","scope":"project","content":"故障注入候选一：重试幂等","rationale":"r","evidence_idx":[1]}`,
		`{"kind":"decision","scope":"project","content":"故障注入候选二：部分写可恢复","rationale":"r","evidence_idx":[1]}`,
	}, "\n")
}

// F1 部分写 + F2 崩溃后重试：两条候选里第二条落库失败 → partial，不消费窗口；
// 撤销故障后重跑补齐第二条，第一条按内容去重，最终恰好两条、无重复。
func TestFaultDistillPartialWriteRetriesWithoutDuplicates(t *testing.T) {
	e := newFaultEnv(t)
	e.installFakeCLI(twoDrafts())
	requireCode(t, e.hookPrompt("s-distill-partial", "决策：部分写故障下候选要可重试"), 0, "seed 证据")

	db := openDB(t, e.db)
	undo, err := testkit.FailWritesWhere(db, "candidates", "INSERT", "NEW.content LIKE '%候选二%'")
	if err != nil {
		t.Fatalf("FailWritesWhere: %v", err)
	}
	r := e.run("distill", "--db", e.db, "--proposal", "--json", "--strict")
	if r.Code == 0 {
		t.Fatalf("--strict 下 partial 应非零退出\nstdout:\n%s\nstderr:\n%s", r.Stdout, r.Stderr)
	}
	out := parseDistill(t, r)
	if out.Status != "partial" || out.Inserted != 1 {
		t.Fatalf("应为 partial/已落 1 条，got %+v", out)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates`); n != 1 {
		t.Fatalf("部分写后只应留下成功那条，got %d", n)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM distill_runs WHERE status='partial'`); n == 0 {
		t.Fatalf("partial 必须记账，供 G7 重试判定")
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	// 清掉退避窗口，模拟「下一次调度」而不是等 1 分钟。
	if _, err := db.Exec(`UPDATE evidence_bundles SET not_before=0`); err != nil {
		t.Fatalf("重置 not_before: %v", err)
	}
	db.Close()

	requireCode(t, e.run("distill", "--db", e.db, "--proposal", "--json", "--strict"), 0, "恢复后重试")
	db = openDB(t, e.db)
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates`); n != 2 {
		t.Fatalf("重试后应恰好 2 条，got %d", n)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM (
		SELECT session_id, content_hash FROM candidates GROUP BY session_id, content_hash HAVING COUNT(*) > 1
	)`); n != 0 {
		t.Fatalf("重试出现重复候选: %d 组", n)
	}
}

// dry-run × 故障：候选写入被全面拦住时，dry-run 仍必须 0 退出、0 写入。
func TestFaultDistillDryRunWritesNothingUnderWriteFault(t *testing.T) {
	e := newFaultEnv(t)
	requireCode(t, e.hookPrompt("s-dry", "决策：dry-run 不落任何状态"), 0, "seed 证据")

	// 先用 none executor 建出 Bundle（backlog，不产候选），拿到 bundle id。
	requireCode(t, e.run("distill", "--db", e.db, "--proposal", "--executor", "none", "--json"), 0, "建 Bundle")
	db := openDB(t, e.db)
	bundleID := queryStr(t, db, `SELECT id FROM evidence_bundles ORDER BY created_at, id LIMIT 1`)
	runsBefore := queryInt(t, db, `SELECT COUNT(*) FROM distill_runs`)
	db.Close()

	e.installFakeCLI(twoDrafts())
	db = openDB(t, e.db)
	undo, err := testkit.FailWrites(db, "candidates")
	if err != nil {
		t.Fatalf("FailWrites: %v", err)
	}
	defer undo()

	r := e.run("distill", "--db", e.db, "--proposal", "--dry-run", "--bundle", bundleID, "--json")
	requireCode(t, r, 0, "dry-run")
	out := parseDistill(t, r)
	if out.Status != "would_generate" {
		t.Fatalf("dry-run 状态应为 would_generate，got %q", out.Status)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates`); n != 0 {
		t.Fatalf("dry-run 不得写候选，got %d", n)
	}
	if n := queryInt(t, db, `SELECT COUNT(*) FROM distill_runs`); n != runsBefore {
		t.Fatalf("dry-run 不得写运行账，before=%d after=%d", runsBefore, n)
	}
	if got := queryStr(t, db, `SELECT status FROM evidence_bundles WHERE id=?`, bundleID); got == "ok" || got == "noop" {
		t.Fatalf("dry-run 不得消费窗口，bundle status=%q", got)
	}
}

// F5 坏输入：executor 输出不是约定 JSON → invalid，不写候选、不崩，
// Bundle 不消费（留给修复/重试），--strict 如实非 0。
func TestFaultDistillBadOutputRejected(t *testing.T) {
	e := newFaultEnv(t)
	e.installFakeCLI("这不是约定的 JSON")
	requireCode(t, e.hookPrompt("s-badout", "决策：坏输出不能变成候选"), 0, "seed 证据")

	r := e.run("distill", "--db", e.db, "--proposal", "--json", "--strict")
	if r.Code == 0 {
		t.Fatalf("坏输出在 --strict 下应非零退出\nstdout:\n%s", r.Stdout)
	}
	out := parseDistill(t, r)
	if out.Status != "invalid" {
		t.Fatalf("状态应为 invalid，got %q", out.Status)
	}
	db := openDB(t, e.db)
	if n := queryInt(t, db, `SELECT COUNT(*) FROM candidates`); n != 0 {
		t.Fatalf("坏输出不得落候选，got %d", n)
	}
	if got := queryStr(t, db, `SELECT status FROM evidence_bundles LIMIT 1`); got == "ok" || got == "noop" {
		t.Fatalf("坏输出不得消费窗口，status=%q", got)
	}
	if strings.Contains(r.Stderr, "panic") {
		t.Fatalf("不得 panic:\n%s", r.Stderr)
	}
}

// F6 边界：Bundle 的 64KB ByteCap 必须切窗而不是丢证据；单条超限也至少出 1 条
// （后半条由 store 层单测守，这里验真实入口的切窗结果）。
func TestFaultDistillBundleByteCapSplitsNotDrops(t *testing.T) {
	e := newFaultEnv(t)
	rnd := rand.New(rand.NewSource(20261125))
	const events = 20
	for i := 0; i < events; i++ {
		// 不可压缩正文：gzip 后仍接近 8KB，才能逼近 64KB 窗口上限。
		raw := make([]byte, 8000)
		rnd.Read(raw)
		prompt := base64.StdEncoding.EncodeToString(raw)
		requireCode(t, e.hookPrompt("s-cap", prompt), 0, "seed 大证据")
	}
	e.installFakeCLI(twoDrafts())
	requireCode(t, e.run("distill", "--db", e.db, "--proposal", "--json"), 0, "切窗蒸馏")

	db := openDB(t, e.db)
	if n := queryInt(t, db, `SELECT COUNT(*) FROM observations`); n != events {
		t.Fatalf("20 条证据不得丢失，got %d", n)
	}
	var sourceID string
	var fromSeq, toSeq int
	if err := db.QueryRow(`SELECT source_id, from_seq_exclusive, to_seq_inclusive
		FROM evidence_bundles ORDER BY created_at, id LIMIT 1`).Scan(&sourceID, &fromSeq, &toSeq); err != nil {
		t.Fatalf("读首个 Bundle: %v", err)
	}
	if toSeq >= events {
		t.Fatalf("64KB 上限下首窗不应装下全部 %d 条，got seq<=%d", events, toSeq)
	}
	var bytes int
	if err := db.QueryRow(`SELECT COALESCE(SUM(a.byte_size),0)
		FROM observations o JOIN evidence_archives a ON a.archive_hash=o.archive_hash
		WHERE o.source_id=? AND o.source_seq>? AND o.source_seq<=?`,
		sourceID, fromSeq, toSeq).Scan(&bytes); err != nil {
		t.Fatalf("统计窗口字节: %v", err)
	}
	if bytes > 64<<10 {
		t.Fatalf("窗口字节应 ≤64KB，got %d", bytes)
	}
	if bytes == 0 {
		t.Fatalf("窗口不能为空（空窗等于静默丢证据）")
	}
}

// F3 只读：库目录不可写时 distill --strict 非 0，不 panic。
func TestFaultDistillReadOnlyDirExitsNonZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 语义在 Windows 不适用")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	e := newFaultEnv(t)
	e.installFakeCLI(twoDrafts())
	requireCode(t, e.hookPrompt("s-distill-ro", "决策：只读目录下蒸馏应如实失败"), 0, "seed 证据")
	db := openDB(t, e.db)
	flushAndClose(t, db)

	restore, err := testkit.ReadOnlyDir(e.dataDir())
	if err != nil {
		t.Fatalf("ReadOnlyDir: %v", err)
	}
	defer restore()
	r := e.run("distill", "--db", e.db, "--proposal", "--strict")
	if r.Code == 0 {
		t.Fatalf("只读目录下 --strict 应非零退出\nstdout:\n%s", r.Stdout)
	}
	if strings.TrimSpace(r.Stderr) == "" {
		t.Fatalf("只读失败必须有可诊断的 stderr")
	}
	if strings.Contains(r.Stderr, "panic") {
		t.Fatalf("不得 panic:\n%s", r.Stderr)
	}
}
