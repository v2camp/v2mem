package store

// 本文件是 T4 读侧供给的持久化面：读侧轨迹（read_traces / read_trace_items）
// 与采纳反馈。表结构已在 schema.sql 中（read_traces/read_trace_items 早于本文件存在），
// 这里只做写入与读取，不改动写入路径的记忆语义。
//
// 三条纪律：
//  1. **读侧不自动 touch**：Search / List / 注入都不写 memories 的命中字段。
//     只有显式反馈（adopted）才更新 adopted_count / last_adopted_at。
//  2. **反馈按 trace item 定位**：同一条记忆可在多次读侧出现，反馈必须落到
//     「哪一次供给」上，否则无法区分「这次采纳了」与「以前采纳过」。
//  3. **批量读取**：证据链/哈希都走批量接口，注入路径不允许按条往返。

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReadTrace 是一次读侧供给的汇总。
type ReadTrace struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	Project     string `json:"project"`
	Harness     string `json:"harness"`
	QueryGate   string `json:"query_gate"` // run|skip
	GateReason  string `json:"gate_reason"`
	RawQuery    string `json:"raw_query"`
	FocusQuery  string `json:"focus_query"`
	Scope       string `json:"scope"`
	TargetLimit int    `json:"target_limit"`
	BudgetChars int    `json:"budget_chars"`
	Overfetch   int    `json:"overfetch"`
	RawHits     int    `json:"raw_hits"`
	Qualified   int    `json:"qualified"`
	Injected    int    `json:"injected"`
	Outcome     string `json:"outcome"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	CreatedAt   int64  `json:"created_at"`
}

// ReadTraceItem 是池中每一条候选的处置记录（含被丢弃的）。
type ReadTraceItem struct {
	TraceID        string `json:"trace_id"`
	Ordinal        int    `json:"ordinal"`
	MemoryID       string `json:"memory_id"`
	ContentHash    string `json:"content_hash"`
	SignalsJSON    string `json:"signals_json"`
	MatchClass     string `json:"match_class"`
	Eligible       bool   `json:"eligible"`
	DropReason     string `json:"drop_reason"`
	InjectionRank  int    `json:"injection_rank"`
	Provenance     string `json:"provenance"`
	Feedback       string `json:"feedback"`
	FeedbackReason string `json:"feedback_reason"`
	FeedbackAt     int64  `json:"feedback_at"`
}

// FeedbackResult 是 mem feedback 的返回体。
type FeedbackResult struct {
	TraceID      string `json:"trace_id"`
	MemoryID     string `json:"memory_id"`
	Feedback     string `json:"feedback"`
	Reason       string `json:"reason,omitempty"`
	AdoptedCount int64  `json:"adopted_count"`
}

// MemoryDetail 是 mem show 需要的完整只读视图。
type MemoryDetail struct {
	ID           string      `json:"id"`
	Content      string      `json:"content"`
	Kind         string      `json:"kind"`
	Project      string      `json:"project"`
	Scope        string      `json:"scope"`
	Salience     float64     `json:"salience"`
	ContentHash  string      `json:"content_hash"`
	CreatedAt    int64       `json:"created_at"`
	UpdatedAt    int64       `json:"updated_at"`
	LastSeenAt   int64       `json:"last_seen_at"`
	ExpiresAt    *int64      `json:"expires_at,omitempty"`
	SupersededBy string      `json:"superseded_by,omitempty"`
	Source       string      `json:"source"`
	Channel      string      `json:"channel"`
	Tool         string      `json:"origin_tool"`
	AccessCount  int64       `json:"access_count"`
	AdoptedCount int64       `json:"adopted_count"`
	LastAdopted  *int64      `json:"last_adopted_at,omitempty"`
	Provenance   *Provenance `json:"provenance,omitempty"`
}

// MemoryTrace 是「这条记忆被哪些读侧供给带出来过」的一行。
type MemoryTrace struct {
	TraceID        string `json:"trace_id"`
	QueryGate      string `json:"query_gate"`
	GateReason     string `json:"gate_reason"`
	RawQuery       string `json:"raw_query"`
	Outcome        string `json:"outcome"`
	Ordinal        int    `json:"ordinal"`
	InjectionRank  int    `json:"injection_rank"`
	Feedback       string `json:"feedback"`
	FeedbackReason string `json:"feedback_reason"`
	FeedbackAt     int64  `json:"feedback_at"`
	CreatedAt      int64  `json:"created_at"`
}

// WriteReadTrace 原子写入一条轨迹及其全部候选条目。
func (s *Store) WriteReadTrace(t ReadTrace, items []ReadTraceItem) (string, error) {
	if strings.TrimSpace(t.ID) == "" {
		t.ID = newID()
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = time.Now().Unix()
	}
	if t.QueryGate == "" {
		t.QueryGate = "run"
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO read_traces
		   (id, session_id, project, harness, query_gate, gate_reason, raw_query, focus_query,
		    scope, target_limit, budget_chars, overfetch, raw_hits, qualified, injected,
		    outcome, elapsed_ms, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.SessionID, t.Project, t.Harness, t.QueryGate, t.GateReason,
		t.RawQuery, t.FocusQuery, t.Scope, t.TargetLimit, t.BudgetChars,
		t.Overfetch, t.RawHits, t.Qualified, t.Injected, t.Outcome, t.ElapsedMS, t.CreatedAt,
	); err != nil {
		return "", fmt.Errorf("写 read_traces 失败: %w", err)
	}

	for i := range items {
		it := items[i]
		if it.Ordinal == 0 {
			it.Ordinal = i + 1
		}
		if strings.TrimSpace(it.SignalsJSON) == "" {
			it.SignalsJSON = "{}"
		}
		if _, err := tx.Exec(
			`INSERT INTO read_trace_items
			   (trace_id, ordinal, memory_id, content_hash, signals_json, match_class,
			    eligible, drop_reason, injection_rank, provenance,
			    feedback, feedback_reason, feedback_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, it.Ordinal, it.MemoryID, it.ContentHash, it.SignalsJSON, it.MatchClass,
			boolInt(it.Eligible), it.DropReason, it.InjectionRank, it.Provenance,
			it.Feedback, it.FeedbackReason, it.FeedbackAt,
		); err != nil {
			return "", fmt.Errorf("写 read_trace_items 失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return t.ID, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetReadTrace 精确读取一条轨迹及其条目。
func (s *Store) GetReadTrace(traceID string) (ReadTrace, []ReadTraceItem, error) {
	var t ReadTrace
	err := s.db.QueryRow(
		`SELECT id, session_id, project, harness, query_gate, gate_reason, raw_query,
		        focus_query, scope, target_limit, budget_chars, overfetch, raw_hits,
		        qualified, injected, outcome, elapsed_ms, created_at
		   FROM read_traces WHERE id = ?`, traceID,
	).Scan(&t.ID, &t.SessionID, &t.Project, &t.Harness, &t.QueryGate, &t.GateReason,
		&t.RawQuery, &t.FocusQuery, &t.Scope, &t.TargetLimit, &t.BudgetChars,
		&t.Overfetch, &t.RawHits, &t.Qualified, &t.Injected, &t.Outcome,
		&t.ElapsedMS, &t.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReadTrace{}, nil, fmt.Errorf("未找到读取轨迹: %s", traceID)
		}
		return ReadTrace{}, nil, err
	}

	rows, err := s.db.Query(
		`SELECT trace_id, ordinal, memory_id, content_hash, signals_json, match_class,
		        eligible, drop_reason, injection_rank, provenance,
		        feedback, feedback_reason, feedback_at
		   FROM read_trace_items WHERE trace_id = ? ORDER BY ordinal`, traceID)
	if err != nil {
		return ReadTrace{}, nil, err
	}
	defer rows.Close()
	var items []ReadTraceItem
	for rows.Next() {
		var it ReadTraceItem
		var eligible int
		if err := rows.Scan(&it.TraceID, &it.Ordinal, &it.MemoryID, &it.ContentHash,
			&it.SignalsJSON, &it.MatchClass, &eligible, &it.DropReason, &it.InjectionRank,
			&it.Provenance, &it.Feedback, &it.FeedbackReason, &it.FeedbackAt); err != nil {
			return ReadTrace{}, nil, err
		}
		it.Eligible = eligible != 0
		items = append(items, it)
	}
	return t, items, rows.Err()
}

// ResolveReadTrace 支持完整 id 或前缀。
func (s *Store) ResolveReadTrace(idOrPrefix string) (string, error) {
	idOrPrefix = strings.TrimSpace(idOrPrefix)
	if idOrPrefix == "" {
		return "", errors.New("trace ID 为空")
	}
	var id string
	err := s.db.QueryRow(`SELECT id FROM read_traces WHERE id = ?`, idOrPrefix).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	rows, err := s.db.Query(`SELECT id FROM read_traces WHERE id LIKE ?`, idOrPrefix+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return "", err
		}
		ids = append(ids, x)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("未找到读取轨迹: %s", idOrPrefix)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("trace 前缀 %q 歧义，匹配到 %d 条，请使用更长的 ID", idOrPrefix, len(ids))
	}
}

