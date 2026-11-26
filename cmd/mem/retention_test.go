package main

// S13 红 case（P4/P6）：文件侧出口轮转 + --dry-run 零写；gc --retention 备份前置
// 仍生效、默认不删。

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testDay = 24 * time.Hour

// openRawDB 只在测试里直接读写库表，绕开 store.Open 的迁移副作用。
func openRawDB(path string) (*sql.DB, error) { return sql.Open("sqlite", path) }

func writeAt(t *testing.T, path string, age time.Duration, now time.Time, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	mtime := now.Add(-age)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("Chtimes(%s): %v", path, err)
	}
}

// newSweepFixture 造一份「五类出口都有可删样本」的目录。
func newSweepFixture(t *testing.T, now time.Time) string {
	t.Helper()
	root := t.TempDir()
	writeAt(t, filepath.Join(root, "plans", "plan-old.jsonl"), 100*testDay, now, "{\"id\":\"old\"}\n")
	writeAt(t, filepath.Join(root, "plans", "plan-fresh.jsonl"), 1*testDay, now, "{\"id\":\"fresh\"}\n")
	for i, age := range []time.Duration{100 * testDay, 40 * testDay, 10 * testDay, 5 * testDay, 1 * testDay} {
		writeAt(t, filepath.Join(root, "backup", "mem-"+string(rune('1'+i))+".db"), age, now, "快照")
	}
	oldDir := filepath.Join(root, "backup", "evidence-gc-old")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", oldDir, err)
	}
	mtime := now.Add(-100 * testDay)
	if err := os.Chtimes(oldDir, mtime, mtime); err != nil {
		t.Fatalf("Chtimes(%s): %v", oldDir, err)
	}
	writeAt(t, filepath.Join(root, "spawn.log"), 0, now, strings.Repeat("x", 128))
	writeAt(t, filepath.Join(root, "spool", "sess", "seg-old.tmp"), 10*testDay, now, "半截段")
	writeAt(t, filepath.Join(root, "spool", "sess", "seg-old.jsonl"), 10*testDay, now, "完整段")
	writeAt(t, filepath.Join(root, "spool", "sess", "seg-new.tmp"), time.Hour, now, "刚写的半截段")
	// 超龄但仍有未读余量的 cursor：终态未达成，必须保留。
	transcript := filepath.Join(root, "transcripts", "sess.jsonl")
	writeAt(t, transcript, 0, now, "尚未消费的转录字节")
	writeAt(t, filepath.Join(root, "evidence", "transcripts", "sess.cursor"), 100*testDay, now,
		cursorJSON(t, transcript, 0))
	return root
}

func sweepCfg() fileRetentionConfig {
	return fileRetentionConfig{
		PlansAge:      90 * testDay,
		BackupsKeep:   3,
		BackupsAge:    30 * testDay,
		SpawnMaxBytes: 32,
		SpoolTmpAge:   24 * time.Hour,
		CursorAge:     30 * testDay,
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// countBackupSnapshots 数 backup/ 下的库快照（mem-*.db），用于 dry-run 零写断言。
func countBackupSnapshots(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "backup"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "mem-") && strings.HasSuffix(e.Name(), ".db") {
			n++
		}
	}
	return n
}

