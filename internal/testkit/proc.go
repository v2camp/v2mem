package testkit

import (
	"errors"
	"os/exec"
)

// KillNow 立即杀掉子进程（Unix 上是 SIGKILL），模拟宿主/机器强杀。
// 它不等待进程退出——调用方需要自己 Wait 收尸并检查退出状态。
func KillNow(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("testkit.KillNow: 进程未启动")
	}
	return cmd.Process.Kill()
}
