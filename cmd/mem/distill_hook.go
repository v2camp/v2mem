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
// harness 由调用方逐层传入，供按宿主灰度选择器判定本次是否走 Proposal；
// 选择器本身不读全局配置、不写文件、不引入包级可变状态。
var startDistillProc = defaultStartDistill

func defaultStartDistill(db, session, harness string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.HasSuffix(exe, ".test") {
		return fmt.Errorf("测试二进制不拉起 distill")
	}
	args := []string{"distill"}
	if proposalHookEnabled(harness) {
		args = append(args, "--proposal", "--executor", "auto", "--max-bundles", "1")
	} else {
		args = append(args, "--session", session, "--provider", "none")
	}
	args = append(args, "--db", db)
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}

// normalizeHarness 把宿主名（规范名或别名）经注册表归一化到 h.Name；
// harness.Get 本身大小写不敏感、去空格、支持 Aliases。解析失败时返回去空格后的
// 原串，由调用方按保守字符串匹配处理。
func normalizeHarness(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if h, err := harness.Get(name); err == nil {
		return h.Name
	}
	return name
}

// proposalHookEnabled 是旧规则蒸馏与 T2 Proposal 的显式切换点，按当前 harness 灰度。
//
//   - 未设置 / 空串 / "0" → false：保持旧规则蒸馏（--session <sid> --provider none）。
//     这是默认值，绝不改变。
//   - "1" / "all" / "on"（大小写不敏感）→ true：保持既有全量语义。
//   - 其它值按逗号分隔的 harness 名单匹配：名单项与当前宿主名都经
//     normalizeHarness 归一化到规范名后比较（大小写不敏感、去空格），
//     只有当前宿主在名单里才 true；解析失败的项按原字符串保守比较。
//     因此 MEM_PROPOSAL_HOOK=trae 与 =traecode 都能命中 traecode。
//
// harness 名由调用方从 harnessName(--harness, in) 逐层传入；宿主名无法解析
// （为空）时名单灰度保守返回 false、回归默认旧行为，全量值不受影响。
// 选择器不读全局配置、不写文件、不引入包级可变状态；失败一律 fail-open。
func proposalHookEnabled(host string) bool {
	raw := strings.TrimSpace(os.Getenv("MEM_PROPOSAL_HOOK"))
	if raw == "" || raw == "0" {
		return false
	}
	if strings.EqualFold(raw, "1") || strings.EqualFold(raw, "all") || strings.EqualFold(raw, "on") {
		return true
	}
	current := normalizeHarness(host)
	if current == "" {
		return false
	}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.EqualFold(normalizeHarness(item), current) {
			return true
		}
	}
	return false
}

// maybeSpawnDistill 在 Stop / SessionEnd 末尾可选地拉起规则蒸馏。
// 默认不拉起。无 transcript_path 时跳过。失败只打 stderr。
func maybeSpawnDistill(db string, in hookInput, harness string) {
	if strings.TrimSpace(os.Getenv("MEM_DISTILL_SPAWN")) != "1" {
		return
	}
	sid := in.sessionID()
	if proposalHookEnabled(harness) {
		if sid == "" {
			fmt.Fprintln(os.Stderr, "v2mem hook: 无 session_id，跳过 proposal")
			return
		}
		if err := startDistillProc(db, sid, harness); err != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 proposal 失败，已放过:", err)
		}
		return
	}
	if strings.TrimSpace(in.TranscriptPath) == "" {
		fmt.Fprintln(os.Stderr, "v2mem hook: 无 transcript_path，跳过 distill")
		return
	}
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
	if err := startDistillProc(db, sid, harness); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 distill 失败，已放过:", err)
	}
}

// distillPendingFailOpen 在 SessionStart 用规则抽取处理尚未蒸馏的会话。
// 只用 provider none，有时间上限，失败不阻断注入，不写 memories。
func distillPendingFailOpen(db, auditPath, harness string) {
	if strings.TrimSpace(os.Getenv("MEM_NO_DISTILL")) == "1" {
		return
	}
	if proposalHookEnabled(harness) {
		if err := startDistillProc(db, "", harness); err != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: 拉起 proposal pending 失败，已放过:", err)
		}
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
		if _, err := distillOne(st, distillJob{
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