// P4-a：非 dry-run + 备份有效时，五类出口各自按超龄/代数回收；
// 受保护的（新鲜 plans、完整 spool 段、保护期 .tmp、cursor）一律保留。
func TestP4FileSweepReclaimsEachExit(t *testing.T) {
	now := time.Now()
	root := newSweepFixture(t, now)
	got, err := sweepRetentionFiles(root, now, sweepCfg(), false, true)
	if err != nil {
		t.Fatalf("sweepRetentionFiles: %v", err)
	}
	if got.Plans != 1 {
		t.Errorf("P4: 超龄 plans 应回收 1 份，got %+v", got)
	}
	if !exists(filepath.Join(root, "plans", "plan-fresh.jsonl")) {
		t.Error("P4: 保护期内 plans 应保留")
	}
	if got.Backups < 2 {
		t.Errorf("P4: backup 应按代数+年龄回收至少 2 代，got %+v", got)
	}
	if exists(filepath.Join(root, "backup", "mem-1.db")) {
		t.Error("P4: 100 天前的库快照应回收")
	}
	if !exists(filepath.Join(root, "backup", "mem-5.db")) {
		t.Error("P4: 最新一代库快照应保留")
	}
	if got.SpawnRotated != 1 || !exists(filepath.Join(root, "spawn.log.1")) {
		t.Errorf("P4: 超阈值 spawn.log 应轮转为 .1，got %+v", got)
	}
	if got.SpoolTmp != 1 {
		t.Errorf("P4: 超龄 seg-*.tmp 应回收 1 个，got %+v", got)
	}
	if exists(filepath.Join(root, "spool", "sess", "seg-old.tmp")) {
		t.Error("P4: 超龄 .tmp 应删除")
	}
	if !exists(filepath.Join(root, "spool", "sess", "seg-old.jsonl")) ||
		!exists(filepath.Join(root, "spool", "sess", "seg-new.tmp")) {
		t.Error("P4: 完整段与保护期内的 .tmp 不许动")
	}
	// transcript cursor 有未读余量（Size > Offset）：会话未消费完，必须保留。
	if !exists(filepath.Join(root, "evidence", "transcripts", "sess.cursor")) {
		t.Error("P4: 有未读余量的 cursor 必须保留")
	}
	if got.CursorsListed < 1 || got.CursorsDeleted != 0 {
		t.Errorf("P4: 有未读余量的 cursor 应只报数不删除，got %+v", got)
	}
}

