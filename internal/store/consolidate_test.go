package store

import (
	"strings"
	"testing"
)

// ---------- M5a: 相似知识归并 ----------
//
// 相似归并处理的是「不同 hash 但说的是一件事」的记忆：
//   - 相同内容（同 hash）由 M1 的覆盖机制处理，不走这里
//   - 归并后旧条目设 superseded_by 指向存活者，并从检索结果中隐去
//
// 测试数据的选取注意：必须满足相似度护栏的前置条件（长度 >= 10 字符、
// 数字一致、否定词计数一致），否则测到的是护栏分支而不是归并逻辑。

const (
	simBase    = "记忆库数据文件必须放在用户主目录的隐藏目录里"
	simVariant = "记忆库数据文件必须放在用户主目录下的隐藏目录里"
)

func TestConsolidateSupersedesNearDuplicateKeepsSalient(t *testing.T) {
	s := newTestStore(t)
	keep := addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	dup := addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})

	res, err := s.Consolidate(0.7)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.Superseded != 1 {
		t.Fatalf("应取代 1 条，got %+v", res)
	}

	var sup *string
	if err := s.db.QueryRow(`SELECT superseded_by FROM memories WHERE id = ?`, dup).Scan(&sup); err != nil {
		t.Fatalf("查询被取代者: %v", err)
	}
	if sup == nil {
		t.Fatal("低 importance 的近重复应被标记 superseded_by")
	}
	if *sup != keep {
		t.Errorf("superseded_by 应指向高 salience 的存活者 %s，got %s", keep[:8], (*sup)[:8])
	}

	var survivorSup *string
	if err := s.db.QueryRow(`SELECT superseded_by FROM memories WHERE id = ?`, keep).Scan(&survivorSup); err != nil {
		t.Fatalf("查询存活者: %v", err)
	}
	if survivorSup != nil {
		t.Error("存活者不应有 superseded_by")
	}
}

// 被取代的记忆必须从检索结果中消失，否则用户会看到两条互相矛盾的答案。
func TestSupersededMemoriesAreExcludedFromSearch(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})

	before, err := s.Search(SearchQuery{Query: "隐藏目录", Limit: 10})
	if err != nil {
		t.Fatalf("Search before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("前置条件：归并前应检索到 2 条，got %d", len(before))
	}

	if _, err := s.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	after, err := s.Search(SearchQuery{Query: "隐藏目录", Limit: 10})
	if err != nil {
		t.Fatalf("Search after: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("归并后应只剩 1 条，got %d", len(after))
	}
	if after[0].Content != simBase {
		t.Errorf("留下的应是高 salience 的那条，got %q", after[0].Content)
	}
}

// 归并必须幂等：第二次跑不产生新的取代，也不改库状态。
func TestConsolidateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})

	if _, err := s.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate #1: %v", err)
	}
	first := supersessionSnapshot(t, s)

	second, err := s.Consolidate(0.7)
	if err != nil {
		t.Fatalf("Consolidate #2: %v", err)
	}
	if second.Superseded != 0 {
		t.Errorf("第二次不应再取代任何条目，got %+v", second)
	}
	if got := supersessionSnapshot(t, s); got != first {
		t.Errorf("第二次归并改变了取代关系\n首次: %s\n再次: %s", first, got)
	}
	if n := countAll(t, s); n != 2 {
		t.Errorf("归并不应删除行（保留可追溯），got %d", n)
	}
}

