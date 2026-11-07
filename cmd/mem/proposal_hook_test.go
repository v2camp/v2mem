package main

import (
	"os"
	"path/filepath"
	"testing"
)

// unsetGrayEnv 真正取消某个环境变量（区别于 t.Setenv 设为空串），
// 用于覆盖「未设置」这一默认分支。
func unsetGrayEnv(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// grayDB 返回 t.TempDir() 内的临时库路径，测试绝不碰 ~/.v2mem。
func grayDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "hook.db")
}

func TestProposalHookFlagSpawnsWithoutTranscript(t *testing.T) {
	t.Setenv("MEM_DISTILL_SPAWN", "1")
	t.Setenv("MEM_PROPOSAL_HOOK", "1")
	var called bool
	prev := startDistillProc
	startDistillProc = func(db, session, harness string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { startDistillProc = prev })

	maybeSpawnDistill("test.db", hookInput{SessionID: "s-proposal"}, "claude")
	if !called {
		t.Fatal("MEM_PROPOSAL_HOOK=1 时 Stop 应拉起 Proposal，而不是被 transcript_path 挡住")
	}
}

func TestProposalHookPendingSpawnsOnSessionStart(t *testing.T) {
	t.Setenv("MEM_PROPOSAL_HOOK", "1")
	var called bool
	prev := startDistillProc
	startDistillProc = func(db, session, harness string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { startDistillProc = prev })

	distillPendingFailOpen("test.db", "-", "claude")
	if !called {
		t.Fatal("MEM_PROPOSAL_HOOK=1 时 SessionStart 应拉起 Proposal pending")
	}
}

// TestGrayProposalHookEnabled 覆盖选择器矩阵：未设置 / 0 / 空串 / 1 / all / on /
// 单宿主名单 / 逗号名单，与当前 harness 的组合，含大小写与空格。
func TestGrayProposalHookEnabled(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		unset   bool
		harness string
		want    bool
	}{
		{"未设置默认关闭", "", true, "claude", false},
		{"空串默认关闭", "", false, "claude", false},
		{"0 默认关闭", "0", false, "claude", false},
		{"0 去空格后关闭", "  0  ", false, "claude", false},
		{"1 全量 claude", "1", false, "claude", true},
		{"1 全量 trae", "1", false, "trae", true},
		{"1 全量未知宿主", "1", false, "unknown", true},
		{"all 全量", "all", false, "trae", true},
		{"on 全量", "on", false, "trae", true},
		{"ALL 大小写不敏感", "ALL", false, "trae", true},
		{"On 大小写不敏感", "On", false, "trae", true},
		{"claude 命中 claude", "claude", false, "claude", true},
		{"claude 非目标 trae 关闭", "claude", false, "trae", false},
		{"trae 命中 trae", "trae", false, "trae", true},
		{"trae 非目标 claude 关闭", "trae", false, "claude", false},
		{"CLAUDE 大小写不敏感", "CLAUDE", false, "claude", true},
		{"单名单去空格", "  claude  ", false, "claude", true},
		{"名单 claude,trae 命中 claude", "claude,trae", false, "claude", true},
		{"名单 claude,trae 命中 trae", "claude,trae", false, "trae", true},
		{"名单 claude,trae 非目标 cursor 关闭", "claude,trae", false, "cursor", false},
		{"名单项带空格去空格", "claude, trae", false, "trae", true},
		{"名单大小写混合", "Claude, TRAE", false, "trae", true},
		{"未知名单关闭", "cursor", false, "claude", false},
		{"空 harness 名单不命中", "claude", false, "", false},
		{"空 harness 全量仍生效", "1", false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				unsetGrayEnv(t, "MEM_PROPOSAL_HOOK")
			} else {
				t.Setenv("MEM_PROPOSAL_HOOK", tc.env)
			}
			if got := proposalHookEnabled(tc.harness); got != tc.want {
				t.Fatalf("MEM_PROPOSAL_HOOK=%q harness=%q: got %v, want %v", tc.env, tc.harness, got, tc.want)
			}
		})
	}
}

type graySpawnCase struct {
	name    string
	env     string
	harness string
	want    bool
}

// graySpawnCases 是 Stop / SessionStart 两条路径共用的灰度矩阵。
func graySpawnCases() []graySpawnCase {
	return []graySpawnCase{
		{"灰度 claude 命中 claude 拉起", "claude", "claude", true},
		{"灰度 claude 非目标 trae 不拉起", "claude", "trae", false},
		{"灰度 trae 命中 trae 拉起", "trae", "trae", true},
		{"灰度 trae 非目标 claude 不拉起", "trae", "claude", false},
		{"名单 claude,trae 命中 claude", "claude,trae", "claude", true},
		{"名单 claude,trae 命中 trae", "claude,trae", "trae", true},
		{"名单 claude,trae 非目标 cursor 不拉起", "claude,trae", "cursor", false},
		{"全量 1 任意宿主拉起", "1", "cursor", true},
		{"0 默认不拉起", "0", "claude", false},
	}
}

