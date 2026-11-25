package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCountReflectsReality(t *testing.T) {
	s := newTestStore(t)
	if n, err := s.Count(); err != nil || n != 0 {
		t.Fatalf("空库 Count 应为 0，got %d err=%v", n, err)
	}
	mustAdd(t, s, "计数用第一条")
	mustAdd(t, s, "计数用第二条")
	if n, err := s.Count(); err != nil || n != 2 {
		t.Fatalf("Count 应为 2，got %d err=%v", n, err)
	}
}

func TestBackupProducesOpenableSnapshot(t *testing.T) {
	s := newTestStore(t)
	mustAdd(t, s, "备份前的一条记忆，内容应原样保留")

	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := s.Backup(dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("应有备份文件: %v", err)
	}

	snap, err := Open(dest)
	if err != nil {
		t.Fatalf("备份应能作为独立库打开: %v", err)
	}
	defer snap.Close()
	if n, err := snap.Count(); err != nil || n != 1 {
		t.Fatalf("备份库 Count 应为 1，got %d err=%v", n, err)
	}
}

func TestBackupRefusesEmptyDest(t *testing.T) {
	s := newTestStore(t)
	if err := s.Backup(""); err == nil {
		t.Fatal("空备份路径应报错")
	}
}

// 备份校验（S3）：删除前必须确认新备份有效，否则备份只是「多了一个文件」。
// 三道闸口依次是「文件非空 → quick_check 返回 ok → memories 表可查」。
func TestVerifyBackupAcceptsGoodSnapshot(t *testing.T) {
	s := newTestStore(t)
	mustAdd(t, s, "校验用的记忆")
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := s.Backup(dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBackup(dest); err != nil {
		t.Fatalf("真快照应校验通过: %v", err)
	}
	// 校验必须只读打开：备份被「验证」一次就变字节（例如写 db_meta 戳），
	// 说明用的是 store.Open 这类读写路径，备份不再是快照当时的样子。
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("校验不应改动备份文件（必须 mode=ro 只读打开）")
	}
}

// 截断文件是「看起来有、实际不能用」的典型：大小非零能骗过 Stat，
// 只有真读一遍页才发现坏。校验必须拦住它。
func TestVerifyBackupRejectsTruncatedSnapshot(t *testing.T) {
	s := newTestStore(t)
	mustAdd(t, s, "截断校验用的记忆")
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := s.Backup(dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := os.Truncate(dest, 512); err != nil {
		t.Fatalf("截断快照: %v", err)
	}
	if err := VerifyBackup(dest); err == nil {
		t.Fatal("截断的快照必须校验失败")
	}
}

func TestVerifyBackupRejectsMissingFile(t *testing.T) {
	if err := VerifyBackup(filepath.Join(t.TempDir(), "nope.db")); err == nil {
		t.Fatal("不存在的备份路径必须报错")
	}
}
