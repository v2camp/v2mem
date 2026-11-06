package store

import (
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
// 只有 AcceptCandidate 才把正文写进 memories，并记一条 derived_from 边。

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
	Expired int `json:"expired"`
	Deleted int `json:"deleted"`
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
		salience, obs_ids_json, created_at
	) VALUES (?,?,?,?,?,?, 'pending', ?,?,?,?,?)`,
		id, in.SessionID, in.Project, in.Kind, in.Content, hash, in.Source, in.Channel,
		in.Salience, marshalObsIDs(in.ObsIDs), now)
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
	c, err := s.candidateByID(id)
	if err != nil {
		return nil, err
	}
	return &CandidateResult{Candidate: *c, Created: true}, nil
}

func (s *Store) candidateByID(id string) (*Candidate, error) {
	row := s.db.QueryRow(`SELECT id, session_id, project, kind, content, content_hash,
		status, source, channel, salience, obs_ids_json, created_at, decided_at, memory_id
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
		&c.Status, &c.Source, &c.Channel, &c.Salience, &obsJSON, &c.CreatedAt, &decided, &memory); err != nil {
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
func (s *Store) ListCandidates(status, session string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, session_id, project, kind, content, content_hash,
		status, source, channel, salience, obs_ids_json, created_at, decided_at, memory_id
		FROM candidates WHERE 1=1`
	var args []any
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if session != "" {
		q += ` AND session_id = ?`
		args = append(args, session)
	}
	q += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit)
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

// AcceptCandidate 把 pending 候选写进 memories（source=distill-accept），并记 derived_from。
// 已采纳的再调用是幂等的：只补证据链/派生边/provenance，不重复计数。
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
func (s *Store) GCCandidates(pendingAge, acceptedAge time.Duration) (*CandGCResult, error) {
	if pendingAge <= 0 {
		pendingAge = 14 * 24 * time.Hour
	}
	if acceptedAge <= 0 {
		acceptedAge = 90 * 24 * time.Hour
	}
	now := time.Now()
	res, err := s.db.Exec(
		`UPDATE candidates SET status='expired', decided_at=?
		  WHERE status='pending' AND created_at < ?`,
		now.Unix(), now.Add(-pendingAge).Unix())
	if err != nil {
		return nil, err
	}
	expired, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	res, err = s.db.Exec(
		`DELETE FROM candidates
		  WHERE status='accepted' AND decided_at IS NOT NULL AND decided_at < ?`,
		now.Add(-acceptedAge).Unix())
	if err != nil {
		return nil, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	return &CandGCResult{Expired: int(expired), Deleted: int(deleted)}, nil
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

// SessionsNeedingDistill 返回「有观测、且比上次成功蒸馏更新」的会话。
// empty 与 ok 一样算成功：空结果不会因为重跑而变出事实。
func (s *Store) SessionsNeedingDistill(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
		SELECT o.session_id
		  FROM observations o
		  LEFT JOIN (
		    SELECT session_id, MAX(finished_at) AS fin
		      FROM distill_runs WHERE status IN ('ok','empty') GROUP BY session_id
		  ) d ON d.session_id = o.session_id
		 WHERE o.session_id <> ''
		 GROUP BY o.session_id
		HAVING MAX(o.created_at) > IFNULL(d.fin, 0)
		 ORDER BY MAX(o.created_at) DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ObservationsForDistill 取某会话的观测。sinceUnix>0 时只取该时刻之后的。
func (s *Store) ObservationsForDistill(sessionID string, sinceUnix int64, limit int) ([]Observation, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + observationColumns + ` FROM observations WHERE 1=1`
	var args []any
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	if sinceUnix > 0 {
		q += ` AND created_at >= ?`
		args = append(args, sinceUnix)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		o, err := scanObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
