package store

import (
	"testing"
)

// 审计账本准出的数据面测试。
//
// 这组用例守的不是「删掉了几行」，而是「删完之后判据还成立」：
//   - distill_runs 是蒸馏游标（每会话最新一次 ok/empty 运行必须保留——
//     S12 后 proposal 单轨不再按观测水位挑会话，但游标仍是审计 GC 与
//     R12 判据的依据，删错最新运行等于抹掉「蒸馏过」的事实）；
//   - read_traces 是注入去重的依据（SessionInjectedRuleIDs 按 session 无时间窗读），
//     删掉活跃会话的轨迹会让同一条规则在同一会话里被重复注入。
// 所以每个用例都断言「允许删的删了」+「不该删的还在」。

const (
	auditTestOld = int64(1_700_000_000) // 远早于 cutoff
	auditTestNew = int64(1_900_000_000) // 远晚于 cutoff
	auditTestCut = int64(1_800_000_000) // cutoff
)

func addDistillRun(t *testing.T, s *Store, id, session, status string, startedAt int64, bundleID string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO distill_runs(
		id, session_id, provider, status, error, cand_count, started_at, finished_at,
		window_key, bundle_id, executor, model, model_version, prompt_version,
		schema_version, generated_count, valid_count, invalid_count, duplicate_count,
		attempt, source_fingerprint)
		VALUES(?, ?, 'test', ?, '', 0, ?, ?, '', ?, '', '', '', '', 0, 0, 0, 0, 0, 0, '')`,
		id, session, status, startedAt, startedAt, bundleID); err != nil {
		t.Fatalf("插入 distill_run %s 失败: %v", id, err)
	}
}

func addObservationAt(t *testing.T, s *Store, id, session string, createdAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO observations(
		id, session_id, project, harness, event, tool_name, summary, summary_idx,
		raw_ref, content_hash, created_at, meta_json)
		VALUES(?, ?, 'p', 'h', 'stop', '', 's', '', '', ?, ?, '{}')`,
		id, session, id, createdAt); err != nil {
		t.Fatalf("插入 observation %s 失败: %v", id, err)
	}
}

