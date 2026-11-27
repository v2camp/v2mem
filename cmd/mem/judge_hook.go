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
//   - 落库由子进程裁决：judgeArgs 显式带 --apply-reject/--apply-accept，冲突类仍跳过。
//
// 自动触发的开关有两层：
//   - MEM_JUDGE_SPAWN：显式总闸，优先级最高。=1 强制开（脚本/CI），=0 强制关（GUI 宿主关不掉 export 时的退路）。
//   - model.json 的 auto_spawn：三态。缺省=有可用模型配置就开（升级后默认启用），
//     显式 false=用户明确拒绝，mode=off=关掉模型来源。
//
// P4 起默认启用：无需额外授权，但仍受 20 条 pending / 24h 冷却约束。

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

// judgeArgs 抽成纯函数：自动子进程必须显式打开落库开关（手动 judge 默认影子），
// 这条要可断言。
func judgeArgs(limit int) []string {
	return []string{"judge", "--limit", strconv.Itoa(limit), "--apply-reject", "--apply-accept"}
}

// maybeSpawnJudge 在写侧事件末尾按阈值拉起判定。所有失败都静默（只写 stderr）。
func maybeSpawnJudge(db string, event harness.Event) {
	// 事件判定必须最先：读路径连配置文件都不该读，守住「读路径零额外 I/O」。
	if event != harness.EventStop && event != harness.EventSessionEnd {
		return
	}
	// 唯一总闸：判定会自动烧模型调用，总闸下必须连配置都不读。
	if automationDisabled() {
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
	apply := strings.TrimSpace(os.Getenv("MEM_JUDGE_APPLY")) == "1"
	if apply {
		// 旧环境变量残留时明确说一句：不报错（不能阻断写侧钩子），但落库已由
		// --apply-reject/--apply-accept 决定，旧开关不再有任何作用。
		fmt.Fprintln(os.Stderr, "v2mem hook: MEM_JUDGE_APPLY 已废弃（落库由 --apply-reject/--apply-accept 决定）")
	}
	// 拉起成功后才写冷却：spawn 失败（fork/exec/临时资源）不该占掉 24h 窗口，
	// 否则一次瞬时失败会让积压整整一天无人处理。写状态失败仍只记 stderr：
	// 宁可下次多跑一次，也不阻断写侧钩子。
	if err := startJudgeProc(db, minPending); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 judge 失败，已放过:", err)
		return
	}
	if err := writeJudgeState(path, time.Now().Unix()); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 判定触发写状态失败，已放过:", err)
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
	if err != nil {
		return false
	}
	return cfg.AutoSpawnEnabled()
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
