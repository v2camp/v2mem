package store

// Ebbinghaus 衰减：salience 的「如果现在重新计算，应该是多少」。
//
// 这里只提供纯函数，**不落库、不写回**。写回式衰减在这个仓库里不可行，三条硬理由：
//  1. 没有 last_decayed_at 列，Δt 只能取 last_seen_at。写回会压低 salience 却不动
//     last_seen_at ⇒ 下次又从「已衰减值」再乘一次，重复衰减，跑几次 GC 就清空。
//  2. sync.go 的归并用 MAX(salience, ?)（见 sync.go:182、sync.go:428）。衰减写回后，
//     任何一次重复写入或对端同步都会把 salience 抬回旧值 ⇒ 不收敛，多端结果互斥。
//  3. min(1,·) 饱和 + MAX 合并不是半格同态：
//     MAX(decay(s,t1), decay(s,t2)) ≠ decay(s, max(t1,t2))，跨设备结果依赖同步顺序。
//
// 因此调用方应当在「读时」用本文件的函数算出一个派生分，用于排序 / GC 判据，
// 而不是把结果塞回 memories.salience，也**不得**改写 Hit.Salience 的语义：
// cmd/mem/notes.go:19 用 h.Salience >= 0.8 判「硬规则」，衰减值若写进该字段，
// 全局红线会在 30 天后静默消失。派生分要单独成字段（接入见后续波次）。

import (
	"math"
	"strconv"
	"strings"
)

// secondsPerDay 是 Δt 的换算基准：库里所有时间戳都是 Unix 秒。
const secondsPerDay = 86400.0

// DecayConfig 是衰减公式的三个参数。
//
// 零值（DecayConfig{}）语义明确：λ=0、σ=0、floor=0 ⇒ 「不衰减、只截断到 [0,1]」，
// 便于调用方做 A/B 或测试时临时关掉衰减，不需要额外开关。
type DecayConfig struct {
	// Lambda 是遗忘速率（单位：1/天）。0.01 对应「半衰期约 69 天」。
	Lambda float64
	// Sigma 是复习（命中）增益的权重。
	Sigma float64
	// Floor 是分数下限，防止所有记忆一起趋零。
	//
	// 必须有下限：GC 的判据是 salience < minSalience 的硬删除（store.go:1038），
	// 若派生分随 Δt 无限趋零，一次 GC 就会把整个库清掉。
	Floor float64
}

// DefaultDecayConfig 返回默认参数：λ=0.01/天、σ=0.3、floor=0.05。
func DefaultDecayConfig() DecayConfig {
	return DecayConfig{Lambda: 0.01, Sigma: 0.3, Floor: 0.05}
}

// normalized 把非法参数收敛到有意义的范围，而不是把它们静默当成默认值——
// 静默回落会让「我明明传了 λ=0」的调用方拿到意料之外的衰减。
func (c DecayConfig) normalized() DecayConfig {
	if math.IsNaN(c.Lambda) || c.Lambda < 0 {
		c.Lambda = 0
	}
	if math.IsNaN(c.Sigma) || c.Sigma < 0 {
		c.Sigma = 0
	}
	if math.IsNaN(c.Floor) || c.Floor < 0 {
		c.Floor = 0
	}
	if c.Floor > 1 {
		c.Floor = 1
	}
	return c
}

// DecayedSalience 返回「若此刻重算」的 salience 派生分，范围 [floor, 1]。
//
// 公式（与 agentmemory/src/functions/retention.ts 同构）：
//
//	score = min(1, salience·exp(-λΔt_days) + σ·Σ(1/daysSinceAccess))
//
// 其中第二项 σ·Σ(1/daysSinceAccess) 在本仓库只能**近似**，见 reinforceBonus 的说明。
//
// 参数：
//   - salience     : 当前存量分数（memories.salience）
//   - lastSeenAt   : 上次命中时间，Unix 秒（memories.last_seen_at）
//   - now          : 当前时间，Unix 秒。由调用方传入，函数内部不取时钟，保证可测、可复现
//   - accessCount  : 累计命中次数（memories.access_count）
//
// 非法输入一律按「更保守」的方向处理，且行为在测试里固定下来：
//   - now < lastSeenAt（时钟回拨 / 对端时间超前）⇒ Δt 取 0，即「刚命中过」，
//     不产生负指数（负 Δt 会让分数越算越高，等于给错乱的时钟发奖励）
//   - accessCount < 0 ⇒ 按 0 处理（不能用负数把分数往下拽）
//   - salience 为 NaN ⇒ 返回 floor（不可信的分数不能留在库的判据里）
//   - salience 越界 ⇒ 先夹到 [0,1]
func DecayedSalience(salience float64, lastSeenAt, now, accessCount int64, cfg DecayConfig) float64 {
	cfg = cfg.normalized()

	// NaN 参与任何比较都是 false，会绕过下面的 clamp 直接污染结果，先拦掉。
	if math.IsNaN(salience) {
		return cfg.Floor
	}

	dtDays := daysBetween(lastSeenAt, now)

	score := clamp01(salience)*math.Exp(-cfg.Lambda*dtDays) + reinforceBonus(accessCount, dtDays, cfg.Sigma)

	// 与原公式一致：饱和到 1。饱和是非线性操作，正是它让写回 + MAX 归并不成立。
	if score > 1 {
		score = 1
	}
	// 下限放在饱和之后：floor 是「不可击穿的地板」，不是再乘一次系数。
	if score < cfg.Floor {
		score = cfg.Floor
	}
	return score
}

