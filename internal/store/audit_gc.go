package store

import "fmt"

// 审计账本的准出。
//
// 与观测、归档不同，这两张表不是「证据」而是「运维账本」——它们记录系统做过什么，
// 不构成任何记忆的溯源目标。没有保留期它们只会单调增长（活库 2026-11-23：77 条
// read_traces、48 条 distill_runs，全部永久留），但**不能按年龄一刀切**：
//
//   - distill_runs 是蒸馏游标。SessionsNeedingDistill 用「每会话 MAX(finished_at)
//     的 ok/empty 运行」和观测最新时间比较。删掉某会话最新那次成功运行，该会话就会
//     永远满足「有观测且比上次蒸馏更新」，每轮巡检都被重新捡起来重跑——烧推理、
//     重复产出候选。所以每个会话必须留一条游标。
//   - read_traces 是注入去重的依据。SessionInjectedRuleIDs 按 session 读、**没有
//     时间窗**，用来判断「这条 rule 在本会话是否已经注入过」。删掉活跃会话的轨迹，
//     同一条规则会在同一会话里被重复注入。
//
// 两条约束合起来给出「什么时候才算真的可以扔」：会话已经结束（没有新观测），
// 且历史已经不再承担游标/去重职责。这也是为什么 --audit 要放在 --obs 之后跑。

// AuditGCResult 是一次审计账本准出的统计。
type AuditGCResult struct {
	DistillRuns    int `json:"distill_runs"`
	ReadTraces     int `json:"read_traces"`
	ReadTraceItems int `json:"read_trace_items"`
}

// deadTracePredicate 选出「可以回收的读轨迹」：早于 cutoff，且所属会话没有
// 晚于 cutoff 的观测。后者就是「会话还活着」的判据——只要它还在写观测，
// 它的注入去重记录就不能动。
const deadTracePredicate = `rt.created_at < ?
	  AND NOT EXISTS (
	    SELECT 1 FROM observations o
	     WHERE o.session_id = rt.session_id AND o.created_at >= ?)`

// GCAudit 回收审计账本中「已结束会话」的超期记录，cutoff 为 unix 秒。
//
// 返回的三项分别是 distill_runs 行数、read_traces 行数、read_trace_items 行数。
func (s *Store) GCAudit(cutoff int64) (*AuditGCResult, error) {
	res := &AuditGCResult{}

	// 明细先删：read_trace_items 虽有 ON DELETE CASCADE，但这里显式删有两个理由——
	// 不依赖连接上的 foreign_keys 开关（历史上它被临时关过），且能如实报出清了几行。
	r, err := s.db.Exec(`DELETE FROM read_trace_items
		 WHERE trace_id IN (SELECT rt.id FROM read_traces rt WHERE `+deadTracePredicate+`)`,
		cutoff, cutoff)
	if err != nil {
		return nil, fmt.Errorf("回收读轨迹明细失败: %w", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return nil, err
	}
	res.ReadTraceItems = int(n)

	r, err = s.db.Exec(`DELETE FROM read_traces AS rt WHERE `+deadTracePredicate, cutoff, cutoff)
	if err != nil {
		return nil, fmt.Errorf("回收读轨迹失败: %w", err)
	}
	if n, err = r.RowsAffected(); err != nil {
		return nil, err
	}
	res.ReadTraces = int(n)

	// distill_runs 三条保留规则，缺一不可：
	//   ① 保留期内不动（started_at >= cutoff 已在 WHERE 里排除）；
	//   ② 每个会话留最新一条 ok/empty —— 它就是游标，删了会话会被无限重蒸；
	//   ③ 仍被 evidence_bundles 引用的不动 —— ProposalRuns 按 bundle_id 读它做溯源，
	//      删了 bundle 详情会显示「从未运行过」。
	r, err = s.db.Exec(`DELETE FROM distill_runs
		 WHERE started_at < ?
		   AND NOT EXISTS (SELECT 1 FROM evidence_bundles b WHERE b.id = distill_runs.bundle_id)
		   AND id <> COALESCE((
		         SELECT d2.id FROM distill_runs d2
		          WHERE d2.session_id = distill_runs.session_id
		            AND d2.status IN ('ok','empty')
		          ORDER BY d2.finished_at DESC, d2.id DESC
		          LIMIT 1), '')`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("回收蒸馏运行账失败: %w", err)
	}
	if n, err = r.RowsAffected(); err != nil {
		return nil, err
	}
	res.DistillRuns = int(n)

	return res, nil
}
