package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

func installColdStartProc(t *testing.T, err error) *int {
	t.Helper()
	n := 0
	prev := startColdStartProc
	startColdStartProc = func(db, session string) error {
		n++
		return err
	}
	t.Cleanup(func() { startColdStartProc = prev })
	return &n
}

func coldStartInput(sid string) hookInput {
	return hookInput{SessionID: sid, TranscriptPath: "/tmp/t.jsonl"}
}

// clearColdStartEnv 取消自动化相关 env，避免宿主机环境让用例假绿。
// S12 后唯一开关是 MEM_NO_AUTOMATION；退役的 MEM_NO_DISTILL / MEM_NO_COLDSTART /
// MEM_DISTILL_SPAWN 也一并清掉，证明它们的残留值不再影响行为。
func clearColdStartEnv(t *testing.T) {
	t.Helper()
	unsetGrayEnv(t, "MEM_NO_AUTOMATION")
	unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
	unsetGrayEnv(t, "MEM_NO_DISTILL")
	unsetGrayEnv(t, "MEM_NO_COLDSTART")
}

func TestColdStartSpawnsOnFirstSessionEndOnly(t *testing.T) {
	clearColdStartEnv(t)
	const sid = "s-cold"
	db := grayDB(t)
	calls := installColdStartProc(t, nil)

	// Stop 每轮都来，不能触发冷启动。
	maybeSpawnColdStart(db, coldStartInput(sid), "demo", harness.EventStop)
	if *calls != 0 {
		t.Fatalf("Stop 不应触发冷启动，got %d", *calls)
	}

	maybeSpawnColdStart(db, coldStartInput(sid), "demo", harness.EventSessionEnd)
	if *calls != 1 {
		t.Fatalf("SessionEnd 应触发一次冷启动，got %d", *calls)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, ok, err := st.MetaGet(coldStartMetaKey("demo")); err != nil || !ok || v == "" {
		t.Fatalf("冷启动后应写一次性标记，got %q %v %v", v, ok, err)
	}

	maybeSpawnColdStart(db, coldStartInput(sid), "demo", harness.EventSessionEnd)
	if *calls != 1 {
		t.Fatalf("同一工程只应冷启动一次，got %d", *calls)
	}
}

func TestColdStartSkipsWhenProjectHasMemory(t *testing.T) {
	clearColdStartEnv(t)
	const sid = "s-has-memory"
	db := grayDB(t)

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add(store.AddInput{Content: "已有记忆", Device: "test", Project: "demo"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	calls := installColdStartProc(t, nil)
	maybeSpawnColdStart(db, coldStartInput(sid), "demo", harness.EventSessionEnd)
	if *calls != 0 {
		t.Fatalf("工程已有记忆时不应冷启动，got %d", *calls)
	}
}

func TestColdStartSkipsOtherProjectsMemory(t *testing.T) {
	clearColdStartEnv(t)
	const sid = "s-other-project"
	db := grayDB(t)

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add(store.AddInput{Content: "别的工程记忆", Device: "test", Project: "other"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	calls := installColdStartProc(t, nil)
	maybeSpawnColdStart(db, coldStartInput(sid), "demo", harness.EventSessionEnd)
	if *calls != 1 {
		t.Fatalf("别的工程有记忆不应挡住本工程冷启动，got %d", *calls)
	}
}

// S12 后 MEM_DISTILL_SPAWN 退役：冷启动不再有「常规路径已覆盖」的互斥让位，
// 首会话冷启动照常发生（幂等，由标记兜底）。
func TestColdStartRunsAfterSpawnGateRetired(t *testing.T) {
	clearColdStartEnv(t)
	t.Setenv("MEM_DISTILL_SPAWN", "1") // 退役开关：必须被无视
	const sid = "s-spawn-on"
	db := grayDB(t)

	calls := installColdStartProc(t, nil)
	maybeSpawnColdStart(db, coldStartInput(sid), "demo", harness.EventSessionEnd)
	if *calls != 1 {
		t.Fatalf("MEM_DISTILL_SPAWN 退役后冷启动应照常拉起，got %d", *calls)
	}
}

func TestColdStartKillSwitch(t *testing.T) {
	clearColdStartEnv(t)
	t.Setenv("MEM_NO_AUTOMATION", "1")
	const sid = "s-killed"
	db := grayDB(t)

	calls := installColdStartProc(t, nil)
	maybeSpawnColdStart(db, coldStartInput(sid), "demo", harness.EventSessionEnd)
	if *calls != 0 {
		t.Fatalf("MEM_NO_AUTOMATION=1 应关闭冷启动，got %d", *calls)
	}
}

func TestColdStartSkipsIncompleteInput(t *testing.T) {
	clearColdStartEnv(t)
	db := grayDB(t)
	calls := installColdStartProc(t, nil)

	cases := []struct {
		name string
		in   hookInput
		proj string
	}{
		{"缺 transcript", hookInput{SessionID: "s1"}, "demo"},
		{"缺 session", hookInput{TranscriptPath: "/tmp/t.jsonl"}, "demo"},
		{"缺 project", coldStartInput("s3"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			maybeSpawnColdStart(db, tc.in, tc.proj, harness.EventSessionEnd)
			if *calls != 0 {
				t.Fatalf("输入不完整时不应冷启动，got %d", *calls)
			}
		})
	}
}

// TestColdStartSpawnsWithoutObservations：单轨后 coldstart 不再做 due 判定。
// proposal 幂等，没有证据窗口时会 noop 消费窗口，因此无观测也照常拉起。
func TestColdStartSpawnsWithoutObservations(t *testing.T) {
	clearColdStartEnv(t)
	db := grayDB(t)
	calls := installColdStartProc(t, nil)

	maybeSpawnColdStart(db, coldStartInput("s-empty"), "demo", harness.EventSessionEnd)
	if *calls != 1 {
		t.Fatalf("无 due 判定后 coldstart 应照常拉起，got %d", *calls)
	}
}

func TestColdStartSpawnFailureDoesNotMark(t *testing.T) {
	clearColdStartEnv(t)
	const sid = "s-fail"
	db := grayDB(t)
	calls := installColdStartProc(t, errors.New("spawn 失败"))

	maybeSpawnColdStart(db, coldStartInput(sid), "demo", harness.EventSessionEnd)
	if *calls != 1 {
		t.Fatalf("失败路径应尝试 1 次，got %d", *calls)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok, err := st.MetaGet(coldStartMetaKey("demo")); err != nil || ok {
		t.Fatalf("拉起失败不应写标记，got ok=%v err=%v", ok, err)
	}

	// 没写标记，下一次 SessionEnd 仍可重试。
	maybeSpawnColdStart(db, coldStartInput(sid), "demo", harness.EventSessionEnd)
	if *calls != 2 {
		t.Fatalf("失败后应允许重试，got %d", *calls)
	}
}

// TestCmdHookSessionEndTriggersColdStart 验证入口接线：不是只有直接调
// maybeSpawnColdStart 才生效，真实的 SessionEnd 钩子路径也必须走到。
func TestCmdHookSessionEndTriggersColdStart(t *testing.T) {
	clearColdStartEnv(t)
	db := testDB(t)
	proj, _ := hookProjectDir(t, "cold-repo")
	const sid = "s-cmd-cold"

	calls := installColdStartProc(t, nil)
	raw, _ := json.Marshal(map[string]string{
		"hook_event_name": "SessionEnd",
		"cwd":             proj,
		"session_id":      sid,
		"transcript_path": "/tmp/t.jsonl",
	})
	if _, err := withStdin(t, string(raw), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--audit", "-", "--dedup-window", "0"})
	}); err != nil {
		t.Fatalf("cmdHook(SessionEnd): %v", err)
	}
	if *calls != 1 {
		t.Fatalf("SessionEnd 钩子应触发一次冷启动，got %d", *calls)
	}
}

func TestDefaultColdStartDistillRefusesTestBinary(t *testing.T) {
	if err := defaultColdStartDistill(testDB(t), "s"); err == nil {
		t.Fatal("测试二进制不应真的拉起冷启动蒸馏")
	}
}
