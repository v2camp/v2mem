package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

// 积压触发：pending 到阈值就拉起一次判定，不等会话里的人想起来点按钮。
//
// 三条硬约束（都来自 VALUE.md §4）：
//   - 不在读路径上：只挂 Stop / SessionEnd，绝不影响 PromptSubmit 的注入延迟。
//   - 钩子不等待：spawn 后立即返回，判定在子进程里跑完，失败只写 stderr。
//   - 默认不落库：子进程默认影子（只写 verdict）；要真改状态得显式 MEM_JUDGE_APPLY=1。
//
// 默认不发（MEM_JUDGE_SPAWN 未设置即关）：它走网络、花钱，不该在升级后悄悄开始。

const (
	judgeDefaultMinPending = 20 // 低于 R8 的 FAIL 线（>15）留一点余量，避免刚 WARN 就烧一次调用
	judgeDefaultCooldownH  = 24 // 一个会话可能 Stop 几十次，没有冷却就等于每轮都跑
	judgeStateFile         = "judge-state.json"
)

// judgeState 只存「上次拉起的时间」。放文件而不是库：它是调度状态，不是记忆，
// 不该出现在 mem.db 里被同步、备份、gc 牵动。
type judgeState struct {
	TS int64 `json:"ts"`
}

var startJudgeProc = defaultStartJudge

func defaultStartJudge(db string, limit int, apply bool) error {
	args := []string{"judge", "--limit", strconv.Itoa(limit)}
	if apply {
		args = append(args, "--apply")
	}
	return startMemProc(db, args)
}

// maybeSpawnJudge 在写侧事件末尾按阈值拉起判定。所有失败都静默（只写 stderr）。
func maybeSpawnJudge(db string, event harness.Event) {
	if strings.TrimSpace(os.Getenv("MEM_JUDGE_SPAWN")) != "1" {
		return
	}
	if event != harness.EventStop && event != harness.EventSessionEnd {
		return
	}
	minPending := positiveEnvInt("MEM_JUDGE_MIN_PENDING")
	if minPending <= 0 {
		minPending = judgeDefaultMinPending
	}
	cooldown := time.Duration(judgeDefaultCooldownH) * time.Hour
	if h := positiveEnvInt("MEM_JUDGE_COOLDOWN_H"); h > 0 {
		cooldown = time.Duration(h) * time.Hour
	}

	st, err := store.Open(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 判定触发打开库失败，已放过:", err)
		return
	}
	pending, err := st.CountPending()
	st.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 判定触发读积压失败，已放过:", err)
		return
	}
	path := judgeStatePath()
	last := readJudgeState(path)
	if !shouldSpawnJudge(pending, last, time.Now().Unix(), minPending, cooldown) {
		return
	}
	// 先写状态再拉起：宁可少跑一次，也不要在 spawn 成功后因写状态失败而反复重跑。
	if err := writeJudgeState(path, time.Now().Unix()); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 判定触发写状态失败，已放过:", err)
		return
	}
	apply := strings.TrimSpace(os.Getenv("MEM_JUDGE_APPLY")) == "1"
	if err := startJudgeProc(db, minPending, apply); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 judge 失败，已放过:", err)
	}
}

// shouldSpawnJudge 是触发判据本身，抽成纯函数才可测：
// 阈值看积压条数，冷却看上次拉起时间。last==0 表示从未跑过，视为已过冷却。
func shouldSpawnJudge(pending int, last, now int64, minPending int, cooldown time.Duration) bool {
	if pending < minPending {
		return false
	}
	if last <= 0 {
		return true
	}
	return now-last >= int64(cooldown.Seconds())
}

func judgeStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return judgeStateFile
	}
	return filepath.Join(home, ".v2mem", judgeStateFile)
}

func readJudgeState(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var s judgeState
	if json.Unmarshal(b, &s) != nil {
		return 0
	}
	return s.TS
}

func writeJudgeState(path string, ts int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(judgeState{TS: ts})
	return os.WriteFile(path, b, 0o644)
}
