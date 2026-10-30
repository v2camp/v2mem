package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// 观测缓冲与 memories 物理同库、逻辑隔离。
//
// hooks 只许走 AppendObservation。这里没有通往 memories 的写入。
// 默认检索只查 fts_mem。痕迹要显式 layer=observation 或 both 才走 fts_obs。
// 候选仍然不进任何检索。

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
	idx := observationIndexText(summary)

	res, err := s.db.Exec(`INSERT OR IGNORE INTO observations(
		id, session_id, project, harness, event, tool_name, summary, summary_idx, raw_ref,
		content_hash, created_at, meta_json
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, in.SessionID, in.Project, in.Harness, event, in.ToolName, summary, idx, in.RawRef,
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

// observationIndexText 是痕迹的可检索形态。没有可索引字符时放一个占位，
// 避免空串被每次打开都当成「还没回填」。
func observationIndexText(summary string) string {
	idx := indexText(summary)
	if idx == "" {
		return "-"
	}
	return idx
}

const (
	obsFTSCreate = `CREATE VIRTUAL TABLE IF NOT EXISTS fts_obs USING fts5(
		summary_idx,
		content = 'observations',
		content_rowid = 'rowid'
	)`
	obsTriggerAI = `CREATE TRIGGER IF NOT EXISTS observations_ai AFTER INSERT ON observations BEGIN
		INSERT INTO fts_obs(rowid, summary_idx) VALUES (new.rowid, new.summary_idx);
	END`
	obsTriggerAD = `CREATE TRIGGER IF NOT EXISTS observations_ad AFTER DELETE ON observations BEGIN
		INSERT INTO fts_obs(fts_obs, rowid, summary_idx) VALUES ('delete', old.rowid, old.summary_idx);
	END`
	obsTriggerAU = `CREATE TRIGGER IF NOT EXISTS observations_au AFTER UPDATE ON observations BEGIN
		INSERT INTO fts_obs(fts_obs, rowid, summary_idx) VALUES ('delete', old.rowid, old.summary_idx);
		INSERT INTO fts_obs(rowid, summary_idx) VALUES (new.rowid, new.summary_idx);
	END`
)

// ensureObservationSearch 给旧库补 summary_idx，并让 fts_obs 与行数对齐。
//
// 触发器要等列存在再建：旧表没有 summary_idx 时，schema 里的触发器会建失败。
// 回填时先摘掉 UPDATE 触发器，避免对一条还没进索引的旧行发 delete。
func ensureObservationSearch(db *sql.DB) error {
	if err := ensureColumn(db, "observations", "summary_idx", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	for _, stmt := range []string{obsFTSCreate, obsTriggerAI, obsTriggerAD, obsTriggerAU} {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	var empty, obsN, ftsN int
	if err := db.QueryRow(`SELECT COUNT(*) FROM observations WHERE summary_idx = '' AND summary != ''`).Scan(&empty); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&obsN); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM fts_obs`).Scan(&ftsN); err != nil {
		return err
	}
	if empty == 0 && obsN == ftsN {
		return nil
	}
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS observations_au`); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT id, summary FROM observations WHERE summary_idx = '' AND summary != ''`)
	if err != nil {
		return err
	}
	type pair struct{ id, summary string }
	var pending []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.summary); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, p := range pending {
		if _, err := db.Exec(`UPDATE observations SET summary_idx = ? WHERE id = ?`, observationIndexText(p.summary), p.id); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`INSERT INTO fts_obs(fts_obs) VALUES('rebuild')`); err != nil {
		return err
	}
	_, err = db.Exec(obsTriggerAU)
	return err
}

// searchObservations 检索会话痕迹。不做 MinHash，不读 candidates。
// kind 与 tag 只属于已定记忆；在这一层出现就是调用方用错了层。
func (s *Store) searchObservations(q SearchQuery) ([]Hit, error) {
	if q.Kind != "" || len(q.Tags) > 0 {
		return nil, errors.New("kind 与 tag 只过滤已定记忆；查痕迹请用 layer=observation 并不要带 kind/tag，或改用 layer=both")
	}
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" {
		return nil, errors.New("查询为空")
	}
	q.Query = s.ExpandQuery(q.Query, q.Project)
	expr := queryExpr(q.Query)
	if expr == "" {
		return nil, errors.New("查询不含可检索内容（需至少含一个汉字或字母数字）")
	}
	if q.Limit <= 0 {
		q.Limit = 10
	}
	hits, err := s.obsSearchWith(expr, q)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 && !q.NoBroadFallback {
		if broad := queryExprBroad(q.Query); broad != "" && broad != expr {
			hits, err = s.obsSearchWith("("+broad+")", q)
			if err != nil {
				return nil, err
			}
		}
	}
	for i := range hits {
		hits[i].Layer = "observation"
	}
	return hits, nil
}

func (s *Store) obsSearchWith(expr string, q SearchQuery) ([]Hit, error) {
	where, args := obsFilter(q)
	args = append([]any{expr}, args...)
	args = append(args, q.Limit)
	sqlWhere := "fts_obs MATCH ?"
	if where != "" {
		sqlWhere += " AND " + where
	}
	rows, err := s.db.Query(
		`SELECT o.id, o.summary, o.event, o.tool_name, o.project, o.created_at, bm25(fts_obs) AS rank
		   FROM fts_obs
		   JOIN observations o ON o.rowid = fts_obs.rowid
		  WHERE `+sqlWhere+`
		  ORDER BY rank ASC, o.created_at DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		var event, tool string
		if err := rows.Scan(&h.ID, &h.Content, &event, &tool, &h.Project, &h.UpdatedAt, &h.Rank); err != nil {
			return nil, err
		}
		h.Kind = tool
		if h.Kind == "" {
			h.Kind = event
		}
		h.Layer = "observation"
		out = append(out, h)
	}
	return out, rows.Err()
}

func obsFilter(q SearchQuery) (string, []any) {
	var where []string
	var args []any
	switch q.Scope {
	case "current":
		where = append(where, "(o.project = ? OR o.project = '')")
		args = append(args, q.Project)
	case "global":
		where = append(where, "o.project = ''")
	case "all":
	default:
		if q.Project != "" {
			where = append(where, "o.project = ?")
			args = append(args, q.Project)
		}
	}
	return strings.Join(where, " AND "), args
}
