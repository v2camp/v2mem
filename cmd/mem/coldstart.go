package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

// coldStartMetaPrefix 是「这个工程已经尝试过冷启动」的一次性标记。
// 带 v1 版本号：以后算法升级可以换 key 重新冷启动，而不是永久锁死。
const coldStartMetaPrefix = "coldstart:v1:"

// startColdStartProc 拉起一次性规则蒸馏。测试可替换。
var startColdStartProc = defaultColdStartDistill

// defaultColdStartDistill 固定走 --provider none，不受 Proposal 灰度影响：
// 首会话还没有历史证据，冷启动只做零模型的规则抽取，绝不在这里引入 LLM。
func defaultColdStartDistill(db, session string) error {
	return startMemProc(db, []string{"distill", "--session", session, "--provider", "none"})
}

func coldStartMetaKey(project string) string { return coldStartMetaPrefix + project }

// maybeSpawnColdStart 把首会话的蒸馏从下一次 SessionStart 前移到 SessionEnd。
//
// 触发条件全部满足才动作：
//   - 事件是 SessionEnd（Stop 每轮都会来，不能把冷启动挂在那里）；
//   - MEM_DISTILL_SPAWN 未开启（开启时常规 maybeSpawnDistill 已覆盖）；
//   - MEM_NO_DISTILL / MEM_NO_COLDSTART 未开启；
//   - 工程非空，有 session_id 与 transcript_path；
//   - 该工程还没有任何已采纳记忆，且没有 coldstart 标记；
//   - 该会话确实有未蒸馏的观测（复用 SessionsNeedingDistill 的 due 判定）。
//
// 成功 Start 后才写标记。子进程后续失败时，下一次 SessionStart 的
// distillPendingFailOpen 仍是兜底；这里只负责把成本挪出热路径。
// 不写 memories、不调 LLM。
func maybeSpawnColdStart(db string, in hookInput, project string, event harness.Event) {
	if event != harness.EventSessionEnd {
		return
	}
	if distillSpawnEnabled() {
		return
	}
	if strings.TrimSpace(os.Getenv("MEM_NO_DISTILL")) == "1" ||
		strings.TrimSpace(os.Getenv("MEM_NO_COLDSTART")) == "1" {
		return
	}
	if project == "" || strings.TrimSpace(in.TranscriptPath) == "" {
		return
	}
	sid := in.sessionID()
	if sid == "" {
		return
	}

	st, err := store.Open(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 冷启动打开库失败，已放过:", err)
		return
	}
	defer st.Close()

	done, err := coldStartDone(st, project)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 冷启动标记读取失败，已放过:", err)
		return
	}
	if done {
		return
	}
	n, err := st.ProjectMemoryCount(project)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 冷启动工程计数失败，已放过:", err)
		return
	}
	if n > 0 {
		return
	}
	need, err := st.SessionsNeedingDistill(50)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 冷启动检查蒸馏窗口失败，已放过:", err)
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
	if err := startColdStartProc(db, sid); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 拉起冷启动蒸馏失败，已放过:", err)
		return
	}
	if err := st.MetaSet(coldStartMetaKey(project), strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 写冷启动标记失败（下次可能重试）:", err)
	}
}

func coldStartDone(st *store.Store, project string) (bool, error) {
	v, ok, err := st.MetaGet(coldStartMetaKey(project))
	if err != nil {
		return false, err
	}
	return ok && v != "", nil
}
