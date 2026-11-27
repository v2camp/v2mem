package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/commit"
	"modernc.org/sqlite"
)

// 候选是蒸馏 / 导入的隔离区。默认不进 fts_mem，mem search 看不见。
// 只有 AcceptCandidate 才把正文写进 memories，且记忆沿用候选 id（派生关系由 id 同值表达）。

// ErrAlreadyDistilled 表示这个会话窗口已经成功蒸馏过。
var ErrAlreadyDistilled = errors.New("该会话窗口已蒸馏")

// Candidate 是隔离区里的一条待采纳事实。
type Candidate struct {
	ID        string   `json:"id"`
	SessionID string   `json:"session_id"`
	Project   string   `json:"project"`
	Kind      string   `json:"kind"`
	Content   string   `json:"content"`
	Hash      string   `json:"hash"`
	Status    string   `json:"status"`
	Source    string   `json:"source"`
	Channel   string   `json:"channel,omitempty"`
	Salience  float64  `json:"salience"`
	ObsIDs    []string `json:"obs_ids"`
	CreatedAt int64    `json:"created_at"`
	DecidedAt int64    `json:"decided_at,omitempty"`
	MemoryID  string   `json:"memory_id,omitempty"`
	// AttributedTo 是事实的原始来源，不是写候选的人：user|assistant|空串（旧行）。
	AttributedTo string `json:"attributed_to,omitempty"`
	// Scope 只给准入判定用（global / cross_project 一律不许自动 accept）。
	// 它是 WHERE 之外的额外列，不影响 ListCandidates 的空值语义。
	Scope string `json:"scope,omitempty"`
}

// normalizeAttributedTo 收敛写入值，避免第三类来源悄悄进库。
func normalizeAttributedTo(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "user", "assistant":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return ""
	}
}

// CandidateInput 是写入一条候选的参数。
type CandidateInput struct {
	SessionID string
	Project   string
	Kind      string
	Content   string
	Source    string
	Channel   string
	Salience  float64
	ObsIDs    []string
	// AttributedTo：规则蒸馏按观测事件类型推导，见 internal/distill.SpeakerForEvent。
	AttributedTo string
}

// CandidateResult 描述写入结果。Created=false 表示命中同会话同内容的去重。
type CandidateResult struct {
	Candidate Candidate `json:"candidate"`
	Created   bool      `json:"created"`
}

// AcceptResult 是一次采纳的结果。
type AcceptResult struct {
	Candidate Candidate `json:"candidate"`
	MemoryID  string    `json:"memory_id"`
	Hash      string    `json:"hash"`
	Already   bool      `json:"already"`
}

// CandGCResult 是候选回收的结果。不触及 memories。
type CandGCResult struct {
	Expired         int `json:"expired"`
	Deleted         int `json:"deleted"`
	DeletedExpired  int `json:"deleted_expired"`
	DeletedRejected int `json:"deleted_rejected"`
}

// DistillRun 是一次蒸馏的审计行。
type DistillRun struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	Provider   string `json:"provider"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	CandCount  int    `json:"cand_count"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
	WindowKey  string `json:"window_key"`
}

