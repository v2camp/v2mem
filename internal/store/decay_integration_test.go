package store

import (
	"math"
	"testing"
	"time"
)

// 本文件固化「Ebbinghaus 派生分接入调用方」的行为，与 decay_test.go 分工：
// decay_test.go 测纯函数本身，这里测它接进 Search 与 GC 之后的效果。
// 单独成文件是为了让「接入改了什么」在 review 时一眼可见。

// TestSearchPopulatesDecayed 确认 Search 会把派生分算出来，且落在 [floor, 1]。
//
// 这个字段存在的意义是让调用方能解释排序：同 rank 时为什么这条在前。
// 若它恒为零，说明 SQL 表达式没接上（DecaySQLExprFor 拼错列名不会报错，
// 只会静默产出 NULL → 0，所以必须有断言钉住）。
func TestSearchPopulatesDecayed(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "检索派生分填充测试")
	setColumn(t, s, id, "salience", 0.8)
	setColumn(t, s, id, "last_seen_at", time.Now().Add(-100*24*time.Hour).Unix())

	hits, err := s.Search(SearchQuery{Query: "派生分", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("应命中 1 条，got %d", len(hits))
	}

	got := hits[0].Decayed
	cfg := DefaultDecayConfig()
	want := DecayedSalience(0.8, time.Now().Add(-100*24*time.Hour).Unix(), time.Now().Unix(), 0, cfg)

	if got < cfg.Floor || got > 1 {
		t.Errorf("派生分越界: %v（应在 [%v, 1]）", got, cfg.Floor)
	}
	// SQL 与 Go 的 exp/ln 有末位差异，用容差而非相等。
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("SQL 派生分与 Go 侧不一致: got %v, want %v", got, want)
	}
	// 原始 salience 必须保持原样：它是 notes 判硬规则的依据，不能被派生分污染。
	if hits[0].Salience != 0.8 {
		t.Errorf("Salience 被改写了: got %v, want 0.8", hits[0].Salience)
	}
}

// TestSearchOrdersByDecayedOnEqualRank 验证「同 bm25 时按派生分排序」。
//
// 用**完全相同的正文 + 不同工程**来保证 bm25 逐位相同：content_hash 的唯一约束是
// (hash, project)，因此两条可以共存；分词与文档长度完全一致，不存在归一化差异。
// 若换成「正文相近」的构造，一元/二元组集合会不同，bm25 就不再相等，测不到 tiebreak。
func TestSearchOrdersByDecayedOnEqualRank(t *testing.T) {
	s := newTestStore(t)

	freshRes, err := s.Add(AddInput{Content: "缓存预热策略", Project: "proj-fresh", Device: "test"})
	if err != nil {
		t.Fatalf("Add fresh: %v", err)
	}
	staleRes, err := s.Add(AddInput{Content: "缓存预热策略", Project: "proj-stale", Device: "test"})
	if err != nil {
		t.Fatalf("Add stale: %v", err)
	}
	fresh, stale := freshRes.ID, staleRes.ID

	// fresh 刚被访问且命中多；stale 两年没动、从未命中。
	setColumn(t, s, fresh, "last_seen_at", time.Now().Unix())
	setColumn(t, s, fresh, "access_count", 10)
	setColumn(t, s, stale, "last_seen_at", time.Now().Add(-730*24*time.Hour).Unix())
	setColumn(t, s, stale, "access_count", 0)

	// Scope=all 才能同时看到两个工程；否则工程过滤会让其中一条根本没被召回。
	hits, err := s.Search(SearchQuery{Query: "缓存预热策略", Limit: 5, Scope: "all"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("应命中 2 条，got %d", len(hits))
	}
	// 相同正文 ⇒ bm25 逐位相同 ⇒ 单路 SQL 的 ORDER BY 只能靠 decayed 分先后，
	// 名次又决定 RRF 贡献，最终体现在融合结果上。所以这里断言最终顺序即可，
	// 不要拿 Hit.Rank 当 bm25 用——它在融合后已被改写成名次（1、2…）。
	if hits[0].ID != fresh {
		t.Errorf("同等相关时应由派生分决定顺序，期望新近的 %s 在前，got %s（decayed %.4f vs %.4f）",
			fresh, hits[0].ID, hits[0].Decayed, hits[1].Decayed)
	}
	if hits[0].Decayed <= hits[1].Decayed {
		t.Errorf("排序与派生分矛盾: %.6f <= %.6f", hits[0].Decayed, hits[1].Decayed)
	}
}

// TestGCUsesDecayedScoreNotRawSalience 固化这处**语义变化**。
//
// 旧判据是「salience < 阈值」——静态值，写进去是多少就永远是多少，
// 一条高 salience 的记忆无论多久没被用到都不会被回收。
// 新判据是「派生分 < 阈值」——重要性随时间衰减，这正是遗忘曲线的意图。
//
// 该用例的存在价值是：将来若有人把判据改回静态 salience，
// 这里必须失败，而不是让语义悄悄回退。
func TestGCUsesDecayedScoreNotRawSalience(t *testing.T) {
	s := newTestStore(t)

	// 三年未命中的高 salience：派生分跌破 0.05 的地板后必然低于阈值 → 回收。
	forgotten := mustAdd(t, s, "三年没被用到的重要记忆")
	setColumn(t, s, forgotten, "salience", 0.9)
	setColumn(t, s, forgotten, "last_seen_at", time.Now().Add(-1095*24*time.Hour).Unix())

	// 刚命中过的低 salience：虽低于阈值但派生分被新近度托住 → 保留。
	fresh := mustAdd(t, s, "刚用过但不重要的记忆")
	setColumn(t, s, fresh, "salience", 0.15)
	setColumn(t, s, fresh, "last_seen_at", time.Now().Unix())

	res, err := s.GC(720*time.Hour, 0.2)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Decayed != 1 {
		t.Errorf("应只回收长期未命中的那条，Decayed=%d", res.Decayed)
	}
	if n := countAll(t, s); n != 1 {
		t.Fatalf("应保留 1 条，got %d", n)
	}
	var remaining string
	if err := s.db.QueryRow(`SELECT id FROM memories`).Scan(&remaining); err != nil {
		t.Fatalf("查剩余: %v", err)
	}
	if remaining != fresh {
		t.Errorf("保留的应是刚命中过的那条")
	}
}

// TestGCHonorsIdleGateEvenAtFloor 确认 maxIdle 仍是硬门槛：
// 派生分再低，也必须闲置满 maxIdle 才进入候选，否则刚写入的低声望记忆
// 会在同一次 gc 里被立刻清掉。
func TestGCHonorsIdleGateEvenAtFloor(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "刚写入且声望极低的记忆")
	setColumn(t, s, id, "salience", 0.0)                       // 派生分会落到 floor
	setColumn(t, s, id, "last_seen_at", time.Now().Add(-time.Hour).Unix()) // 但只闲置 1 小时

	res, err := s.GC(720*time.Hour, 0.2)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Decayed != 0 {
		t.Errorf("未满 maxIdle 不应进入回收候选，Decayed=%d", res.Decayed)
	}
	if n := countAll(t, s); n != 1 {
		t.Errorf("应保留，got %d", n)
	}
}
