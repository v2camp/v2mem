package store

import (
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