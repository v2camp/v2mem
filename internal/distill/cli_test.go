package distill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeOrderAndTemplate(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"codex", "claude"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+n+"-v1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if name, ok := Probe(); !ok || name != "claude" {
		t.Fatalf("应按白名单顺序取 claude，got %q ok=%v", name, ok)
	}
	tag := VersionTag("claude")
	if !strings.Contains(tag, "claude@claude-v1+tpl") {
		t.Errorf("VersionTag 不对: %q", tag)
	}
}

func TestProbeAllOrder(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"codex", "claude"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+n+"-v1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	got := ProbeAll()
	want := []string{"claude", "codex"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ProbeAll 应按白名单顺序返回全部可用 CLI，want %v got %v", want, got)
	}
	// Probe 必须仍是 ProbeAll 的首个元素，老调用方语义不变。
	if name, ok := Probe(); !ok || name != got[0] {
		t.Fatalf("Probe 应返回 ProbeAll 首项，got %q ok=%v", name, ok)
	}
}

func TestRunCLIStdinStdoutAndError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\n/bin/cat\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out, err := runCLI(context.Background(), "claude", "hello")
	if err != nil || out != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho boom >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(context.Background(), "claude", "x"); err == nil || !errors.Is(err, ErrLLMUnavailable) {
		t.Fatalf("非零退出应归 ErrLLMUnavailable，got %v", err)
	}
}

func TestResolveCLIWins(t *testing.T) {
	name, spec, err := resolve(Options{CLI: "claude", ModelTag: "claude@1+tpl1"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "cli:claude@1+tpl1" || spec.Kind != "cli" || spec.Name != "claude" {
		t.Fatalf("got %q %+v", name, spec)
	}
}

func TestCompleteLLMCLI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n/bin/cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out, err := completeLLM(context.Background(), ModelSpec{Kind: "cli", Name: "claude"}, "[]")
	if err != nil || out != "[]" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestRunCLITimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n/bin/sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := runCLI(ctx, "claude", "x"); err == nil {
		t.Fatal("超时应报错")
	}
}

// claude --version 可能打印 "2.1.34 (Claude Code)"：整行进标签会带空格与括号，
// judge_model 按版本分组的键就不稳定，只取第一个 token。
func TestVersionTagFirstToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho \"2.1.34 (Claude Code)\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := VersionTag("claude"); got != "claude@2.1.34+tpl1" {
		t.Errorf("VersionTag 应只取版本首 token，got %q", got)
	}
}
