package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/commit"
	"github.com/v2camp/v2mem/internal/similarity"
)

// T3 提交治理与证据链。
//
// 三条不可让步的约束：
//  1. 一个候选的「状态迁移 + 记忆写入 + 证据链 + 派生边 + 裁决记录」必须在**同一个**
//     BEGIN IMMEDIATE 事务里完成。分开提交会留下「记忆写了但证据没挂」这种无法自动
//     修复的半状态；而 DEFERRED 事务要到第一条写语句才取锁，两个进程会同时读到
//     同一个 revision 再各自尝试更新，CAS 形同虚设。
//  2. 幂等键 = candidate + revision + action + evidence_hash。重试、崩溃恢复、并发
//     重复调用都必须收敛到同一条 commit_decisions，不能靠「再查一次状态」猜。
//  3. 任何证据字段都只能来自 candidate_evidence / observations，不得为 legacy 行
//     编造 event_uid、archive_hash 或 seq —— 那是伪证，比没有证据更糟。

var (
	// ErrRevisionConflict 表示候选 revision 已变化：调用方拿着过期的裁决前提。
	ErrRevisionConflict = errors.New("候选 revision 已变化，请重新读取后再裁决")
	// ErrCandidateClosed 表示候选已不在 pending（已拒绝/过期）。
	ErrCandidateClosed = errors.New("候选已裁决，不能再提交")
)

// 编译期确认提交侧真的满足冻结契约，避免「实现了同名方法但签名漂移」。
var (
	_ commit.LinkReader          = (*Store)(nil)
	_ commit.ProvenanceFormatter = commit.Compact{}
	_ commit.Policy              = commit.Matrix{}
)

// sqlQuerier 同时覆盖 *sql.DB、*sql.Tx、*sql.Conn。提交路径统一用 Context 版本，
// 这样同一段代码既能在事务连接上跑，也能被只读查询复用。
type sqlQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// CommitCandidate 是候选在提交侧的完整视图。
//
// 不复用 Candidate：老结构缺 revision/scope/review_class，而提交矩阵必须看这些字段。
// 新开一个类型比给旧结构加字段更安全——旧结构被 CLI/MCP 的多处 JSON 输出依赖，
// 加字段会悄悄改变对外契约。
type CommitCandidate struct {
	ID          string  `json:"id"`
	SessionID   string  `json:"session_id"`
	Project     string  `json:"project"`
	Kind        string  `json:"kind"`
	Content     string  `json:"content"`
	ContentHash string  `json:"content_hash"`
	Status      string  `json:"status"`
	Channel     string  `json:"channel,omitempty"`
	Scope       string  `json:"scope,omitempty"`
	ReviewClass string  `json:"review_class,omitempty"`
	SubjectKey  string  `json:"subject_key,omitempty"`
	TaskState   string  `json:"task_state,omitempty"`
	Rationale   string  `json:"rationale,omitempty"`
	Revision    int     `json:"revision"`
	Salience    float64 `json:"salience"`
	MemoryID    string  `json:"memory_id,omitempty"`
}

// Proposal 把提交视图映射成 commit 包的只读候选，供矩阵裁决。
func (c CommitCandidate) Proposal() commit.Candidate {
	return commit.Candidate{
		ID: c.ID, SessionID: c.SessionID, Project: c.Project, Kind: c.Kind,
		Scope: c.Scope, Status: c.Status, ReviewClass: c.ReviewClass,
		Revision: c.Revision, Content: c.Content, ContentHash: c.ContentHash,
		SubjectKey: c.SubjectKey, TaskState: c.TaskState, Rationale: c.Rationale,
		Salience: c.Salience,
	}
}

// CommitResult 是一次提交/裁决的完整结果。
type CommitResult struct {
	Candidate      Candidate     `json:"candidate"`
	MemoryID       string        `json:"memory_id,omitempty"`
	Hash           string        `json:"hash,omitempty"`
	Action         commit.Action `json:"action"`
	Actor          string        `json:"actor"`
	EvidenceHash   string        `json:"evidence_hash"`
	EvidenceCount  int           `json:"evidence_count"`
	Provenance     string        `json:"provenance,omitempty"`
	ReasonCodes    []string      `json:"reason_codes,omitempty"`
	IdempotencyKey string        `json:"idempotency_key,omitempty"`
	Already        bool          `json:"already"`        // accepted 重放
	CreatedMemory  bool          `json:"created_memory"` // 新建了记忆行
	Committed      bool          `json:"committed"`      // 真的写入/采纳了
}