// daysBetween 返回 lastSeenAt → now 的天数，负值按 0 处理（理由见 DecayedSalience）。
func daysBetween(lastSeenAt, now int64) float64 {
	if now <= lastSeenAt {
		return 0
	}
	return float64(now-lastSeenAt) / secondsPerDay
}

// clamp01 把分数夹到 [0,1]；NaN 已在上游拦截。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// reinforceBonus 是 σ 项的**近似**实现。这里必须说清楚它不等价于原公式。
//
// 原公式是 σ·Σ(1/daysSinceAccess)：对每一次访问的时间戳 t_i 求 1/(now-t_i) 再求和，
// 于是「很久以前被查过 10 次」和「昨天被查过 10 次」得分完全不同——前者每次访问的
// 权重都已经衰减过了。
//
// 本仓库缺什么：memories 只有 access_count 这个**计数**，没有任何一次访问的时间戳
// （没有 access_log 表，也不为此新增表/列）。Σ 里的每个 t_i 都不可得，分母无从计算。
//
// 因此改用：
//
//	σ · ln(1 + access_count) / (1 + Δt_days)
//
// 它在下面几点上与原公式方向一致，但数值不等价：
//   - 对 access_count 单调递增（ln 抑制重复命中的边际收益，避免计数刷分）
//   - 对 Δt 单调递减（用「距最后一次访问的时间」当所有访问的共同替代量）
//   - Δt=0 时退化为 σ·ln(1+n)，即刚命中时给出足额增益
//
// 已知偏差：把「经常访问但很久没访问」的记忆估高了——原公式会让每一次旧访问各自
// 衰减到接近 0，而这里只按一个共同 Δt 折算。命中次数越多、间隔越不均匀，偏差越大。
// 若要真正等价，需要一张 (memory_id, accessed_at) 的访问流水表——那是新增表，
// 超出本次范围。
func reinforceBonus(accessCount int64, dtDays, sigma float64) float64 {
	if sigma == 0 {
		return 0
	}
	n := accessCount
	if n < 0 {
		n = 0
	}
	return sigma * math.Log1p(float64(n)) / (1 + dtDays)
}

// DecaySQLExpr 返回与 DecayedSalience 同构的 SQL 标量表达式（使用默认参数，
// 列名不带表别名）。now 为 Unix 秒。
//
// 实测结论：modernc.org/sqlite v1.59.0（内置 SQLite 3.53.4）**支持** exp()/ln()，
// 这两个函数在编译时已带 SQLITE_ENABLE_MATH_FUNCTIONS。验证方式：
//
//	SELECT exp(-1)            → 0.36787944117144233
//	SELECT ln(2), log(2)      → 0.6931471805599453 0.6931471805599453
//
// 所以这里可以下推 SQL，不需要退化成 Go 侧计算或分段线性近似。仍然要注意：
// SQL 里 exp()/ln() 的精度与 Go math.Exp/Log1p 有末位差异，断言相等要用容差。
//
// now 以整数字面量内联（int64，不存在注入面）：同一表达式既要用在 ORDER BY 又要
// 用在 WHERE，内联能让调用方只写一次、不必为两个 ? 占位符绑两次数。代价是 SQL
// 文本每次变化、无法命中预编译缓存——在 CLI 的查询频次下无关紧要。
func DecaySQLExpr(now int64) string {
	return DecaySQLExprFor("", now)
}

// DecaySQLExprFor 同 DecaySQLExpr，但给列名加前缀，用于带表别名的查询
// （例如检索语句里的 m.salience / m.last_seen_at / m.access_count）。
//
// prefix 会原样拼进 SQL，只应传编译期常量（"m." 之类），不要传用户输入。
func DecaySQLExprFor(prefix string, now int64) string {
	cfg := DefaultDecayConfig().normalized()

	col := func(name string) string { return prefix + name }
	num := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

	// Δt 秒 → 天，负数截 0，与 daysBetween 对齐。
	dtDays := "(MAX(0, " + strconv.FormatInt(now, 10) + " - " + col("last_seen_at") + ") / " +
		num(secondsPerDay) + ")"

	// 第一项：Go 侧是 clamp01(salience)·exp(-λΔt)，SQL 侧用 MIN/MAX 复刻 clamp01。
	base := "MIN(1.0, MAX(0.0, " + col("salience") + ")) * exp(-" + num(cfg.Lambda) + " * " + dtDays + ")"

	// 第二项：与 reinforceBonus 同式（σ·ln(1+n)/(1+Δt)），access_count 负值截 0。
	bonus := ""
	if cfg.Sigma != 0 {
		bonus = " + " + num(cfg.Sigma) + " * ln(1 + MAX(0, " + col("access_count") + ")) / (1.0 + " + dtDays + ")"
	}

	// 先饱和再落地板，顺序与 DecayedSalience 一致。
	return "MAX(" + num(cfg.Floor) + ", MIN(1.0, " + base + bonus + "))"
}

// decaySQLContainsMath 报告表达式里是否确实用到了 exp/ln，供测试守住「没有退化近似」。
func decaySQLContainsMath(expr string) bool {
	return strings.Contains(expr, "exp(") && strings.Contains(expr, "ln(")
}
