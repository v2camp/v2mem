package store

// 准入判定（docs/design-admission.md）。
//
// 判定结果与「是否据此改状态」分开：MarkJudgeVerdict 只写痕迹，影子期靠它
// 零风险积累样本；改 status 必须走 CommitAccept / CommitReject，与人工裁决同一条路径，
// 这样审计、revision 冲突、唯一索引的行为完全一致——判定器不是特权通道。

// MarkJudgeVerdict 记录一次判定，不动 status。
//
// 重复判定会覆盖上一次的 verdict（同一个候选可能换模型重判），judge_model 一起覆盖，
// 否则「谁判的」与「判成什么」会分属两个模型，事后无法归因。
func (s *Store) MarkJudgeVerdict(id, verdict, model, reason string) error {
	_, err := s.db.Exec(`UPDATE candidates
		SET judge_verdict = ?, judge_model = ?, rationale = ?
		WHERE id = ?`, verdict, model, reason, id)
	return err
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
