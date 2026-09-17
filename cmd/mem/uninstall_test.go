package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initCodex 把 codex 钩子装进 proj，返回写出的配置文件路径与命令。
func initCodex(t *testing.T, proj string) string {
	t.Helper()
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "codex", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	return filepath.Join(proj, ".codex", "hooks.json")
}

func readCfg(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal %s: %v", path, err)
	}
	return cfg
}

// extractCommands 收集指定事件里所有 hooks 的 command。
func extractCommands(cfg map[string]any, event string) []string {
	hooks, _ := cfg["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	var out []string
	for _, g := range groups {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			out = append(out, h.(map[string]any)["command"].(string))
		}
	}
	return out
}

// ---------- M: mem uninstall ----------

func TestCmdUninstallRemovesHooksFromProjectConfig(t *testing.T) {
	proj := t.TempDir()
	path := initCodex(t, proj)

	if _, err := captureStdout(t, func() error {
		return cmdUninstall([]string{"--harness", "codex", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdUninstall: %v", err)
	}

	cfg := readCfg(t, path)
	hooks, _ := cfg["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit"} {
		if gs, ok := hooks[ev].([]any); ok && len(gs) > 0 {
			t.Errorf("uninstall 后事件 %s 不应再有 v2mem 钩子，got %v", ev, gs)
		}
	}
}

func TestCmdUninstallKeepsUserHooksAndUnrelatedKeys(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "settings.json")
	existing := `{
  "model": "my-model",
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "/usr/local/bin/my-own-hook.sh"}]},
      {"hooks": [{"type": "command", "command": "/tmp/nonsense  # v2mem"}]}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/guard.sh"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := captureStdout(t, func() error {
		return cmdUninstall([]string{"--harness", "claude", "--project", proj})
	}); err != nil {
		t.Fatalf("cmdUninstall: %v", err)
	}

	cfg := readCfg(t, path)
	if cfg["model"] != "my-model" {
		t.Errorf("无关配置键应保留，got %+v", cfg["model"])
	}
	hooks := cfg["hooks"].(map[string]any)
	ss := extractCommands(cfg, "SessionStart")
	if len(ss) != 1 || !strings.Contains(ss[0], "my-own-hook.sh") {
		t.Errorf("只应保留用户自己的 SessionStart 钩子，got %v", ss)
	}
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("用户自己的 PreToolUse 配置应保留")
	}
}

func TestCmdUninstallIsIdempotent(t *testing.T) {
	proj := t.TempDir()
	path := initCodex(t, proj)
	for i := 0; i < 2; i++ {
		if _, err := captureStdout(t, func() error {
			return cmdUninstall([]string{"--harness", "codex", "--project", proj})
		}); err != nil {
			t.Fatalf("cmdUninstall #%d: %v", i+1, err)
		}
	}
	cfg := readCfg(t, path)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit"} {
		for _, c := range extractCommands(cfg, ev) {
			if isV2memHookCommand(c) {
				t.Errorf("重复 uninstall 后仍残留 v2mem 钩子 %q（%s）", c, ev)
			}
		}
	}
}

func TestCmdUninstallDryRunWritesNothing(t *testing.T) {
	proj := t.TempDir()
	path := initCodex(t, proj)
	before, _ := os.ReadFile(path)

	out, err := captureStdout(t, func() error {
		return cmdUninstall([]string{"--harness", "codex", "--project", proj, "--dry-run"})
	})
	if err != nil {
		t.Fatalf("cmdUninstall --dry-run: %v", err)
	}
	if !strings.Contains(out, "将移除") {
		t.Errorf("dry-run 应说明将移除什么，got:\n%s", out)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Errorf("dry-run 不得写文件")
	}
}

func TestCmdUninstallMissingFileIsNotError(t *testing.T) {
	proj := t.TempDir()
	out, err := captureStdout(t, func() error {
		return cmdUninstall([]string{"--harness", "codex", "--project", proj})
	})
	if err != nil {
		t.Fatalf("文件不存在不应报错: %v", err)
	}
	if !strings.Contains(out, "未找到") {
		t.Errorf("应报告未找到，got:\n%s", out)
	}
}

func TestCmdUninstallRemovesInstructionBlock(t *testing.T) {
	proj := t.TempDir()
	path := filepath.Join(proj, "AGENTS.md")
	user := "# 我的项目记忆\n\n已有内容要保留\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "workbuddy", "--project", proj, "--scope", "instruction"})
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdUninstall([]string{"--harness", "workbuddy", "--project", proj, "--scope", "instruction"})
	}); err != nil {
		t.Fatalf("cmdUninstall: %v", err)
	}
	body, _ := os.ReadFile(path)
	s := string(body)
	if strings.Contains(s, instrBegin) || strings.Contains(s, "mem search") {
		t.Errorf("指令块应被移除，got:\n%s", s)
	}
	if !strings.Contains(s, "已有内容要保留") {
		t.Error("用户内容应保留")
	}
}

func TestCmdUninstallNoProjectStaysInTestHome(t *testing.T) {
	tmpHome := os.Getenv("HOME")
	if tmpHome == "" || !strings.Contains(tmpHome, "v2mem-testhome-") {
		t.Fatalf("测试 HOME 未生效，got %q", tmpHome)
	}
	// 先全局装，再全局卸 —— 都必须在测试 HOME 内，绝不碰真实家目录
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "codex"})
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	out, err := captureStdout(t, func() error {
		return cmdUninstall([]string{"--harness", "codex"})
	})
	if err != nil {
		t.Fatalf("cmdUninstall: %v", err)
	}
	if !strings.Contains(out, tmpHome) {
		t.Errorf("卸载路径应在测试 HOME 内，got:\n%s", out)
	}
	if strings.Contains(out, "/Users/") {
		t.Errorf("不得出现真实家目录路径，got:\n%s", out)
	}
}

func TestCmdUninstallRejectsUnknownHarness(t *testing.T) {
	if err := cmdUninstall([]string{"--harness", "no-such-tool"}); err == nil {
		t.Error("未知 harness 应报错")
	}
	if err := cmdUninstall([]string{"--harness", "codex", "--project", t.TempDir(), "--scope", "bogus"}); err == nil {
		t.Error("非法 --scope 应报错")
	}
}

func TestCmdUninstallWithFileOverride(t *testing.T) {
	proj := t.TempDir()
	target := filepath.Join(proj, "custom.json")
	if err := os.WriteFile(target, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/tmp/x  # v2mem"}]}]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdUninstall([]string{"--harness", "codex", "--file", target})
	}); err != nil {
		t.Fatalf("cmdUninstall --file: %v", err)
	}
	cfg := readCfg(t, target)
	if gs, _ := cfg["hooks"].(map[string]any)["SessionStart"].([]any); len(gs) != 0 {
		t.Errorf("--file 目标中的 v2mem 钩子应被移除，got %v", gs)
	}
}