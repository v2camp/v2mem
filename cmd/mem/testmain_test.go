package main

import (
	"os"
	"strings"
	"testing"
)

// TestMain 把整个测试二进制的 HOME 指到临时目录。
//
// 这是**隔离级别的**修复，不是逐用例的补丁：`mem init` 在未给 --project 时
// 会写「用户级配置」路径，而那些路径由 harness.ExpandHome 从 HOME 解析。
// 只要 HOME 是真实的，任何一条忘了带 --project 的用例都会写进用户的真实配置 ——
// 实测已发生：~/.claude/settings.json 与 ~/.qoderworkcn/settings.json 被写入了
// go-build 临时二进制路径。逐条补 --project 治不了这个病，隔离 HOME 才能。
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "v2mem-testhome-")
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

// 固化这条护栏：不带 --project 时，写入目标必须落在测试 HOME 内，
// 绝不能指向真实家目录。否则将来有人改了 ExpandHome 的实现，污染会再次静默发生。
func TestInitWithoutProjectNeverTouchesRealHome(t *testing.T) {
	tmpHome := os.Getenv("HOME")
	if tmpHome == "" || !strings.Contains(tmpHome, "v2mem-testhome-") {
		t.Fatalf("测试 HOME 未生效，got %q", tmpHome)
	}
	out, err := withStdin(t, "codex\n", func() error { return cmdInit(nil) })
	if err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	if !strings.Contains(out, tmpHome) {
		t.Errorf("写入路径应位于测试 HOME 内，got:\n%s", out)
	}
	if strings.Contains(out, "/Users/") {
		t.Errorf("不得出现真实家目录路径，got:\n%s", out)
	}
}
