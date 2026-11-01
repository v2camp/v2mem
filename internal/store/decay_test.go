package store

import (
	"database/sql"
	"math"
	"testing"
)

// ---------- P1: Ebbinghaus 衰减纯函数 ----------
//
// 这组测试守的是「派生分能安全地当排序 / GC 判据」这件事，而不是某个魔数：
//   - 衰减必须单调、幂等、有下限，否则跨设备、多次执行会给出互相矛盾的结果
//   - λ / σ 必须真的在起作用，否则默认参数退化成不衰减也测不出来
//   - SQL 表达式与 Go 函数必须同解，否则 GC（SQL）与排序（Go）会各说各话

const daySeconds = int64(86400)

// approx 用相对容差比较浮点；衰减公式里混了 exp 与 ln，末位差异是正常的。
func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.17g, want %.17g (tol %g)", name, got, want, tol)
	}
}

// TestDecayZeroDeltaKeepsSalience：Δt=0 且没有命中时，分数就是原值。
// 这是公式的锚点——衰减不可以凭空抬分或压分。
func TestDecayZeroDeltaKeepsSalience(t *testing.T) {
	cfg := DefaultDecayConfig()
	now := int64(1_700_000_000)
	for _, sal := range []float64{0.2, 0.5, 0.87, 1.0} {
		got := DecayedSalience(sal, now, now, 0, cfg)
		approx(t, "Δt=0", got, sal, 1e-12)
	}
	// 未来时间戳（now < lastSeenAt）同样按 Δt=0 处理，不得放大分数。
	got := DecayedSalience(0.5, now+10*daySeconds, now, 0, cfg)
	approx(t, "时钟回拨", got, 0.5, 1e-12)
}

// TestDecayHugeDeltaHitsFloor：Δt 极大时趋近 floor，且永远不击穿。
func TestDecayHugeDeltaHitsFloor(t *testing.T) {
	cfg := DefaultDecayConfig()
	now := int64(1_700_000_000)
	last := now - 36500*daySeconds // 100 年
	got := DecayedSalience(0.9, last, now, 0, cfg)
	approx(t, "百年未命中", got, cfg.Floor, 1e-9)
	if got < cfg.Floor {
		t.Fatalf("击穿地板: %.17g < %v", got, cfg.Floor)
	}

	// 地板对所有输入生效，包括 salience=0 和负 salience。
	for _, sal := range []float64{0, -0.4, 0.0001} {
		if v := DecayedSalience(sal, last, now, 0, cfg); v < cfg.Floor-1e-12 {
			t.Fatalf("salience=%v 时击穿地板: %.17g", sal, v)
		}
	}
}

// TestDecayAccessCountSlowsDecay：命中越多，同一 Δt 下分数越高（衰减更慢）。
func TestDecayAccessCountSlowsDecay(t *testing.T) {
	cfg := DefaultDecayConfig()
	now := int64(1_700_000_000)
	last := now - 60*daySeconds

	prev := math.Inf(-1)
	for _, n := range []int64{0, 1, 5, 20, 100} {
		got := DecayedSalience(0.5, last, now, n, cfg)
		if got < prev {
			t.Fatalf("access_count=%d 时分数 %.17g 低于更小计数的 %.17g（应单调不减）", n, got, prev)
		}
		prev = got
	}

	// 同样 Δt，有命中必须明显高于无命中（不只是相等）。
	low := DecayedSalience(0.5, last, now, 0, cfg)
	high := DecayedSalience(0.5, last, now, 20, cfg)
	if high <= low {
		t.Fatalf("命中未带来增益: n=0 → %.17g, n=20 → %.17g", low, high)
	}
}

// TestDecayMonotonicInDelta：Δt 增大，分数不增。单调性是「时间只能让它更淡」的保证。
func TestDecayMonotonicInDelta(t *testing.T) {
	cfg := DefaultDecayConfig()
	now := int64(1_700_000_000)
	for _, n := range []int64{0, 3, 50} {
		prev := math.Inf(1)
		for d := int64(0); d <= 400; d += 5 {
			got := DecayedSalience(0.8, now-d*daySeconds, now, n, cfg)
			if got > prev+1e-15 {
				t.Fatalf("n=%d, Δt=%d 天: %.17g > 前一天 %.17g（应单调不增）", n, d, got, prev)
			}
			if got < cfg.Floor {
				t.Fatalf("n=%d, Δt=%d 天: %.17g 低于地板", n, d, got)
			}
			prev = got
		}
	}
}

