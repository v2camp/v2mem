package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

// seedAuditLedgers 在库里造一份「已结束会话的历史 + 保留期内的新记录」。
//
// 直接写 SQL 而不是走 store 的写入 API：这些表的 created_at / started_at 由
// 写入时刻决定，测试要控制年龄就只能自己给时间戳。
func seedAuditLedgers(t *testing.T, db string) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer raw.Close()

	old := time.Now().Add(-40 * 24 * time.Hour).Unix()
	// 会话 dead 是已结束会话：观测都在 40 天前。
	if _, err := raw.Exec(`INSERT INTO observations(id, session_id, project, harness, event,
		tool_name, summary, summary_idx, raw_ref, content_hash, created_at, meta_json)
		VALUES('o1','dead','p','h','stop','','s','','','h1',?,'{}')`, old); err != nil {
		t.Fatalf("插入观测: %v", err)
	}
	// 两条超期读轨迹 + 一条保留期内的。
	for _, row := range []struct {
		id      string
		session string
		at      int64
	}{
		{"t-old1", "dead", old},
		{"t-old2", "dead", old + 10},
		{"t-new", "dead", time.Now().Unix()},
	} {
		if _, err := raw.Exec(`INSERT INTO read_traces(id, session_id, project, harness, query_gate,
			gate_reason, raw_query, focus_query, scope, target_limit, budget_chars, overfetch,
			raw_hits, qualified, injected, outcome, elapsed_ms, created_at)
			VALUES(?,?,'p','h','run','','q','q','',10,3600,0,0,0,0,'abstained_no_hit',1,?)`,
			row.id, row.session, row.at); err != nil {
			t.Fatalf("插入读轨迹: %v", err)
		}
	}
	// 蒸馏账：同一会话三次运行，最新一次 ok 是游标。
	for _, row := range []struct {
		id     string
		status string
		at     int64
	}{
		{"d-old", "ok", old},
		{"d-mid", "empty", old + 10},
		{"d-new", "ok", old + 20},
	} {
		if _, err := raw.Exec(`INSERT INTO distill_runs(id, session_id, provider, status, error,
			cand_count, started_at, finished_at, window_key, bundle_id, executor, model,
			model_version, prompt_version, schema_version, generated_count, valid_count,
			invalid_count, duplicate_count, attempt, source_fingerprint)
			VALUES(?, 'dead', 'test', ?, '', 0, ?, ?, '', '', '', '', '', '', 0,0,0,0,0,0,'')`,
			row.id, row.status, row.at, row.at); err != nil {
			t.Fatalf("插入蒸馏账: %v", err)
		}
	}
}

func auditCount(t *testing.T, db, query string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("计数失败 (%s): %v", query, err)
	}
	return n
}

// --audit 回收超期账本，但留下每个会话的蒸馏游标。
func TestCmdGCAuditReclaimsExpiredLedgers(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	seedAuditLedgers(t, db)

	out, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--audit", "--audit-age", "1ns"})
	})
	if err != nil {
		t.Fatalf("cmdGC --audit: %v", err)
	}
	if !strings.Contains(out, "审计账本") {
		t.Errorf("输出应说明回收的是审计账本，got %q", out)
	}

	if n := auditCount(t, db, `SELECT COUNT(*) FROM read_traces`); n != 1 {
		t.Errorf("read_traces 剩 %d 条，期望 1（保留期内的 t-new）", n)
	}
	if n := auditCount(t, db, `SELECT COUNT(*) FROM read_trace_items`); n != 0 {
		t.Errorf("read_trace_items 应随轨迹一起清空，实际剩 %d 条", n)
	}
	if n := auditCount(t, db, `SELECT COUNT(*) FROM distill_runs`); n != 1 {
		t.Errorf("distill_runs 剩 %d 条，期望 1（每会话最新 ok 是游标）", n)
	}
	if n := auditCount(t, db, `SELECT COUNT(*) FROM distill_runs WHERE id='d-new'`); n != 1 {
		t.Errorf("游标行 d-new 必须保留")
	}
}

// 单独跑 --audit 不能动活跃会话的轨迹；把观测收掉之后会话才算结束。
// 这条用例锁的是「会话还活着」的判据：它在写观测，它的注入去重记录就不能动。
func TestCmdGCObsThenAuditFreesSessionTraces(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	seedAuditLedgers(t, db)
	// 让 dead 会话「还活着」：一条 1 分钟前的观测。轨迹本身是 40 天前的，
	// 单看年龄早该回收，只有活跃会话保护能拦住它。
	if err := execRaw(t, db, `INSERT INTO observations(id, session_id, project, harness, event,
		tool_name, summary, summary_idx, raw_ref, content_hash, created_at, meta_json)
		VALUES('o-live','dead','p','h','prompt','','s','','','h2',?,'{}')`,
		time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}

	// 第一轮：保留期按 24h 算，40 天前的轨迹超期，但会话还在写观测。
	if _, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--audit", "--audit-age", "24h"})
	}); err != nil {
		t.Fatalf("cmdGC --audit: %v", err)
	}
	if n := auditCount(t, db, `SELECT COUNT(*) FROM read_traces WHERE session_id='dead'`); n != 3 {
		t.Fatalf("会话仍有新观测时轨迹不该被回收，剩 %d 条", n)
	}

	// 第二轮：先回收观测（会话结束），再收账本。
	if _, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--obs", "--max-age", "1ns",
			"--audit", "--audit-age", "24h"})
	}); err != nil {
		t.Fatalf("cmdGC --obs --audit: %v", err)
	}
	if n := auditCount(t, db, `SELECT COUNT(*) FROM observations`); n != 0 {
		t.Errorf("观测应被 --obs 收掉，剩 %d 条", n)
	}
	if n := auditCount(t, db, `SELECT COUNT(*) FROM read_traces`); n != 1 {
		t.Errorf("会话结束后超期轨迹应回收，剩 %d 条（期望只留保留期内的 1 条）", n)
	}
}

// execRaw 直接改库，用于制造 store 写入 API 无法指定的时间戳。
func execRaw(t *testing.T, db, query string, args ...any) error {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer raw.Close()
	_, err = raw.Exec(query, args...)
	return err
}

// --audit 单独用不能把 memories 一起动了。
func TestCmdGCAuditDoesNotTouchMemories(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	if _, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--audit"})
	}); err != nil {
		t.Fatalf("cmdGC --audit: %v", err)
	}

	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	after, err := st2.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if before.ByTable["memories"] != after.ByTable["memories"] {
		t.Errorf("memories 条数变化：%d → %d", before.ByTable["memories"], after.ByTable["memories"])
	}
}
