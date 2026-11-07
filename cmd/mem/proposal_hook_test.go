package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
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

// installCountProc 安装计数替身：每次被拉起就自增，返回计数器指针。
// 断言「不拉起」时配合「旧规则确实跑过」的旁证使用，避免「进程没起来所以计数为 0」的假绿。
func installCountProc(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := startDistillProc
	startDistillProc = func(db, session, harness string) error {
		n++
		return nil
	}
	t.Cleanup(func() { startDistillProc = prev })
	return &n
}

// TestProposalHookPendingSpawnsOnSessionStart：SessionStart 的 Proposal 分支受
// MEM_DISTILL_SPAWN 总闸约束，拆成「有许可」与「无许可」两条用例。
func TestProposalHookPendingSpawnsOnSessionStart(t *testing.T) {
	t.Run("SPAWN=1 且灰度命中拉起 Proposal", func(t *testing.T) {
		t.Setenv("MEM_DISTILL_SPAWN", "1")
		t.Setenv("MEM_PROPOSAL_HOOK", "1")
		calls := installCountProc(t)

		distillPendingFailOpen(grayDB(t), "-", "claude")
		if *calls == 0 {
			t.Fatal("MEM_DISTILL_SPAWN=1 且灰度命中时 SessionStart 应拉起 Proposal pending")
		}
		if *calls != 1 {
			t.Fatalf("SessionStart 应恰好拉起 1 次，got %d", *calls)
		}
	})
	t.Run("SPAWN 未设回落到内联旧规则", func(t *testing.T) {
		unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
		t.Setenv("MEM_PROPOSAL_HOOK", "1")
		db := grayDB(t)
		calls := installCountProc(t)

		distillPendingFailOpen(db, "-", "claude")
		if *calls != 0 {
			t.Fatalf("MEM_DISTILL_SPAWN 未设时不得拉起任何子进程，got %d 次", *calls)
		}
		// 反证：内联旧规则会打开库，库文件存在说明真的走到了旧规则分支，
		// 而不是被某个早退分支顺手跳过。
		if _, err := os.Stat(db); err != nil {
			t.Fatalf("无 spawn 许可时应继续走内联旧规则（会打开库），但库未创建: %v", err)
		}
	})
}

// TestSessionStartSpawnGateMatrix 覆盖 MEM_DISTILL_SPAWN × MEM_PROPOSAL_HOOK ×
// MEM_NO_DISTILL 的组合：只有「SPAWN=1 且灰度命中」才拉子进程，其余一律内联旧规则；
// MEM_NO_DISTILL=1 仍然最先短路，完全不动作。
func TestSessionStartSpawnGateMatrix(t *testing.T) {
	cases := []struct {
		name       string
		proposal   string
		propUnset  bool
		spawn      string
		spawnUnset bool
		noDistill  string
		harness    string
		wantSpawn  int
		wantLegacy bool
	}{
		{"SPAWN 未设+灰度命中:内联旧规则", "1", false, "", true, "", "claude", 0, true},
		{"SPAWN=1+灰度命中:拉 Proposal", "1", false, "1", false, "", "claude", 1, false},
		{"SPAWN=1+灰度未设:内联旧规则", "", true, "1", false, "", "claude", 0, true},
		{"SPAWN=1+灰度未命中:内联旧规则", "trae", false, "1", false, "", "claude", 0, true},
		{"NO_DISTILL=1 全不动作", "1", false, "1", false, "1", "claude", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.spawnUnset {
				unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
			} else {
				t.Setenv("MEM_DISTILL_SPAWN", tc.spawn)
			}
			if tc.propUnset {
				unsetGrayEnv(t, "MEM_PROPOSAL_HOOK")
			} else {
				t.Setenv("MEM_PROPOSAL_HOOK", tc.proposal)
			}
			t.Setenv("MEM_NO_DISTILL", tc.noDistill)
			db := grayDB(t)
			calls := installCountProc(t)

			distillPendingFailOpen(db, "-", tc.harness)

			if *calls != tc.wantSpawn {
				t.Fatalf("拉起次数=%d, want %d", *calls, tc.wantSpawn)
			}
			_, err := os.Stat(db)
			if tc.wantLegacy && err != nil {
				t.Fatalf("应走内联旧规则（会打开库），但库未创建: %v", err)
			}
			if !tc.wantLegacy && err == nil {
				t.Fatal("不该走到内联旧规则，却打开了库")
			}
		})
	}
}

