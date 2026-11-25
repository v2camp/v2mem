package testkit

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestManualClockFreezeAndAdvance(t *testing.T) {
	start := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	c := NewManualClock(start)
	if !c.Now().Equal(start) {
		t.Fatalf("Now() = %v, want frozen %v", c.Now(), start)
	}
	c.Advance(90 * time.Minute)
	want := start.Add(90 * time.Minute)
	if !c.Now().Equal(want) {
		t.Fatalf("after Advance Now() = %v, want %v", c.Now(), want)
	}
	c.Set(start)
	if !c.Now().Equal(start) {
		t.Fatalf("after Set Now() = %v, want %v", c.Now(), start)
	}
}

func TestFailWritesBlocksThenReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('before')`); err != nil {
		t.Fatalf("insert before: %v", err)
	}

	undo, err := FailWrites(db, "t")
	if err != nil {
		t.Fatalf("FailWrites: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('during')`); err == nil {
		t.Fatalf("insert during: want error, got nil")
	}
	if _, err := db.Exec(`DELETE FROM t WHERE v='before'`); err == nil {
		t.Fatalf("delete during: want error, got nil")
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('after')`); err != nil {
		t.Fatalf("insert after undo: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2（before + after，during 必须被拦）", n)
	}
}

func TestFailWritesRejectsUnknownTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := FailWrites(db, "no_such_table"); err == nil {
		t.Fatalf("FailWrites on unknown table: want error, got nil")
	}
}

func TestFailWritesPartialInstallCleansUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 预占第二个触发器名，让 FailWrites 建到一半失败：已装的触发器必须被回收。
	if _, err := db.Exec(`CREATE TRIGGER testkit_fail_UPDATE_t BEFORE UPDATE ON t BEGIN SELECT RAISE(ABORT, 'pre'); END`); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if _, err := FailWrites(db, "t"); err == nil {
		t.Fatalf("FailWrites with occupied trigger name: want error, got nil")
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('recovered')`); err != nil {
		t.Fatalf("insert after partial install: %v（残留触发器未回收）", err)
	}
}

