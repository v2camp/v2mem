package main

// S12 命令面收编 + 开关收敛 + MCP 统一 的判据用例（docs/design-data-lifecycle.md §12）。
//
// C1 退役 flag：distill 的 --session/--pending/--provider/--since 与
//     import-transcript 的 --into 删除后，必须按「未定义 flag」报错且退出码 2
//     （与未知子命令同级）。
// C2 开关收敛：MEM_NO_DISTILL / MEM_NO_COLDSTART / MEM_DISTILL_SPAWN 退役，
//     MEM_NO_AUTOMATION 是唯一总闸；生产钩子裸跑（无任何 env）行为不变。
// C6 judge --stats 退役：指向 mem review stats 的引导文案。

import (
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/harness"
)

// ---------- C1：退役 flag → 用法错误 + 退出码 2 ----------

func TestRetiredDistillFlagsAreUsageErrors(t *testing.T) {
	db := testDB(t)
	for _, args := range [][]string{
		{"--db", db, "--session", "s"},
		{"--db", db, "--pending"},
		{"--db", db, "--provider", "none"},
		{"--db", db, "--since", "24h"},
	} {
		err := cmdDistill(args)
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("退役 flag %v 应按未定义 flag 报错，got %v", args, err)
		}
	}
}

func TestRetiredImportIntoFlagIsUsageError(t *testing.T) {
	err := cmdImportTranscript([]string{"--db", testDB(t), "--into", "obs"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("--into 已退役，应按未定义 flag 报错，got %v", err)
	}
}

func TestRunUsageErrorsExitTwo(t *testing.T) {
	// 退役 flag 与未知子命令同级：都是用法错误，退出码 2（main 的可测形态 run）。
	for _, args := range [][]string{
		{"distill", "--session", "x"},
		{"distill", "--pending"},
		{"import-transcript", "--into", "both"},
		{"no-such-cmd"},
	} {
		if got := run(args); got != 2 {
			t.Errorf("run(%v) 退出码 = %d，want 2", args, got)
		}
	}
	if got := run([]string{"no-such-cmd"}); got != 2 {
		t.Errorf("未知子命令退出码 = %d，want 2", got)
	}
	// 正常 flag 组合不是用法错误：业务错误退出码仍为 1。
	if got := run([]string{"distill", "--proposal", "--db", testDB(t)}); got == 2 {
		t.Errorf("正常 flag 组合不应被判为用法错误，got %d", got)
	}
}

// ---------- C2：开关收敛 ----------

// 退役开关不再关断 SessionStart：唯一关断是 MEM_NO_AUTOMATION。
func TestSessionStartSpawnIgnoresRetiredSwitches(t *testing.T) {
	t.Setenv("MEM_NO_DISTILL", "1") // 退役开关：必须被无视
	calls := installCountProc(t)
	distillPendingFailOpen(grayDB(t))
	if *calls != 1 {
		t.Fatalf("MEM_NO_DISTILL 退役后 SessionStart 应照常拉起，got %d", *calls)
	}
}

func TestColdStartIgnoresRetiredSwitches(t *testing.T) {
	t.Setenv("MEM_NO_DISTILL", "1")
	t.Setenv("MEM_NO_COLDSTART", "1") // 退役开关：必须被无视
	calls := installColdStartProc(t, nil)
	maybeSpawnColdStart(grayDB(t), coldStartInput("s-retired"), "demo", harness.EventSessionEnd)
	if *calls != 1 {
		t.Fatalf("MEM_NO_DISTILL/MEM_NO_COLDSTART 退役后冷启动应照常拉起，got %d", *calls)
	}
}

// ---------- C6：judge --stats 退役 ----------

func TestCmdJudgeStatsRetiredGuidance(t *testing.T) {
	err := cmdJudge([]string{"--db", testDB(t), "--stats"})
	if err == nil {
		t.Fatal("judge --stats 已退役，应返回引导错误")
	}
	if !strings.Contains(err.Error(), "mem review stats") {
		t.Fatalf("引导文案必须指向 mem review stats，got %v", err)
	}
}