type graySpawnProbe struct {
	called  bool
	harness string
}

// installGrayProc 安装替换函数，记录是否被调用与逐层传入的 harness。
func installGrayProc(t *testing.T) *graySpawnProbe {
	t.Helper()
	p := &graySpawnProbe{}
	prev := startDistillProc
	startDistillProc = func(db, session, harness string) error {
		p.called = true
		p.harness = harness
		return nil
	}
	t.Cleanup(func() { startDistillProc = prev })
	return p
}

// TestGrayStopSpawnByHarness：Stop 路径。无 transcript_path 时只有 Proposal 分支会
// 拉起 startDistillProc，旧蒸馏会被挡住，因此 called 精确反映「是否拉起 Proposal」。
func TestGrayStopSpawnByHarness(t *testing.T) {
	for _, tc := range graySpawnCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEM_DISTILL_SPAWN", "1")
			t.Setenv("MEM_PROPOSAL_HOOK", tc.env)
			p := installGrayProc(t)
			maybeSpawnDistill(grayDB(t), hookInput{SessionID: "s-gray"}, tc.harness)
			if p.called != tc.want {
				t.Fatalf("MEM_PROPOSAL_HOOK=%q harness=%q: 拉起 Proposal=%v, want %v", tc.env, tc.harness, p.called, tc.want)
			}
			if p.called && p.harness != tc.harness {
				t.Fatalf("harness 未逐层传入: got %q, want %q", p.harness, tc.harness)
			}
		})
	}
}

// TestGraySessionStartSpawnByHarness：SessionStart 路径。Proposal 分支会拉起
// startDistillProc；旧蒸馏分支只做同步 distillOne，不拉起子进程。
func TestGraySessionStartSpawnByHarness(t *testing.T) {
	for _, tc := range graySpawnCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEM_NO_DISTILL", "") // 避免被早退分支短路
			t.Setenv("MEM_PROPOSAL_HOOK", tc.env)
			p := installGrayProc(t)
			distillPendingFailOpen(grayDB(t), "-", tc.harness)
			if p.called != tc.want {
				t.Fatalf("MEM_PROPOSAL_HOOK=%q harness=%q: 拉起 Proposal=%v, want %v", tc.env, tc.harness, p.called, tc.want)
			}
			if p.called && p.harness != tc.harness {
				t.Fatalf("harness 未逐层传入: got %q, want %q", p.harness, tc.harness)
			}
		})
	}
}

// TestProposalHookDefaultNoSpawn：默认值绝对不变——无环境变量时选择器仍判旧蒸馏，
// 且 MEM_DISTILL_SPAWN 未设时不拉起任何进程。
func TestProposalHookDefaultNoSpawn(t *testing.T) {
	t.Run("MEM_DISTILL_SPAWN 未设不拉起", func(t *testing.T) {
		unsetGrayEnv(t, "MEM_PROPOSAL_HOOK")
		unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
		if proposalHookEnabled("claude") {
			t.Fatal("默认值未改变：无环境变量时 proposalHookEnabled 必须为 false（旧蒸馏）")
		}
		p := installGrayProc(t)
		// 即便补齐 transcript_path，MEM_DISTILL_SPAWN 未设也应直接放过。
		maybeSpawnDistill(grayDB(t), hookInput{SessionID: "s-default", TranscriptPath: "/tmp/t.jsonl"}, "claude")
		if p.called {
			t.Fatal("MEM_DISTILL_SPAWN 未设时不应拉起任何 distill/proposal")
		}
	})
	t.Run("默认走旧蒸馏而非 Proposal", func(t *testing.T) {
		unsetGrayEnv(t, "MEM_PROPOSAL_HOOK")
		t.Setenv("MEM_DISTILL_SPAWN", "1")
		p := installGrayProc(t)
		// 无 transcript_path：旧蒸馏分支跳过；若默认被误改成 Proposal 则会拉起。
		maybeSpawnDistill(grayDB(t), hookInput{SessionID: "s-default"}, "claude")
		if p.called {
			t.Fatal("默认值未改变：默认应是旧蒸馏（缺 transcript_path 不拉起），却拉起了 Proposal")
		}
	})
}
