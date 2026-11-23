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
	if !strings.Contains(VersionTag("claude"), "claude@claude-v1+tpl") {
		t.Errorf("VersionTag 不对: %q", VersionTag("claude"))
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