func addReadTrace(t *testing.T, s *Store, id, session string, createdAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO read_traces(
		id, session_id, project, harness, query_gate, gate_reason, raw_query,
		focus_query, scope, target_limit, budget_chars, overfetch, raw_hits,
		qualified, injected, outcome, elapsed_ms, created_at)
		VALUES(?, ?, 'p', 'h', 'run', '', 'q', 'q', '', 10, 3600, 0, 0, 0, 0, 'abstained_no_hit', 1, ?)`,
		id, session, createdAt); err != nil {
		t.Fatalf("插入 read_trace %s 失败: %v", id, err)
	}
	if _, err := s.db.Exec(`INSERT INTO read_trace_items(trace_id, ordinal, memory_id)
		VALUES(?, 1, 'm1')`, id); err != nil {
		t.Fatalf("插入 read_trace_item %s 失败: %v", id, err)
	}
}

func auditCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("计数失败 (%s): %v", query, err)
	}
	return n
}

// 游标不变是这组用例里最硬的一条：删历史可以，删掉会话的最新一次成功运行
// 就等于宣布「这个会话从没蒸馏过」，下一轮巡检会把它重新捡起来重跑。
func TestGCAuditKeepsNewestDistillRunPerSession(t *testing.T) {
	s := newTestStore(t)

	// 会话 A：三次运行，最新一次是 ok —— 必须留下。
	addDistillRun(t, s, "a-old", "A", "ok", auditTestOld, "")
	addDistillRun(t, s, "a-mid", "A", "empty", auditTestOld+100, "")
	addDistillRun(t, s, "a-new", "A", "ok", auditTestOld+200, "")
	// 会话 B：只有一次失败 —— 不构成游标，过了保留期可以删。
	addDistillRun(t, s, "b-fail", "B", "fail", auditTestOld, "")
	// 会话 C：只有一次 ok，且时间在保留期内 —— 不动。
	addDistillRun(t, s, "c-new", "C", "ok", auditTestNew, "")

	before := auditCount(t, s, `SELECT COUNT(*) FROM distill_runs`)

	res, err := s.GCAudit(auditTestCut)
	if err != nil {
		t.Fatalf("GCAudit: %v", err)
	}
	// 删 3 条：会话 A 的两条历史成功运行 + 会话 B 的失败运行（失败不构成游标）。
	if res.DistillRuns != 3 {
		t.Errorf("DistillRuns = %d，期望 3（a-old / a-mid / b-fail）", res.DistillRuns)
	}
	got := auditCount(t, s, `SELECT COUNT(*) FROM distill_runs`)
	if want := before - 3; got != want {
		t.Errorf("剩余 distill_runs = %d，期望 %d（只留 a-new 与 c-new）", got, want)
	}

	// 游标断言：每个会话的 MAX(finished_at) 与 GC 前一致。
	if n := auditCount(t, s, `SELECT COUNT(*) FROM distill_runs WHERE id IN ('a-new','c-new')`); n != 2 {
		t.Errorf("会话最新成功运行必须保留，实际剩 %d 条", n)
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM distill_runs WHERE id IN ('a-old','a-mid')`); n != 0 {
		t.Errorf("会话 A 的历史成功运行应已回收，实际剩 %d 条", n)
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM distill_runs WHERE id = 'b-fail'`); n != 0 {
		t.Errorf("失败运行不构成游标，超期应回收，实际剩 %d 条", n)
	}
}

// 仍被 evidence_bundles 引用的运行是提案溯源（ProposalRuns 按 bundle_id 读），
// 不能因为「超期」就抹掉——否则 bundle 详情页会显示「从未运行过」。
func TestGCAuditKeepsRunsReferencedByLiveBundle(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`INSERT INTO evidence_sources(id, kind, locator, generation, last_seq, cursor_seq,
		created_at, updated_at) VALUES('src1','transcript','/tmp/t',1,10,10,?,?)`,
		auditTestOld, auditTestOld); err != nil {
		t.Fatalf("插入 evidence_source: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO evidence_bundles(id, source_id, generation,
		from_seq_exclusive, to_seq_inclusive, evidence_hash, status, created_at, updated_at)
		VALUES('bdl1','src1',1,0,10,'h','ok',?,?)`, auditTestOld, auditTestOld); err != nil {
		t.Fatalf("插入 evidence_bundle: %v", err)
	}
	addDistillRun(t, s, "run-of-bundle", "S", "ok", auditTestOld, "bdl1")
	addDistillRun(t, s, "run-free", "S", "ok", auditTestOld+10, "")
	// 失败行不参与游标：它既超期又无引用，是这条用例里唯一该被删的行。
	addDistillRun(t, s, "run-stale", "S", "fail", auditTestOld+5, "")

	if _, err := s.GCAudit(auditTestCut); err != nil {
		t.Fatalf("GCAudit: %v", err)
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM distill_runs WHERE id = 'run-of-bundle'`); n != 1 {
		t.Errorf("被 bundle 引用的运行必须保留，实际剩 %d 条", n)
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM distill_runs WHERE id = 'run-free'`); n != 1 {
		t.Errorf("会话最新成功运行是游标，必须保留，实际剩 %d 条", n)
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM distill_runs WHERE id = 'run-stale'`); n != 0 {
		t.Errorf("超期失败行既非游标也无引用，应回收，实际剩 %d 条", n)
	}
}

// 读轨迹是纯日志，但 SessionInjectedRuleIDs 按 session 无时间窗读它做注入去重，
// 所以「会话还活着」的轨迹不能动，否则同一条规则会在同一会话里重复注入。
func TestGCAuditDeletesReadTracesButKeepsLiveSessionTraces(t *testing.T) {
	s := newTestStore(t)

	// 会话 live：有晚于 cutoff 的观测，属于活跃会话。
	addObservationAt(t, s, "ol", "live", auditTestNew)
	addReadTrace(t, s, "t-live", "live", auditTestOld)
	// 会话 dead：观测都在 cutoff 之前，属于已结束会话。
	addObservationAt(t, s, "od", "dead", auditTestOld)
	addReadTrace(t, s, "t-dead", "dead", auditTestOld)
	addReadTrace(t, s, "t-dead2", "dead", auditTestOld+100)
	// 保留期内的轨迹：不动。
	addReadTrace(t, s, "t-new", "other", auditTestNew)

	res, err := s.GCAudit(auditTestCut)
	if err != nil {
		t.Fatalf("GCAudit: %v", err)
	}
	if res.ReadTraces != 2 {
		t.Errorf("ReadTraces = %d，期望 2（t-dead / t-dead2）", res.ReadTraces)
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM read_traces WHERE id = 't-live'`); n != 1 {
		t.Errorf("活跃会话的轨迹必须保留，实际剩 %d 条", n)
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM read_traces WHERE id IN ('t-dead','t-dead2')`); n != 0 {
		t.Errorf("已结束会话的超期轨迹应回收，实际剩 %d 条", n)
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM read_traces WHERE id = 't-new'`); n != 1 {
		t.Errorf("保留期内的轨迹不应动，实际剩 %d 条", n)
	}
	// 明细必须跟着走：trace 删了 items 还在就是孤儿行。剩 t-live 与 t-new 两条。
	if n := auditCount(t, s, `SELECT COUNT(*) FROM read_trace_items`); n != 2 {
		t.Errorf("read_trace_items 剩 %d 条，期望 2（只剩 t-live / t-new 的明细）", n)
	}
	if res.ReadTraceItems != 2 {
		t.Errorf("ReadTraceItems = %d，期望 2", res.ReadTraceItems)
	}
}

// cutoff 之前的一条不留、之后的一条不动，是这条命令能被定时任务信任的前提。
func TestGCAuditRespectsCutoffBoundary(t *testing.T) {
	s := newTestStore(t)

	addReadTrace(t, s, "t-just-before", "s", auditTestCut-1)
	addReadTrace(t, s, "t-at-cut", "s", auditTestCut)

	if _, err := s.GCAudit(auditTestCut); err != nil {
		t.Fatalf("GCAudit: %v", err)
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM read_traces WHERE id = 't-just-before'`); n != 0 {
		t.Errorf("cutoff 之前的轨迹应删除")
	}
	if n := auditCount(t, s, `SELECT COUNT(*) FROM read_traces WHERE id = 't-at-cut'`); n != 1 {
		t.Errorf("cutoff 当刻的轨迹不应删除（严格小于才删）")
	}
}

// 空库上跑不能报错：定时任务会无条件调用。
func TestGCAuditOnEmptyStore(t *testing.T) {
	s := newTestStore(t)
	res, err := s.GCAudit(auditTestCut)
	if err != nil {
		t.Fatalf("GCAudit: %v", err)
	}
	if res.DistillRuns != 0 || res.ReadTraces != 0 || res.ReadTraceItems != 0 {
		t.Errorf("空库应全 0，got %+v", res)
	}
}
