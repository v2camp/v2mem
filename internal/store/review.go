package store

// S10a 复核数据面：记忆的人工后审核状态、候选的 failed/quarantined 两档，
// 以及给 CLI/MCP 复用的四态投影（pending / summary / quarantined / failed）。
//
// 口径来源 docs/design-data-lifecycle.md §2.1 / §6.4：
//   - 未审核记忆**立即成为最终记忆**并进检索，积压不能从 pending 搬进另一个看不见的队列；
//   - 撤回（withdrawn）立即退出主检索，但行与证据保留（可追溯、可 restore）；
//   - 直写默认视为人工已审核，只有**可证明的直写来源**在迁移时回填 confirmed。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/commit"
	"github.com/v2camp/v2mem/internal/similarity"
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
	// Project/Content/CreatedAt 仅候选态填充：`mem review ls` 承接原 candidates ls
	// 的人与巡检视图（R8 按工程积压、R9 载荷残留、R15 TTL），不再有第二个命令面。
	Project   string `json:"project,omitempty"`
	Content   string `json:"content,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
	// AttributedTo 只参与 Reviewable 计算，不随 JSON 外泄。
	AttributedTo string `json:"-"`
	// Reviewable 由 CLI 用 internal/distill 的现行准入判据补算；store 不 import
	// distill（会形成循环），故不在这里算。
	Reviewable *bool `json:"reviewable,omitempty"`
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
	return s.markCandidateTerminalTx(context.Background(), s.db, id, status, reason)
}

// markCandidateTerminalTx 是事务版，供复核动作面在单事务里复用（judge 非法输出链路、
// 撤回候选恢复等）。逻辑与 markCandidateTerminal 完全一致，只是执行者换成事务连接。
func (s *Store) markCandidateTerminalTx(ctx context.Context, q sqlQuerier, id, status, reason string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("缺少候选 id")
	}
	// 只允许 pending/failed 进入：其余状态都有明确结论，被改写成 failed/quarantined
	// 等于把已有裁决抹平（accepted 的记忆还在库里，状态却说它没成）。
	res, err := q.ExecContext(ctx, `UPDATE candidates
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
	now := time.Now().Unix()
	q := &ReviewQueue{
		States: make(map[string][]ReviewItem, 4),
		Counts: make(map[string]int, 4),
	}
	// 先建齐四个键：空态必须输出 [] 而不是 null。
	for _, st := range ReviewQueueStates() {
		q.States[st] = []ReviewItem{}
	}

	for _, st := range ReviewQueueStates() {
		if st == ReviewQueueSummary {
			// count 与 list 必须同源：一次取全量，计数就是 len，列表取前 limit 条。
			// S10a 的遗留是 count 只 JOIN memories、list 还 INNER JOIN candidates，
			// 候选被 GC 后出现「计数 1、列表 0」。这里从根上消除第二套口径。
			items, err := s.reviewSummaryItems(now)
			if err != nil {
				return nil, err
			}
			q.Counts[st] = len(items)
			if state != ReviewQueueAll && state != st {
				continue
			}
			if len(items) > limit {
				items = items[:limit]
			}
			q.States[st] = items
			continue
		}
		n, err := s.countReviewState(st)
		if err != nil {
			return nil, err
		}
		q.Counts[st] = n
		if state != ReviewQueueAll && state != st {
			continue
		}
		items, err := s.listReviewState(st, limit, now)
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

func (s *Store) listReviewState(state string, limit int, now int64) ([]ReviewItem, error) {
	switch state {
	case ReviewQueuePending, ReviewQueueQuarantined, ReviewQueueFailed:
		return s.listReviewCandidates(state, limit, now)
	}
	return nil, fmt.Errorf("未知复核状态: %s", state)
}

func (s *Store) listReviewCandidates(state string, limit int, now int64) ([]ReviewItem, error) {
	q := `SELECT c.id, c.kind, c.scope, c.status, c.status_reason, c.attempt, c.created_at,
			COALESCE(c.memory_id, ''), COALESCE(d.actor, ''), COALESCE(d.action, ''),
			c.project, c.content, c.attributed_to
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
			&it.Attempt, &createdAt, &it.MemoryID, &it.Actor, &it.Action,
			&it.Project, &it.Content, &it.AttributedTo); err != nil {
			return nil, err
		}
		it.CreatedAt = createdAt
		it.AgeSeconds = ageSeconds(now, createdAt)
		out = append(out, it)
	}
	return out, rows.Err()
}

// reviewSummaryItems 是复核摘要的**唯一**查询口径：count 与 list 都从这里取。
//
// 只 JOIN memories，不 JOIN candidates：候选是终态账，会被 GC 清掉；摘要关心的是
// 「哪条记忆还没人看过」，记忆行与裁决账才是真源。kind/scope 也从 memories 取。
// 这从根上修掉 S10a 的「count 1、list 0」：两处不再有各自的 JOIN 集合。
//
// 摘要只放三类（docs/design-data-lifecycle.md §6.4，默认信任其余 model-auto 产物）：
//  1. 低置信：理由码含糊 / self-score 低（low_confidence / unsure / vague_reason / self_score_low）；
//  2. 冲突标记：action=conflict 或理由码含 conflict；
//  3. 高影响抽样：global/cross_project/decision/preference/rule，按 hash(memory_id)%10==0
//     确定性抽约 10%，不引状态列（同一 id 在任何机器、任何重跑结论一致）。
//
// 一个记忆可能被多次 model-auto 裁决（重放、多候选收敛），取每个记忆最近一条裁决。
func (s *Store) reviewSummaryItems(now int64) ([]ReviewItem, error) {
	rows, err := s.db.Query(`SELECT d.memory_id, m.kind, m.scope, d.action,
			d.reason_codes_json, d.actor, d.created_at
		FROM commit_decisions d
		JOIN memories m ON m.id = d.memory_id
		WHERE d.actor = ? AND m.review_state = '' AND d.memory_id <> ''
		  AND d.id = (
			SELECT d2.id FROM commit_decisions d2
			WHERE d2.memory_id = d.memory_id AND d2.actor = ?
			ORDER BY d2.created_at DESC, d2.revision DESC, d2.id DESC LIMIT 1)
		ORDER BY d.created_at ASC, d.memory_id ASC`,
		modelAutoActor, modelAutoActor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReviewItem{}
	for rows.Next() {
		var it ReviewItem
		var action, reasonsJSON string
		var at int64
		if err := rows.Scan(&it.MemoryID, &it.Kind, &it.Scope, &action,
			&reasonsJSON, &it.Actor, &at); err != nil {
			return nil, err
		}
		var reasons []string
		_ = json.Unmarshal([]byte(reasonsJSON), &reasons)
		if !reviewSummaryEligible(it.MemoryID, reasons, action, it.Kind, it.Scope) {
			continue
		}
		it.ID = it.MemoryID
		it.Status = "accepted"
		it.Action = action
		it.AgeSeconds = ageSeconds(now, at)
		out = append(out, it)
	}
	return out, rows.Err()
}

// lowConfidenceReasons 是「模型自己没把握」的闭集理由码，命中即必进摘要。
var lowConfidenceReasons = map[string]bool{
	"low_confidence": true, "unsure": true, "vague_reason": true, "self_score_low": true,
}

// reviewSummaryEligible 是三类判定的唯一入口（纯函数，count 与 list 共用）。
func reviewSummaryEligible(memoryID string, reasons []string, action, kind, scope string) bool {
	// 冲突标记优先，必进。
	if strings.EqualFold(strings.TrimSpace(action), "conflict") || hasReason(reasons, "conflict") {
		return true
	}
	// 低置信，必进。
	for _, r := range reasons {
		if lowConfidenceReasons[strings.ToLower(strings.TrimSpace(r))] {
			return true
		}
	}
	// 高影响抽样：确定性约 10%。
	if highImpactKindOrScope(kind, scope) {
		return sampleHighImpact(memoryID)
	}
	// 其余 model-auto 产物默认信任，不进摘要（例行审核队列是反模式）。
	return false
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if strings.EqualFold(strings.TrimSpace(r), want) {
			return true
		}
	}
	return false
}

func highImpactKindOrScope(kind, scope string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "decision", "preference", "rule":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "global", "cross_project":
		return true
	}
	return false
}

// sampleHighImpact 按 hash(memory_id)%10==0 确定性抽约 10%（§6.4）。
//
// 用 sha256 首字节而不是随机数：同一 id 在任何机器、任何重跑都得到同一结论，
// 且不需要额外状态列、不会因 limit/--state 漂移。256 个取值里约 26 个命中 ≈ 10%。
func sampleHighImpact(memoryID string) bool {
	sum := sha256.Sum256([]byte(memoryID))
	return sum[0]%10 == 0
}

// ageSeconds 让「年龄」永不为负：客户机时钟回拨不该显示成「-3 天」。
func ageSeconds(now, at int64) int64 {
	if at <= 0 || now <= at {
		return 0
	}
	return now - at
}

// ---------- S10b 复核动作面（§6.4 / §11.1） ----------

// 人工后审核动作的闭集字面值。动作本身写进 commit_decisions.reason_codes_json，
// 不新增 schema 列，也不改 audit.go 的 Summarize。
const (
	ReviewActionConfirm  = "confirm"
	ReviewActionCorrect  = "correct"
	ReviewActionWithdraw = "withdraw"
	ReviewActionRestore  = "restore"
)

// reviewActionAudit 是承载人工后审核动作的裁决账 action 值。
// 一律配 actor=human（任务口径：新增审计事件用 commit_decisions 承载，不改 Summarize）。
const reviewActionAudit = "review_action"

// MemoryReviewInput 是一次人工后审核动作的输入。
type MemoryReviewInput struct {
	MemoryID string
	Action   string
	Content  string // correct：人工修订后的正文
	Reason   string // withdraw：撤回原因
	Actor    string // 默认 human
	Now      int64  // <=0 由 store 取当前时间
	ModelTag string // 可选：写进裁决账，供 stats 按版本归因
}

// MemoryReviewState 是动作前后的一行快照，供 --json 与审计比较。
type MemoryReviewState struct {
	ReviewState string `json:"review_state"`
	ReviewedAt  int64  `json:"reviewed_at"`
	ReviewedBy  string `json:"reviewed_by"`
	Content     string `json:"content,omitempty"`
}

// MemoryReviewReport 是一次动作的完整回报（--json 直接输出）。
type MemoryReviewReport struct {
	Action             string            `json:"action"`
	MemoryID           string            `json:"memory_id"`
	Before             MemoryReviewState `json:"before"`
	After              MemoryReviewState `json:"after"`
	AffectedCandidates []string          `json:"affected_candidates"`
	Reason             string            `json:"reason,omitempty"`
}

type memoryReviewRow struct {
	content, state, by string
	at                 int64
}

func (s *Store) memoryReviewByID(ctx context.Context, q sqlQuerier, id string) (memoryReviewRow, error) {
	var r memoryReviewRow
	err := q.QueryRowContext(ctx,
		`SELECT content, review_state, reviewed_at, reviewed_by FROM memories WHERE id = ?`, id).
		Scan(&r.content, &r.state, &r.at, &r.by)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("未找到记忆: %s", id)
	}
	return r, err
}

// ApplyMemoryReview 在单事务里完成一次人工后审核动作：状态迁移 +（withdraw）候选
// 恢复明确终态 + 一条 review_action 裁决账。全部校验在事务内读到的当前行上做，
// 避免并发下「读到旧状态、覆写别人的决定」。
func (s *Store) ApplyMemoryReview(in MemoryReviewInput) (*MemoryReviewReport, error) {
	in.MemoryID = strings.TrimSpace(in.MemoryID)
	if in.MemoryID == "" {
		return nil, errors.New("缺少记忆 id")
	}
	switch in.Action {
	case ReviewActionConfirm, ReviewActionCorrect, ReviewActionWithdraw, ReviewActionRestore:
	default:
		return nil, fmt.Errorf("非法复核动作 %q（可选：confirm|correct|withdraw|restore）", in.Action)
	}
	if in.Action == ReviewActionCorrect && strings.TrimSpace(in.Content) == "" {
		return nil, errors.New("correct 需要 --content 人工修订正文")
	}
	if in.Actor == "" {
		in.Actor = "human"
	}
	if in.Now <= 0 {
		in.Now = time.Now().Unix()
	}

	var rep *MemoryReviewReport
	err := s.withImmediateTx(func(ctx context.Context, q sqlQuerier) error {
		cur, err := s.memoryReviewByID(ctx, q, in.MemoryID)
		if err != nil {
			return err
		}
		before := MemoryReviewState{
			ReviewState: cur.state, ReviewedAt: cur.at, ReviewedBy: cur.by, Content: cur.content,
		}
		after := before
		switch in.Action {
		case ReviewActionRestore:
			if cur.state != ReviewStateWithdrawn {
				return fmt.Errorf("记忆 %s 当前 review_state=%q，restore 只能用于已撤回的记忆", in.MemoryID, cur.state)
			}
			// 撤回的逆动作：回到未审核并清掉 reviewed_*。
			after = MemoryReviewState{Content: cur.content}
		case ReviewActionWithdraw:
			if cur.state == ReviewStateWithdrawn {
				return fmt.Errorf("记忆 %s 已撤回，不得重复撤回", in.MemoryID)
			}
			after.ReviewState, after.ReviewedAt, after.ReviewedBy = ReviewStateWithdrawn, in.Now, in.Actor
		case ReviewActionConfirm:
			if cur.state == ReviewStateWithdrawn {
				return fmt.Errorf("记忆 %s 已撤回，请先 restore 再确认", in.MemoryID)
			}
			after.ReviewState, after.ReviewedAt, after.ReviewedBy = ReviewStateConfirmed, in.Now, in.Actor
		case ReviewActionCorrect:
			if cur.state == ReviewStateWithdrawn {
				return fmt.Errorf("记忆 %s 已撤回，请先 restore 再修正", in.MemoryID)
			}
			after.ReviewState, after.ReviewedAt, after.ReviewedBy = ReviewStateCorrected, in.Now, in.Actor
			after.Content = strings.TrimSpace(in.Content)
		}

		if in.Action == ReviewActionCorrect {
			// 人工修订正文：content_idx 必须同步，FTS 触发器据此重建索引。
			if _, err := q.ExecContext(ctx, `UPDATE memories
				SET content = ?, content_idx = ?, review_state = ?, reviewed_at = ?, reviewed_by = ?, updated_at = ?
				WHERE id = ?`, after.Content, indexText(after.Content), after.ReviewState,
				after.ReviewedAt, after.ReviewedBy, in.Now, in.MemoryID); err != nil {
				return err
			}
			// 签名与正文同事务更新。sync.go 头部约定「凡是改 content 的分支都会同步
			// mem_sigs 签名与 content_idx」，这里此前漏了签名：修正过的记忆仍拿旧文本
			// 的 MinHash 参与模糊检索，改完反而搜不到。content_hash 不动——它是身份键，
			// 人工换写法不改变「这是哪条事实」。
			if _, err := q.ExecContext(ctx,
				`INSERT OR REPLACE INTO mem_sigs(memory_id, sig) VALUES (?, ?)`,
				in.MemoryID, encodeSig(similarity.Sign(after.Content))); err != nil {
				return err
			}
		} else if _, err := q.ExecContext(ctx, `UPDATE memories
			SET review_state = ?, reviewed_at = ?, reviewed_by = ? WHERE id = ?`,
			after.ReviewState, after.ReviewedAt, after.ReviewedBy, in.MemoryID); err != nil {
			return err
		}

		affected := []string{}
		if in.Action == ReviewActionWithdraw {
			ids, err := s.recoverCandidatesForMemory(ctx, q, in.MemoryID, in.Reason, in.Now)
			if err != nil {
				return err
			}
			affected = ids
		}

		// 归因：未显式带 model_tag 时沿用产生该记忆的 model@promptver，
		// 让 restore（误拒回捞）等动作也能按版本计入标定统计。
		if strings.TrimSpace(in.ModelTag) == "" {
			var tag string
			_ = q.QueryRowContext(ctx, `SELECT model_tag FROM commit_decisions
				WHERE memory_id = ? AND model_tag <> '' ORDER BY created_at DESC, id DESC LIMIT 1`,
				in.MemoryID).Scan(&tag)
			in.ModelTag = tag
		}

		// 人工动作记一条 review_action：动作、前后状态、原提案（correct 的 before/after）、
		// 撤回原因都进 reason_codes_json，不新增 schema 列。
		reasons := []string{"review_action=" + in.Action,
			"before_state=" + before.ReviewState, "after_state=" + after.ReviewState}
		if in.Action == ReviewActionCorrect {
			reasons = append(reasons, "before="+truncateForAudit(before.Content),
				"after="+truncateForAudit(after.Content))
		}
		if r := strings.TrimSpace(in.Reason); r != "" {
			reasons = append(reasons, "reason="+r)
		}
		if err := s.recordReviewAction(ctx, q, in, reasons); err != nil {
			return err
		}

		rep = &MemoryReviewReport{
			Action: in.Action, MemoryID: in.MemoryID,
			Before: before, After: after,
			AffectedCandidates: affected, Reason: strings.TrimSpace(in.Reason),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// recordReviewAction 写一条 action=review_action 的裁决账。
//
// candidate_id 用 "review:<newID>" 而不是真实候选 id：这是**记忆级**动作而不是
// 候选裁决，挂到任何候选上都会让 eval flow 的 G9/G10 把它误读成一次裁决。
// memory_id 保留关联，按版本统计靠 model_tag；每次动作一条唯一 idempotency_key。
func (s *Store) recordReviewAction(ctx context.Context, q sqlQuerier, in MemoryReviewInput, reasons []string) error {
	raw, err := json.Marshal(reasons)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT OR IGNORE INTO commit_decisions(
		id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
		memory_id, model_tag, created_memory, idempotency_key, created_at
	) VALUES (?,?,0,'',?,?,?,?,?,0,?,?)`,
		"dec_"+newID(), "review:"+newID(), reviewActionAudit, in.Actor,
		string(raw), in.MemoryID, strings.TrimSpace(in.ModelTag), "ra_"+newID(), in.Now)
	return err
}