func TestFailWritesNilDBAndUndoAfterClose(t *testing.T) {
	if _, err := FailWrites(nil, "t"); err == nil {
		t.Fatalf("FailWrites(nil): want error, got nil")
	}
	path := filepath.Join(t.TempDir(), "fault.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	undo, err := FailWrites(db, "t")
	if err != nil {
		t.Fatalf("FailWrites: %v", err)
	}
	_ = db.Close()
	if err := undo(); err == nil {
		t.Fatalf("undo after close: want error, got nil")
	}
}

func TestFailWritesOnOnlyBlocksSelectedOps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	undo, err := FailWritesOn(db, "t", "delete")
	if err != nil {
		t.Fatalf("FailWritesOn: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('during')`); err != nil {
		t.Fatalf("未列出的 INSERT 不应被拦: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM t`); err == nil {
		t.Fatalf("DELETE 应被拦，got nil")
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM t`); err != nil {
		t.Fatalf("undo 后 DELETE 应放行: %v", err)
	}
}

func TestFailWritesOnRejectsBadInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	cases := []struct {
		name  string
		db    *sql.DB
		table string
		ops   []string
	}{
		{"nil db", nil, "t", []string{"INSERT"}},
		{"missing table", db, "missing", []string{"INSERT"}},
		{"no ops", db, "t", nil},
		{"unknown op", db, "t", []string{"TRUNCATE"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FailWritesOn(tc.db, tc.table, tc.ops...); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

func TestFailWritesWhereOnlyBlocksMatchingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('a'), ('b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	undo, err := FailWritesWhere(db, "t", "DELETE", "OLD.v='a'")
	if err != nil {
		t.Fatalf("FailWritesWhere: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM t WHERE v='b'`); err != nil {
		t.Fatalf("不匹配的行应放行: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM t WHERE v='a'`); err == nil {
		t.Fatalf("匹配的行应被拦，got nil")
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM t WHERE v='a'`); err != nil {
		t.Fatalf("undo 后应放行: %v", err)
	}
}

func TestFailWritesWhereRejectsBadInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := FailWritesWhere(nil, "t", "INSERT", "1=1"); err == nil {
		t.Fatalf("nil db: want error")
	}
	if _, err := FailWritesWhere(db, "missing", "INSERT", "1=1"); err == nil {
		t.Fatalf("missing table: want error")
	}
	if _, err := FailWritesWhere(db, "t", "UPSERT", "1=1"); err == nil {
		t.Fatalf("unknown op: want error")
	}
	if _, err := FailWritesWhere(db, "t", "INSERT", "  "); err == nil {
		t.Fatalf("empty where: want error")
	}
}

func TestReadOnlyDirBlocksThenRestores(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 语义在 Windows 不适用")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	dir := t.TempDir()
	restore, err := ReadOnlyDir(dir)
	if err != nil {
		t.Fatalf("ReadOnlyDir: %v", err)
	}
	// 先注册兜底恢复，确保断言失败时 TempDir 仍可清理。
	t.Cleanup(func() { _ = restore() })
	if err := os.WriteFile(filepath.Join(dir, "blocked.txt"), []byte("x"), 0o600); err == nil {
		t.Fatalf("只读目录下新建文件应失败")
	}
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("恢复后应可写: %v", err)
	}
}

func TestReadOnlyDirRejectsBadTarget(t *testing.T) {
	if _, err := ReadOnlyDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatalf("不存在的目录应报错")
	}
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOnlyDir(file); err == nil {
		t.Fatalf("文件不是目录，应报错")
	}
	if _, err := ReadOnlyDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatalf("再次确认不存在目录报错")
	}
}

func TestHoldWriteLockBlocksConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Close()

	release, err := HoldWriteLock(path)
	if err != nil {
		t.Fatalf("HoldWriteLock: %v", err)
	}
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if _, err := other.Exec(`PRAGMA busy_timeout=0`); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if _, err := other.Exec(`INSERT INTO t VALUES ('blocked')`); err == nil {
		t.Fatalf("持锁期间写入应失败")
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := other.Exec(`INSERT INTO t VALUES ('ok')`); err != nil {
		t.Fatalf("释放后应可写: %v", err)
	}
	other.Close()
}

func TestHoldWriteLockRejectsBadPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := HoldWriteLock(dir); err == nil {
		t.Fatalf("目录路径不能作为库，应报错")
	}
}

func TestKillNowTerminatesProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep 不是 Windows 命令")
	}
	if err := KillNow(nil); err == nil {
		t.Fatalf("nil cmd: want error")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := KillNow(cmd); err != nil {
		t.Fatalf("KillNow: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatalf("被杀进程 Wait 应返回非零")
	}
}

func TestFailWritesHelpersOnClosedDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Close()
	if _, err := FailWrites(db, "t"); err == nil {
		t.Fatalf("FailWrites 关闭库应报错")
	}
	if _, err := FailWritesOn(db, "t", "INSERT"); err == nil {
		t.Fatalf("FailWritesOn 关闭库应报错")
	}
	if _, err := FailWritesWhere(db, "t", "INSERT", "1=1"); err == nil {
		t.Fatalf("FailWritesWhere 关闭库应报错")
	}
}

func TestFailWritesOnDuplicateOpsAndPartialInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 重复操作只装一个触发器，不报错。
	undo, err := FailWritesOn(db, "t", "DELETE", "delete")
	if err != nil {
		t.Fatalf("重复操作应幂等: %v", err)
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	// 先装一个再遇到非法操作：已装的必须回收，下一次能正常写。
	if _, err := FailWritesOn(db, "t", "INSERT", "TRUNCATE"); err == nil {
		t.Fatalf("非法操作应报错")
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('after')`); err != nil {
		t.Fatalf("部分安装后 INSERT 应可用: %v", err)
	}
}

func TestFailWritesOnTriggerInstallFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conflict.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER testkit_fail_DELETE_t BEFORE DELETE ON t BEGIN SELECT RAISE(ABORT, 'pre'); END`); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if _, err := FailWritesOn(db, "t", "INSERT", "DELETE"); err == nil {
		t.Fatalf("触发器名冲突应报错")
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('recovered')`); err != nil {
		t.Fatalf("冲突后已装触发器应回收: %v", err)
	}
}

func TestFailWritesWhereTriggerInstallFailureAndUndoAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "where-conflict.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER testkit_fail_where_INSERT_t BEFORE INSERT ON t WHEN 1=1 BEGIN SELECT RAISE(ABORT, 'pre'); END`); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if _, err := FailWritesWhere(db, "t", "INSERT", "1=1"); err == nil {
		t.Fatalf("触发器名冲突应报错")
	}
	if _, err := db.Exec(`DROP TRIGGER testkit_fail_where_INSERT_t`); err != nil {
		t.Fatalf("drop pre: %v", err)
	}
	undo, err := FailWritesWhere(db, "t", "INSERT", "NEW.v='blocked'")
	if err != nil {
		t.Fatalf("FailWritesWhere: %v", err)
	}
	db.Close()
	if err := undo(); err == nil {
		t.Fatalf("关闭库后 undo 应报错")
	}
}

func TestHoldWriteLockSecondHolderFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Close()
	first, err := HoldWriteLock(path)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := HoldWriteLock(path); err == nil {
		t.Fatalf("第二个持锁者应立即失败（busy_timeout=0）")
	}
	if err := first(); err != nil {
		t.Fatalf("first release: %v", err)
	}
}
