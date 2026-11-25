package store

// S10a 复核数据面：记忆的人工后审核状态、候选的 failed/quarantined 两档，
// 以及给 CLI/MCP 复用的四态投影（pending / summary / quarantined / failed）。
//
// 口径来源 docs/design-data-lifecycle.md §2.1 / §6.4：
//   - 未审核记忆**立即成为最终记忆**并进检索，积压不能从 pending 搬进另一个看不见的队列；
//   - 撤回（withdrawn）立即退出主检索，但行与证据保留（可追溯、可 restore）；
//   - 直写默认视为人工已审核，只有**可证明的直写来源**在迁移时回填 confirmed。

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 记忆的人工后审核状态。” 表示未审核（直写来源之外的默认值）。
const (
	ReviewStateNone      = ""
	ReviewStateConfirmed = "confirmed"
	ReviewStateCorrected = "corrected"
	ReviewStateWithdrawn = "withdrawn"
)

// 复核队列的四个分段名。CLI `mem review ls --state` 与 MCP review_ls 共用这一套字面值。
const (
	ReviewQueuePending     = "pending"
	ReviewQueueSummary     = "summary"
	ReviewQueueQuarantined = "quarantined"
	ReviewQueueFailed      = "failed"
	ReviewQueueAll         = "all"
)

// directWriteSourcesSQL 是「可证明的直写来源」白名单（SQL IN 列表）。
//
// 只收 mem add / mcp add / ingest 直接落 memories 的 source 取值域
// （与 cmd/mem 的 `--source` 帮助文本一致）。distill-accept 是复核通道的产物，
// 空 source 是迁移前旧行——两者都无从证明「人看过」，一律留未审核。
const directWriteSourcesSQL = "('human','llm','ingest','harness-summary','bench','test')"

// isDirectWriteSource 与 directWriteSourcesSQL 同源（Go 侧版本），供写入路径判定。
func isDirectWriteSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "human", "llm", "ingest", "harness-summary", "bench", "test":
		return true
	}
	return false
}

// ReviewItem 是复核队列的一行投影。四态共用同一个形状：
// 前端/模型只需要认一套字段，不必为每种状态各写一套解析。
type ReviewItem struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Scope  string `json:"scope"`
	Status string `json:"status"`
	// StatusReason 是候选最后一次失败/隔离的原因（pending/summary 为空）。
	StatusReason string `json:"status_reason,omitempty"`
	Attempt      int    `json:"attempt"`
	// AgeSeconds 是这一行进入当前状态后过了多久（候选按 created_at，
	// 复核摘要按模型那次裁决的时间）。由调用方决定怎么展示（天/小时）。
	AgeSeconds int64 `json:"age_seconds"`
	// MemoryID 只在已落库的行上有值（摘要态必填），候选态通常为空。
	MemoryID string `json:"memory_id,omitempty"`
	// Actor/Action 是该行最近一次裁决；没有裁决留空。
	Actor  string `json:"actor,omitempty"`
	Action string `json:"action,omitempty"`
}

// ReviewQueue 是四态视图的完整投影，可直接 JSON 序列化。
//
// States 恒含四个键（空态是 []，不是 null）：输出形状稳定，前端不必判空分支。
// Counts 恒为全量计数，**不受 --state 与 limit 影响**——一屏能看到别处还压着多少等人工项。
type ReviewQueue struct {
	States map[string][]ReviewItem `json:"states"`
	Counts map[string]int          `json:"counts"`
}

// ReviewQueueStates 返回四个分段的固定顺序，供 CLI 渲染与 JSON 键校验。
func ReviewQueueStates() []string {
	return []string{ReviewQueuePending, ReviewQueueSummary, ReviewQueueQuarantined, ReviewQueueFailed}
}

