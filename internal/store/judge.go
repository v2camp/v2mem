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

// JudgeStat 是按判定结果分组的影子期统计。
//
// Accepted / Rejected 数的是「判定之后被裁决成什么」：影子期两个数都由人工产生，
// 它们之比就是该 verdict 的人工一致率；转正后由判定器自己产生，那时它只是计数。
type JudgeStat struct {
	Verdict  string `json:"verdict"`
	Total    int    `json:"total"`
	Accepted int    `json:"accepted"`
	Rejected int    `json:"rejected"`
	Pending  int    `json:"pending"`
}

// JudgeStats 汇总所有已判定的候选。只读单次聚合，命中 ix_cand_status 之外的顺序扫描——
// 候选表量级在千条以内，不为它加索引。
func (s *Store) JudgeStats() ([]JudgeStat, error) {
	rows, err := s.db.Query(`SELECT judge_verdict, COUNT(*),
		SUM(CASE WHEN status = 'accepted' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status = 'rejected' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status = 'pending'  THEN 1 ELSE 0 END)
		FROM candidates WHERE judge_verdict != '' GROUP BY judge_verdict ORDER BY judge_verdict`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JudgeStat
	for rows.Next() {
		var st JudgeStat
		if err := rows.Scan(&st.Verdict, &st.Total, &st.Accepted, &st.Rejected, &st.Pending); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
