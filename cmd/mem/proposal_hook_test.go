package main

import (
	"os"
	"path/filepath"
	"reflect"
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

// installCountProc 安装计数替身：每次被拉起就自增，返回计数器指针。
func installCountProc(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := startDistillProc
	startDistillProc = func(db string) error {
		n++
		return nil
	}
	t.Cleanup(func() { startDistillProc = prev })
	return &n
}

// TestDistillProcArgs 守单轨 argv：所有自动写侧都走 proposal，规则由链尾 executor 兜底。
func TestDistillProcArgs(t *testing.T) {
	want := []string{"distill", "--proposal", "--executor", "auto", "--max-bundles", "1"}
	if got := distillProcArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("distillProcArgs() = %v, want %v", got, want)
	}
}

// TestColdStartProcArgs 守冷启动也走 proposal（不再固定 --provider none）。
func TestColdStartProcArgs(t *testing.T) {
	if got, want := coldStartProcArgs(), distillProcArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("coldStartProcArgs() = %v, want %v", got, want)
	}
}

// TestMaybeSpawnDistillRequiresSessionOnly：Stop / SessionEnd 单轨后只看 session_id，
// transcript_path 与 due 窗口都不再是前置条件。
func TestMaybeSpawnDistillRequiresSessionOnly(t *testing.T) {
	t.Run("SPAWN=1 且只有 session_id 也拉起", func(t *testing.T) {
		t.Setenv("MEM_DISTILL_SPAWN", "1")
		calls := installCountProc(t)
		maybeSpawnDistill(grayDB(t), hookInput{SessionID: "s-only"})
		if *calls != 1 {
			t.Fatalf("只需 session_id 就应拉起 proposal，got %d", *calls)
		}
	})
	t.Run("SPAWN=1 且会话无待蒸馏观测也拉起", func(t *testing.T) {
		t.Setenv("MEM_DISTILL_SPAWN", "1")
		calls := installCountProc(t)
		// 新库无任何观测：旧规则会因 due 判定跳过；proposal 幂等，无窗口自然 skipped。
		maybeSpawnDistill(grayDB(t), hookInput{SessionID: "s-not-due", TranscriptPath: "/tmp/t.jsonl"})
		if *calls != 1 {
			t.Fatalf("proposal 不再做 due 判定，应直接拉起，got %d", *calls)
		}
	})
	t.Run("无 session_id 不拉起", func(t *testing.T) {
		t.Setenv("MEM_DISTILL_SPAWN", "1")
		calls := installCountProc(t)
		maybeSpawnDistill(grayDB(t), hookInput{TranscriptPath: "/tmp/t.jsonl"})
		if *calls != 0 {
			t.Fatalf("无 session_id 不应拉起，got %d", *calls)
		}
	})
	t.Run("MEM_DISTILL_SPAWN 未设不拉起", func(t *testing.T) {
		unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
		calls := installCountProc(t)
		maybeSpawnDistill(grayDB(t), hookInput{SessionID: "s-no-spawn", TranscriptPath: "/tmp/t.jsonl"})
		if *calls != 0 {
			t.Fatalf("MEM_DISTILL_SPAWN 未设时不得拉起，got %d", *calls)
		}
	})
	t.Run("拉起失败只打 stderr，不阻断钩子", func(t *testing.T) {
		t.Setenv("MEM_DISTILL_SPAWN", "1")
		prev := startDistillProc
		startDistillProc = func(string) error { return os.ErrPermission }
		t.Cleanup(func() { startDistillProc = prev })
		maybeSpawnDistill(grayDB(t), hookInput{SessionID: "s-fail"})
	})
}

// TestSessionStartSpawnsProposalUnconditionally：SessionStart 是自动写侧的主路径，
// 只受 MEM_NO_DISTILL 关断；MEM_DISTILL_SPAWN 不参与（否则单轨后写侧会静默停摆）。
func TestSessionStartSpawnsProposalUnconditionally(t *testing.T) {
	t.Run("两个开关都未设也拉起", func(t *testing.T) {
		unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
		t.Setenv("MEM_NO_DISTILL", "")
		calls := installCountProc(t)
		distillPendingFailOpen(grayDB(t))
		if *calls != 1 {
			t.Fatalf("SessionStart 应无条件拉起 1 次 proposal，got %d", *calls)
		}
	})
	t.Run("SPAWN 未设也拉起", func(t *testing.T) {
		unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
		t.Setenv("MEM_NO_DISTILL", "")
		calls := installCountProc(t)
		distillPendingFailOpen(grayDB(t))
		if *calls != 1 {
			t.Fatalf("SessionStart 不受 MEM_DISTILL_SPAWN 约束，got %d", *calls)
		}
	})
	t.Run("MEM_NO_DISTILL=1 短路", func(t *testing.T) {
		t.Setenv("MEM_NO_DISTILL", "1")
		t.Setenv("MEM_DISTILL_SPAWN", "1") // 证明拦住它的是 NO_DISTILL 而非 SPAWN
		calls := installCountProc(t)
		db := grayDB(t)
		distillPendingFailOpen(db)
		if *calls != 0 {
			t.Fatalf("MEM_NO_DISTILL=1 应关闭 SessionStart 蒸馏，got %d", *calls)
		}
		if _, err := os.Stat(db); err == nil {
			t.Fatal("短路后不应打开库（内联旧规则已删除）")
		}
	})
	t.Run("拉起失败 fail-open", func(t *testing.T) {
		t.Setenv("MEM_NO_DISTILL", "")
		prev := startDistillProc
		startDistillProc = func(string) error { return os.ErrPermission }
		t.Cleanup(func() { startDistillProc = prev })
		distillPendingFailOpen(grayDB(t))
	})
}