// P4-b：--dry-run 零写入：报告有计数，但盘上一个字节都不许变。
func TestP4FileSweepDryRunWritesNothing(t *testing.T) {
	now := time.Now()
	root := newSweepFixture(t, now)
	before, err := os.ReadFile(filepath.Join(root, "spawn.log"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := sweepRetentionFiles(root, now, sweepCfg(), true, false)
	if err != nil {
		t.Fatalf("dry-run 不应因 backupReady=false 报错: %v", err)
	}
	if got.Plans < 1 || got.Backups < 1 || got.SpawnRotated < 1 || got.SpoolTmp < 1 || got.CursorsListed < 1 {
		t.Errorf("P4: dry-run 应给出各类预览计数，got %+v", got)
	}
	after, err := os.ReadFile(filepath.Join(root, "spawn.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || exists(filepath.Join(root, "spawn.log.1")) {
		t.Error("P4: dry-run 不许轮转 spawn.log（零写）")
	}
	for _, rel := range []string{
		filepath.Join("plans", "plan-old.jsonl"),
		filepath.Join("backup", "mem-1.db"),
		filepath.Join("spool", "sess", "seg-old.tmp"),
		filepath.Join("evidence", "transcripts", "sess.cursor"),
	} {
		if !exists(filepath.Join(root, rel)) {
			t.Errorf("P4: dry-run 不许删 %s", rel)
		}
	}
}

// P4-c：§8.1 —— 备份未确认有效时，非 dry-run 必须拒绝物理删。
func TestP4FileSweepRefusesWithoutBackup(t *testing.T) {
	now := time.Now()
	root := newSweepFixture(t, now)
	if _, err := sweepRetentionFiles(root, now, sweepCfg(), false, false); err == nil {
		t.Fatal("P4: 备份无效时应拒绝物理删（§8.1）")
	}
	if !exists(filepath.Join(root, "plans", "plan-old.jsonl")) {
		t.Error("P4: 拒绝后文件必须原样在")
	}
}

// P6：mem gc --retention 默认关（不带 flag 不回收）、备份前置仍生效、
// --dry-run 零写。
func TestP6GCRetentionGuards(t *testing.T) {
	db := testDB(t)
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", db, "库非空才有备份前置"})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	root := filepath.Dir(db)
	planPath := filepath.Join(root, "plans", "plan-old.jsonl")
	writeAt(t, planPath, 100*testDay, time.Now(), "{\"id\":\"old\"}\n")

	// 默认（不带 --retention）：既有 gc 不碰文件侧出口。
	if _, err := captureStdout(t, func() error { return cmdGC([]string{"--db", db}) }); err != nil {
		t.Fatalf("默认 gc: %v", err)
	}
	if !exists(planPath) {
		t.Error("P6: 默认 gc（retention 关）不许回收 plans")
	}

	// 备份前置：不可写备份目录 → --retention 实删必须在派发前拒绝。
	blocker := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(blocker, []byte("不是目录"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--retention", "--backup-dir", filepath.Join(blocker, "sub")})
	})
	if err == nil || !strings.Contains(err.Error(), "备份") {
		t.Fatalf("P6: 备份失败应拒绝执行并点明备份环节，got %v", err)
	}
	if !exists(planPath) {
		t.Error("P6: 备份失败后业务零变更")
	}

	// --dry-run 零写：不产生新备份快照、不删 plans，且输出预览计数。
	snapshotsBefore := countBackupSnapshots(t, root)
	dryOut, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--retention", "--dry-run"})
	})
	if err != nil {
		t.Fatalf("--retention --dry-run: %v", err)
	}
	if !exists(planPath) {
		t.Error("P6: dry-run 必须零写（plans 仍在）")
	}
	if got := countBackupSnapshots(t, root); got != snapshotsBefore {
		t.Error("P6: dry-run 不许生成备份快照（零写）")
	}
	if !strings.Contains(dryOut, "dry-run") && !strings.Contains(dryOut, "预览") {
		t.Errorf("P6: dry-run 应输出预览计数，got %q", dryOut)
	}

	// --dry-run 单独使用（不带 --retention）应被拒，避免「以为预览了其实没跑」。
	if err := cmdGC([]string{"--db", db, "--dry-run"}); err == nil {
		t.Error("P6: --dry-run 必须配合 --retention")
	}
}

// P6：--retention 实删一次后，DB 侧 rejected 候选与文件侧超龄 plans 都被回收。
func TestP6GCRetentionActuallyReclaims(t *testing.T) {
	db := testDB(t)
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", db, "库非空"})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	root := filepath.Dir(db)
	planPath := filepath.Join(root, "plans", "plan-old.jsonl")
	writeAt(t, planPath, 100*testDay, time.Now(), "{\"id\":\"old\"}\n")

	// 一条 91 天前的 rejected 候选（GCCandidates 补的口径）。
	st, err := openRawDB(db)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-91 * testDay).Unix()
	if _, err := st.Exec(`INSERT INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at, decided_at)
		VALUES ('c-rej-old','s','p','fact','被拒内容','h-c-rej-old','rejected','distill',?,?)`, old, old); err != nil {
		t.Fatalf("insert rejected: %v", err)
	}
	st.Close()

	_, err = captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--retention", "--no-backup"})
	})
	if err != nil {
		t.Fatalf("--retention 实删: %v", err)
	}
	if exists(planPath) {
		t.Error("P6: --retention 实删应回收超龄 plans（红阶段空分支会失败在这里）")
	}
	st, err = openRawDB(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.QueryRow(`SELECT COUNT(*) FROM candidates WHERE id='c-rej-old'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("P6: --retention 实删应回收 91 天前的 rejected 候选")
	}
}

// cursorJSON 生成一份合法游标内容。offset 为已消费到 transcript 的字节位置。
func cursorJSON(t *testing.T, transcript string, offset int64) string {
	t.Helper()
	b, err := json.Marshal(transcriptCursor{Path: transcript, Offset: offset})
	if err != nil {
		t.Fatalf("marshal cursor: %v", err)
	}
	return string(b)
}

// newCursorFixture 造一个只有 cursor 的最小根目录，返回 cursor 路径。
func newCursorFixture(t *testing.T, now time.Time, age time.Duration, content string) (string, string) {
	t.Helper()
	root := t.TempDir()
	cursor := filepath.Join(root, "evidence", "transcripts", "sess.cursor")
	writeAt(t, cursor, age, now, content)
	return root, cursor
}

// Task H：旧 cursor + transcript 已不存在 → 终态回收。
func TestCursorReclaimWhenTranscriptMissing(t *testing.T) {
	now := time.Now()
	root, cursor := newCursorFixture(t, now, 100*testDay, cursorJSON(t, filepath.Join(t.TempDir(), "gone.jsonl"), 0))
	got, err := sweepRetentionFiles(root, now, sweepCfg(), false, true)
	if err != nil {
		t.Fatalf("sweepRetentionFiles: %v", err)
	}
	if exists(cursor) {
		t.Error("transcript 缺失的超龄 cursor 应删除")
	}
	if got.CursorsDeleted != 1 || got.CursorsListed != 1 {
		t.Errorf("transcript 缺失应计 1 删 1 报，got %+v", got)
	}
}

// Task H：旧 cursor + transcript 已完全消费（Size <= Offset）→ 终态回收。
func TestCursorReclaimWhenFullyConsumed(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	transcript := filepath.Join(root, "transcripts", "sess.jsonl")
	writeAt(t, transcript, 0, now, strings.Repeat("x", 64))
	cursor := filepath.Join(root, "evidence", "transcripts", "sess.cursor")
	writeAt(t, cursor, 100*testDay, now, cursorJSON(t, transcript, 64))
	got, err := sweepRetentionFiles(root, now, sweepCfg(), false, true)
	if err != nil {
		t.Fatalf("sweepRetentionFiles: %v", err)
	}
	if exists(cursor) {
		t.Error("已完全消费的超龄 cursor 应删除")
	}
	if got.CursorsDeleted != 1 {
		t.Errorf("已完全消费应计 1 删，got %+v", got)
	}
}

// Task H：旧 cursor + 仍有未读余量（Size > Offset）→ 保留。
func TestCursorReclaimKeepsUnreadRemainder(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	transcript := filepath.Join(root, "transcripts", "sess.jsonl")
	writeAt(t, transcript, 0, now, strings.Repeat("x", 128))
	cursor := filepath.Join(root, "evidence", "transcripts", "sess.cursor")
	writeAt(t, cursor, 100*testDay, now, cursorJSON(t, transcript, 64))
	got, err := sweepRetentionFiles(root, now, sweepCfg(), false, true)
	if err != nil {
		t.Fatalf("sweepRetentionFiles: %v", err)
	}
	if !exists(cursor) {
		t.Error("有未读余量的 cursor 必须保留")
	}
	if got.CursorsDeleted != 0 || got.CursorsListed != 1 {
		t.Errorf("有未读余量应 0 删 1 报，got %+v", got)
	}
}

// Task H：未到 30 天的 cursor 一律保留，即使 transcript 缺失。
func TestCursorReclaimKeepsFresh(t *testing.T) {
	now := time.Now()
	root, cursor := newCursorFixture(t, now, 1*testDay, cursorJSON(t, filepath.Join(t.TempDir(), "gone.jsonl"), 0))
	got, err := sweepRetentionFiles(root, now, sweepCfg(), false, true)
	if err != nil {
		t.Fatalf("sweepRetentionFiles: %v", err)
	}
	if !exists(cursor) {
		t.Error("未超龄 cursor 必须保留")
	}
	if got.CursorsDeleted != 0 || got.CursorsListed != 0 {
		t.Errorf("未超龄 cursor 不应进入报数/删除，got %+v", got)
	}
}

// Task H：dry-run 只计数不删除。
func TestCursorReclaimDryRunCountsOnly(t *testing.T) {
	now := time.Now()
	root, cursor := newCursorFixture(t, now, 100*testDay, cursorJSON(t, filepath.Join(t.TempDir(), "gone.jsonl"), 0))
	got, err := sweepRetentionFiles(root, now, sweepCfg(), true, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !exists(cursor) {
		t.Error("dry-run 不许删除 cursor")
	}
	if got.CursorsDeleted != 1 {
		t.Errorf("dry-run 应预览 1 个待删 cursor，got %+v", got)
	}
}

// Task H：坏 cursor JSON 无法判定消费状态 → 保留并计入 listed。
func TestCursorReclaimKeepsCorruptJSON(t *testing.T) {
	now := time.Now()
	root, cursor := newCursorFixture(t, now, 100*testDay, "{not json")
	got, err := sweepRetentionFiles(root, now, sweepCfg(), false, true)
	if err != nil {
		t.Fatalf("sweepRetentionFiles: %v", err)
	}
	if !exists(cursor) {
		t.Error("坏 cursor JSON 应保留")
	}
	if got.CursorsDeleted != 0 || got.CursorsListed != 1 {
		t.Errorf("坏 cursor 应 0 删 1 报，got %+v", got)
	}
}
