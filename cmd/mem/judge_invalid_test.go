package main

// R7：模型/Judge 输出非法 → 候选 failed（attempt+1、status_reason）→ 重试超上限
// quarantined；quarantined 不得被 CommitAccept 接受。
//
// 真实链路选 cmd/mem/judge.go 的解析路径：parseJudgeVerdicts 失败时不能把
// 「模型说了一堆不像 JSON 的话」当成「判为 0 条」——影子期只写痕迹的前提是
// 输出可解析；坏输出必须留痕并进入重试/隔离，否则明天又抓同一批、又烧一次调用。
//
// 用白名单里的 CLI 名造一个 stdout 恒为坏 JSON 的 fake CLI（临时 PATH 前置），
// 完整跑 cmdJudge，不引用任何尚未落地的符号：实现前候选状态不动，逐 case FAIL。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

func TestJudgeInvalidOutputDrivesFailedThenQuarantined(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	c := seedCandidate(t, st, "fact", "非法输出要留痕并重试的候选")
	id := c.ID

	// fake CLI：白名单里的名字，任何参数都只往 stdout 吐一行坏 JSON。
	home := t.TempDir()
	bindir := filepath.Join(home, "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(bindir, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'NOT A JSON ARRAY\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := filepath.Join(home, "model.json")
	if err := os.WriteFile(cfg, []byte(`{"mode":"cli","cli":"claude","model_tag":"fake@v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// 连续三次非法输出：failed(1) → failed(2) → quarantined(3)。
	// ListUnjudgedCandidates 只取 pending，failed 候选由重试队列重新入队；
	// 这里显式把 status 放回 pending 模拟下一次重试抓取（judge_verdict 始终为空）。
	for i := 1; i <= 3; i++ {
		if i > 1 {
			if _, err := st.DB().Exec(`UPDATE candidates SET status = 'pending' WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}
		err := cmdJudge([]string{"--config", cfg, "--db", db, "--audit-file", "-"})
		if err == nil {
			t.Fatalf("第 %d 次非法输出应如实报错（不得当成判为 0 条）", i)
		}
	}
	st.Close()

	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var status, reason string
	var attempt int
	if err := st2.DB().QueryRow(`SELECT status, status_reason, attempt FROM candidates WHERE id = ?`, id).
		Scan(&status, &reason, &attempt); err != nil {
		t.Fatal(err)
	}
	if status != "quarantined" || reason == "" || attempt < 3 {
		t.Fatalf("重试超上限应 quarantined 且 attempt>=3、status_reason 留原因，got %q/%q/%d",
			status, reason, attempt)
	}
	if _, err := st2.CommitAccept(id, -1, "human"); !errors.Is(err, store.ErrCandidateClosed) {
		t.Fatalf("quarantined 不得被 CommitAccept 接受，got %v", err)
	}
	if _, err := st2.CommitReject(id, -1, "human", ""); !errors.Is(err, store.ErrCandidateClosed) {
		t.Fatalf("quarantined 不得被 CommitReject 接受，got %v", err)
	}
	// 影子期正常路径不误伤：合法输出仍只写痕迹，不改状态。
}
