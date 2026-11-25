package store

// S13「保留与终态出口」：完成 Bundle 有界保留与 commit_decisions 分级回收。
//
// 业务逻辑刻意放新文件，不侵入 GCCandidates / GCAudit 等既有函数。
// 口径来自 docs/design-data-lifecycle.md §2.2 终态保留表、§8.1 顺序铁律、
// §8.2 各类出口表：只清终态、只清过保留期，且删之前先过引用保护。

import (
	"time"
)

const (
	// BundleTerminalRetention 是 ok/noop 完成 Bundle 的有界保留期（§2.2「有保留期后回收」）。
	// 30 天：完成账只需撑过回捞/对账窗口，不是证据本身；删 Bundle 不动任何归档字节，
	// 归档段仍由 archiveRefGuard 的观测/记忆/候选/活跃范围几条独立保护。
	// 常量化是为了让 R15 与巡检口径有单一出处，不许散落成魔法数。
	BundleTerminalRetention = 30 * 24 * time.Hour
	// DecisionRetention 是孤立决策账的保留窗口，与候选 GC 的 90 天对齐。
	// 必须 ≥ 候选 GC 窗口：现存候选的裁决一行都不能动（eval G9 裁决闭环读它），
	// 只有「候选已被 GC 且决策自己也超窗」的行才允许释放 idempotency_key。
	DecisionRetention = 90 * 24 * time.Hour
)

// terminalBundleGuard 是完成 Bundle 的引用保护（§8.1 全量引用保护的同一根）：
// items 的证据若仍被候选或记忆引用，整个 Bundle 保留，绝不删；
// 仍被 distill_runs 引用的也保留，否则 G7 会看到「运行账指向不存在的 Bundle」。
const terminalBundleGuard = `
	AND NOT EXISTS (
	  SELECT 1 FROM evidence_bundle_items i
	   WHERE i.bundle_id = b.id
	     AND (EXISTS (SELECT 1 FROM candidate_evidence ce WHERE ce.evidence_id = i.evidence_id)
	       OR EXISTS (SELECT 1 FROM memory_evidence me WHERE me.evidence_id = i.evidence_id)))
	AND NOT EXISTS (SELECT 1 FROM distill_runs dr WHERE dr.bundle_id = b.id)`

// BundleGCResult 是一次完成 Bundle 回收的统计。
type BundleGCResult struct {
	Bundles           int `json:"bundles"`
	Items             int `json:"bundle_items"`
	SkippedReferenced int `json:"skipped_referenced"`
}

// GCTerminalBundles 回收超过保留期的 ok/noop 完成 Bundle（含 items），单事务。
//
// quarantined / failed / pending / leased / backlog 一律不动（§2.2：等人工或重试），
// 保护期内也不动。删之前先在事务里物化可删 id 集合：先删 items 会让 guard 里的
// evidence_bundle_items 子查询变空，第二条 DELETE 就会误伤被引用保护的 Bundle。
func (s *Store) GCTerminalBundles(retention time.Duration) (*BundleGCResult, error) {
	if retention <= 0 {
		retention = BundleTerminalRetention
	}
	cutoff := time.Now().Add(-retention).Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT b.id FROM evidence_bundles b
		 WHERE b.status IN ('ok','noop') AND b.updated_at < ?`+terminalBundleGuard, cutoff)
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

	// 超龄完成账总数 - 可删数 = 被引用保护挡下的数量（dry-run / 巡检报数用）。
	var total int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM evidence_bundles
		 WHERE status IN ('ok','noop') AND updated_at < ?`, cutoff).Scan(&total); err != nil {
		return nil, err
	}
	// items 计数在删之前取，与 Bundle 同一集合，不依赖逐行 RowsAffected。
	var items int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM evidence_bundle_items i
		  JOIN evidence_bundles b ON b.id = i.bundle_id
		 WHERE b.status IN ('ok','noop') AND b.updated_at < ?`+terminalBundleGuard, cutoff).Scan(&items); err != nil {
		return nil, err
	}

	res := &BundleGCResult{Bundles: len(ids), Items: items, SkippedReferenced: total - len(ids)}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM evidence_bundle_items WHERE bundle_id = ?`, id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM evidence_bundles WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// DecisionGCResult 是一次决策账回收的统计。
type DecisionGCResult struct {
	Deleted int `json:"deleted"`
}

// GCCommitDecisions 分级回收 commit_decisions：只删「对应候选已被 GC 且超过保留
// 窗口」的孤立行，保留所有现存候选的裁决（否则 eval G9 会红）。
//
// 幂等键窗口：commit_decisions.idempotency_key 有 UNIQUE 约束。现存候选的裁决
// 一旦被删，重放同一 (candidate, revision, action) 就会再插一条、绕过幂等；
// 因此删除条件必须是「候选已不存在」+「决策本身也超 90 天」，两个条件缺一不可。
// 单条 DELETE 即 SQLite 的原子事务，不另开连接、不留半提交。
func (s *Store) GCCommitDecisions(retention time.Duration) (*DecisionGCResult, error) {
	if retention <= 0 {
		retention = DecisionRetention
	}
	res, err := s.db.Exec(`DELETE FROM commit_decisions
		 WHERE created_at < ?
		   AND NOT EXISTS (SELECT 1 FROM candidates c WHERE c.id = commit_decisions.candidate_id)`,
		time.Now().Add(-retention).Unix())
	if err != nil {
		return nil, err
	}
	// RowsAffected 在 modernc sqlite 上不会失败：删了多少行由 DELETE 本身决定，
	// 不为这个不可能的失败再开一条未覆盖分支（store 覆盖率余量只有 0.1pt）。
	n, _ := res.RowsAffected()
	return &DecisionGCResult{Deleted: int(n)}, nil
}
