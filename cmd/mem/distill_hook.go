package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

// startDistillProc 拉起一次性 distill 子进程。测试可替换。
// 钩子不等待它，也不把候选写进 memories。
var startDistillProc = defaultStartDistill

func defaultStartDistill(db, session string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.HasSuffix(exe, ".test") {
		return fmt.Errorf("测试二进制不拉起 distill")
	}
	cmd := exec.Command(exe, "distill", "--session", session, "--provider", "none", "--db", db)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}

// maybeSpawnDistill 在 Stop / SessionEnd 末尾可选地拉起规则蒸馏。
// 默认不拉起。无 transcript_path 时跳过。失败只打 stderr。
func maybeSpawnDistill(db string, in hookInput) {
	if strings.TrimSpace(os.Getenv("MEM_DISTILL_SPAWN")) != "1" {
		return
	}
	if strings.TrimSpace(in.TranscriptPath) == "" {
		fmt.Fprintln(os.Stderr, "v2mem hook: 无 transcript_path，跳过 distill")
		return
	}
	sid := in.sessionID()
	if sid == "" {
		fmt.Fprintln(os.Stderr, "v2mem hook: 无 session_id，跳过 distill")
		return
	}
	st, err := store.Open(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 打开库失败，跳过 distill:", err)
		return
	}
	need, err := st.SessionsNeedingDistill(50)
	st.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 检查蒸馏窗口失败，已放过:", err)
		return
	}
	due := false
	for _, id := range need {
		if id == sid {
			due = true
			break
		}
	}
	if !due {
		return
	}
	if err := startDistillProc(db, sid); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 distill 失败，已放过:", err)
	}
}

// distillPendingFailOpen 在 SessionStart 用规则抽取处理尚未蒸馏的会话。
// 只用 provider none，有时间上限，失败不阻断注入，不写 memories。
func distillPendingFailOpen(db, auditPath string) {
	if strings.TrimSpace(os.Getenv("MEM_NO_DISTILL")) == "1" {
		return
	}
	st, err := store.Open(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: distill pending 打开库失败，已跳过:", err)
		return
	}
	defer st.Close()
	sessions, err := st.SessionsNeedingDistill(5)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: distill pending 查询失败，已跳过:", err)
		return
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for _, sid := range sessions {
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "v2mem hook: distill pending 到达时间上限，剩余留到下次")
			return
		}
		if _, _, err := distillOne(st, distillJob{
			Session: sid, Provider: "none", Audit: auditPath, DB: db,
			Model: "rules", Timeout: time.Second,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: distill pending 失败，已放过:", err)
		}
	}
}

func observeEventMayDistill(event harness.Event) bool {
	return event == harness.EventStop || event == harness.EventSessionEnd
}
