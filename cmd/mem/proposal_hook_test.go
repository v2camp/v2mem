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

// TestSessionStartSpawnsProposalUnconditionally：SessionStart 是自动写侧的**唯一**常规路径。
// S12 后 MEM_NO_DISTILL / MEM_DISTILL_SPAWN 双轨闸已退役，唯一关断是 MEM_NO_AUTOMATION
// 总闸——开关越多，「自动化在不在跑」越难一句话回答（§12.4）。
func TestSessionStartSpawnsProposalUnconditionally(t *testing.T) {
	t.Run("无任何 env（生产裸跑）也拉起", func(t *testing.T) {
		unsetGrayEnv(t, "MEM_NO_AUTOMATION")
		unsetGrayEnv(t, "MEM_NO_DISTILL")
		unsetGrayEnv(t, "MEM_DISTILL_SPAWN")
		calls := installCountProc(t)
		distillPendingFailOpen(grayDB(t))
		if *calls != 1 {
			t.Fatalf("生产裸跑应无条件拉起 1 次 proposal，got %d", *calls)
		}
	})
	t.Run("退役开关不关断", func(t *testing.T) {
		t.Setenv("MEM_NO_DISTILL", "1")
		t.Setenv("MEM_DISTILL_SPAWN", "0")
		calls := installCountProc(t)
		distillPendingFailOpen(grayDB(t))
		if *calls != 1 {
			t.Fatalf("退役开关必须被无视，got %d", *calls)
		}
	})
	t.Run("MEM_NO_AUTOMATION=1 短路", func(t *testing.T) {
		t.Setenv("MEM_NO_AUTOMATION", "1")
		calls := installCountProc(t)
		db := grayDB(t)
		distillPendingFailOpen(db)
		if *calls != 0 {
			t.Fatalf("总闸应关闭 SessionStart 蒸馏，got %d", *calls)
		}
		if _, err := os.Stat(db); err == nil {
			t.Fatal("短路后不应打开库（内联旧规则已删除）")
		}
	})
	t.Run("拉起失败 fail-open", func(t *testing.T) {
		unsetGrayEnv(t, "MEM_NO_AUTOMATION")
		prev := startDistillProc
		startDistillProc = func(string) error { return os.ErrPermission }
		t.Cleanup(func() { startDistillProc = prev })
		distillPendingFailOpen(grayDB(t))
	})
}
