package main

// 方案 B P2 的 hook 层回归：把灰度开关开/关两条路径固化进常规测试。
// 黑盒收益验收见 /tmp/v2mem-p2-bench.py；这里守的是「行为不退化」。

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

func TestObserveStopEvidenceBatchSwitch(t *testing.T) {
	const n = 20
	for _, tc := range []struct {
		name      string
		batch     string
		wantFiles int
	}{
		{"默认关闭走逐条", "", n + 1},
		{"开启走批量", "1", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEM_EVIDENCE_BATCH", tc.batch)
			db := testDB(t)
			proj, _ := hookProjectDir(t, "demo-repo")
			path := filepath.Join(t.TempDir(), "tx-batch.jsonl")
			sid := "tx-batch"
			lines := make([]string, 0, n)
			for i := 0; i < n; i++ {
				lines = append(lines, fmt.Sprintf(
					`{"role":"user","content":"第%d行","sessionId":"%s"}`, i, sid))
			}
			writeClaudeTranscript(t, path, lines...)
			stop := hookJSON(t, map[string]any{
				"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
				"status": "completed", "transcript_path": path,
			})
			if _, err := withStdin(t, stop, func() error {
				return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
			}); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			st, err := store.Open(db)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			obs, err := st.ListObservations(sid, 200)
			if err != nil {
				t.Fatal(err)
			}
			if len(obs) != n+1 {
				t.Fatalf("两条路径都应写入 %d 条观测（%d 行 transcript + 1 条 stop），got %d", n+1, n, len(obs))
			}

			archiveDir := filepath.Join(filepath.Dir(db), "evidence", "archives")
			entries, err := os.ReadDir(archiveDir)
			if err != nil {
				t.Fatal(err)
			}
			files := 0
			for _, x := range entries {
				if !x.IsDir() && filepath.Ext(x.Name()) == ".gz" {
					files++
				}
			}
			if files != tc.wantFiles {
				t.Fatalf("归档文件数 want %d got %d", tc.wantFiles, files)
			}

			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			cur := readTranscriptCursor(transcriptCursorPath(db, sid))
			if cur.Offset != fi.Size() {
				t.Fatalf("批量成功时游标应走到文件末尾，got %d want %d", cur.Offset, fi.Size())
			}
		})
	}
}