// TestDecayIdempotent：纯函数，同输入多次调用结果逐位相同（没有隐藏状态、不读时钟）。
func TestDecayIdempotent(t *testing.T) {
	cfg := DefaultDecayConfig()
	now := int64(1_700_000_000)
	last := now - 30*daySeconds

	first := DecayedSalience(0.63, last, now, 7, cfg)
	for i := 0; i < 5; i++ {
		if got := DecayedSalience(0.63, last, now, 7, cfg); got != first {
			t.Fatalf("第 %d 次调用结果 %v 与首次 %v 不同", i+2, got, first)
		}
	}

	// 顺序无关：先算别的输入不应影响后续结果（防全局状态）。
	_ = DecayedSalience(0.1, now-1000*daySeconds, now, 999, cfg)
	if got := DecayedSalience(0.63, last, now, 7, cfg); got != first {
		t.Fatalf("交叉调用后结果漂移: %v != %v", got, first)
	}
}

// TestDecayFloorRespected：任意输入组合都落在 [floor, 1]。
func TestDecayFloorRespected(t *testing.T) {
	cfg := DefaultDecayConfig()
	now := int64(1_700_000_000)
	sals := []float64{-1, 0, 0.01, 0.5, 1, 1.7}
	counts := []int64{-3, 0, 1, 1000, math.MaxInt32}
	offsets := []int64{0, 1, 7, 30, 365, 36500}

	for _, s := range sals {
		for _, n := range counts {
			for _, d := range offsets {
				got := DecayedSalience(s, now-d*daySeconds, now, n, cfg)
				if got < cfg.Floor-1e-12 || got > 1+1e-12 {
					t.Fatalf("sal=%v n=%d Δt=%d → %.17g 越界 [%v,1]", s, n, d, got, cfg.Floor)
				}
			}
		}
	}

	// 自定义地板必须被尊重，而不是永远用默认 0.05。
	custom := DecayConfig{Lambda: 0.01, Sigma: 0.3, Floor: 0.4}
	if got := DecayedSalience(0.5, now-36500*daySeconds, now, 0, custom); got < 0.4-1e-12 {
		t.Fatalf("自定义地板未生效: %.17g", got)
	}
}

// TestDecayLambdaEffective：λ 变大 ⇒ 同样 Δt 下分数更低。
func TestDecayLambdaEffective(t *testing.T) {
	now := int64(1_700_000_000)
	last := now - 100*daySeconds

	slow := DecayedSalience(0.9, last, now, 0, DecayConfig{Lambda: 0.001, Sigma: 0.3, Floor: 0.01})
	fast := DecayedSalience(0.9, last, now, 0, DecayConfig{Lambda: 0.05, Sigma: 0.3, Floor: 0.01})
	if fast >= slow {
		t.Fatalf("λ 未生效: λ=0.05 → %.17g, λ=0.001 → %.17g", fast, slow)
	}

	// λ=0 时第一项不衰减：分数应等于原值（无命中、无地板干扰）。
	if got := DecayedSalience(0.9, last, now, 0, DecayConfig{Lambda: 0, Sigma: 0, Floor: 0}); math.Abs(got-0.9) > 1e-12 {
		t.Fatalf("λ=0 时应当不衰减: %.17g", got)
	}

	// σ=0 时命中项完全关闭：不同 access_count 结果相同。
	a := DecayedSalience(0.5, last, now, 0, DecayConfig{Lambda: 0.01, Sigma: 0, Floor: 0.05})
	b := DecayedSalience(0.5, last, now, 100, DecayConfig{Lambda: 0.01, Sigma: 0, Floor: 0.05})
	if a != b {
		t.Fatalf("σ=0 时命中项未关闭: %.17g != %.17g", a, b)
	}
}

// TestDecayInvalidInput：非法输入按文档约定处理，且不会造出 NaN / Inf。
func TestDecayInvalidInput(t *testing.T) {
	cfg := DefaultDecayConfig()
	now := int64(1_700_000_000)

	// 负数 Δt：按 0 处理 —— 不得高于 Δt=0 的分数。
	neg := DecayedSalience(0.5, now+86400, now, 0, cfg)
	zero := DecayedSalience(0.5, now, now, 0, cfg)
	approx(t, "负 Δt 应等价于 Δt=0", neg, zero, 1e-12)

	// 负 access_count：按 0 处理。
	if negCount, zeroCount := DecayedSalience(0.5, now-10*daySeconds, now, -9, cfg),
		DecayedSalience(0.5, now-10*daySeconds, now, 0, cfg); negCount != zeroCount {
		t.Fatalf("负 access_count 应等价于 0: %.17g != %.17g", negCount, zeroCount)
	}

	// NaN salience：落到地板（不可信分数不能参与 GC 判据）。
	if got := DecayedSalience(math.NaN(), now, now, 5, cfg); got != cfg.Floor {
		t.Fatalf("NaN salience 应返回地板: %.17g", got)
	}

	// 越界 salience 先夹到 [0,1]：1.7 不得算出 >1，-1 不得算出负分。
	if got := DecayedSalience(1.7, now, now, 0, cfg); got > 1+1e-12 {
		t.Fatalf("salience>1 未夹紧: %.17g", got)
	}
	if got := DecayedSalience(-1, now, now, 0, cfg); got != cfg.Floor {
		t.Fatalf("负 salience 应落到地板: %.17g", got)
	}

	// 非法配置：负 λ / 负 σ / NaN / floor>1 都要收敛到有界行为。
	bad := DecayConfig{Lambda: -1, Sigma: -1, Floor: 2}
	if got := DecayedSalience(0.5, now-10*daySeconds, now, 4, bad); math.IsNaN(got) || got < 0 || got > 1 {
		t.Fatalf("非法配置产出非法分数: %.17g", got)
	}
	nanCfg := DecayConfig{Lambda: math.NaN(), Sigma: math.NaN(), Floor: math.NaN()}
	if got := DecayedSalience(0.5, now-10*daySeconds, now, 4, nanCfg); math.IsNaN(got) {
		t.Fatalf("NaN 配置产出 NaN 分数")
	}

	// 极端参数不得溢出成 ±Inf。Δt 上限取 1e7 天（≈2.7 万年），
	// 再大就会让 now-dt*86400 自身溢出 int64，测的就不是公式了。
	for _, dt := range []int64{0, 1, 365, 100_000, 10_000_000} {
		if got := DecayedSalience(1, now-dt*daySeconds, now, math.MaxInt64, cfg); math.IsInf(got, 0) || math.IsNaN(got) {
			t.Fatalf("Δt=%d 时产出 %v", dt, got)
		}
	}
}

