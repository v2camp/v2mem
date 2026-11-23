package store

import "testing"

// Stats.Total 是全库口径：观测表里明明有数据，即使 memories 为空也不能报 0。
// 这条正是本机 8MB 库却显示「总条数 0」的成因。
func TestStatsTotalCountsObservationsWhenNoMemories(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendObservation(ObsInput{
		SessionID: "s", Event: "stop", Summary: "库里只有观测没有记忆",
	}); err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}

	st, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Total == 0 {
		t.Fatal("Total = 0，期望计入 observations（memories 为空不代表全库为空）")
	}
}

// 全库分布：Total 必须等于各数据表行数之和，且运维表（去重/迁移/版本戳）
// 与 FTS 影子表不计入——它们是索引/痕迹，不是数据条数。
func TestStatsByTableExcludesOperationalTables(t *testing.T) {
	s := newTestStore(t)
	mustAdd(t, s, "一条记忆")
	if _, err := s.AppendObservation(ObsInput{
		SessionID: "s", Event: "stop", Summary: "一条观测",
	}); err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO hook_dedup(key, ts) VALUES('k', 1)`); err != nil {
		t.Fatalf("插入运维表: %v", err)
	}

	st, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.ByTable["memories"] != 1 || st.ByTable["observations"] != 1 {
		t.Fatalf("ByTable = %+v，期望 memories/observations 各 1", st.ByTable)
	}
	for _, op := range []string{"hook_dedup", "migration_marks", "db_meta"} {
		if _, ok := st.ByTable[op]; ok {
			t.Errorf("运维表 %s 不应计入 ByTable: %+v", op, st.ByTable)
		}
	}
	for name := range st.ByTable {
		if len(name) >= 4 && name[:4] == "fts_" {
			t.Errorf("FTS 影子表 %s 不应计入 ByTable", name)
		}
	}
	sum := 0
	for _, n := range st.ByTable {
		sum += n
	}
	if st.Total != sum {
		t.Errorf("Total = %d，期望等于 ByTable 之和 %d（%+v）", st.Total, sum, st.ByTable)
	}
}
