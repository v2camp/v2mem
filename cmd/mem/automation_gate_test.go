package main

// S11：MEM_NO_AUTOMATION 唯一总闸的测试矩阵（design-data-lifecycle §12.4）。
//
// 覆盖面：四个开关两两组合（含总闸 × 每个既有开关）× 三种写侧事件。
// 断言的是**行为**：总闸=1 时三个自动 spawn 出口全部为零；既有
// MEM_NO_DISTILL / MEM_NO_COLDSTART 语义不受影响；SessionStart 的读侧注入仍在。
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
	name      string
	noAuto    bool
	noDistill bool
	noCold    bool
	spawn     bool
}

// 四个开关两两组合：总闸与每个既有开关各配一次，另保留三个单开关基线。
func automationCombos() []automationCombo {
	return []automationCombo{
		{"全关", false, false, false, false},
		{"总闸", true, false, false, false},
		{"总闸+NO_DISTILL", true, true, false, false},
		{"总闸+NO_COLDSTART", true, false, true, false},
		{"总闸+SPAWN", true, false, false, true},
		{"仅 NO_DISTILL", false, true, false, false},
		{"仅 NO_COLDSTART", false, false, true, false},
		{"仅 SPAWN", false, false, false, true},
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

func expectedAutomation(combo automationCombo, event harness.Event) (distill, cold, judge int) {
	if combo.noAuto {
		return 0, 0, 0
	}
	switch event {
	case harness.EventSessionStart:
		if !combo.noDistill {
			distill = 1
		}
	case harness.EventStop:
		if combo.spawn {
			distill = 1
		}
		judge = 1
	case harness.EventSessionEnd:
		if combo.spawn {
			distill = 1
		}
		if !combo.spawn && !combo.noDistill && !combo.noCold {
			cold = 1
		}
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
				t.Setenv("MEM_NO_DISTILL", boolEnv(combo.noDistill))
				t.Setenv("MEM_NO_COLDSTART", boolEnv(combo.noCold))
				t.Setenv("MEM_DISTILL_SPAWN", boolEnv(combo.spawn))
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
