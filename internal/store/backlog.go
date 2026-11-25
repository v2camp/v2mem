package store

// S4：积压五指标（design-data-lifecycle §3.2）。
//
// 一条 SQL 取 9 个标量，保证所有计数来自同一快照：分 9 次查询会在两次查询之间
// 被并发维护改写，出现「源还有积压、年龄却已归零」这种自相矛盾的读数。
// MIN 列一律用 sql.NullInt64：无数据必须投影成 null（指针 nil），绝不报 0 年龄。

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// Backlog 是 mem stats 暴露的积压原始值。不判级、不带阈值——判级仍归巡检的 R ID。
type Backlog struct {
	PendingCount           int    `json:"pending_count"`
	OldestPendingAge       *int64 `json:"oldest_pending_age_seconds,omitempty"`
	EvidenceBacklogSources int    `json:"evidence_backlog_sources"`
	// OldestUnconsumedEvidenceAge 只算「代际匹配且 to_seq > cursor_seq」的归档：源轮转后，
	// 旧代际的归档不属于当前游标，算进来会制造永远追不上的假积压。
	OldestUnconsumedEvidenceAge *int64 `json:"oldest_unconsumed_evidence_age_seconds,omitempty"`
	OpenBundles                 int    `json:"open_bundles"`
	OldestOpenBundleAge         *int64 `json:"oldest_open_bundle_age_seconds,omitempty"`
	CandidatesIsolated          int    `json:"candidates_isolated"`
	BundlesIsolated             int    `json:"bundles_isolated"`
	// EvidenceGaps 是已处置的序号洞数（cursor 已越过、区间不会再产出证据）。
	// 它不计入积压判级上限：洞是「已跳过并记录」的事实，不是待办队列。
	EvidenceGaps      int    `json:"evidence_gaps"`
	LastMaintainOKAt  *int64 `json:"last_maintain_ok_at,omitempty"`
	LastMaintainOKAge *int64 `json:"last_maintain_ok_age_seconds,omitempty"`
}

// Backlog 读取当前积压。now 由调用方传入（测试用固定时钟），年龄 = now-ts，负值夹 0。
func (s *Store) Backlog(now int64) (*Backlog, error) {
	var (
		pending            int
		minPending         sql.NullInt64
		evidenceSources    int
		minEvidence        sql.NullInt64
		openBundles        int
		minOpenBundle      sql.NullInt64
		candidatesIsolated int
		bundlesIsolated    int
		evidenceGaps       int
	)
	err := s.db.QueryRow(`
		SELECT
		  (SELECT COUNT(*) FROM candidates WHERE status = 'pending'),
		  (SELECT MIN(created_at) FROM candidates WHERE status = 'pending'),
		  (SELECT COUNT(*) FROM evidence_sources WHERE last_seq > cursor_seq),
		  (SELECT MIN(a.created_at) FROM evidence_archives a
		     JOIN evidence_sources s
		       ON s.id = a.source_id AND s.generation = a.generation
		    WHERE a.to_seq > s.cursor_seq),
		  (SELECT COUNT(*) FROM evidence_bundles WHERE status IN `+proposalOpenStatuses+`),
		  (SELECT MIN(created_at) FROM evidence_bundles WHERE status IN `+proposalOpenStatuses+`),
		  (SELECT COUNT(*) FROM candidates WHERE status IN ('quarantined','failed')),
		  (SELECT COUNT(*) FROM evidence_bundles WHERE status = 'quarantined'),
		  (SELECT COUNT(*) FROM evidence_gaps)
	`).Scan(
		&pending, &minPending,
		&evidenceSources, &minEvidence,
		&openBundles, &minOpenBundle,
		&candidatesIsolated, &bundlesIsolated, &evidenceGaps,
	)
	if err != nil {
		return nil, fmt.Errorf("读取积压指标: %w", err)
	}

	b := &Backlog{
		PendingCount:                pending,
		OldestPendingAge:            ageOrNil(now, minPending),
		EvidenceBacklogSources:      evidenceSources,
		OldestUnconsumedEvidenceAge: ageOrNil(now, minEvidence),
		OpenBundles:                 openBundles,
		OldestOpenBundleAge:         ageOrNil(now, minOpenBundle),
		CandidatesIsolated:          candidatesIsolated,
		BundlesIsolated:             bundlesIsolated,
		EvidenceGaps:                evidenceGaps,
	}

	v, ok, err := s.MetaGet(MetaMaintainLastOK)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", MetaMaintainLastOK, err)
	}
	if ok {
		// 坏值必须报错：把它当「从未维护」会静默掩盖一条被写坏的维护水位。
		at, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("解析 %s=%q: %w", MetaMaintainLastOK, v, perr)
		}
		b.LastMaintainOKAt = &at
		age := now - at
		if age < 0 {
			age = 0
		}
		b.LastMaintainOKAge = &age
	}
	return b, nil
}

// ageOrNil 把 MIN(created_at) 转成年龄；NULL 保持 nil，未来时间夹 0。
func ageOrNil(now int64, ts sql.NullInt64) *int64 {
	if !ts.Valid {
		return nil
	}
	age := now - ts.Int64
	if age < 0 {
		age = 0
	}
	return &age
}