// RecentReadTraces 返回最近的轨迹（不含条目），供观测与测试查最近一次 gate 判定。
func (s *Store) RecentReadTraces(limit int) ([]ReadTrace, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(
		`SELECT id, session_id, project, harness, query_gate, gate_reason, raw_query,
		        focus_query, scope, target_limit, budget_chars, overfetch, raw_hits,
		        qualified, injected, outcome, elapsed_ms, created_at
		   FROM read_traces ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReadTrace
	for rows.Next() {
		var t ReadTrace
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Project, &t.Harness, &t.QueryGate,
			&t.GateReason, &t.RawQuery, &t.FocusQuery, &t.Scope, &t.TargetLimit,
			&t.BudgetChars, &t.Overfetch, &t.RawHits, &t.Qualified, &t.Injected,
			&t.Outcome, &t.ElapsedMS, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ContentHashes 批量取 id → content_hash。找不到的 id 不出现在结果里。
func (s *Store) ContentHashes(ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	uniq := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	const batch = 200
	for start := 0; start < len(uniq); start += batch {
		end := start + batch
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			ph[i] = "?"
			args[i] = id
		}
		rows, err := s.db.Query(
			`SELECT id, content_hash FROM memories WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, hash string
			if err := rows.Scan(&id, &hash); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = hash
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// AdoptedSubjectKeys 返回「有过正反馈」的 subject 集合。
//
// subject 用 kind|project 表达：读侧供给要在同 subject 上获得结构支持，
// 需要的是「这一类事被采纳过」而不是某一条具体记忆。project 为空表示全局。
func (s *Store) AdoptedSubjectKeys() (map[string]bool, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT kind, project FROM memories WHERE adopted_count > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var kind, project string
		if err := rows.Scan(&kind, &project); err != nil {
			return nil, err
		}
		out[kind+"|"+project] = true
	}
	return out, rows.Err()
}

// Adopt 是 touch 的兼容语义：记录一次采纳。
//
// 保留旧 touch 的副作用（刷新 last_seen_at、累加 access_count），让衰减/GC
// 不因语义升级而改变；新增的是 adopted_count / last_adopted_at。
// 返回复用后的完整 id 与累计采纳次数。
func (s *Store) Adopt(idOrPrefix string) (string, int64, error) {
	id, err := s.resolveID(idOrPrefix)
	if err != nil {
		return "", 0, err
	}
	now := time.Now().Unix()
	if _, err := s.db.Exec(
		`UPDATE memories
		    SET adopted_count = adopted_count + 1,
		        last_adopted_at = ?,
		        last_seen_at = ?,
		        access_count = access_count + 1
		  WHERE id = ?`, now, now, id,
	); err != nil {
		return "", 0, err
	}
	var n int64
	if err := s.db.QueryRow(`SELECT adopted_count FROM memories WHERE id = ?`, id).Scan(&n); err != nil {
		return "", 0, err
	}
	return id, n, nil
}

// ApplyReadFeedback 给一条轨迹里的某条记忆落反馈。
//
// feedback 只接受 adopted / negative。adopted 是唯一会更新 memories 采纳字段的
// 反馈；negative 只改 read_trace_items（否定一次供给，不代表记忆被删）。
// 同值重复提交是幂等的（不重复计数）；改判必须先撤销，避免静默双计。
func (s *Store) ApplyReadFeedback(traceIDOrPrefix, memoryIDOrPrefix, feedback, reason string) (*FeedbackResult, error) {
	feedback = strings.ToLower(strings.TrimSpace(feedback))
	if feedback != "adopted" && feedback != "negative" {
		return nil, fmt.Errorf("未知反馈 %q（可选 adopted|negative）", feedback)
	}
	traceID, err := s.ResolveReadTrace(traceIDOrPrefix)
	if err != nil {
		return nil, err
	}
	memoryIDOrPrefix = strings.TrimSpace(memoryIDOrPrefix)
	if memoryIDOrPrefix == "" {
		return nil, errors.New("memory ID 为空")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var n int
	var found string
	if err := tx.QueryRow(
		`SELECT COUNT(*), COALESCE(MIN(memory_id), '') FROM read_trace_items
		  WHERE trace_id = ? AND (memory_id = ? OR memory_id LIKE ?)`,
		traceID, memoryIDOrPrefix, memoryIDOrPrefix+"%",
	).Scan(&n, &found); err != nil {
		return nil, err
	}
	switch n {
	case 0:
		return nil, fmt.Errorf("该轨迹里没有记忆 %s", memoryIDOrPrefix)
	case 1: // 唯一命中
	default:
		return nil, fmt.Errorf("记忆前缀 %q 在该轨迹里歧义（匹配 %d 条）", memoryIDOrPrefix, n)
	}
	memoryID := found

	var prev string
	if err := tx.QueryRow(
		`SELECT feedback FROM read_trace_items WHERE trace_id = ? AND memory_id = ?`,
		traceID, memoryID,
	).Scan(&prev); err != nil {
		return nil, err
	}
	if prev != "" && prev != feedback {
		return nil, fmt.Errorf("该条目已有反馈 %q，不能直接改判为 %q（需先撤销）", prev, feedback)
	}

	now := time.Now().Unix()
	if _, err := tx.Exec(
		`UPDATE read_trace_items
		    SET feedback = ?, feedback_reason = ?, feedback_at = ?
		  WHERE trace_id = ? AND memory_id = ?`,
		feedback, reason, now, traceID, memoryID,
	); err != nil {
		return nil, err
	}

	var adopted int64
	if err := tx.QueryRow(`SELECT adopted_count FROM memories WHERE id = ?`, memoryID).Scan(&adopted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("记忆已不存在: %s", memoryID)
		}
		return nil, err
	}
	// 只有「首次 adopted」累加；同值重复提交是幂等的。
	if feedback == "adopted" && prev != "adopted" {
		res, err := tx.Exec(
			`UPDATE memories
			    SET adopted_count = adopted_count + 1,
			        last_adopted_at = ?,
			        last_seen_at = ?,
			        access_count = access_count + 1
			  WHERE id = ?`, now, now, memoryID)
		if err != nil {
			return nil, err
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			return nil, fmt.Errorf("更新记忆采纳计数失败: %s", memoryID)
		}
		if err := tx.QueryRow(`SELECT adopted_count FROM memories WHERE id = ?`, memoryID).Scan(&adopted); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &FeedbackResult{
		TraceID: traceID, MemoryID: memoryID, Feedback: feedback,
		Reason: reason, AdoptedCount: adopted,
	}, nil
}

// MemoryDetailByID 读取一条记忆的完整只读视图（支持 id 前缀）。
func (s *Store) MemoryDetailByID(idOrPrefix string) (*MemoryDetail, error) {
	id, err := s.resolveID(idOrPrefix)
	if err != nil {
		return nil, err
	}
	var (
		d          MemoryDetail
		expires    sql.NullInt64
		lastAdopt  sql.NullInt64
		superseded sql.NullString
		prov       sql.NullString
	)
	err = s.db.QueryRow(
		`SELECT id, content, kind, project, COALESCE(scope,''), salience, content_hash,
		        created_at, updated_at, last_seen_at, expires_at, superseded_by,
		        COALESCE(source,''), COALESCE(channel,''), COALESCE(origin_tool,''),
		        access_count, adopted_count, last_adopted_at, provenance
		   FROM memories WHERE id = ?`, id,
	).Scan(&d.ID, &d.Content, &d.Kind, &d.Project, &d.Scope, &d.Salience, &d.ContentHash,
		&d.CreatedAt, &d.UpdatedAt, &d.LastSeenAt, &expires, &superseded,
		&d.Source, &d.Channel, &d.Tool, &d.AccessCount, &d.AdoptedCount, &lastAdopt, &prov)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("未找到记忆: %s", idOrPrefix)
		}
		return nil, err
	}
	if expires.Valid {
		v := expires.Int64
		d.ExpiresAt = &v
	}
	if superseded.Valid {
		d.SupersededBy = superseded.String
	}
	if lastAdopt.Valid {
		v := lastAdopt.Int64
		d.LastAdopted = &v
	}
	if prov.Valid {
		d.Provenance = parseProvenance(prov.String)
	}
	return &d, nil
}

// TracesForMemory 返回某条记忆最近参与过的读侧供给。
func (s *Store) TracesForMemory(memoryID string, limit int) ([]MemoryTrace, error) {
	if strings.TrimSpace(memoryID) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(
		`SELECT rt.id, rt.query_gate, rt.gate_reason, rt.raw_query, rt.outcome,
		        rti.ordinal, rti.injection_rank, rti.feedback, rti.feedback_reason,
		        rti.feedback_at, rt.created_at
		   FROM read_trace_items rti
		   JOIN read_traces rt ON rt.id = rti.trace_id
		  WHERE rti.memory_id = ?
		  ORDER BY rt.created_at DESC, rt.id DESC, rti.ordinal ASC
		  LIMIT ?`, memoryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryTrace
	for rows.Next() {
		var t MemoryTrace
		if err := rows.Scan(&t.TraceID, &t.QueryGate, &t.GateReason, &t.RawQuery, &t.Outcome,
			&t.Ordinal, &t.InjectionRank, &t.Feedback, &t.FeedbackReason, &t.FeedbackAt,
			&t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
