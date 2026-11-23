package main

// 子进程 detach 后没有终端：失败必须落在 spawn.log，否则界面上只剩「什么都没发生」。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenSpawnLogAppends(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, line := range []string{"第一次失败\n", "第二次失败\n"} {
		f, err := openSpawnLog()
		if err != nil {
			t.Fatalf("openSpawnLog: %v", err)
		}
		if _, err := f.WriteString(line); err != nil {
			f.Close()
			t.Fatal(err)
		}
		f.Close()
	}
	b, err := os.ReadFile(filepath.Join(home, ".v2mem", "spawn.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "第一次失败") || !strings.Contains(string(b), "第二次失败") {
		t.Fatalf("应追加而不是覆盖，got %q", b)
	}
}