// ensureReviewSchema 给旧库补 S10a 的列与索引，并在补列的那一次回填直写来源。
//
// 为什么不把列/索引写进 schema.sql：schema.sql 只对新建库生效，存量库恰恰是最需要
// 补齐的那批；而索引写在建表脚本里会先于 ALTER 执行，旧库还没有列时直接让 Open 失败。
func ensureReviewSchema(db *sql.DB) error {
	added, err := ensureColumnAdded(db, "memories", "review_state", "TEXT NOT NULL DEFAULT ''")
	if err != nil {
		return err
	}
	cols := []struct{ table, column, ddl string }{
		{"memories", "reviewed_at", "INTEGER NOT NULL DEFAULT 0"},
		{"memories", "reviewed_by", "TEXT NOT NULL DEFAULT ''"},
		{"candidates", "status_reason", "TEXT NOT NULL DEFAULT ''"},
		{"candidates", "judge_reason", "TEXT NOT NULL DEFAULT ''"},
		{"candidates", "attempt", "INTEGER NOT NULL DEFAULT 0"},
		{"commit_decisions", "model_tag", "TEXT NOT NULL DEFAULT ''"},
		{"commit_decisions", "created_memory", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, c := range cols {
		if err := ensureColumn(db, c.table, c.column, c.ddl); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS ix_mem_review_state ON memories(review_state)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS ix_commit_model ON commit_decisions(model_tag, action)`); err != nil {
		return err
	}
	if added {
		return backfillDirectWriteReviews(db)
	}
	return nil
}

// ensureColumnAdded 与 ensureColumn 等价，但额外报告「这一列是不是这次刚加上」。
// 迁移期的一次性数据修补必须挂在「刚加上」这个事实上，否则每次 Open 都会重跑——
// 人工撤回过的行会被下一次打开悄悄改回 confirmed。
func ensureColumnAdded(db *sql.DB, table, column, ddl string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, ddl)); err != nil {
		return false, err
	}
	return true, nil
}

// backfillDirectWriteReviews 只把**可证明的直写来源**标成已审核。
//
// reviewed_by 写的是来源标记而不是人名：迁移期没有「谁审的」这回事，
// 能证明的只有「这一行是从直写通道进来的」。distill/proposal 通道一律留 ”，
// 全库历史一律标已审核会让复核摘要（summary）永远看不到模型写入的行。
func backfillDirectWriteReviews(db *sql.DB) error {
	_, err := db.Exec(`UPDATE memories
		SET review_state = ?, reviewed_at = created_at, reviewed_by = 'direct-write'
		WHERE review_state = '' AND source IN `+directWriteSourcesSQL,
		ReviewStateConfirmed)
	return err
}

// SetMemoryReviewState 设置一条记忆的人工后审核状态。
//
// state 只认 ”/confirmed/corrected/withdrawn：自由文本会让「撤回」变成搜不到的错别字。
// by/now 由调用方给（CLI 传 actor 与当前时间），这里不自己猜时钟。
func (s *Store) SetMemoryReviewState(id, state, by string, now int64) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("缺少记忆 id")
	}
	switch state {
	case ReviewStateNone, ReviewStateConfirmed, ReviewStateCorrected, ReviewStateWithdrawn:
	default:
		return fmt.Errorf("非法 review_state: %q（可选：''|confirmed|corrected|withdrawn）", state)
	}
	if now <= 0 {
		now = time.Now().Unix()
	}
	res, err := s.db.Exec(`UPDATE memories SET review_state = ?, reviewed_at = ?, reviewed_by = ?
		WHERE id = ?`, state, now, strings.TrimSpace(by), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("未找到记忆: %s", id)
	}
	return nil
}

// MarkCandidateFailed 把候选标成 failed：可重试，attempt 递增，原因落 status_reason。
//
// 只允许从 pending/failed 进入：accepted/rejected/expired/quarantined 都有明确结论，
// 被这里改写成 failed 等于把已有裁决抹平。0 行更新一律报 ErrCandidateClosed，
// 让调用方知道「状态没动」而不是以为写成功了。
func (s *Store) MarkCandidateFailed(id, reason string) error {
	return s.markCandidateTerminal(id, "failed", reason)
}

// MarkCandidateQuarantined 把候选标成 quarantined：终态，只能人工确认修复后显式重放。
//
// 与 failed 的差别只有一处：failed 还能再进来一次（attempt 继续涨），
// quarantined 进来第二次就是调用方的错——它已经不在自动重试队列里了。
func (s *Store) MarkCandidateQuarantined(id, reason string) error {
	return s.markCandidateTerminal(id, "quarantined", reason)
}

func (s *Store) markCandidateTerminal(id, status, reason string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("缺少候选 id")
	}
	// 只允许 pending/failed 进入：其余状态都有明确结论，被改写成 failed/quarantined
	// 等于把已有裁决抹平（accepted 的记忆还在库里，状态却说它没成）。
	res, err := s.db.Exec(`UPDATE candidates
		SET status = ?, status_reason = ?, attempt = attempt + 1, decided_at = ?
		WHERE id = ? AND status IN ('pending', 'failed')`,
		status, strings.TrimSpace(reason), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: 候选 %s 不能在当前状态转入 %s", ErrCandidateClosed, id, status)
	}
	return nil
}

// ListReviewQueue 返回四态视图。state 取 all|pending|summary|quarantined|failed。
//
// 只做查询与投影，判定口径全部写在这里而不是 CLI：
// 未来 MCP 的 review_ls 直接复用，避免「同一状态两处给不同数」。
// limit 按**每个分段**截断（一屏四段各自的前 N 条），计数不受 limit 影响。
func (s *Store) ListReviewQueue(state string, limit int) (*ReviewQueue, error) {
	state = strings.ToLower(strings.TrimSpace(state))
	if state == "" {
		state = ReviewQueueAll
	}
	switch state {
	case ReviewQueueAll, ReviewQueuePending, ReviewQueueSummary, ReviewQueueQuarantined, ReviewQueueFailed:
	default:
		return nil, fmt.Errorf("非法复核状态: %q（可选：all|pending|summary|quarantined|failed）", state)
	}
	if limit <= 0 {
		limit = 50
	}
	q := &ReviewQueue{
		States: make(map[string][]ReviewItem, 4),
		Counts: make(map[string]int, 4),
	}
	// 先建齐四个键：空态必须输出 [] 而不是 null。
	for _, st := range ReviewQueueStates() {
		q.States[st] = []ReviewItem{}
	}

	for _, st := range ReviewQueueStates() {
		n, err := s.countReviewState(st)
		if err != nil {
			return nil, err
		}
		q.Counts[st] = n
		if state != ReviewQueueAll && state != st {
			continue
		}
		items, err := s.listReviewState(st, limit)
		if err != nil {
			return nil, err
		}
		q.States[st] = items
	}
	return q, nil
}

func (s *Store) countReviewState(state string) (int, error) {
	var q string
	switch state {
	case ReviewQueuePending:
		q = `SELECT COUNT(*) FROM candidates WHERE status = 'pending' AND judge_verdict = ''`
	case ReviewQueueQuarantined:
		q = `SELECT COUNT(*) FROM candidates WHERE status = 'quarantined'`
	case ReviewQueueFailed:
		q = `SELECT COUNT(*) FROM candidates WHERE status = 'failed'`
	case ReviewQueueSummary:
		// 按记忆去重：同一条知识被多次自动裁决只算一条待复核。
		q = `SELECT COUNT(*) FROM (
			SELECT d.memory_id FROM commit_decisions d
			JOIN memories m ON m.id = d.memory_id
			WHERE d.actor = '` + modelAutoActor + `' AND m.review_state = '' AND d.memory_id <> ''
			GROUP BY d.memory_id)`
	default:
		return 0, fmt.Errorf("未知复核状态: %s", state)
	}
	var n int
	if err := s.db.QueryRow(q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// modelAutoActor 与 commit.ActorModelAuto 同值。store 不 import commit 的常量表
// 只是为了避免把 SQL 字符串拼成变量；两处一致性由 cmd/mem 的测试守卫。
const modelAutoActor = "model-auto"

func (s *Store) listReviewState(state string, limit int) ([]ReviewItem, error) {
	now := time.Now().Unix()
	switch state {
	case ReviewQueuePending, ReviewQueueQuarantined, ReviewQueueFailed:
		return s.listReviewCandidates(state, limit, now)
	case ReviewQueueSummary:
		return s.listReviewSummaries(limit, now)
	}
	return nil, fmt.Errorf("未知复核状态: %s", state)
}

func (s *Store) listReviewCandidates(state string, limit int, now int64) ([]ReviewItem, error) {
	q := `SELECT c.id, c.kind, c.scope, c.status, c.status_reason, c.attempt, c.created_at,
			COALESCE(c.memory_id, ''), COALESCE(d.actor, ''), COALESCE(d.action, '')
		FROM candidates c
		LEFT JOIN commit_decisions d ON d.id = (
			SELECT d2.id FROM commit_decisions d2 WHERE d2.candidate_id = c.id
			ORDER BY d2.created_at DESC, d2.revision DESC, d2.id DESC LIMIT 1)
		WHERE c.status = ?`
	args := []any{state}
	if state == ReviewQueuePending {
		// pending 段 = 模型还没跑过的：judge_verdict 非空的已被看过，留在人工裁决队列而不是这里。
		q += ` AND c.judge_verdict = ''`
	}
	q += ` ORDER BY c.created_at ASC, c.id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReviewItem{}
	for rows.Next() {
		var it ReviewItem
		var createdAt int64
		if err := rows.Scan(&it.ID, &it.Kind, &it.Scope, &it.Status, &it.StatusReason,
			&it.Attempt, &createdAt, &it.MemoryID, &it.Actor, &it.Action); err != nil {
			return nil, err
		}
		it.AgeSeconds = ageSeconds(now, createdAt)
		out = append(out, it)
	}
	return out, rows.Err()
}

// listReviewSummaries 按记忆投影「模型自动写库、还没人看过」的行。
//
// 一个记忆可能被多次 model-auto 裁决（重放、多个候选收敛到同一条知识），
// 这里取每个记忆最近的一条裁决，保证一条记忆只出现一次。
func (s *Store) listReviewSummaries(limit int, now int64) ([]ReviewItem, error) {
	rows, err := s.db.Query(`SELECT d.memory_id, c.kind, c.scope, c.status, c.attempt, d.created_at,
			d.actor, d.action
		FROM commit_decisions d
		JOIN memories m ON m.id = d.memory_id
		JOIN candidates c ON c.id = d.candidate_id
		WHERE d.actor = ? AND m.review_state = '' AND d.memory_id <> ''
		  AND d.id = (
			SELECT d2.id FROM commit_decisions d2
			WHERE d2.memory_id = d.memory_id AND d2.actor = ?
			ORDER BY d2.created_at DESC, d2.revision DESC, d2.id DESC LIMIT 1)
		ORDER BY d.created_at ASC, d.memory_id ASC LIMIT ?`,
		modelAutoActor, modelAutoActor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReviewItem{}
	for rows.Next() {
		var it ReviewItem
		var at int64
		if err := rows.Scan(&it.MemoryID, &it.Kind, &it.Scope, &it.Status, &it.Attempt,
			&at, &it.Actor, &it.Action); err != nil {
			return nil, err
		}
		it.ID = it.MemoryID
		it.AgeSeconds = ageSeconds(now, at)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ageSeconds 让「年龄」永不为负：客户机时钟回拨不该显示成「-3 天」。
func ageSeconds(now, at int64) int64 {
	if at <= 0 || now <= at {
		return 0
	}
	return now - at
}