// supersessionSnapshot 把「谁取代了谁」的映射序列化成可比较的字符串。
func supersessionSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, COALESCE(superseded_by, '') FROM memories ORDER BY id`)
	if err != nil {
		t.Fatalf("查询取代关系: %v", err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var id, sup string
		if err := rows.Scan(&id, &sup); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if sup != "" {
			sup = sup[:8]
		}
		parts = append(parts, id[:8]+"->"+sup)
	}
	return strings.Join(parts, ",")
}

// 跨工程不得互相归并：同一句话在两个工程里是两条独立记忆。
func TestConsolidateDoesNotMergeAcrossProjects(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: simBase, Project: "projA", Salience: 0.9})
	addWith(t, s, AddInput{Content: simVariant, Project: "projB", Salience: 0.3})

	res, err := s.Consolidate(0.7)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.Superseded != 0 {
		t.Errorf("跨工程不应归并，got %+v", res)
	}
}

// 🔴 护栏接线验证：相似度极高但数字不同（开关 0 vs 1）时不得归并。
// 这条用例的意义是证明 similarity.Judge 真的接在归并路径上，而不只是包内自测通过。
func TestConsolidateRefusesDigitReversal(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: "构建时必须关闭 CGO 开关参数设为 0", Project: "p1", Salience: 0.9})
	addWith(t, s, AddInput{Content: "构建时必须关闭 CGO 开关参数设为 1", Project: "p1", Salience: 0.3})

	res, err := s.Consolidate(0.5) // 阈值刻意压低，确保不是靠阈值拦下的
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.Superseded != 0 {
		t.Errorf("数字反转必须被护栏拦下（阈值已压到 0.5），got %+v", res)
	}
}

// 阈值抬高后近重复不应被归并 —— 证明阈值参数真的生效。
func TestConsolidateRespectsThreshold(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})

	res, err := s.Consolidate(0.999)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.Superseded != 0 {
		t.Errorf("阈值 0.999 时不应归并，got %+v", res)
	}
}

// salience 相同时按 access_count 降序，再相同按 created_at 升序 —— 结果必须确定。
func TestConsolidateTieBreaksDeterministically(t *testing.T) {
	s := newTestStore(t)
	a := addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.5})
	b := addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.5})
	setColumn(t, s, a, "access_count", 1)
	setColumn(t, s, b, "access_count", 9)

	res, err := s.Consolidate(0.7)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.Superseded != 1 {
		t.Fatalf("应取代 1 条，got %+v", res)
	}
	var sup *string
	if err := s.db.QueryRow(`SELECT superseded_by FROM memories WHERE id = ?`, a).Scan(&sup); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if sup == nil || *sup != b {
		t.Errorf("salience 相同时应保留 access_count 更高者 %s，got %v", b[:8], sup)
	}
}

// 被取代者的标记必须并入存活者，否则标记承载的机器/工程信息会随归并丢失。
func TestConsolidateTransfersTagsToSurvivor(t *testing.T) {
	s := newTestStore(t)
	keep := addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.9,
		Tags: map[string]string{"machine": "mini"}})
	dup := addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.3,
		Tags: map[string]string{"topic": "sqlite"}})

	if _, err := s.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tags WHERE memory_id = ?`, keep).Scan(&n); err != nil {
		t.Fatalf("查询存活者标记: %v", err)
	}
	if n != 2 {
		t.Errorf("存活者应接管被取代者的标记，期望 2 个，got %d", n)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tags WHERE memory_id = ?`, dup).Scan(&n); err != nil {
		t.Fatalf("查询被取代者标记: %v", err)
	}
	if n != 0 {
		t.Errorf("被取代者的标记应已转移，got %d", n)
	}
}

// 三条近重复应聚成一个簇，只留 1 条、取代 2 条。
func TestConsolidateClustersMoreThanTwo(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})
	addWith(t, s, AddInput{Content: "记忆库数据文件必须放在用户主目录的隐藏文件夹里", Project: "p1", Salience: 0.4})

	res, err := s.Consolidate(0.7)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.Superseded != 2 {
		t.Errorf("三条近重复应取代 2 条，got %+v", res)
	}
	if res.Groups != 1 {
		t.Errorf("应聚成 1 个簇，got %d", res.Groups)
	}
}

func TestConsolidateIgnoresExpiredMemories(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	expired := addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})
	setColumn(t, s, expired, "expires_at", 1) // 早已过期

	res, err := s.Consolidate(0.7)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.Superseded != 0 {
		t.Errorf("已过期的记忆不该参与归并，got %+v", res)
	}
}
