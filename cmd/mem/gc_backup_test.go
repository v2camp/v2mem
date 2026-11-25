package main

// S3：破坏性 GC 的备份前置与备份校验（design-data-lifecycle §8.1 顺序铁律）。
// 两条红线：
//  1. 备份不可写 → 一个业务动作都不执行（含 --obs / --candidates / --evidence / --audit）；
//  2. 备份生成了但校验不过 → 同样拒绝执行，因为「有个文件」不等于「能恢复」。

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gcBackupFixture 造一份「四个命令都能删到东西」的库况。
type gcBackupFixture struct {
	db       string
	archDir  string
	archPath string
	raw      *sql.DB
}

func newGCBackupFixture(t *testing.T) gcBackupFixture {
	t.Helper()
	db := testDB(t)
	// 库必须非空：空库本来就不备份，也就没有「备份失败」可言。
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", db, "备份失败时不许被删的记忆"})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	old := time.Now().Add(-40 * 24 * time.Hour).Unix()
	if _, err := raw.Exec(`INSERT INTO observations(id, session_id, project, harness, event,
		tool_name, summary, summary_idx, raw_ref, content_hash, created_at, meta_json)
		VALUES('o-old','dead','p','h','stop','','s','','','h1',?,'{}')`, old); err != nil {
		t.Fatalf("插入观测: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO candidates(id, session_id, project, kind, content,
		content_hash, status, source, created_at)
		VALUES('c-old','s','p','fact','超期候选','ch1','pending','distill',?)`, old); err != nil {
		t.Fatalf("插入候选: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO read_traces(id, session_id, project, harness, query_gate,
		gate_reason, raw_query, focus_query, scope, target_limit, budget_chars, overfetch,
		raw_hits, qualified, injected, outcome, elapsed_ms, created_at)
		VALUES('t-old','dead','p','h','run','','q','q','',10,3600,0,0,0,0,'abstained_no_hit',1,?)`, old); err != nil {
		t.Fatalf("插入读轨迹: %v", err)
	}

	archDir := filepath.Join(filepath.Dir(db), "evidence", "archives")
	if err := os.MkdirAll(archDir, 0o700); err != nil {
		t.Fatalf("建归档目录: %v", err)
	}
	hash := strings.Repeat("a", 64)
	archPath := filepath.Join(archDir, hash+".jsonl.gz")
	body := []byte("账本外的归档字节")
	if err := os.WriteFile(archPath, body, 0o600); err != nil {
		t.Fatalf("写归档文件: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO evidence_archives(archive_hash, source_id, generation,
		from_seq, to_seq, raw_count, byte_size, path, created_at)
		VALUES(?,'src-a',1,1,1,1,?,?,?)`, hash, int64(len(body)), archPath, old); err != nil {
		t.Fatalf("插入归档账本: %v", err)
	}
	return gcBackupFixture{db: db, archDir: archDir, archPath: archPath, raw: raw}
}

func (f gcBackupFixture) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := f.raw.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("计数失败 (%s): %v", query, err)
	}
	return n
}

// A1：备份目录不可建（普通文件下的子目录）→ 四个子命令都必须在派发之前拒绝，
// 业务数据一条都不许动。
func TestCmdGCSubcommandsRefuseWhenBackupFails(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		check func(t *testing.T, f gcBackupFixture)
	}{
		{
			name: "obs",
			args: []string{"--obs", "--max-age", "1ns"},
			check: func(t *testing.T, f gcBackupFixture) {
				if n := f.count(t, `SELECT COUNT(*) FROM observations`); n != 1 {
					t.Errorf("观测应原样 1 条，got %d", n)
				}
			},
		},
		{
			name: "candidates",
			args: []string{"--candidates"},
			check: func(t *testing.T, f gcBackupFixture) {
				if n := f.count(t, `SELECT COUNT(*) FROM candidates WHERE status = 'pending'`); n != 1 {
					t.Errorf("候选仍应是 pending，got %d", n)
				}
			},
		},
		{
			name: "evidence",
			args: []string{"--evidence", "--evidence-grace", "1ms"},
			check: func(t *testing.T, f gcBackupFixture) {
				if _, err := os.Stat(f.archPath); err != nil {
					t.Errorf("归档文件必须原样在: %v", err)
				}
				if n := f.count(t, `SELECT COUNT(*) FROM evidence_archives`); n != 1 {
					t.Errorf("归档账本应原样 1 行，got %d", n)
				}
			},
		},
		{
			name: "audit",
			args: []string{"--audit", "--audit-age", "1ns"},
			check: func(t *testing.T, f gcBackupFixture) {
				if n := f.count(t, `SELECT COUNT(*) FROM read_traces`); n != 1 {
					t.Errorf("读轨迹应原样 1 条，got %d", n)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGCBackupFixture(t)
			blocker := filepath.Join(t.TempDir(), "afile")
			if err := os.WriteFile(blocker, []byte("不是目录"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"--db", f.db, "--backup-dir", filepath.Join(blocker, "sub")}, tc.args...)
			_, err := captureStdout(t, func() error { return cmdGC(args) })
			if err == nil {
				t.Fatal("备份目录不可写时必须拒绝执行")
			}
			if !strings.Contains(err.Error(), "备份") {
				t.Fatalf("错误应说明是备份环节: %v", err)
			}
			tc.check(t, f)
		})
	}
}

// A2：备份文件生成了但校验不过 → 同样拒绝执行，业务零变更。
// 用测试缝 verifyBackupFn 注入，只替换校验结果，不碰真实快照。
func TestCmdGCRefusesWhenBackupUnverified(t *testing.T) {
	f := newGCBackupFixture(t)
	orig := verifyBackupFn
	verifyBackupFn = func(string) error { return errors.New("造一个坏快照") }
	t.Cleanup(func() { verifyBackupFn = orig })

	_, err := captureStdout(t, func() error { return cmdGC([]string{"--db", f.db}) })
	if err == nil {
		t.Fatal("备份校验失败时必须拒绝执行")
	}
	if !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("错误应点明「校验失败」: %v", err)
	}
	if n := len(listBackups(t, f.db)); n != 1 {
		t.Errorf("备份文件应已生成（只是校验不过），got %d", n)
	}
	// 零变更：decay GC 没跑，记忆/观测/候选/账本全部原样。
	for _, q := range []string{
		`SELECT COUNT(*) FROM memories`,
		`SELECT COUNT(*) FROM observations`,
		`SELECT COUNT(*) FROM candidates WHERE status = 'pending'`,
		`SELECT COUNT(*) FROM evidence_archives`,
		`SELECT COUNT(*) FROM read_traces`,
	} {
		if n := f.count(t, q); n != 1 {
			t.Errorf("%s 应原样 1 条，got %d", q, n)
		}
	}
	if _, statErr := os.Stat(f.archPath); statErr != nil {
		t.Errorf("归档文件必须原样在: %v", statErr)
	}
}