const commitCandidateColumns = `id, session_id, project, kind, content, content_hash,
	status, channel, scope, review_class, subject_key, task_state, rationale,
	revision, salience, memory_id`

func scanCommitCandidate(row interface{ Scan(...any) error }) (*CommitCandidate, error) {
	var c CommitCandidate
	var memory sql.NullString
	var review, scope, subject, task, rationale, channel string
	if err := row.Scan(&c.ID, &c.SessionID, &c.Project, &c.Kind, &c.Content, &c.ContentHash,
		&c.Status, &channel, &scope, &review, &subject, &task, &rationale,
		&c.Revision, &c.Salience, &memory); err != nil {
		return nil, err
	}
	c.Channel, c.Scope, c.ReviewClass = channel, scope, review
	c.SubjectKey, c.TaskState, c.Rationale = subject, task, rationale
	if memory.Valid {
		c.MemoryID = memory.String
	}
	return &c, nil
}

func (s *Store) commitCandidateByID(ctx context.Context, q sqlQuerier, id string) (*CommitCandidate, error) {
	return scanCommitCandidate(q.QueryRowContext(ctx,
		`SELECT `+commitCandidateColumns+` FROM candidates WHERE id = ?`, id))
}

// CommitCandidate 按 id 或前缀取提交视图。
func (s *Store) CommitCandidate(idOrPrefix string) (*CommitCandidate, error) {
	base, err := s.ResolveCandidate(idOrPrefix)
	if err != nil {
		return nil, err
	}
	c, err := s.commitCandidateByID(context.Background(), s.db, base.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("未找到候选: %s", idOrPrefix)
	}
	return c, err
}

