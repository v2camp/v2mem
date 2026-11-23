package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

// 积压触发：pending 到阈值就拉起一次判定，不等会话里的人想起来点按钮。
//
// 三条硬约束（都来自 VALUE.md §4）：
//   - 不在读路径上：只挂 Stop / SessionEnd，绝不影响 PromptSubmit 的注入延迟。
//   - 钩子不等待：spawn 后立即返回，判定在子进程里跑完，失败只写 spawn 日志。
//   - 不落库：子进程只写 verdict，没有任何改状态的参数——落库入口要等回滚与标定门槛一起交付。
//
// 自动触发的开关有两层：
//   - model.json 的 auto_spawn：mem setup 配好模型后写入，是常态授权；
//   - MEM_JUDGE_SPAWN：显式总闸。=1 强制开（脚本/CI），=0 强制关（GUI 宿主关不掉 export 时的退路）。
//
// 两层都不开就不发。老配置没有 auto_spawn，升级后不会突然开始烧 API。

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

func defaultStartJudge(db string, limit int) error {
	return startMemProc(db, judgeArgs(limit))
}

// judgeArgs 抽成纯函数：自动拉起的参数里永远不能出现改库开关，这条要可断言。
func judgeArgs(limit int) []string {
	return []string{"judge", "--limit", strconv.Itoa(limit)}
}

// maybeSpawnJudge 在写侧事件末尾按阈值拉起判定。所有失败都静默（只写 stderr）。
func maybeSpawnJudge(db string, event harness.Event) {
	// 事件判定必须最先：读路径连配置文件都不该读，守住「读路径零额外 I/O」。
	if event != harness.EventStop && event != harness.EventSessionEnd {
		return
	}
	if !judgeAutoSpawnEnabled() {
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
	if apply {
		// 旧环境变量残留时明确说一句：不报错（不能阻断写侧钩子），但不再有任何改库效果。
		fmt.Fprintln(os.Stderr, "v2mem hook: MEM_JUDGE_APPLY 已废弃（--apply 入口已撤），本次仍按影子运行")
	}
	if err := startJudgeProc(db, minPending); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 judge 失败，已放过:", err)
	}
}

// judgeAutoSpawnEnabled 决策开关：显式环境变量优先，未设时看 model.json 的授权。
//
// 只有 "1" / "0" 是有效覆盖；其它值（含空、拼写错）一律退回文件，不猜用户意图。
// 配置读不出来时 fail-closed：宁可少跑一次，也不该在配置损坏时凭空开始外发。
func judgeAutoSpawnEnabled() bool {
	switch strings.TrimSpace(os.Getenv("MEM_JUDGE_SPAWN")) {
	case "1":
		return true
	case "0":
		return false
	}
	cfg, err := distill.LoadModelConfig("")
	if err != nil || cfg.Mode == "off" {
		return false
	}
	return cfg.AutoSpawn
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
