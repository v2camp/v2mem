package main

// S11/S12：MEM_NO_AUTOMATION 唯一总闸的测试矩阵（design-data-lifecycle §12.4）。
//
// S12 开关收敛后只剩两个口径：
//   - 裸跑（无任何 env，即生产钩子的默认形态）：SessionStart 拉起 proposal（自动写侧
//     唯一常规路径）、SessionEnd 一次性冷启动、Stop/SessionEnd 的 judge 按阈值触发；
//     Stop 不再有蒸馏 spawn（MEM_DISTILL_SPAWN 已退役）。这三个出口的次数在此锁定，
//     与收敛前的裸跑行为逐项一致 —— 退役开关不得改变 unset 语义。
//   - 总闸 MEM_NO_AUTOMATION=1：上述自动 spawn 全部为零；SessionStart 的读侧注入仍在。
//
// MEM_JUDGE_* 是配置不是闸（显式开/关与阈值/冷却），由 judge_hook_test 单独覆盖；
// 手动命令不经这些出口，另有单测证明总闸不误伤。

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

type automationCombo struct {
	name    string
	noAuto  bool
	spawnOn bool // 退役的 MEM_DISTILL_SPAWN：必须被无视，不得复活 Stop spawn
}

func automationCombos() []automationCombo {
	return []automationCombo{
		{"裸跑", false, false},
		{"裸跑+退役SPAWN", false, true},
		{"总闸", true, false},
		{"总闸+退役SPAWN", true, true},
	}
}

type automationCounters struct {
	distill, cold, judge *int
}

func installAutomationCounters(t *testing.T) automationCounters {
	t.Helper()
	d, c, j := 0, 0, 0
	prevD, prevC, prevJ := startDistillProc, startColdStartProc, startJudgeProc
	startDistillProc = func(string) error { d++; return nil }
	startColdStartProc = func(string, string) error { c++; return nil }
	startJudgeProc = func(string, int) error { j++; return nil }
	t.Cleanup(func() {
		startDistillProc, startColdStartProc, startJudgeProc = prevD, prevC, prevJ
	})
	return automationCounters{&d, &c, &j}
}

func automationStdin(t *testing.T, event, cwd string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"hook_event_name": event,
		"cwd":             cwd,
		"session_id":      "s-matrix",
		"transcript_path": "/tmp/matrix.jsonl",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedAutomationDB 造「足以触发 judge」的积压，且工程没有任何已采纳记忆（冷启动条件成立）。
func seedAutomationDB(t *testing.T, db string) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	seedPending(t, st, 25)
}

// expectedAutomation 锁「裸跑逐字节一致」的口径：S12 退役三个散闸后，
// 无 env 的 spawn 次数必须与收敛前完全相同（SessionStart 1 次 distill、
// SessionEnd 1 次冷启动、Stop/SessionEnd 各 1 次 judge、Stop 零 distill）。
func expectedAutomation(combo automationCombo, event harness.Event) (distill, cold, judge int) {
	if combo.noAuto {
		return 0, 0, 0
	}
	switch event {
	case harness.EventSessionStart:
		distill = 1
	case harness.EventStop:
		judge = 1
	case harness.EventSessionEnd:
		cold = 1
		judge = 1
	}
	return distill, cold, judge
}

func TestAutomationKillSwitchMatrix(t *testing.T) {
	events := []harness.Event{harness.EventSessionStart, harness.EventStop, harness.EventSessionEnd}
	for _, combo := range automationCombos() {
		for _, event := range events {
			t.Run(combo.name+"/"+string(event), func(t *testing.T) {
				// 每个子用例独立 HOME：judge 冷却状态按文件落盘，不能串味。
				t.Setenv("HOME", t.TempDir())
				t.Setenv("MEM_NO_AUTOMATION", boolEnv(combo.noAuto))
				t.Setenv("MEM_DISTILL_SPAWN", boolEnv(combo.spawnOn))
				// 固定打开 judge 自动触发的显式开关，让「总闸是否拦得住」可被观测。
				t.Setenv("MEM_JUDGE_SPAWN", "1")

				db := filepath.Join(t.TempDir(), "matrix.db")
				seedAutomationDB(t, db)
				projDir, _ := hookProjectDir(t, "matrix-repo")

				calls := installAutomationCounters(t)
				out, err := withStdin(t, automationStdin(t, string(event), projDir), func() error {
					return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
				})
				if err != nil {
					t.Fatalf("cmdHook 必须恒返回 nil（fail-open），got %v", err)
				}

				wantD, wantC, wantJ := expectedAutomation(combo, event)
				if *calls.distill != wantD || *calls.cold != wantC || *calls.judge != wantJ {
					t.Fatalf("spawn 次数 = distill %d / cold %d / judge %d，want %d/%d/%d",
						*calls.distill, *calls.cold, *calls.judge, wantD, wantC, wantJ)
				}

				if event == harness.EventSessionStart {
					// 总闸只拦自动化，不拦读侧注入。
					if !strings.Contains(out, "mem search") {
						t.Fatalf("SessionStart 的读侧注入不应受总闸影响，got:\n%s", out)
					}
				} else if strings.TrimSpace(out) != "" {
					t.Fatalf("观测类事件不应写 stdout，got:\n%s", out)
				}
			})
		}
	}
}

func boolEnv(on bool) string {
	if on {
		return "1"
	}
	return ""
}

// 总闸不误伤手动命令：add 照常写库，与钩子自动化无关。
func TestAutomationKillSwitchLeavesManualCommandsAlone(t *testing.T) {
	t.Setenv("MEM_NO_AUTOMATION", "1")
	db := filepath.Join(t.TempDir(), "manual.db")
	if _, err := captureStdout(t, func() error {
		return cmdAdd([]string{"--db", db, "--project", "p", "总闸不影响手动 add"})
	}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if n, err := st.ProjectMemoryCount("p"); err != nil || n != 1 {
		t.Fatalf("手动 add 应照常落库，got n=%d err=%v", n, err)
	}
}
