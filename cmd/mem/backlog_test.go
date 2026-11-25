package main

// S4：积压五指标的 CLI 侧（spool 扫描、stats 文本/JSON、最后维护打点）。
// 巡检脚本只解析「总条数」「按工程」行与 JSON 的 size_bytes：新增行不得占用这两个前缀，
// JSON 只加顶层 backlog，不改旧键。

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开库 %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// T7：spool 只认 seg-*.jsonl[.processing]，.tmp 与非 seg 文件不计；取最老 mtime。
func TestScanSpoolBacklog(t *testing.T) {
	root := filepath.Join(t.TempDir(), "spool")
	s1 := filepath.Join(root, "s1")
	s2 := filepath.Join(root, "s2")
	if err := os.MkdirAll(s1, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s2, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	write := func(dir, name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	write(s1, "seg-a.jsonl", 48*time.Hour)
	write(s2, "seg-b.jsonl.processing", 24*time.Hour)
	write(s1, "seg-c.jsonl.tmp", 2*time.Hour)
	write(s2, "note.txt", time.Hour)

	files, oldest, err := scanSpoolBacklog(root)
	if err != nil {
		t.Fatalf("scanSpoolBacklog: %v", err)
	}
	if files != 2 {
		t.Fatalf("只应数 seg-*.jsonl[.processing]，got %d", files)
	}
	want := now.Add(-48 * time.Hour)
	if oldest.IsZero() || oldest.Sub(want) > time.Minute || oldest.Sub(want) < -time.Minute {
		t.Fatalf("最老 mtime 应为 ~2 天前，got %v", oldest)
	}

	missing := filepath.Join(t.TempDir(), "nope")
	files, oldest, err = scanSpoolBacklog(missing)
	if err != nil || files != 0 || !oldest.IsZero() {
		t.Fatalf("root 不存在应 0/零值/nil，got files=%d oldest=%v err=%v", files, oldest, err)
	}

	blocker := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scanSpoolBacklog(blocker); err == nil {
		t.Fatal("root 是普通文件时必须报读失败")
	}
}

// statsFixture 造一份「五个指标都有值」的库况，并把 spool 目录放在库旁。
func statsFixture(t *testing.T) (dbPath string, now int64) {
	t.Helper()
	dbPath = testDB(t)
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", dbPath, "--project", "p", "用于 stats 的记忆"})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	now = time.Now().Unix()
	raw := rawDB(t, dbPath)
	if _, err := raw.Exec(`INSERT INTO candidates(id, session_id, project, kind, content,
		content_hash, status, source, created_at)
		VALUES('c-1','s1','p','fact','积压候选','h1','pending','distill',?)`, now-2*24*3600); err != nil {
		t.Fatalf("插入候选: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO evidence_sources(id, kind, locator, generation,
		last_seq, cursor_seq, created_at, updated_at)
		VALUES('src-1','hook','/tmp/s',1,5,2,?,?)`, now, now); err != nil {
		t.Fatalf("插入证据源: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO evidence_archives(archive_hash, source_id, generation,
		from_seq, to_seq, raw_count, byte_size, path, created_at)
		VALUES('ah-1','src-1',1,3,5,1,1,'/tmp/ah-1.gz',?)`, now-3*3600); err != nil {
		t.Fatalf("插入归档: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO evidence_bundles(id, source_id, generation,
		from_seq_exclusive, to_seq_inclusive, evidence_hash, status, created_at, updated_at)
		VALUES('b-1','src-1',1,2,5,'eh','pending',?,?)`, now-5*3600, now-5*3600); err != nil {
		t.Fatalf("插入 Bundle: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO db_meta(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		store.MetaMaintainLastOK, strconv.FormatInt(now-30*60, 10)); err != nil {
		t.Fatalf("写最后维护时间: %v", err)
	}

	spool := filepath.Join(filepath.Dir(dbPath), "spool", "s1")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(spool, "seg-old.jsonl")
	if err := os.WriteFile(seg, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(now-2*24*3600, 0)
	if err := os.Chtimes(seg, mt, mt); err != nil {
		t.Fatal(err)
	}
	return dbPath, now
}

// T8：文本三行 + JSON 顶层 backlog；旧键仍在顶层；空库无年龄键。
func TestCmdStatsBacklogTextAndJSON(t *testing.T) {
	dbPath, _ := statsFixture(t)

	out, err := captureStdout(t, func() error { return cmdStats([]string{"--db", dbPath}) })
	if err != nil {
		t.Fatalf("cmdStats 文本: %v", err)
	}
	for _, want := range []string{"积压", "隔离", "维护", "pending 1", "2天", "3小时", "5小时", "1 段", "洞 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("文本应含 %q，got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "按工程") {
		t.Errorf("旧「按工程」行必须保留，got:\n%s", out)
	}
	if strings.Index(out, "按工程") > strings.Index(out, "积压") {
		t.Errorf("新三行应追加在「按工程」之后，got:\n%s", out)
	}

	jsonOut, err := captureStdout(t, func() error { return cmdStats([]string{"--db", dbPath, "--json"}) })
	if err != nil {
		t.Fatalf("cmdStats JSON: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonOut), &top); err != nil {
		t.Fatalf("解析 stats JSON: %v\n%s", err, jsonOut)
	}
	for _, key := range []string{"size_bytes", "total", "by_project", "backlog"} {
		if _, ok := top[key]; !ok {
			t.Errorf("顶层旧键/新键 %q 必须存在，got %v", key, top)
		}
	}
	var bl map[string]json.RawMessage
	if err := json.Unmarshal(top["backlog"], &bl); err != nil {
		t.Fatalf("解析 backlog: %v", err)
	}
	for _, key := range []string{
		"pending_count", "oldest_pending_age_seconds", "evidence_backlog_sources",
		"oldest_unconsumed_evidence_age_seconds", "open_bundles",
		"oldest_open_bundle_age_seconds", "candidates_isolated", "bundles_isolated",
		"evidence_gaps",
		"last_maintain_ok_at", "last_maintain_ok_age_seconds", "spool_backlog_files",
		"oldest_spool_backlog_age_seconds",
	} {
		if _, ok := bl[key]; !ok {
			t.Errorf("backlog 缺键 %q，got %v", key, bl)
		}
	}

	// 空库：文本必须显示「从未维护」；JSON 的年龄键应省略而不是 0。
	emptyDB := testDB(t)
	emptyText, err := captureStdout(t, func() error { return cmdStats([]string{"--db", emptyDB}) })
	if err != nil {
		t.Fatalf("cmdStats 空库文本: %v", err)
	}
	if !strings.Contains(emptyText, "从未维护") {
		t.Errorf("空库应显示「从未维护」，got:\n%s", emptyText)
	}
	emptyJSON, err := captureStdout(t, func() error { return cmdStats([]string{"--db", emptyDB, "--json"}) })
	if err != nil {
		t.Fatalf("cmdStats 空库 JSON: %v", err)
	}
	var emptyTop map[string]json.RawMessage
	if err := json.Unmarshal([]byte(emptyJSON), &emptyTop); err != nil {
		t.Fatalf("解析空库 JSON: %v", err)
	}
	var emptyBL map[string]json.RawMessage
	if err := json.Unmarshal(emptyTop["backlog"], &emptyBL); err != nil {
		t.Fatalf("解析空库 backlog: %v", err)
	}
	for _, key := range []string{
		"oldest_pending_age_seconds", "oldest_unconsumed_evidence_age_seconds",
		"oldest_open_bundle_age_seconds", "last_maintain_ok_at", "last_maintain_ok_age_seconds",
		"oldest_spool_backlog_age_seconds",
	} {
		if _, ok := emptyBL[key]; ok {
			t.Errorf("空库不应输出年龄键 %q（无数据不得报 0），got %v", key, emptyBL)
		}
	}
}

func readLastOK(t *testing.T, dbPath string) (int64, bool) {
	t.Helper()
	raw := rawDB(t, dbPath)
	var v string
	err := raw.QueryRow(`SELECT v FROM db_meta WHERE k = ?`, store.MetaMaintainLastOK).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatalf("读 last_ok: %v", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("解析 last_ok %q: %v", v, err)
	}
	return n, true
}

// T9：gc/consolidate 成功才打点；失败不打点；closed store 与坏路径只告警不 panic。
func TestMarkMaintainOKPaths(t *testing.T) {
	t.Run("gc 成功打点", func(t *testing.T) {
		dbPath := testDB(t)
		if _, err := captureStdout(t, func() error {
			return cmdGC([]string{"--db", dbPath, "--no-backup", "--obs"})
		}); err != nil {
			t.Fatalf("cmdGC: %v", err)
		}
		got, ok := readLastOK(t, dbPath)
		if !ok {
			t.Fatal("gc 成功后必须写 maintain:last_ok")
		}
		if d := time.Since(time.Unix(got, 0)); d < -5*time.Second || d > time.Minute {
			t.Fatalf("last_ok 应≈now，got %v（差 %v）", got, d)
		}
	})

	t.Run("备份失败不打点", func(t *testing.T) {
		dbPath := testDB(t)
		if _, err := captureStdout(t, func() error {
			return cmdAdd([]string{"--db", dbPath, "非空库才需要备份"})
		}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
		blocker := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := captureStdout(t, func() error {
			return cmdGC([]string{"--db", dbPath, "--backup-dir", filepath.Join(blocker, "sub")})
		})
		if err == nil {
			t.Fatal("备份失败时 gc 必须报错")
		}
		if _, ok := readLastOK(t, dbPath); ok {
			t.Fatal("备份失败时不得写 maintain:last_ok")
		}
	})

	t.Run("consolidate 成功打点", func(t *testing.T) {
		dbPath := testDB(t)
		if _, err := captureStdout(t, func() error {
			return cmdConsolidate([]string{"--db", dbPath, "--no-backup"})
		}); err != nil {
			t.Fatalf("cmdConsolidate: %v", err)
		}
		if _, ok := readLastOK(t, dbPath); !ok {
			t.Fatal("consolidate 成功后必须写 maintain:last_ok")
		}
	})

	t.Run("closed store 只告警", func(t *testing.T) {
		dbPath := testDB(t)
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		var out string
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("closed store 打点不得 panic: %v", p)
				}
			}()
			out = captureStderr(t, func() { markMaintainOK(st) })
		}()
		if !strings.Contains(out, "警告") {
			t.Fatalf("closed store 应只告警，got %q", out)
		}
	})

	t.Run("坏路径只告警", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out string
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("坏路径打点不得 panic: %v", p)
				}
			}()
			out = captureStderr(t, func() { markMaintainOKPath(filepath.Join(blocker, "sub", "mem.db")) })
		}()
		if !strings.Contains(out, "警告") {
			t.Fatalf("坏路径应只告警，got %q", out)
		}
	})
}
