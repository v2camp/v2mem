package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/v2camp/v2mem/internal/harness"
)

// startDistillProc 拉起一次性 proposal 蒸馏子进程。测试可替换。
// 钩子不等待它，也不把候选写进 memories。
var startDistillProc = defaultStartDistill

// distillProcArgs 是自动写侧的固定 argv：单轨后只有 proposal 一条路，
// 规则抽取由链尾 executor（--executor auto 的规则兜底）完成。
// 抽成纯函数便于直接断言，不靠拉起子进程间接验证参数。
func distillProcArgs() []string {
	return []string{"distill", "--proposal", "--executor", "auto", "--max-bundles", "1"}
}

func defaultStartDistill(db string) error {
	return startMemProc(db, distillProcArgs())
}

// startMemProc 拉起一个短命 mem 子进程，不等待它结束。
// 冷启动与常规蒸馏共用这一条出口，避免两处各自实现 .test 守卫与参数拼接。
//
// stderr 追加到 ~/.v2mem/spawn.log：这些子进程 detach 后没有终端，坏 key、
// 端点不可达、落库失败在界面上都表现为「什么都没发生」，没有日志就只能翻库猜。
func startMemProc(db string, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.HasSuffix(exe, ".test") {
		return fmt.Errorf("测试二进制不拉起 mem 子进程")
	}
	args = append(args, "--db", db)
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	if f, err := openSpawnLog(); err == nil {
		defer f.Close() // 子进程已继承 fd，父进程这份可以立刻关
		cmd.Stderr = f
	}
	return cmd.Start()
}

// openSpawnLog 打开 ~/.v2mem/spawn.log（追加，0644）。
// 打不开就算了：日志是观测手段，不能因为它失败而让钩子拉不起子进程。
func openSpawnLog() (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".v2mem")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "spawn.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// distillPendingFailOpen 在 SessionStart 拉起 proposal 子进程，兜住上一会话可能
// 漏掉的写侧蒸馏。单轨后这是自动写侧的唯一常规路径：SessionStart 无条件拉起，
// 唯一关断是 MEM_NO_AUTOMATION 总闸（S12 已退役 MEM_NO_DISTILL / MEM_DISTILL_SPAWN
// 双轨闸——开关越多，「自动化在不在跑」越难一句话回答，见 §12.4）。
//
// 子进程幂等：没有待处理证据窗口时 proposal 自然 skipped。拉起失败只打 stderr，
// 不阻断本会话的注入（fail-open）。
func distillPendingFailOpen(db string) {
	// 唯一总闸：MEM_NO_AUTOMATION=1 时 SessionStart 也不拉起写侧子进程。
	if automationDisabled() {
		return
	}
	if err := startDistillProc(db); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 proposal pending 失败，已放过:", err)
	}
}

// observeEventMayDistill 现在只决定冷启动挂点：Stop 每轮都来，冷启动只挂 SessionEnd。
func observeEventMayDistill(event harness.Event) bool {
	return event == harness.EventStop || event == harness.EventSessionEnd
}