// seedDueSession 造一个「有待蒸馏观测」的会话，供旧规则分支的 due 判定使用。
func seedDueSession(t *testing.T, db, sid string) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AppendObservation(store.ObsInput{
		SessionID: sid, Project: "demo", Event: "post-tool-use",
		Summary: "决策：gate 用例需要一条待蒸馏观测",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestStopSpawnGateMatrix：Stop / SessionEnd 路径的 gate 语义回归。
// SPAWN 未设时连库都不打开；SPAWN=1 且灰度命中走 Proposal；灰度未命中走旧规则，
// 且旧规则仍然受 due 判定约束（非 due 会话不会拉起）。
func TestStopSpawnGateMatrix(t *testing.T) {
	const sid = "s-due"
	t.Run("SPAWN 未设不拉", func(t *testing.T) {
		unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
		t.Setenv("MEM_PROPOSAL_HOOK", "1")
		db := grayDB(t)
		// 先造出 due 会话：若 gate 失效，旧规则会真的拉起，计数不会是 0。
		seedDueSession(t, db, sid)
		calls := installCountProc(t)

		maybeSpawnDistill(db, hookInput{SessionID: sid, TranscriptPath: "/tmp/t.jsonl"}, "claude")
		if *calls != 0 {
			t.Fatalf("MEM_DISTILL_SPAWN 未设时不应拉起任何进程，got %d", *calls)
		}
	})
	t.Run("SPAWN=1 且灰度命中拉 Proposal", func(t *testing.T) {
		t.Setenv("MEM_DISTILL_SPAWN", "1")
		t.Setenv("MEM_PROPOSAL_HOOK", "1")
		calls := installCountProc(t)

		// Proposal 分支不依赖 transcript_path 与 due 判定。
		maybeSpawnDistill(grayDB(t), hookInput{SessionID: sid}, "claude")
		if *calls != 1 {
			t.Fatalf("灰度命中应拉起 Proposal 1 次，got %d", *calls)
		}
	})
	t.Run("SPAWN=1 且灰度未命中走旧规则", func(t *testing.T) {
		t.Setenv("MEM_DISTILL_SPAWN", "1")
		t.Setenv("MEM_PROPOSAL_HOOK", "trae")
		calls := installCountProc(t)

		db := grayDB(t)
		seedDueSession(t, db, sid)
		maybeSpawnDistill(db, hookInput{SessionID: sid, TranscriptPath: "/tmp/t.jsonl"}, "claude")
		if *calls != 1 {
			t.Fatalf("灰度未命中时 due 会话应走旧规则拉起 1 次，got %d", *calls)
		}
		// 旧规则才有的 due 判定：非 due 会话不拉起，说明走的确实是旧规则分支。
		maybeSpawnDistill(db, hookInput{SessionID: "s-not-due", TranscriptPath: "/tmp/t.jsonl"}, "claude")
		if *calls != 1 {
			t.Fatalf("非 due 会话不应拉起旧规则，got %d", *calls)
		}
	})
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
		{"别名 trae 命中规范名 traecode", "trae", false, "traecode", true},
		{"规范名 traecode 命中别名 trae 名单", "traecode", false, "trae", true},
		{"别名 claude-code 命中 claude", "claude-code", false, "claude", true},
		{"别名 cc 命中 claude", "cc", false, "claude", true},
		{"大小写别名 TRAE 命中 traecode", "TRAE", false, "traecode", true},
		{"未知名单解析失败不命中", "cursorx", false, "claude", false},
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
// 必须在 SPAWN=1 的前提下跑，否则全部用例都会被总闸挡住而失去区分力。
func TestGraySessionStartSpawnByHarness(t *testing.T) {
	for _, tc := range graySpawnCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEM_NO_DISTILL", "") // 避免被早退分支短路
			t.Setenv("MEM_DISTILL_SPAWN", "1")
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

// TestGrayUnresolvedHarnessKeepsLegacy：未传 --harness 且环境变量无法推导宿主时，
// harnessName 返回空串。此时即便设了灰度名单，也必须保守走旧蒸馏、不得误开 Proposal；
// 全量值 1 不受宿主名影响（既有语义）。
func TestGrayUnresolvedHarnessKeepsLegacy(t *testing.T) {
	// 清掉注册表里全部项目目录环境变量，模拟「环境变量无法推导宿主名」。
	for _, k := range []string{
		"CLAUDE_PROJECT_DIR", "TRAE_PROJECT_DIR", "CODEBUDDY_PROJECT_DIR",
		"QODER_PROJECT_DIR", "QODERCN_PROJECT_DIR", "CURSOR_PROJECT_DIR",
	} {
		unsetGrayEnv(t, k)
	}
	host := harnessName("", hookInput{})
	if host != "" {
		t.Fatalf("环境变量无法推导时 harnessName 应为空，got %q", host)
	}

	t.Run("选择器保守失败", func(t *testing.T) {
		t.Setenv("MEM_PROPOSAL_HOOK", "claude")
		if proposalHookEnabled(host) {
			t.Fatal("宿主名无法解析时灰度名单应保守失败、保持旧蒸馏")
		}
	})
	t.Run("Stop 路径不拉起 Proposal", func(t *testing.T) {
		t.Setenv("MEM_DISTILL_SPAWN", "1")
		t.Setenv("MEM_PROPOSAL_HOOK", "claude")
		p := installGrayProc(t)
		maybeSpawnDistill(grayDB(t), hookInput{SessionID: "s-unresolved"}, host)
		if p.called {
			t.Fatal("宿主名无法解析时 Stop 不得拉起 Proposal（应走旧蒸馏）")
		}
	})
	t.Run("SessionStart 路径不拉起 Proposal", func(t *testing.T) {
		t.Setenv("MEM_NO_DISTILL", "")
		t.Setenv("MEM_DISTILL_SPAWN", "1") // 证明拦住它的是宿主名而非总闸
		t.Setenv("MEM_PROPOSAL_HOOK", "claude")
		db := grayDB(t)
		p := installGrayProc(t)
		distillPendingFailOpen(db, "-", host)
		if p.called {
			t.Fatal("宿主名无法解析时 SessionStart 不得拉起 Proposal（应走旧蒸馏）")
		}
		if _, err := os.Stat(db); err != nil {
			t.Fatalf("应走内联旧规则（会打开库），但库未创建: %v", err)
		}
	})
	t.Run("全量值不受宿主名影响", func(t *testing.T) {
		t.Setenv("MEM_PROPOSAL_HOOK", "1")
		if !proposalHookEnabled(host) {
			t.Fatal("全量值 1 应保持既有语义，不受宿主名无法解析影响")
		}
	})
}
