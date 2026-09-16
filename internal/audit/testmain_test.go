package audit

import (
	"os"
	"testing"
)

// TestMain 把整个测试二进制的 HOME 指到临时目录。
//
// 为什么必须隔离：本包的 Path() 由 os.UserHomeDir() 解析，Read("")/Append("")
// 会落到那个默认位置。只要 HOME 是真的，测试就会**读写用户的真实审计日志** ——
// 实测发生过：一条测试记录被写进了 ~/.v2mem/audit.jsonl。
// 这与 cmd/mem 包的隔离是同一条纪律：任何会触碰用户级路径的包，测试都必须先隔离 HOME。
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "v2mem-audit-testhome-")
	if err != nil {
		panic("无法创建测试 HOME: " + err.Error())
	}
	origHome, hadHome := os.LookupEnv("HOME")
	origProfile, hadProfile := os.LookupEnv("USERPROFILE")
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home)

	code := m.Run()

	_ = os.RemoveAll(home)
	if hadHome {
		_ = os.Setenv("HOME", origHome)
	} else {
		_ = os.Unsetenv("HOME")
	}
	if hadProfile {
		_ = os.Setenv("USERPROFILE", origProfile)
	} else {
		_ = os.Unsetenv("USERPROFILE")
	}
	os.Exit(code)
}
