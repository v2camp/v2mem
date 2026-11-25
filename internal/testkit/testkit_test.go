package testkit

import (
	"database/sql"
	"path/filepath"
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
