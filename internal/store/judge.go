package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// 准入判定（docs/design-admission.md）。
//
// 判定结果与「是否据此改状态」分开：MarkJudgeVerdict 只写痕迹，影子期靠它
// 零风险积累样本；改 status 必须走 CommitAccept / CommitReject，与人工裁决同一条路径，
// 这样审计、revision 冲突、唯一索引的行为完全一致——判定器不是特权通道。

// ErrJudgeVerdictStale 表示候选 revision 已变化或已离开 pending：
// 判定结果不能再覆盖当前行（P4 §7 第 2 条 status/revision CAS）。
var ErrJudgeVerdictStale = errors.New("候选 revision 已变化或已离开 pending")

// MarkJudgeVerdict 是旧调用方的兼容入口：先读当前 revision 再做 CAS。
// 生产批量判定直接用 MarkJudgeVerdictCAS(cand.Revision)，避免多一次读。
func (s *Store) MarkJudgeVerdict(id, verdict, model, reason string) error {
	var rev int
	if err := s.db.QueryRow(`SELECT revision FROM candidates WHERE id = ?`, id).Scan(&rev); err != nil {
		return err
	}
	return s.MarkJudgeVerdictCAS(id, verdict, model, reason, rev)
}

// MarkJudgeVerdictCAS 记录一次判定，不动 status；只有 revision 与 pending 状态
// 都没变时才写入，防止 Judge 覆盖并发的自动裁决。
//
// 重复判定会覆盖上一次的 verdict（同一个候选可能换模型重判），judge_model 一起覆盖，
// 否则「谁判的」与「判成什么」会分属两个模型，事后无法归因。
//
// 理由写进独立的 judge_reason，**不覆盖 rationale**：后者是提案自己给的理由，
// 是「模型为什么这么提」的唯一记录，覆盖掉就再也无法比较判定与提案的分歧
// （docs/design-data-lifecycle.md §7 第 5 条）。
func (s *Store) MarkJudgeVerdictCAS(id, verdict, model, reason string, expectedRevision int) error {
	res, err := s.db.Exec(`UPDATE candidates
		SET judge_verdict = ?, judge_model = ?, judge_reason = ?
		WHERE id = ? AND revision = ? AND status = 'pending'`,
		verdict, model, reason, id, expectedRevision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrJudgeVerdictStale
	}
	return nil
}

