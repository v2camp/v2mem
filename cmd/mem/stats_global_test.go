package main

import (
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// mem stats 必须呈现全库数据分布：本机 8MB 库 memories=0 时，
// 旧版输出「总条数 0」让人误以为库是空的，观测/候选全被隐身。
func TestCmdStatsShowsTableDistributionWhenNoMemories(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.AppendObservation(store.ObsInput{
		SessionID: "s", Event: "stop", Summary: "只有观测没有记忆",
	}); err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}
	st.Close()

	out, err := captureStdout(t, func() error { return cmdStats([]string{"--db", db}) })
	if err != nil {
		t.Fatalf("cmdStats: %v", err)
	}
	if !strings.Contains(out, "按表") {
		t.Errorf("输出应含「按表」分布行，got:\n%s", out)
	}
	if !strings.Contains(out, "observations=1") {
		t.Errorf("输出应含 observations=1，got:\n%s", out)
	}
	if !strings.Contains(out, "总条数   1") {
		t.Errorf("总条数应为全库口径 1，got:\n%s", out)
	}
}
