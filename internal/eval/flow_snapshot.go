package eval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
)

// 动线快照：只做 SQL → 结构体的投影，不含任何判定。
//
// 判定全部在 flow_check.go 的纯函数里；这一层薄到可以逐列对照 schema.sql。
// 字段顺序 = SELECT 列顺序，flowQuery 按位置扫描（新增列时必须两边同改）。
// 数值型 legacy 用 int 0/1 承接：database/sql 不把 SQLite 的 int 直接转 bool。

// FlowObservation 是 observations 的只读投影。
type FlowObservation struct {
	ID          string
	SourceID    string
	Generation  int
	Seq         int
	EventUID    string
	ArchiveHash string
	CreatedAt   int64
	Legacy      int // 1 = migration_marks 标记 legacy_unpositioned
}

// FlowArchive 是 evidence_archives 的只读投影。
type FlowArchive struct {
	Hash       string
	SourceID   string
	Generation int
	FromSeq    int
	ToSeq      int
	RawCount   int
}

// FlowBundle 是 evidence_bundles 的只读投影。
type FlowBundle struct {
	ID               string
	SourceID         string
	Generation       int
	FromSeqExclusive int
	ToSeqInclusive   int
	Status           string
	Attempt          int
	LastRunID        string
}

// FlowSource 是证据源的水位。
type FlowSource struct {
	ID         string
	Generation int
	LastSeq    int
	CursorSeq  int
}

// FlowRun 是 distill_runs 的完成账。
type FlowRun struct {
	ID        string
	BundleID  string
	Status    string
	Attempt   int
	Generated int
	Duplicate int
}

// FlowCandidate 是候选裁决前的只读投影。
type FlowCandidate struct {
	ID          string
	SessionID   string
	ContentHash string
	Status      string
	MemoryID    string
	BundleID    string
	Revision    int
	CreatedAt   int64
}

// FlowDecision 是 commit_decisions 的只读投影。
type FlowDecision struct {
	CandidateID    string
	Revision       int
	Action         string
	Actor          string
	MemoryID       string
	IdempotencyKey string
}

// FlowCandidateEvidence 是候选引用的证据定位。
type FlowCandidateEvidence struct {
	CandidateID string
	EvidenceID  string
	Role        string
}

// FlowMemoryEvidence 是记忆落库后的证据绑定。
type FlowMemoryEvidence struct {
	MemoryID   string
	EvidenceID string
}

// FlowMemory 只取 id，用于验证「采纳后记忆确实存在」。
type FlowMemory struct {
	ID string
}

// FlowSnapshot 是一次 eval flow 的全部输入。
type FlowSnapshot struct {
	Now               int64
	Observations      []FlowObservation
	Archives          []FlowArchive
	Bundles           []FlowBundle
	Sources           []FlowSource
	Runs              []FlowRun
	Candidates        []FlowCandidate
	Decisions         []FlowDecision
	CandidateEvidence []FlowCandidateEvidence
	MemoryEvidence    []FlowMemoryEvidence
	Memories          []FlowMemory
}

// Queryer 是 *sql.DB 的最小读接口，便于测试注入查询失败。
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// LoadFlowSnapshot 用一组只读查询拼出快照。
//
// 任一查询失败都返回 error（含全部失败原因），绝不拿半张快照出结论——
// 缺表的「通过」比失败更危险（E1：空库必须显式 fail，而不是静默 PASS）。
func LoadFlowSnapshot(q Queryer, now int64) (FlowSnapshot, error) {
	s := FlowSnapshot{Now: now}
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	obs, err := flowQuery[FlowObservation](q, `SELECT o.id, o.source_id, o.source_generation,
		o.source_seq, o.event_uid, o.archive_hash, o.created_at,
		CASE WHEN m.entity_id IS NULL THEN 0 ELSE 1 END
		FROM observations o
		LEFT JOIN migration_marks m
		  ON m.entity_type='observation' AND m.entity_id=o.id
		 AND m.mark='legacy_unpositioned'`)
	collect(err)
	s.Observations = obs

	s.Archives, err = flowQuery[FlowArchive](q, `SELECT archive_hash, source_id, generation,
		from_seq, to_seq, raw_count FROM evidence_archives`)
	collect(err)

	s.Bundles, err = flowQuery[FlowBundle](q, `SELECT id, source_id, generation,
		from_seq_exclusive, to_seq_inclusive, status, attempt, last_run_id
		FROM evidence_bundles`)
	collect(err)

	s.Sources, err = flowQuery[FlowSource](q, `SELECT id, generation, last_seq, cursor_seq
		FROM evidence_sources`)
	collect(err)

	s.Runs, err = flowQuery[FlowRun](q, `SELECT id, bundle_id, status, attempt,
		generated_count, duplicate_count FROM distill_runs`)
	collect(err)

	s.Candidates, err = flowQuery[FlowCandidate](q, `SELECT id, session_id, content_hash,
		status, COALESCE(memory_id,''), bundle_id, revision, created_at FROM candidates`)
	collect(err)

	s.Decisions, err = flowQuery[FlowDecision](q, `SELECT candidate_id, revision, action,
		actor, memory_id, idempotency_key FROM commit_decisions`)
	collect(err)

	s.CandidateEvidence, err = flowQuery[FlowCandidateEvidence](q, `SELECT candidate_id,
		evidence_id, evidence_role FROM candidate_evidence`)
	collect(err)

	s.MemoryEvidence, err = flowQuery[FlowMemoryEvidence](q, `SELECT memory_id, evidence_id
		FROM memory_evidence`)
	collect(err)

	s.Memories, err = flowQuery[FlowMemory](q, `SELECT id FROM memories`)
	collect(err)

	if len(errs) > 0 {
		return s, errors.Join(errs...)
	}
	return s, nil
}

// flowQuery 把查询结果按字段位置扫进 T 的切片。
//
// 用反射而不是逐查询写 scan：列顺序由 SELECT 与结构体共同保证，
// 新增表只加一个结构体，不复制一段易漂移的 Scan 代码。
func flowQuery[T any](q Queryer, query string) ([]T, error) {
	rows, err := q.QueryContext(context.Background(), query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []T
	for rows.Next() {
		var v T
		dest, err := flowDest(&v, len(cols))
		if err != nil {
			return nil, err
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// flowDest 按位置返回字段地址；列数与字段数不一致直接报错（不做部分扫描）。
func flowDest(v any, n int) ([]any, error) {
	rv := reflect.ValueOf(v).Elem()
	if rv.NumField() < n {
		return nil, fmt.Errorf("eval: 列数 %d 多于结构体字段 %d（%T）", n, rv.NumField(), v)
	}
	dest := make([]any, n)
	for i := 0; i < n; i++ {
		dest[i] = rv.Field(i).Addr().Interface()
	}
	return dest, nil
}
