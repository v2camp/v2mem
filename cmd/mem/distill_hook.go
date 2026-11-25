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

// distillSpawnEnabled 是 Stop / SessionEnd 常规蒸馏子进程的总闸。
// 单轨后它只管这两条收尾路径；SessionStart 的兜底不受此闸约束（见 distillPendingFailOpen），
// 首会话冷启动同样是一次性例外（coldstart.go）。
// 未设置 / 非 "1" → false，即默认不在 Stop / SessionEnd 拉起蒸馏子进程。
func distillSpawnEnabled() bool {
	return strings.TrimSpace(os.Getenv("MEM_DISTILL_SPAWN")) == "1"
}

// maybeSpawnDistill 在 Stop / SessionEnd 末尾可选地拉起 proposal 蒸馏。
// 默认不拉起；只要求 session_id 非空——proposal 从证据 Bundle 读输入，
// 不依赖 transcript_path，也不再做 SessionsNeedingDistill 的 due 判定。
// 失败只打 stderr。
func maybeSpawnDistill(db string, in hookInput) {
	if !distillSpawnEnabled() {
		return
	}
	if in.sessionID() == "" {
		fmt.Fprintln(os.Stderr, "v2mem hook: 无 session_id，跳过 proposal")
		return
	}
	if err := startDistillProc(db); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 proposal 失败，已放过:", err)
	}
}

// distillPendingFailOpen 在 SessionStart 拉起 proposal 子进程，兜住上一会话可能
// 漏掉的写侧蒸馏。单轨后这是自动写侧的主路径：SessionStart 无条件拉起，
// 唯一关断是 MEM_NO_DISTILL=1——若再受 MEM_DISTILL_SPAWN 约束，裸 hook 配置
// （生产默认）会让写侧静默停摆。
//
// 子进程幂等：没有待处理证据窗口时 proposal 自然 skipped。拉起失败只打 stderr，
// 不阻断本会话的注入（fail-open）。
func distillPendingFailOpen(db string) {
	if strings.TrimSpace(os.Getenv("MEM_NO_DISTILL")) == "1" {
		return
	}
	if err := startDistillProc(db); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 proposal pending 失败，已放过:", err)
	}
}

func observeEventMayDistill(event harness.Event) bool {
	return event == harness.EventStop || event == harness.EventSessionEnd
}