// TestDecayDefaultConfig 固定三个默认参数：改动它们等于改动全局行为。
func TestDecayDefaultConfig(t *testing.T) {
	cfg := DefaultDecayConfig()
	if cfg.Lambda != 0.01 || cfg.Sigma != 0.3 || cfg.Floor != 0.05 {
		t.Fatalf("默认参数变了: %+v", cfg)
	}
}

// TestDecaySQLExprMatchesGo：SQL 表达式与 Go 函数同解。
//
// 这条测试同时是「modernc.org/sqlite 支持 exp()/ln()」的回归保护：
// 驱动升级若丢掉 SQLITE_ENABLE_MATH_FUNCTIONS，这里会直接报 no such function。
func TestDecaySQLExprMatchesGo(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE probe (id INTEGER PRIMARY KEY, salience REAL, last_seen_at INTEGER, access_count INTEGER)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	now := int64(1_700_000_000)
	type row struct {
		id       int
		salience float64
		lastSeen int64
		count    int64
	}
	rows := []row{
		{1, 0.9, now, 0},                    // 刚命中
		{2, 0.5, now - 30*daySeconds, 3},    // 常规衰减
		{3, 0.2, now - 365*daySeconds, 10},  // 一年
		{4, 0.95, now - 3650*daySeconds, 0}, // 十年，应触地板
		{5, 1.2, now - 7*daySeconds, 2},     // 越界 salience
		{6, -0.3, now - 100*daySeconds, -5}, // 非法输入
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO probe (id, salience, last_seen_at, access_count) VALUES (?,?,?,?)`,
			r.id, r.salience, r.lastSeen, r.count); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	cfg := DefaultDecayConfig()
	q := `SELECT ` + DecaySQLExprFor("p.", now) + ` FROM probe p ORDER BY p.id`
	rs, err := db.Query(q)
	if err != nil {
		t.Fatalf("执行衰减表达式失败（exp/ln 可能不可用）: %v\nSQL: %s", err, q)
	}
	defer rs.Close()

	i := 0
	for rs.Next() {
		var got float64
		if err := rs.Scan(&got); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		want := DecayedSalience(rows[i].salience, rows[i].lastSeen, now, rows[i].count, cfg)
		approx(t, "SQL vs Go", got, want, 1e-9)
		i++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("遍历失败: %v", err)
	}
	if i != len(rows) {
		t.Fatalf("行数不符: got %d, want %d", i, len(rows))
	}

	// 不带前缀的版本也要能直接用在只查 memories 的语句里。
	var plain float64
	if err := db.QueryRow(`SELECT ` + DecaySQLExpr(now) + ` FROM probe WHERE id = 2`).Scan(&plain); err != nil {
		// 无前缀时 salience 等列名在 probe 上同样可解析，不应报错。
		t.Fatalf("无前缀表达式失败: %v", err)
	}
	approx(t, "无前缀", plain, DecayedSalience(0.5, now-30*daySeconds, now, 3, cfg), 1e-9)
}

// TestDecaySQLExprUsesMathFunctions：表达式确实下推了 exp/ln，没有偷偷退化成线性近似。
func TestDecaySQLExprUsesMathFunctions(t *testing.T) {
	expr := DecaySQLExpr(1_700_000_000)
	if !decaySQLContainsMath(expr) {
		t.Fatalf("表达式未使用 exp()/ln(): %s", expr)
	}
	if want := "MAX(0.05, MIN(1.0,"; len(expr) < len(want) || expr[:len(want)] != want {
		t.Fatalf("表达式未按「先饱和再落地板」排列: %s", expr)
	}
}
