package eval

// 动线级端到端评测的账本对账（design-eval-flows.md §1 的核心度量）。
//
// 仍是**纯函数**：只吃各窗口的准入/终态账（调用方从 commit_decisions 与
// Bundle 完成账汇总），不碰数据库。对账口径（红线，改动须同步文档）：
//   - 全量对账而非抽查：逐窗口要求「终态数 = 准入数」，任何差值计入悬空并点名窗口。
//   - 准出率 = 终态 / (准入 - 豁免)；豁免（休眠/关机/总闸/灾备）必须由调用方
//     显式标记在 Exempt 里，禁止口头声明。
//   - 重复提交、严密度违例（强行写入）任一非 0 即不过，哪怕准出率是 100%。
//   - 坏账（豁免 > 准入、终态 > 分母）不许静默通过：前者分母钳 0，后者记矛盾窗口。

// FlowLedger 是单个处理窗口的准入/终态账。窗口口径由调用方定
// （Bundle 窗或日历窗口），字段全部来自账本计数，不含推断值。
type FlowLedger struct {
	WindowID   string `json:"window_id"`
	Admitted   int    `json:"admitted"`   // 准入事件数
	Terminal   int    `json:"terminal"`   // 到达明确终态数（记忆/拒绝/隔离）
	Exempt     int    `json:"exempt"`     // 豁免剔除数（显式标记）
	Duplicates int    `json:"duplicates"` // 重复提交数
	Forced     int    `json:"forced"`     // 严密度违例：证据不足/冲突/输出非法却强行写入
}

// FlowReport 是全量对账结果。
type FlowReport struct {
	Admitted            int      `json:"admitted"`
	Exempt              int      `json:"exempt"`
	Denominator         int      `json:"denominator"` // Admitted - Exempt，钳 0
	Terminal            int      `json:"terminal"`
	Dangling            int      `json:"dangling"` // 悬空：分母里没走到终态的数量
	DanglingWindows     []string `json:"dangling_windows,omitempty"`
	InconsistentWindows []string `json:"inconsistent_windows,omitempty"` // 终态 > 分母的坏账窗口
	ExitRate            float64  `json:"exit_rate"`                      // Terminal / Denominator；分母 0 记 1
	Duplicates          int      `json:"duplicates"`
	Forced              int      `json:"forced"`
}

// Reconcile 汇总各窗口账并按口径对账。windows 为空视为无未决数据，记通过。
func Reconcile(windows []FlowLedger) FlowReport {
	var r FlowReport
	r.ExitRate = 1
	for _, w := range windows {
		r.Admitted += w.Admitted
		r.Exempt += w.Exempt
		r.Terminal += w.Terminal
		r.Duplicates += w.Duplicates
		r.Forced += w.Forced
		den := w.Admitted - w.Exempt
		if den < 0 {
			den = 0
		}
		switch {
		case w.Terminal > den:
			r.InconsistentWindows = append(r.InconsistentWindows, w.WindowID)
		case w.Terminal < den:
			r.Dangling += den - w.Terminal
			r.DanglingWindows = append(r.DanglingWindows, w.WindowID)
		}
	}
	r.Denominator = r.Admitted - r.Exempt
	if r.Denominator < 0 {
		r.Denominator = 0
	}
	if r.Denominator > 0 {
		r.ExitRate = float64(r.Terminal) / float64(r.Denominator)
	}
	// 终态多于分母只可能来自已点名的坏账窗口；出口率钳 1 防止报表出现 >100% 的假象，
	// 是否放行仍由 InconsistentWindows 一票否决。
	if r.ExitRate > 1 {
		r.ExitRate = 1
	}
	return r
}

// Pass 报告是否满足出口断言：准出率 100%、无悬空、无重复、无违例、无坏账窗口。
func (r FlowReport) Pass() bool {
	return r.ExitRate >= 1.0 && r.Dangling == 0 && r.Duplicates == 0 &&
		r.Forced == 0 && len(r.InconsistentWindows) == 0
}

// ManualRate 人工介入率 = 人工动作 / 自动处置（design-data-lifecycle §11.9 口径）。
// 自动处置为 0 时：无动作记 0，有人工记 1（全人工，视为 100%）。负数钳 0。
func ManualRate(manual, auto int) float64 {
	if manual < 0 {
		manual = 0
	}
	if auto < 0 {
		auto = 0
	}
	if auto == 0 {
		if manual == 0 {
			return 0
		}
		return 1
	}
	return float64(manual) / float64(auto)
}