// truncateForAudit 把正文收成有界片段：原提案/修订正文只用于比较，不该把整篇
// 记忆塞进 reason_codes_json。
func truncateForAudit(s string) string {
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// recoverCandidatesForMemory 把产生该记忆的候选恢复 rejected 明确终态（withdraw /
// 按版本回滚共用）。放 review.go 而不是 candidates.go：这是复核动作面的专属迁移。
//
// 同时写一条 reject 终态裁决：eval flow 的 G9 要求 rejected 候选必须有匹配裁决，
// 这里不放宽 G 断言，也不用「为了过闸伪造裁决」——撤回记忆本就是人工否决了那条候选。
func (s *Store) recoverCandidatesForMemory(ctx context.Context, q sqlQuerier,
	memoryID, reason string, now int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, status FROM candidates WHERE memory_id = ?`, memoryID)
	if err != nil {
		return nil, err
	}
	type candRow struct{ id, status string }
	var cands []candRow
	for rows.Next() {
		var c candRow
		if err := rows.Scan(&c.id, &c.status); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	statusReason := "review_withdraw"
	if r := strings.TrimSpace(reason); r != "" {
		statusReason = r
	}
	out := []string{}
	for _, c := range cands {
		if c.status == "quarantined" {
			continue // 隔离是明确终态，撤回不得覆盖它
		}
		if _, err := q.ExecContext(ctx, `UPDATE candidates
			SET status = 'rejected', status_reason = ?, decided_at = ? WHERE id = ?`,
			statusReason, now, c.id); err != nil {
			return nil, err
		}
		// revision 用 0：不推进 CAS，也不与历史采纳裁决的 revision 混淆。
		if err := recordDecision(ctx, q, c.id, 0, "", commit.ActionReject, "human",
			[]string{"review_withdraw"}, memoryID, "", false); err != nil {
			return nil, err
		}
		out = append(out, c.id)
	}
	return out, nil
}

// ---------- 按版本回滚（§6.4 / §11.1） ----------

// RollbackReport 是按 model@promptver 回滚的完整回报（--json 直接输出）。
// 计数是数字，明细在 *_ids；matched = withdrawn + skipped + already。
type RollbackReport struct {
	ModelTag     string   `json:"model_tag"`
	Matched      int      `json:"matched"`
	Withdrawn    int      `json:"withdrawn"`
	Skipped      int      `json:"skipped"`
	Already      int      `json:"already"`
	Reopened     int      `json:"reopened"`
	DryRun       bool     `json:"dry_run"`
	WithdrawnIDs []string `json:"withdrawn_ids"`
	SkippedIDs   []string `json:"skipped_ids"`
	AlreadyIDs   []string `json:"already_ids"`
	ReopenedIDs  []string `json:"reopened_ids"`
}

// RollbackModelTag 撤回某个 model@promptver 新建的自动记忆（只撤回、不删行）。
//
// 单事务；只命中 created_memory=1 且 model_tag 精确相等的行，不碰别的版本。
// 人工已确认/修正的跳过并报告（不得盲目删除），已撤回的计入 already；
// 其余标 withdrawn + 候选回 rejected + 写一条 reopen 决策（保 G9 口径）。
// dryRun 时只算报告、零业务写入；重跑因状态已是 withdrawn 而天然幂等。
func (s *Store) RollbackModelTag(modelTag string, dryRun bool, now int64) (*RollbackReport, error) {
	modelTag = strings.TrimSpace(modelTag)
	if modelTag == "" {
		return nil, errors.New("缺少 --model-tag")
	}
	if now <= 0 {
		now = time.Now().Unix()
	}
	rep := &RollbackReport{
		ModelTag: modelTag, DryRun: dryRun,
		WithdrawnIDs: []string{}, SkippedIDs: []string{}, AlreadyIDs: []string{}, ReopenedIDs: []string{},
	}
	err := s.withImmediateTx(func(ctx context.Context, q sqlQuerier) error {
		rows, err := q.QueryContext(ctx, `SELECT d.memory_id, d.candidate_id
			FROM commit_decisions d
			JOIN memories m ON m.id = d.memory_id
			WHERE d.model_tag = ? AND d.created_memory = 1 AND d.memory_id <> ''
			ORDER BY d.memory_id, d.created_at`, modelTag)
		if err != nil {
			return err
		}
		type hit struct{ memID, candID string }
		var hits []hit
		for rows.Next() {
			var h hit
			if err := rows.Scan(&h.memID, &h.candID); err != nil {
				rows.Close()
				return err
			}
			hits = append(hits, h)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		seen := map[string]bool{}
		for _, h := range hits {
			if seen[h.memID] {
				continue // 同一记忆被多条裁决命中只处理一次
			}
			seen[h.memID] = true
			var state string
			if err := q.QueryRowContext(ctx,
				`SELECT review_state FROM memories WHERE id = ?`, h.memID).Scan(&state); err != nil {
				return err
			}
			rep.Matched++
			switch state {
			case ReviewStateConfirmed, ReviewStateCorrected:
				rep.Skipped++
				rep.SkippedIDs = append(rep.SkippedIDs, h.memID)
				continue // 人工决定不得被批量回滚覆盖
			case ReviewStateWithdrawn:
				rep.Already++
				rep.AlreadyIDs = append(rep.AlreadyIDs, h.memID)
				continue
			}
			rep.Withdrawn++
			rep.WithdrawnIDs = append(rep.WithdrawnIDs, h.memID)
			if dryRun {
				continue // 零写入：只报计划动作
			}
			if _, err := q.ExecContext(ctx, `UPDATE memories
				SET review_state = ?, reviewed_at = ?, reviewed_by = ? WHERE id = ?`,
				ReviewStateWithdrawn, now, "human", h.memID); err != nil {
				return err
			}
			ids, err := s.recoverCandidatesForMemory(ctx, q, h.memID, "review_rollback:"+modelTag, now)
			if err != nil {
				return err
			}
			for _, cid := range ids {
				// reopen 决策：把「这条按版本被重开/撤回」留在裁决历史里（保 G9 口径）。
				if err := recordDecision(ctx, q, cid, 0, "", commit.ActionReopen, "human",
					[]string{"review_rollback", modelTag}, h.memID, modelTag, false); err != nil {
					return err
				}
				rep.Reopened++
				rep.ReopenedIDs = append(rep.ReopenedIDs, cid)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// ---------- 标定统计（§6.2 / §7 第 1 条） ----------

// ReviewStatsRow 是按 judge_model/model_tag 分组的标定统计。
// 阈值只置 Warn 提示：不改提示词、不自动动作、stats 本身只读（§6.2）。
type ReviewStatsRow struct {
	ModelTag     string  `json:"model_tag"`
	Samples      int     `json:"samples"`
	Confirmed    int     `json:"confirmed"`
	Corrected    int     `json:"corrected"`
	Withdrawn    int     `json:"withdrawn"`
	ConfirmRate  float64 `json:"confirm_rate"`
	CorrectRate  float64 `json:"correct_rate"`
	WithdrawRate float64 `json:"withdraw_rate"`
	Recovered    int     `json:"recovered"` // 误拒回捞（restore）数
	Warn         bool    `json:"warn"`
}

const (
	reviewWarnWithdrawRate = 0.20
	reviewWarnCorrectRate  = 0.30
)

// ReviewStats 按 model_tag 分组汇总样本与三种后审核率。
//
// 分组维度取 commit_decisions.model_tag（model-auto 采纳时落的 provider:model@promptver）；
// candidates.judge_model 是同一条归因链在判定器侧的写法，二者按版本可比。
// 只读单次聚合：JudgeStats 的口径保留给影子判定，这里专供后审核标定。
func (s *Store) ReviewStats() ([]ReviewStatsRow, error) {
	counts := map[string]map[string]int{}
	rows, err := s.db.Query(`SELECT d.model_tag, m.review_state, COUNT(DISTINCT d.memory_id)
		FROM commit_decisions d
		JOIN memories m ON m.id = d.memory_id
		WHERE d.model_tag <> '' AND d.created_memory = 1 AND d.memory_id <> ''
		GROUP BY d.model_tag, m.review_state`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tag, state string
		var n int
		if err := rows.Scan(&tag, &state, &n); err != nil {
			rows.Close()
			return nil, err
		}
		if counts[tag] == nil {
			counts[tag] = map[string]int{}
		}
		counts[tag][state] += n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	recovered := map[string]int{}
	rrows, err := s.db.Query(`SELECT model_tag, COUNT(*) FROM commit_decisions
		WHERE action = ? AND reason_codes_json LIKE '%review_action=restore%'
		GROUP BY model_tag`, reviewActionAudit)
	if err != nil {
		return nil, err
	}
	for rrows.Next() {
		var tag string
		var n int
		if err := rrows.Scan(&tag, &n); err != nil {
			rrows.Close()
			return nil, err
		}
		recovered[tag] = n
	}
	if err := rrows.Err(); err != nil {
		rrows.Close()
		return nil, err
	}
	rrows.Close()

	tags := make([]string, 0, len(counts))
	for tag := range counts {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	out := make([]ReviewStatsRow, 0, len(tags))
	for _, tag := range tags {
		c := counts[tag]
		samples := c[ReviewStateNone] + c[ReviewStateConfirmed] + c[ReviewStateCorrected] + c[ReviewStateWithdrawn]
		row := ReviewStatsRow{
			ModelTag: tag, Samples: samples,
			Confirmed: c[ReviewStateConfirmed], Corrected: c[ReviewStateCorrected],
			Withdrawn: c[ReviewStateWithdrawn], Recovered: recovered[tag],
		}
		if samples > 0 {
			row.ConfirmRate = float64(row.Confirmed) / float64(samples)
			row.CorrectRate = float64(row.Corrected) / float64(samples)
			row.WithdrawRate = float64(row.Withdrawn) / float64(samples)
		}
		row.Warn = row.WithdrawRate > reviewWarnWithdrawRate || row.CorrectRate > reviewWarnCorrectRate
		out = append(out, row)
	}
	return out, nil
}
