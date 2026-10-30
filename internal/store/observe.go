package store

import (
	"strings"
	"time"
)

// 观测缓冲与 memories 物理同库、逻辑隔离。
//
// hooks 只许走 AppendObservation。这里没有通往 memories 的写入，
// mem search 只查 fts_mem，因此观测默认不进永久记忆、也不进检索。

// Observation 是一条会话观测。Summary 是有界摘要，不是工具 stdout 全文。
type Observation struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	Harness   string `json:"harness"`
	Event     string `json:"event"`
	ToolName  string `json:"tool_name"`
	Summary   string `json:"summary"`
	RawRef    string `json:"raw_ref"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
	MetaJSON  string `json:"meta_json"`
}

// ObsInput 是追加一条观测的参数。MetaJSON 空则落成 {}。
type ObsInput struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	Harness   string `json:"harness"`
	Event     string `json:"event"`
	ToolName  string `json:"tool_name"`
	Summary   string `json:"summary"`
	RawRef    string `json:"raw_ref"`
	MetaJSON  string `json:"meta_json"`
}

// ObsResult 描述追加结果。Created=false 表示命中 (session_id, content_hash) 去重。
type ObsResult struct {
	ID      string `json:"id"`
	Created bool   `json:"created"`
	Hash    string `json:"hash"`
}

// ObsGCResult 是一次观测回收的结果。不触及 memories。
type ObsGCResult struct {
	Deleted int `json:"deleted"`
	Kept    int `json:"kept"`
}

// obsHash 把「事件 + 工具 + 摘要」收成去重键。
// 同一会话里相同观测只留一条；不同工具调用摘要不同，不会被误并。
func obsHash(event, tool, summary string) string {
	return HashOf(event + "\n" + tool + "\n" + summary)
}

// AppendObservation 追加一条观测。相同 (session, hash) 不双插。
func (s *Store) AppendObservation(in ObsInput) (*ObsResult, error) {
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = strings.TrimSpace(in.Event)
	}
	if summary == "" {
		summary = "observe"
	}
	meta := strings.TrimSpace(in.MetaJSON)
	if meta == "" {
		meta = "{}"
	}
	event := strings.TrimSpace(in.Event)
	hash := obsHash(event, in.ToolName, summary)
	id := newID()
	now := time.Now().Unix()

	res, err := s.db.Exec(`INSERT OR IGNORE INTO observations(
		id, session_id, project, harness, event, tool_name, summary, raw_ref,
		content_hash, created_at, meta_json
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		id, in.SessionID, in.Project, in.Harness, event, in.ToolName, summary, in.RawRef,
		hash, now, meta)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return &ObsResult{ID: id, Created: true, Hash: hash}, nil
	}
	var existing string
	err = s.db.QueryRow(
		`SELECT id FROM observations WHERE session_id = ? AND content_hash = ?`,
		in.SessionID, hash).Scan(&existing)
	if err != nil {
		return nil, err
	}
	return &ObsResult{ID: existing, Created: false, Hash: hash}, nil
}

// ListObservations 按时间顺序列出观测。sessionID 为空则不限会话。
func (s *Store) ListObservations(sessionID string, limit int) ([]Observation, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, session_id, project, harness, event, tool_name, summary, raw_ref,
	             content_hash, created_at, meta_json
	        FROM observations`
	var args []any
	if sessionID != "" {
		q += ` WHERE session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Observation
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.ID, &o.SessionID, &o.Project, &o.Harness, &o.Event,
			&o.ToolName, &o.Summary, &o.RawRef, &o.Hash, &o.CreatedAt, &o.MetaJSON); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GCObservations 删掉超过 maxAge 的观测，再把条数压到 maxKeep。
//
// 只动 observations。memories / candidates 不在这条路径上。
// maxAge<=0 时按 30 天；maxKeep<=0 时按 5 万条。
func (s *Store) GCObservations(maxAge time.Duration, maxKeep int) (*ObsGCResult, error) {
	if maxAge <= 0 {
		maxAge = 30 * 24 * time.Hour
	}
	if maxKeep <= 0 {
		maxKeep = 50000
	}
	cutoff := time.Now().Add(-maxAge).Unix()
	res, err := s.db.Exec(`DELETE FROM observations WHERE created_at < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&n); err != nil {
		return nil, err
	}
	if n > maxKeep {
		extra := n - maxKeep
		res, err = s.db.Exec(`DELETE FROM observations WHERE id IN (
			SELECT id FROM observations ORDER BY created_at ASC, id ASC LIMIT ?)`, extra)
		if err != nil {
			return nil, err
		}
		more, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		deleted += more
		n -= int(more)
	}
	return &ObsGCResult{Deleted: int(deleted), Kept: n}, nil
}