func marshalObsIDs(ids []string) string {
	if ids == nil {
		ids = []string{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalObsIDs(s string) []string {
	var ids []string
	if err := json.Unmarshal([]byte(s), &ids); err != nil || ids == nil {
		return []string{}
	}
	return ids
}

// isUniqueViolation 用驱动的结构化错误码判断唯一约束冲突。
//
// 为什么不用 strings.Contains(err.Error(), "UNIQUE")：错误文案不是 API。
// SQLite 升级、本地化、包装层次变化都会让这句话对不上；一旦对不上，
// 「重复插入（本应去重）」会被当成真失败上报，蒸馏的成功计数就变成了谎话。
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	const (
		sqliteConstraint       = 19   // SQLITE_CONSTRAINT：驱动未给扩展码时的退化情形
		sqliteConstraintUnique = 2067 // SQLITE_CONSTRAINT_UNIQUE
	)
	code := se.Code()
	return code == sqliteConstraintUnique || code == sqliteConstraint
}

// InsertCandidate 写入一条 pending 候选。同会话同内容已有 pending/accepted 时不双插。
func (s *Store) InsertCandidate(in CandidateInput) (*CandidateResult, error) {
	in.Content = strings.TrimSpace(in.Content)
	if in.Content == "" {
		return nil, errors.New("候选内容为空")
	}
	switch in.Kind {
	case "preference", "decision", "pitfall", "task", "fact":
	default:
		return nil, fmt.Errorf("候选 kind 无效: %q", in.Kind)
	}
	if in.Source == "" {
		in.Source = "distill"
	}
	ch, err := DefaultChannel(in.Channel, in.Source)
	if err != nil {
		return nil, err
	}
	in.Channel = ch
	if in.Salience <= 0 || in.Salience > 1 {
		in.Salience = 0.5
	}
	hash := HashOf(in.Content)
	var existing string
	err = s.db.QueryRow(
		`SELECT id FROM candidates
		  WHERE session_id = ? AND content_hash = ? AND status IN ('pending','accepted')`,
		in.SessionID, hash).Scan(&existing)
	switch {
	case err == nil:
		c, gerr := s.candidateByID(existing)
		if gerr != nil {
			return nil, gerr
		}
		return &CandidateResult{Candidate: *c, Created: false}, nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, err
	}

	id := newID()
	now := time.Now().Unix()
	_, err = s.db.Exec(`INSERT INTO candidates(
		id, session_id, project, kind, content, content_hash, status, source, channel,
		salience, obs_ids_json, created_at, attributed_to
	) VALUES (?,?,?,?,?,?, 'pending', ?,?,?,?,?,?)`,
		id, in.SessionID, in.Project, in.Kind, in.Content, hash, in.Source, in.Channel,
		in.Salience, marshalObsIDs(in.ObsIDs), now, normalizeAttributedTo(in.AttributedTo))
	if err != nil {
		if isUniqueViolation(err) {
			// 并发插入命中部分唯一索引（status IN pending/accepted）：按去重处理。
			// 查不到对应行说明冲突来自别处（比如触发器 ABORT），原样把插入错误报上去，
			// 不能为了「去重」把真实故障吞成 sql.ErrNoRows。
			var found string
			if qerr := s.db.QueryRow(
				`SELECT id FROM candidates
				  WHERE session_id = ? AND content_hash = ? AND status IN ('pending','accepted')
				  ORDER BY created_at DESC, id DESC LIMIT 1`,
				in.SessionID, hash).Scan(&found); qerr != nil {
				return nil, err
			}
			c, gerr := s.candidateByID(found)
			if gerr != nil {
				return nil, gerr
			}
			return &CandidateResult{Candidate: *c, Created: false}, nil
		}
		return nil, err
	}
	// 证据链统一落 candidate_evidence：obs_ids_json 只是快照，采纳侧只读表。
	if _, err := s.candidateEvidenceFromObs(context.Background(), s.db, id, in.ObsIDs, now); err != nil {
		return nil, err
	}
	c, err := s.candidateByID(id)
	if err != nil {
		return nil, err
	}
	return &CandidateResult{Candidate: *c, Created: true}, nil
}

// candidateEvidenceFromObs 把 obs_ids_json 里的观测展开成 candidate_evidence 行。
//
// 为什么需要：候选证据此前有两个载体——规则蒸馏路径（InsertCandidate）只写
// obs_ids_json，proposal 路径才写 candidate_evidence。采纳侧只读表，于是规则路径
// 的候选采纳后 memory_evidence 为空、provenance 退化成 candidate:<短 id>
// （2026-11-27 库内唯一一条记忆就是这个状态，溯源断在 preview）。
//
// 这里统一口径：落候选时就把 obs_ids 展开成表行，obs_ids_json 退化为快照；
// 老候选在采纳前由 commitAcceptView 调同一函数自愈。
func (s *Store) candidateEvidenceFromObs(ctx context.Context, q sqlQuerier, candidateID string, obsIDs []string, now int64) (int, error) {
	added := 0
	for _, obsID := range obsIDs {
		if strings.TrimSpace(obsID) == "" {
			continue
		}
		// evidence_id 用观测主键（ev_<32hex>），与 proposal 路径的 CandidateEvidence
		// 同一命名空间；观测不存在时 SELECT 不产出行，不会留下无来源的引用。
		res, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO candidate_evidence(
			candidate_id, evidence_id, event_uid, source_id, source_seq,
			archive_hash, raw_record_hash, evidence_role, quote_hash, created_at
		)
		SELECT ?, o.id, o.event_uid, o.source_id, o.source_seq,
		       o.archive_hash, o.raw_record_hash, 'direct', '', ?
		FROM observations o WHERE o.id = ?`, candidateID, now, obsID)
		if err != nil {
			return added, err
		}
		if n, err := res.RowsAffected(); err == nil {
			added += int(n)
		}
	}
	return added, nil
}

func (s *Store) candidateByID(id string) (*Candidate, error) {
	row := s.db.QueryRow(`SELECT id, session_id, project, kind, content, content_hash,
		status, source, channel, salience, obs_ids_json, 		created_at, decided_at, memory_id,
		attributed_to, scope
		FROM candidates WHERE id = ?`, id)
	return scanCandidate(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCandidate(row rowScanner) (*Candidate, error) {
	var c Candidate
	var obsJSON string
	var decided sql.NullInt64
	var memory sql.NullString
	if err := row.Scan(&c.ID, &c.SessionID, &c.Project, &c.Kind, &c.Content, &c.Hash,
		&c.Status, &c.Source, &c.Channel, &c.Salience, &obsJSON, &c.CreatedAt, &decided, &memory,
		&c.AttributedTo, &c.Scope); err != nil {
		return nil, err
	}
	c.ObsIDs = unmarshalObsIDs(obsJSON)
	if decided.Valid {
		c.DecidedAt = decided.Int64
	}
	if memory.Valid {
		c.MemoryID = memory.String
	}
	return &c, nil
}

// ListCandidates 按状态列出候选。status 为空则不限。session 为空则不限会话。
//
// 保留旧签名：全仓 20 处调用点（含 internal/proposal 的测试）都依赖它，
// 按工程过滤请改用 ListCandidatesByProject。
func (s *Store) ListCandidates(status, session string, limit int) ([]Candidate, error) {
	return s.ListCandidatesByProject(status, session, "", limit)
}

// PendingCandidateRef 是「只够裁决用」的最小投影：ID + revision。
//
// maintain 自动裁决只需要 CAS 前提，正文与证据由 Commit 路径自己读；
// 返回整行 Candidate 会把不需要的列也扫一遍，且 revision 不在 Candidate 里。
type PendingCandidateRef struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

// ListPendingCandidatesByBundles 取指定 Bundle 下仍在 pending 的候选，供 maintain
// 只裁决「本轮处理过的窗口」。
//
// 空列表返回空集而不是全表：一轮没处理任何窗口时，语义是「无候选可裁」，
// 误退化成全表会让一次空跑把历史积压全部裁决掉。
func (s *Store) ListPendingCandidatesByBundles(bundleIDs []string, limit int) ([]PendingCandidateRef, error) {
	if len(bundleIDs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(bundleIDs)), ",")
	args := make([]any, 0, len(bundleIDs)+1)
	for _, id := range bundleIDs {
		args = append(args, id)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT id, revision FROM candidates
		  WHERE status = 'pending' AND bundle_id IN (`+placeholders+`)
		  ORDER BY created_at ASC, id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingCandidateRef
	for rows.Next() {
		var ref PendingCandidateRef
		if err := rows.Scan(&ref.ID, &ref.Revision); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// candidateSelect 是候选行列表的统一读法，避免两处查询的列顺序漂移。
const candidateSelect = `SELECT id, session_id, project, kind, content, content_hash,
	status, source, channel, salience, obs_ids_json, created_at, decided_at, memory_id,
	attributed_to, scope
	FROM candidates`

func (s *Store) queryCandidates(q string, args ...any) ([]Candidate, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ListCandidatesByProject 在 ListCandidates 之上加工程过滤。
// project 为空表示**不过滤**（与 status / session 的空值语义一致）；
// 想只取全局候选请显式传 project="" 并配合 status，不要依赖空值。
func (s *Store) ListCandidatesByProject(status, session, project string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = 50
	}
	q := candidateSelect + ` WHERE 1=1`
	var args []any
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if session != "" {
		q += ` AND session_id = ?`
		args = append(args, session)
	}
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit)
	return s.queryCandidates(q, args...)
}

// ListUnjudgedCandidates 取「判定器还没看过」的 pending 候选，供 judge 批量判定。
//
// 影子期不改 status，所以不能按「最旧的 pending」取：那会每轮重判同一批，
// 既重复付费，也让后面的候选永远轮不到。judge_verdict=” 是唯一的游标。
func (s *Store) ListUnjudgedCandidates(project string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = 50
	}
	q := candidateSelect + ` WHERE status = 'pending' AND judge_verdict = ''`
	var args []any
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit)
	return s.queryCandidates(q, args...)
}

// CountPendingByProject 统计指定工程仍在 pending 的候选数，供 SessionStart
// 注入「本工程还有几条没裁决」的提示用。
//
// 与 ListCandidatesByProject 的空值语义**不同**：这里的 project 是 WHERE 的等值条件，
// 空串精确匹配全局候选（project 列默认值），不是「不过滤」。调用方必须在拿到
// 已解析的工程名后再调用；未识别工程时不要调用本方法。
//
// 只读、单次 COUNT，命中 ix_cand_status 的 status 前导列。
func (s *Store) CountPendingByProject(project string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM candidates WHERE status = 'pending' AND project = ?`,
		project).Scan(&n)
	return n, err
}

// ResolveCandidate 按 id 或前缀定位一条候选。
func (s *Store) ResolveCandidate(idOrPrefix string) (*Candidate, error) {
	idOrPrefix = strings.TrimSpace(idOrPrefix)
	if idOrPrefix == "" {
		return nil, errors.New("缺少候选 id")
	}
	rows, err := s.db.Query(`SELECT id FROM candidates WHERE id LIKE ?`, idOrPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(ids) {
	case 0:
		return nil, fmt.Errorf("未找到候选: %s", idOrPrefix)
	case 1:
		return s.candidateByID(ids[0])
	default:
		return nil, fmt.Errorf("前缀 %q 歧义，匹配到 %d 条，请使用更长的 ID", idOrPrefix, len(ids))
	}
}

// AcceptCandidate 把 pending 候选写进 memories（source=distill-accept），记忆沿用候选 id。
// 已采纳的再调用是幂等的：只补证据链/provenance，不重复计数。
// 拒绝、过期或 revision 已变化的候选不能采纳。
//
// 实际提交逻辑在 commit.go：这里保留旧签名，供未感知 revision 的调用方继续使用。
// 仍然显式读出 revision 再传下去：同一进程里的并发调用会拿到同一个值，
// 由 CAS 判定唯一赢家；若传 -1（事务内自读），双方都会成功，CAS 就失效了。
func (s *Store) AcceptCandidate(idOrPrefix string) (*AcceptResult, error) {
	view, err := s.CommitCandidate(idOrPrefix)
	if err != nil {
		return nil, err
	}
	res, err := s.CommitAccept(idOrPrefix, view.Revision, commit.ActorHuman)
	if err != nil {
		return nil, err
	}
	return &AcceptResult{
		Candidate: res.Candidate, MemoryID: res.MemoryID,
		Hash: res.Hash, Already: res.Already,
	}, nil
}

// RejectCandidate 把 pending 候选标成 rejected。不写 memories。
// 已拒绝的重复调用幂等；revision 由 CommitReject 用 CAS 推进。
func (s *Store) RejectCandidate(idOrPrefix string) (*Candidate, error) {
	view, err := s.CommitCandidate(idOrPrefix)
	if err != nil {
		return nil, err
	}
	res, err := s.CommitReject(idOrPrefix, view.Revision, commit.ActorHuman, "")
	if err != nil {
		return nil, err
	}
	return &res.Candidate, nil
}

// GCCandidates 把超过 pendingAge 的 pending 标为 expired，并删掉超过 acceptedAge 的已采纳行。
//
// 不删 memories。默认 pending 14 天、accepted 保留 90 天。
// 删行与清边同事务：edges.dst 没有外键（relaxEdgeDstFK 拆掉了 dst→memories），
// 候选删后 dst 指向它的边就成孤边——对端回收在这里一并做掉。
func (s *Store) GCCandidates(pendingAge, acceptedAge time.Duration) (*CandGCResult, error) {
	if pendingAge <= 0 {
		pendingAge = 14 * 24 * time.Hour
	}
	if acceptedAge <= 0 {
		acceptedAge = 90 * 24 * time.Hour
	}
	now := time.Now()
	// 「标过期」与两条删除必须同事务：此前 UPDATE 独立提交，DELETE 失败时
	// 会留下「已标 expired 但没删」的半提交，重跑也不会再把它当 pending。
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	upd, err := tx.Exec(
		`UPDATE candidates SET status='expired', decided_at=?
		  WHERE status='pending' AND created_at < ?`,
		now.Unix(), now.Add(-pendingAge).Unix())
	if err != nil {
		return nil, err
	}
	expired, err := upd.RowsAffected()
	if err != nil {
		return nil, err
	}
	deleted, err := deleteCandidatesWithEdges(tx,
		`status='accepted' AND decided_at IS NOT NULL AND decided_at < ?`,
		now.Add(-acceptedAge).Unix())
	if err != nil {
		return nil, err
	}
	// 过期行也要有出口：它们从未被判过，不参与一致率，留着只会让候选表只增不减。
	// 与 accepted 用同一个保留期，避免两套「多久算老」的口径。
	deletedExpired, err := deleteCandidatesWithEdges(tx,
		`status='expired' AND decided_at IS NOT NULL AND decided_at < ?`,
		now.Add(-acceptedAge).Unix())
	if err != nil {
		return nil, err
	}
	// S13（§2.2 终态保留表）：rejected 带理由码与证据引用保留到到期，再随候选 GC
	// 一起回收；与 accepted/expired 共用同一个 90 天窗口，避免两套「多久算老」。
	// quarantined / withdrawn 不在任何删除条件里：前者等人工显式重放，后者供按版本
	// 统计提示词质量，一律不自动删。
	deletedRejected, err := deleteCandidatesWithEdges(tx,
		`status='rejected' AND decided_at IS NOT NULL AND decided_at < ?`,
		now.Add(-acceptedAge).Unix())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &CandGCResult{
		Expired: int(expired), Deleted: int(deleted), DeletedExpired: int(deletedExpired),
		DeletedRejected: int(deletedRejected),
	}, nil
}

// deleteCandidatesWithEdges 先清 edges.dst 与候选证据引用、再删候选行（同事务）。
// where 是硬编码的删除条件片段，arg 是它的时间参数；顺序不能反——
// 候选行先没了，子查询就再也找不到要清的边。
//
// candidate_evidence 虽有 ON DELETE CASCADE，但显式清有两个理由（与 GCAudit 同口径）：
// 不依赖连接上的 foreign_keys 开关，且「删候选」与「清引用」在同一事务里可验证回滚。
// memory_evidence 属于记忆生命周期（memory→evidence），不随候选删除：rejected /
// accepted 候选都不拥有记忆的证据链，动它会把记忆的溯源目标挖空。
func deleteCandidatesWithEdges(tx *sql.Tx, where string, arg any) (int64, error) {
	if _, err := tx.Exec(
		`DELETE FROM edges WHERE dst IN (SELECT id FROM candidates WHERE `+where+`)`, arg); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`DELETE FROM candidate_evidence WHERE candidate_id IN (SELECT id FROM candidates WHERE `+where+`)`, arg); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM candidates WHERE `+where, arg)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DistillWindowDone 报告这个会话的观测窗口是否已经成功蒸馏过。
//
// empty 也算「已处理」：模型明确说了没有事实，重跑同一窗口只会再烧一次推理，
// 结果仍然是空。所以空结果同样要占住窗口，否则空会话会被无限重复蒸馏。
func (s *Store) DistillWindowDone(sessionID, windowKey string) (bool, error) {
	if sessionID == "" || windowKey == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM distill_runs
		  WHERE session_id = ? AND window_key = ? AND status IN ('ok','empty')`,
		sessionID, windowKey).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SaveDistillRun 记下一次蒸馏。成功且窗口键冲突时返回 ErrAlreadyDistilled。
func (s *Store) SaveDistillRun(r DistillRun) error {
	if r.ID == "" {
		r.ID = newID()
	}
	now := time.Now().Unix()
	if r.StartedAt == 0 {
		r.StartedAt = now
	}
	if r.FinishedAt == 0 {
		r.FinishedAt = now
	}
	// 只有「拿到了确定结论」的运行才占窗口：ok 和 empty 都是确定结论，
	// fail / partial / skipped 都留空，让下轮还能重试。
	if r.Status != "ok" && r.Status != "empty" {
		r.WindowKey = ""
	}
	for i := 0; i < 3; i++ {
		_, err := s.db.Exec(`INSERT INTO distill_runs(
			id, session_id, provider, status, error, cand_count, started_at, finished_at, window_key
		) VALUES (?,?,?,?,?,?,?,?,?)`,
			r.ID, r.SessionID, r.Provider, r.Status, r.Error, r.CandCount,
			r.StartedAt, r.FinishedAt, r.WindowKey)
		if err == nil {
			return nil
		}
		msg := err.Error()
		if strings.Contains(msg, "window_key") {
			return ErrAlreadyDistilled
		}
		if strings.Contains(msg, "started_at") {
			r.StartedAt++
			r.FinishedAt = r.StartedAt
			r.ID = newID()
			continue
		}
		return err
	}
	return ErrAlreadyDistilled
}

// SessionsNeedingDistill / ObservationsForDistill（旧规则蒸馏双轨的取数口）已随
// S12 命令面收编（design-data-lifecycle §12.1）退役：自动写侧只剩 proposal 单轨，
// 从证据 Bundle 取数，不再按观测水位挑会话。蒸馏游标（distill_runs）语义由
// DistillWindowDone / SaveDistillRun 保留，供审计 GC 与 R12 判据继续使用。