// CommitDecisions 返回某候选的裁决历史（按时间正序），供审计与排障。
func (s *Store) CommitDecisions(candidateID string) ([]commit.Decision, error) {
	rows, err := s.db.Query(`SELECT candidate_id, revision, evidence_hash, action, actor,
			reason_codes_json, memory_id, idempotency_key, created_at
		FROM commit_decisions WHERE candidate_id = ? ORDER BY created_at ASC, id ASC`, candidateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []commit.Decision
	for rows.Next() {
		var d commit.Decision
		var reasons string
		if err := rows.Scan(&d.CandidateID, &d.Revision, &d.EvidenceHash, &d.Action,
			&d.Actor, &reasons, &d.MemoryID, &d.IdempotencyKey, &d.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(reasons), &d.ReasonCodes)
		out = append(out, d)
	}
	return out, rows.Err()
}

// withImmediateTx 在专用连接上执行 BEGIN IMMEDIATE。
//
// 为什么不用 db.Begin：它发的是 DEFERRED，两个写者都能先读到同一个 revision，
// 直到写语句才互斥；那时 CAS 已经失去了「唯一赢家」的判定点。IMMEDIATE 在事务
// 开始时就拿到 RESERVED 锁，后到者排在 busy_timeout 上，等前者提交后重新读，
// 于是 revision 比较发生在锁内，结论可信。
func (s *Store) withImmediateTx(fn func(context.Context, sqlQuerier) error) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// busy_timeout 是**连接级** PRAGMA：Open 时设过的那条只作用于池里的某一条连接。
	// 这里在取锁前重设，否则 BEGIN IMMEDIATE 撞锁会立刻返回 SQLITE_BUSY，
	// 并发调用方拿到的不是「等前者提交后按 revision 判定」，而是一个假故障。
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err := fn(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// candidateEvidenceRow 是 candidate_evidence 的一行，带解析后的观测定位。
type candidateEvidenceRow struct {
	EvidenceID   string
	EventUID     string
	ArchiveHash  string
	RawLocator   string
	Grade        commit.Grade
	Role         commit.Role
	Seq          int
	OccurredAtNS int64
}

// loadCandidateEvidence 读取候选证据并补齐观测定位。
//
// 补齐只做「把已有字段搬到正确位置」：观测存在时用它的 raw_ref/source_seq/
// occurred_at_ns；不存在时保留 candidate_evidence 原值，绝不生成新的身份。
func (s *Store) loadCandidateEvidence(ctx context.Context, q sqlQuerier, candidateID string) ([]candidateEvidenceRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT evidence_id, event_uid, archive_hash,
			evidence_role, source_seq
		FROM candidate_evidence WHERE candidate_id = ? ORDER BY evidence_role, evidence_id`, candidateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidateEvidenceRow
	for rows.Next() {
		var e candidateEvidenceRow
		var role string
		if err := rows.Scan(&e.EvidenceID, &e.EventUID, &e.ArchiveHash, &role, &e.Seq); err != nil {
			return nil, err
		}
		e.Role = commit.CanonicalRole(role)
		if strings.TrimSpace(e.EvidenceID) == "" && strings.TrimSpace(e.EventUID) == "" {
			// 既无本机 id 也无 canonical 身份：无法定位、无法验证，不能算作证据。
			continue
		}
		if err := s.fillEvidenceLocator(ctx, q, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) fillEvidenceLocator(ctx context.Context, q sqlQuerier, e *candidateEvidenceRow) error {
	var id, eventUID, archive, rawRef string
	var seq int
	var occurred int64
	query := `SELECT id, event_uid, archive_hash, raw_ref, source_seq, occurred_at_ns
		FROM observations WHERE id = ?`
	args := []any{e.EvidenceID}
	if strings.TrimSpace(e.EvidenceID) == "" {
		if strings.TrimSpace(e.EventUID) == "" {
			e.Grade = commit.GradeC
			return nil
		}
		query = `SELECT id, event_uid, archive_hash, raw_ref, source_seq, occurred_at_ns
			FROM observations WHERE event_uid = ?`
		args = []any{e.EventUID}
	}
	err := q.QueryRowContext(ctx, query, args...).Scan(&id, &eventUID, &archive, &rawRef, &seq, &occurred)
	if errors.Is(err, sql.ErrNoRows) && strings.TrimSpace(e.EventUID) != "" && e.EvidenceID != "" {
		err = q.QueryRowContext(ctx, `SELECT id, event_uid, archive_hash, raw_ref, source_seq, occurred_at_ns
			FROM observations WHERE event_uid = ?`, e.EventUID).
			Scan(&id, &eventUID, &archive, &rawRef, &seq, &occurred)
	}
	switch {
	case err == nil:
		e.EvidenceID = id
		if e.EventUID == "" {
			e.EventUID = eventUID
		}
		if e.ArchiveHash == "" {
			e.ArchiveHash = archive
		}
		if seq > 0 {
			e.Seq = seq
		}
		e.OccurredAtNS = occurred
		e.RawLocator = rawRef
	case errors.Is(err, sql.ErrNoRows):
	default:
		return err
	}
	if e.RawLocator == "" && e.ArchiveHash != "" {
		if e.Seq > 0 {
			e.RawLocator = fmt.Sprintf("%s#%d", e.ArchiveHash, e.Seq)
		} else {
			e.RawLocator = e.ArchiveHash
		}
	}
	e.Grade = gradeOfEvidence(e.EventUID, e.ArchiveHash)
	return nil
}

// gradeOfEvidence 是等级的唯一判定点。
//
// A = 有 canonical 身份且落到了归档段；B = 有其中一项；C = 只有裸摘要/无身份。
// 这个定义让「A 级直接证据」可以被机器稳定复核，也不依赖任何模型判断。
func gradeOfEvidence(eventUID, archiveHash string) commit.Grade {
	hasUID := strings.TrimSpace(eventUID) != ""
	hasArchive := strings.TrimSpace(archiveHash) != ""
	switch {
	case hasUID && hasArchive:
		return commit.GradeA
	case hasUID || hasArchive:
		return commit.GradeB
	default:
		return commit.GradeC
	}
}

func evidenceItems(rows []candidateEvidenceRow) []commit.Item {
	items := make([]commit.Item, 0, len(rows))
	for _, e := range rows {
		items = append(items, commit.Item{
			EvidenceID: e.EvidenceID,
			Role:       e.Role,
			Link: commit.Link{
				EventUID: e.EventUID, ArchiveHash: e.ArchiveHash, RawLocator: e.RawLocator,
				Grade: e.Grade, Seq: e.Seq, OccurredAtNS: e.OccurredAtNS,
			},
		})
	}
	return items
}

func evidenceLinks(rows []candidateEvidenceRow, memoryID string) []commit.Link {
	links := make([]commit.Link, 0, len(rows))
	for _, e := range rows {
		links = append(links, commit.Link{
			MemoryID: memoryID, EventUID: e.EventUID, ArchiveHash: e.ArchiveHash,
			RawLocator: e.RawLocator, Grade: e.Grade, Seq: e.Seq, OccurredAtNS: e.OccurredAtNS,
		})
	}
	return links
}

// DecideCandidate 只裁决不写库，供 CLI 预演与测试使用。
func (s *Store) DecideCandidate(idOrPrefix string) (*CommitCandidate, commit.Decision, error) {
	c, err := s.CommitCandidate(idOrPrefix)
	if err != nil {
		return nil, commit.Decision{}, err
	}
	rows, err := s.loadCandidateEvidence(context.Background(), s.db, c.ID)
	if err != nil {
		return nil, commit.Decision{}, err
	}
	d, err := (commit.Matrix{}).DecideItems(c.Proposal(), evidenceItems(rows))
	return c, d, err
}

// CommitAccept 以人工身份采纳候选，expectedRevision 是调用方读到的 revision。
//
// expectedRevision < 0 表示「按当前 revision 执行」：只用于兼容旧调用（AcceptCandidate），
// 仍然会在事务内重新读取并写入 CAS，只是不把过期前提当成错误。
func (s *Store) CommitAccept(idOrPrefix string, expectedRevision int, actor string) (*CommitResult, error) {
	c, err := s.CommitCandidate(idOrPrefix)
	if err != nil {
		return nil, err
	}
	return s.commitAcceptView(c, expectedRevision, actor, commit.ActionHumanAccept, nil)
}

// AutoCommitCandidate 按矩阵裁决：只有 ActionAutoCommit 才写记忆，其余只记裁决。
//
// 为什么「只记裁决」也要写 commit_decisions：否则同一候选会被自动调度反复裁决，
// 每次都烧一遍证据读取，却看不到「它已经被判为需要人工」这个事实。
func (s *Store) AutoCommitCandidate(idOrPrefix string, expectedRevision int) (*CommitResult, error) {
	c, err := s.CommitCandidate(idOrPrefix)
	if err != nil {
		return nil, err
	}
	rows, err := s.loadCandidateEvidence(context.Background(), s.db, c.ID)
	if err != nil {
		return nil, err
	}
	items := evidenceItems(rows)
	d, err := (commit.Matrix{}).DecideItems(c.Proposal(), items)
	if err != nil {
		return nil, err
	}
	if d.Action == commit.ActionAutoCommit {
		return s.commitAcceptView(c, expectedRevision, commit.ActorAuto, d.Action, d.ReasonCodes)
	}
	res := &CommitResult{
		Action: d.Action, Actor: commit.ActorAuto, EvidenceHash: d.EvidenceHash,
		EvidenceCount: len(rows), ReasonCodes: d.ReasonCodes, IdempotencyKey: d.IdempotencyKey,
	}
	err = s.withImmediateTx(func(ctx context.Context, q sqlQuerier) error {
		cur, err := s.commitCandidateByID(ctx, q, c.ID)
		if err != nil {
			return err
		}
		if err := recordDecision(ctx, q, cur.ID, cur.Revision, d.EvidenceHash, d.Action,
			commit.ActorAuto, d.ReasonCodes, cur.MemoryID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	updated, err := s.candidateByID(c.ID)
	if err != nil {
		return nil, err
	}
	res.Candidate = *updated
	return res, nil
}

func (s *Store) commitAcceptView(view *CommitCandidate, expectedRevision int, actor string,
	action commit.Action, reasons []string) (*CommitResult, error) {
	if actor == "" {
		actor = commit.ActorHuman
	}
	action = commit.NormalizeAction(action)
	if reasons == nil {
		reasons = []string{}
	}

	var out *CommitResult
	err := s.withImmediateTx(func(ctx context.Context, q sqlQuerier) error {
		cur, err := s.commitCandidateByID(ctx, q, view.ID)
		if err != nil {
			return err
		}
		rows, err := s.loadCandidateEvidence(ctx, q, cur.ID)
		if err != nil {
			return err
		}
		items := evidenceItems(rows)
		evidenceHash := commit.HashEvidence(items)
		key := commit.IdempotencyKey(cur.ID, cur.Revision, action, evidenceHash)

		if cur.Status == "accepted" {
			if expectedRevision >= 0 && expectedRevision != cur.Revision {
				return fmt.Errorf("%w: 候选 %s 当前 revision=%d, 期望 %d",
					ErrRevisionConflict, cur.ID, cur.Revision, expectedRevision)
			}
			r, err := s.replayAccepted(ctx, q, cur, rows, evidenceHash, action, actor, reasons)
			if err != nil {
				return err
			}
			r.IdempotencyKey = key
			out = r
			return nil
		}
		if cur.Status != "pending" {
			return fmt.Errorf("%w: 候选 %s 状态为 %s", ErrCandidateClosed, cur.ID, cur.Status)
		}
		if expectedRevision >= 0 && expectedRevision != cur.Revision {
			return fmt.Errorf("%w: 候选 %s 当前 revision=%d, 期望 %d",
				ErrRevisionConflict, cur.ID, cur.Revision, expectedRevision)
		}

		now := time.Now().Unix()
		memoryID, createdMemory, err := s.upsertAcceptedMemory(ctx, q, cur, now)
		if err != nil {
			return err
		}
		prov := commitProvenance(rows, cur.ID)
		if _, err := q.ExecContext(ctx, `UPDATE memories
			SET provenance = CASE WHEN provenance IS NULL OR provenance = '' THEN ? ELSE provenance END
			WHERE id = ?`, prov, memoryID); err != nil {
			return err
		}
		if err := writeMemoryEvidence(ctx, q, memoryID, rows, now); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			`INSERT OR IGNORE INTO edges(src, dst, rel) VALUES (?, ?, 'derived_from')`,
			memoryID, cur.ID); err != nil {
			return err
		}
		// 只允许 pending → accepted：并发唯一赢家由 revision CAS 保证，
		// 第二个写者会看到 0 行而不是覆盖先到者的 memory_id。
		res, err := q.ExecContext(ctx, `UPDATE candidates
			SET status = 'accepted', decided_at = ?, memory_id = ?, revision = revision + 1
			WHERE id = ? AND revision = ? AND status = 'pending'`,
			now, memoryID, cur.ID, cur.Revision)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: 候选 %s", ErrRevisionConflict, cur.ID)
		}
		if err := recordDecision(ctx, q, cur.ID, cur.Revision, evidenceHash, action, actor, reasons, memoryID); err != nil {
			return err
		}
		out = &CommitResult{
			MemoryID: memoryID, Hash: cur.ContentHash, Action: action, Actor: actor,
			EvidenceHash: evidenceHash, EvidenceCount: len(rows), Provenance: prov,
			ReasonCodes: reasons, IdempotencyKey: key, CreatedMemory: createdMemory,
			Committed: true,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	updated, err := s.candidateByID(view.ID)
	if err != nil {
		return nil, err
	}
	out.Candidate = *updated
	if out.Hash == "" {
		out.Hash = updated.Hash
	}
	return out, nil
}

// replayAccepted 处理 accepted 的重放：只补证据链/派生边/provenance/裁决记录。
//
// 关键：**不增加 adopted_count**。重放不是又一次采纳，把它计成采纳会让
// 采纳次数随重试次数膨胀，指标完全失真。同时也不重写 memories 的正文/时间戳。
func (s *Store) replayAccepted(ctx context.Context, q sqlQuerier, cur *CommitCandidate,
	rows []candidateEvidenceRow, evidenceHash string, action commit.Action,
	actor string, reasons []string) (*CommitResult, error) {
	if cur.MemoryID == "" {
		return nil, fmt.Errorf("候选 %s 已采纳但缺少 memory_id，不能重放", cur.ID)
	}
	var exists int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE id = ?`, cur.MemoryID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("候选 %s 的 memory_id %s 已不存在，不能重放", cur.ID, cur.MemoryID)
	}
	now := time.Now().Unix()
	if err := writeMemoryEvidence(ctx, q, cur.MemoryID, rows, now); err != nil {
		return nil, err
	}
	prov := commitProvenance(rows, cur.ID)
	if _, err := q.ExecContext(ctx, `UPDATE memories
		SET provenance = CASE WHEN provenance IS NULL OR provenance = '' THEN ? ELSE provenance END
		WHERE id = ?`, prov, cur.MemoryID); err != nil {
		return nil, err
	}
	if _, err := q.ExecContext(ctx,
		`INSERT OR IGNORE INTO edges(src, dst, rel) VALUES (?, ?, 'derived_from')`,
		cur.MemoryID, cur.ID); err != nil {
		return nil, err
	}
	if err := recordDecision(ctx, q, cur.ID, cur.Revision, evidenceHash, action, actor, reasons, cur.MemoryID); err != nil {
		return nil, err
	}
	return &CommitResult{
		MemoryID: cur.MemoryID, Hash: cur.ContentHash, Action: action, Actor: actor,
		EvidenceHash: evidenceHash, EvidenceCount: len(rows), Provenance: prov,
		ReasonCodes: reasons, Already: true, Committed: true,
	}, nil
}

// upsertAcceptedMemory 写记忆并回填 adopted_count。
//
// 同 (content_hash, project) 已有记忆时不新建，只把这次采纳计入 adopted_count
// 并补 provenance；「不新建」是跨会话/跨设备收敛的必要条件，否则同一事实会
// 在库里出现两行，检索侧永远只能看到其中一行。
func (s *Store) upsertAcceptedMemory(ctx context.Context, q sqlQuerier, cur *CommitCandidate, now int64) (string, bool, error) {
	channel, err := DefaultChannel(cur.Channel, "distill-accept")
	if err != nil {
		return "", false, err
	}
	hash := cur.ContentHash
	if hash == "" {
		hash = HashOf(cur.Content)
	}
	idx := indexText(cur.Content)
	salience := cur.Salience
	if salience <= 0 || salience > 1 {
		salience = 0.5
	}
	device, _ := os.Hostname()

	var id string
	err = q.QueryRowContext(ctx,
		`SELECT id FROM memories WHERE content_hash = ? AND project = ?`, hash, cur.Project).Scan(&id)
	created := false
	switch {
	case err == nil:
		if _, err := q.ExecContext(ctx, `UPDATE memories
			SET content = ?, content_idx = ?, updated_at = ?, last_seen_at = ?,
			    access_count = access_count + 1, salience = MAX(salience, ?),
			    origin_tool = 'distill', source = 'distill-accept',
			    adopted_count = adopted_count + 1, last_adopted_at = ?
			WHERE id = ?`,
			cur.Content, idx, now, now, salience, now, id); err != nil {
			return "", false, err
		}
	case errors.Is(err, sql.ErrNoRows):
		created = true
		id = newID()
		if _, err := q.ExecContext(ctx, `INSERT INTO memories
			(id, content, content_idx, kind, content_hash, project, salience,
			 created_at, updated_at, last_seen_at, expires_at,
			 origin_device, origin_tool, source, channel, provenance,
			 access_count, adopted_count, last_adopted_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,NULL,?,?,?,?,NULL,0,1,?)`,
			id, cur.Content, idx, cur.Kind, hash, cur.Project, salience,
			now, now, now, device, "distill", "distill-accept", channel, now); err != nil {
			return "", false, err
		}
	default:
		return "", false, err
	}
	if _, err := q.ExecContext(ctx,
		`INSERT OR REPLACE INTO mem_sigs(memory_id, sig) VALUES (?, ?)`,
		id, encodeSig(similarity.Sign(cur.Content))); err != nil {
		return "", false, err
	}
	return id, created, nil
}

func writeMemoryEvidence(ctx context.Context, q sqlQuerier, memoryID string, rows []candidateEvidenceRow, now int64) error {
	for _, e := range rows {
		if strings.TrimSpace(e.EvidenceID) == "" && strings.TrimSpace(e.EventUID) == "" {
			continue
		}
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO memory_evidence(
			memory_id, evidence_id, event_uid, archive_hash, raw_locator, grade,
			seq, occurred_at_ns, created_at
		) VALUES (?,?,?,?,?,?,?,?,?)`,
			memoryID, e.EvidenceID, e.EventUID, e.ArchiveHash, e.RawLocator,
			string(e.Grade), e.Seq, e.OccurredAtNS, now); err != nil {
			return err
		}
	}
	return nil
}

func recordDecision(ctx context.Context, q sqlQuerier, candidateID string, revision int,
	evidenceHash string, action commit.Action, actor string, reasons []string, memoryID string) error {
	if reasons == nil {
		reasons = []string{}
	}
	raw, err := json.Marshal(reasons)
	if err != nil {
		return err
	}
	key := commit.IdempotencyKey(candidateID, revision, action, evidenceHash)
	_, err = q.ExecContext(ctx, `INSERT OR IGNORE INTO commit_decisions(
		id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
		memory_id, idempotency_key, created_at
	) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"dec_"+newID(), candidateID, revision, evidenceHash, string(action), actor,
		string(raw), memoryID, key, time.Now().Unix())
	return err
}

// CommitReject 拒绝候选。expectedRevision < 0 表示按当前 revision 执行。
func (s *Store) CommitReject(idOrPrefix string, expectedRevision int, actor, reason string) (*CommitResult, error) {
	view, err := s.CommitCandidate(idOrPrefix)
	if err != nil {
		return nil, err
	}
	if actor == "" {
		actor = commit.ActorHuman
	}
	reasons := []string{"human_reject"}
	if strings.TrimSpace(reason) != "" {
		reasons = append(reasons, strings.TrimSpace(reason))
	}
	var out *CommitResult
	err = s.withImmediateTx(func(ctx context.Context, q sqlQuerier) error {
		cur, err := s.commitCandidateByID(ctx, q, view.ID)
		if err != nil {
			return err
		}
		rows, err := s.loadCandidateEvidence(ctx, q, cur.ID)
		if err != nil {
			return err
		}
		evidenceHash := commit.HashEvidence(evidenceItems(rows))
		if cur.Status == "rejected" {
			if expectedRevision >= 0 && expectedRevision != cur.Revision {
				return fmt.Errorf("%w: 候选 %s 当前 revision=%d, 期望 %d",
					ErrRevisionConflict, cur.ID, cur.Revision, expectedRevision)
			}
			if err := recordDecision(ctx, q, cur.ID, cur.Revision, evidenceHash,
				commit.ActionReject, actor, reasons, ""); err != nil {
				return err
			}
			out = &CommitResult{Action: commit.ActionReject, Actor: actor,
				EvidenceHash: evidenceHash, EvidenceCount: len(rows),
				ReasonCodes: reasons, Already: true, Committed: true}
			return nil
		}
		if cur.Status != "pending" {
			return fmt.Errorf("%w: 候选 %s 状态为 %s", ErrCandidateClosed, cur.ID, cur.Status)
		}
		if expectedRevision >= 0 && expectedRevision != cur.Revision {
			return fmt.Errorf("%w: 候选 %s 当前 revision=%d, 期望 %d",
				ErrRevisionConflict, cur.ID, cur.Revision, expectedRevision)
		}
		res, err := q.ExecContext(ctx, `UPDATE candidates
			SET status = 'rejected', decided_at = ?, revision = revision + 1
			WHERE id = ? AND revision = ? AND status = 'pending'`,
			time.Now().Unix(), cur.ID, cur.Revision)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: 候选 %s", ErrRevisionConflict, cur.ID)
		}
		if err := recordDecision(ctx, q, cur.ID, cur.Revision, evidenceHash,
			commit.ActionReject, actor, reasons, ""); err != nil {
			return err
		}
		out = &CommitResult{Action: commit.ActionReject, Actor: actor,
			EvidenceHash: evidenceHash, EvidenceCount: len(rows),
			ReasonCodes: reasons, Committed: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	updated, err := s.candidateByID(view.ID)
	if err != nil {
		return nil, err
	}
	out.Candidate = *updated
	return out, nil
}

// ---------- 证据链读取（commit.LinkReader） ----------

// EvidenceLinks 返回一条记忆的全部证据引用，按发生顺序排列。
func (s *Store) EvidenceLinks(memoryID string) ([]commit.Link, error) {
	if strings.TrimSpace(memoryID) == "" {
		return nil, nil
	}
	links, err := s.evidenceLinksQuery([]string{memoryID})
	if err != nil {
		return nil, err
	}
	return append([]commit.Link(nil), links[memoryID]...), nil
}

// EvidenceLinksBatch 一次取完多条记忆的 provenance，供 hook 注入路径使用。
//
// 批量是硬要求：注入路径按记忆条数逐条查库时，10 条记忆就是 10 次往返，
// 直接把 hook 的延迟预算吃光。返回的 map 对每个请求 id 都有键（无证据为空切片），
// 调用方不需要再判 nil。
func (s *Store) EvidenceLinksBatch(memoryIDs []string) (map[string][]commit.Link, error) {
	out := make(map[string][]commit.Link, len(memoryIDs))
	uniq := make([]string, 0, len(memoryIDs))
	seen := map[string]bool{}
	for _, id := range memoryIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
		out[id] = []commit.Link{}
	}
	if len(uniq) == 0 {
		return out, nil
	}
	got, err := s.evidenceLinksQuery(uniq)
	if err != nil {
		return nil, err
	}
	for id, links := range got {
		out[id] = links
	}
	return out, nil
}

func (s *Store) evidenceLinksQuery(memoryIDs []string) (map[string][]commit.Link, error) {
	out := make(map[string][]commit.Link, len(memoryIDs))
	const chunk = 200
	for start := 0; start < len(memoryIDs); start += chunk {
		end := start + chunk
		if end > len(memoryIDs) {
			end = len(memoryIDs)
		}
		ids := memoryIDs[start:end]
		ph := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			ph[i] = "?"
			args[i] = id
		}
		rows, err := s.db.Query(`SELECT memory_id, event_uid, archive_hash, raw_locator,
				grade, seq, occurred_at_ns
			FROM memory_evidence WHERE memory_id IN (`+strings.Join(ph, ",")+`)
			ORDER BY memory_id, occurred_at_ns, seq, evidence_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var memoryID, grade string
			var l commit.Link
			if err := rows.Scan(&memoryID, &l.EventUID, &l.ArchiveHash, &l.RawLocator,
				&grade, &l.Seq, &l.OccurredAtNS); err != nil {
				rows.Close()
				return nil, err
			}
			l.MemoryID = memoryID
			l.Grade = commit.Grade(grade)
			out[memoryID] = append(out[memoryID], l)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// commitProvenance 生成 memories.provenance 的非空 JSON。
//
// 证据存在时写紧凑证据链；证据为空（legacy 候选）时写候选来源，仍然不伪造
// event_uid/archive_hash —— 只陈述「这条记忆从哪个候选来」。
func commitProvenance(rows []candidateEvidenceRow, candidateID string) string {
	preview := (commit.Compact{}).CompactProvenance(evidenceLinks(rows, ""))
	if strings.TrimSpace(preview) == "" {
		preview = "candidate:" + shortRef(candidateID)
	}
	b, err := json.Marshal(map[string]string{"source": "commit", "preview": preview})
	if err != nil {
		return `{"source":"commit","preview":"candidate"}`
	}
	return string(b)
}

func shortRef(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= 8 {
		return s
	}
	return string(r[:8])
}