// CountPending 统计全库「还没被判定」的 pending 候选数，供积压触发判定用。
//
// 必须排除 judge_verdict!=”：影子期不改 status，已判的行仍然是 pending；
// 若照旧全数统计，积压会永远 ≥ 阈值，自动触发每 24h 空跑一次。
// 用全库而不是按工程：judge 一次处理的就是全库最老的一批，触发条件与它处理的
// 范围保持一致，否则「某工程 30 条却永不触发」取决于别的工程有没有先到阈值。
func (s *Store) CountPending() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM candidates WHERE status = 'pending' AND judge_verdict = ''`).Scan(&n)
	return n, err
}

// JudgedReject 是「模型判了否决、但还没人裁决」的候选，供批量清积压用。
type JudgedReject struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Kind        string `json:"kind"`
	Content     string `json:"content"`
	JudgeReason string `json:"judge_reason"`
}

// ListJudgeRejects 取出影子判定为 reject 且仍 pending 的候选（最老的在前）。
//
// 只出 reject，不出 accept：模型可以自动否决，不能自动授予。采纳仍必须走
// 人给的方案（design-admission §3.2）——否则「批量准出」就成了换名的 --apply。
// 判定痕迹不清空：退回 pending 的行不会被再判一遍（judge 只取 judge_verdict=”）。
//
// 理由取 judge_reason 而不是 rationale：后者是提案自己的话，把它当成模型的
// 理由码写进裁决，等于给否决编造依据。
func (s *Store) ListJudgeRejects(project string, limit int) ([]JudgedReject, error) {
	if limit <= 0 {
		limit = 500
	}
	q := `SELECT id, COALESCE(project, ''), kind, content, COALESCE(judge_reason, '')
		FROM candidates WHERE status = 'pending' AND judge_verdict = 'reject'`
	args := []any{}
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JudgedReject
	for rows.Next() {
		var r JudgedReject
		if err := rows.Scan(&r.ID, &r.Project, &r.Kind, &r.Content, &r.JudgeReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// JudgeStat 是按判定结果分组的影子期统计。
//
// Accepted / Rejected 数的是「判定之后被裁决成什么」：影子期两个数都由人工产生，
// 它们之比就是该 verdict 的人工一致率；转正后由判定器自己产生，那时它只是计数。
type JudgeStat struct {
	Verdict  string `json:"verdict"`
	Total    int    `json:"total"`
	Accepted int    `json:"accepted"` // 人工采纳
	Rejected int    `json:"rejected"` // 人工否决
	SelfRun  int    `json:"self_run"` // 非人工样本（判定器自裁、或人批准 AI 方案）：不算一致率
	Pending  int    `json:"pending"`
}

// JudgeStats 汇总所有已判定的候选。只读单次聚合，命中 ix_cand_status 之外的顺序扫描——
// 候选表量级在千条以内，不为它加索引。
func (s *Store) JudgeStats() ([]JudgeStat, error) {
	// 只把 actor = 'human' 的裁决算作人工样本。
	//
	// 白名单而不是黑名单（旧写法是 actor != 'judge'）：每新增一种 actor
	// （例如 mem batch 的 human-reviewed-llm），黑名单都会把它误算成人工，
	// 一致率被自己喂到 100%，「≥95% 才转正」这道门槛就白设了。
	// 新 actor 默认不进标定，要进必须显式改这一行并说明理由。
	rows, err := s.db.Query(`SELECT c.judge_verdict, COUNT(*),
		SUM(CASE WHEN c.status = 'accepted' AND d.actor = 'human' THEN 1 ELSE 0 END),
		SUM(CASE WHEN c.status = 'rejected' AND d.actor = 'human' THEN 1 ELSE 0 END),
		SUM(CASE WHEN (c.status = 'accepted' OR c.status = 'rejected') AND d.actor != 'human' THEN 1 ELSE 0 END),
		SUM(CASE WHEN c.status = 'pending' THEN 1 ELSE 0 END)
		FROM candidates c
		LEFT JOIN commit_decisions d
		  ON d.candidate_id = c.id
		 AND d.created_at = (SELECT MAX(d2.created_at) FROM commit_decisions d2 WHERE d2.candidate_id = c.id)
		WHERE c.judge_verdict != ''
		GROUP BY c.judge_verdict ORDER BY c.judge_verdict`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JudgeStat
	for rows.Next() {
		var st JudgeStat
		if err := rows.Scan(&st.Verdict, &st.Total, &st.Accepted, &st.Rejected, &st.SelfRun, &st.Pending); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// JudgeInvalidRetryLimit 是模型/Judge 非法输出的可重试次数上限（B3/B4，
// docs/design-eval-flows.md §3.3）。第 N 次（N>=上限）非法输出后转 quarantined。
const JudgeInvalidRetryLimit = 3

// MarkJudgeInvalidOutput 把一次非法模型输出落到候选状态（单事务）：
// failed（attempt+1、status_reason）→ 达到上限转 quarantined 终态。
//
// 影子期「只写痕迹不改状态」的前提是输出可解析；坏输出必须留痕并进入重试/隔离，
// 否则下一轮又抓同一批、又烧一次调用。只从 pending/failed 进入，其余状态报
// ErrCandidateClosed（不覆盖既有裁决）。返回最终状态，供调用方写日志/断言。
func (s *Store) MarkJudgeInvalidOutput(id, reason string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("缺少候选 id")
	}
	if strings.TrimSpace(reason) == "" {
		reason = "invalid_output"
	}
	var final string
	err := s.withImmediateTx(func(ctx context.Context, q sqlQuerier) error {
		var attempt int
		err := q.QueryRowContext(ctx,
			`SELECT attempt FROM candidates WHERE id = ? AND status IN ('pending','failed')`, id).
			Scan(&attempt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 候选 %s 不能在当前状态转入 failed/quarantined", ErrCandidateClosed, id)
		}
		if err != nil {
			return err
		}
		status := "failed"
		if attempt+1 >= JudgeInvalidRetryLimit {
			status = "quarantined"
		}
		if err := s.markCandidateTerminalTx(ctx, q, id, status, reason); err != nil {
			return err
		}
		final = status
		return nil
	})
	return final, err
}
