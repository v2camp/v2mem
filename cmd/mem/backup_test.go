package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// 破坏性操作（gc / consolidate）前应给库拍快照；空库 / --no-backup 则跳过。

func listBackups(t *testing.T, db string) []string {
	t.Helper()
	bs, _ := filepath.Glob(filepath.Join(filepath.Dir(db), "backup", "mem-*.db"))
	return bs
}

func TestCmdGCBacksUpBeforeMutating(t *testing.T) {
	db := testDB(t)
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", db, "gc 备份校验的记忆"})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := captureStdout(t, func() error { return cmdGC([]string{"--db", db}) })
	if err != nil {
		t.Fatalf("cmdGC: %v", err)
	}
	if !strings.Contains(out, "已备份") {
		t.Errorf("应报告已备份，got:\n%s", out)
	}
	if n := len(listBackups(t, db)); n != 1 {
		t.Errorf("应有 1 个备份文件，got %d", n)
	}
}

func TestCmdGCSkipsBackupWhenEmptyOrMissing(t *testing.T) {
	db := testDB(t) // 尚不存在
	if _, err := captureStdout(t, func() error { return cmdGC([]string{"--db", db}) }); err != nil {
		t.Fatalf("cmdGC（无库）: %v", err)
	}
	if n := len(listBackups(t, db)); n != 0 {
		t.Errorf("库不存在/为空时不应生成备份，got %d", n)
	}
}

func TestCmdGCNoBackupOptsOut(t *testing.T) {
	db := testDB(t)
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", db, "跳过备份校验的记忆"})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	out, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--no-backup"})
	})
	if err != nil {
		t.Fatalf("cmdGC --no-backup: %v", err)
	}
	if strings.Contains(out, "已备份") {
		t.Errorf("--no-backup 不应备份，got:\n%s", out)
	}
	if n := len(listBackups(t, db)); n != 0 {
		t.Errorf("--no-backup 不应生成备份，got %d", n)
	}
}

func TestCmdGCBackupDirOverride(t *testing.T) {
	db := testDB(t)
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", db, "备份目录覆盖的记忆"})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "mybackups")
	if _, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--backup-dir", dir})
	}); err != nil {
		t.Fatalf("cmdGC --backup-dir: %v", err)
	}
	bs, _ := filepath.Glob(filepath.Join(dir, "mem-*.db"))
	if len(bs) != 1 {
		t.Errorf("--backup-dir 应写入指定目录，got %d", len(bs))
	}
}

func TestCmdConsolidateBacksUpBeforeMutating(t *testing.T) {
	db := testDB(t)
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", db, "--project", "p1", "--salience", "0.9", cliSimBase})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", db, "--project", "p1", "--salience", "0.3", cliSimVariant})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}

	out, err := captureStdout(t, func() error { return cmdConsolidate([]string{"--db", db}) })
	if err != nil {
		t.Fatalf("cmdConsolidate: %v", err)
	}
	if !strings.Contains(out, "已备份") {
		t.Errorf("应报告已备份，got:\n%s", out)
	}
	if n := len(listBackups(t, db)); n != 1 {
		t.Errorf("应有 1 个备份文件，got %d", n)
	}
}