package main

// S11：统一触达面（design-data-lifecycle §3.1）——把「等人工」的出口压成一行。
//
// 形态：`待人工 N 项（隔离 x ｜ 未审核最老 y 天 ｜ 模型不可用 ｜ 维护失败）`，
// 仅 N>0 时出现；N 是**列出的项数**，不是条数（隔离 3 条仍算 1 项）。
//
// 三条约束：
//  1. 只有一条注入行：原 [待裁决] 计数与指引合并到同一行，不再新增第二条 stdout。
//  2. 与 head/类码表/hint 同属固定开销，先预留预算；超预算按
//     「隔离 → 未审核 → 模型 → 维护」的固定顺序整项丢弃，不截出半个词。
//  3. 只读、失败开放：任何查询失败都退回「不注入这一行」，绝不阻断宿主。
//
// 数据口径（与 S4 `mem stats` 同源）：
//   - 隔离 x = candidates_isolated + bundles_isolated；
//   - 未审核 y 天 = 全局 pending 的最老年龄，**向上取整**（1 秒也算 1 天，
//     宁可在展示上「显得久一点」，也不把刚积压的项显示成 0 天）；
//   - evidence_gaps（序号洞）**不进来**：§3.2 明确它是「已跳过并记录」的既成事实，
//     不是待办队列，也不需要人工；把它算进「待人工」会制造假待办。
//
// 「模型不可用」的信号选型（最小、可从现有数据真实判定）：
//   读审计里**最近一条** proposal distill 记录，`mode=backlog` 且 `provider` 为空
//   即为不可用；后续任何一条 ok/fail 记录都会把它清掉（只看最近一次）。
//   依据是 runner.go 对 backlog 的定义——「所有 provider 都不可用」，不是内容失败。
//   审计读不到/没有 distill 记录时不显示该项（宁缺勿假）。
//   已知边界：租约竞争也会落 StatusBacklog 且 provider 为空，二者在审计里不可区分；
//   这是「宁可偶尔多报一次模型不可用」与「多读一列审计」之间的取舍，注释在此备查。
//
// 「维护失败」的独立项：maintain 失败时写 maintain:last_fail_at（+ 简短原因），
// 24h 内且晚于最后一次成功（maintain:last_ok）才点亮。与「模型不可用」单列：
// 维护失败的原因可能是配置损坏、写库失败等，不都是模型问题，合并会误导人。

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/store"
)

const (
	// touchMaintainFailWindow 是维护失败的展示窗口：超过 24h 的旧失败不再点亮，
	// 避免一次早已修复的故障永久挂在每个会话开场。
	touchMaintainFailWindow = 24 * 60 * 60
	// touchAuditTailBytes 是审计尾部读取上限。触达面挂在 SessionStart 上，
	// 不能为了找一条 distill 记录把 5MB 审计整份解析一遍。
	touchAuditTailBytes = 64 << 10
)

// touchItem 是一项待人工信号。kind 用于固定顺序与「未审核」附带的 [待裁决] 指引。
type touchItem struct {
	kind  string
	label string
}

// sessionStartTouchLine 构造 SessionStart 的触达行；budget 是留给这一行的字符预算。
// 无待人工项、预算不足或查询失败时返回空串（调用方不写任何空行）。
func sessionStartTouchLine(st *store.Store, project string, now time.Time, auditFile string, budget int) string {
	if budget <= 0 {
		return ""
	}
	items, err := collectTouchItems(st, now.Unix(), auditFile)
	if err != nil {
		// fail-open：触达面是锦上添花，读不出来就整行不注入，绝不让会话收错误。
		fmt.Fprintln(os.Stderr, "v2mem hook: 待人工项统计失败，本次不注入触达行:", err)
		return ""
	}
	if len(items) == 0 {
		return ""
	}
	// [待裁决] 指引只跟「未审核」项走：未审核项被预算挤掉时，指引也一起让路，
	// 不能出现「有操作指引、却没有对应积压」的悬空提示。
	note := ""
	for _, it := range items {
		if it.kind == "unreviewed" {
			note = strings.TrimSpace(pendingCandidatesLine(st, project))
			break
		}
	}
	return renderTouchLine(items, note, budget)
}

// collectTouchItems 按固定顺序收集信号：隔离 → 未审核 → 模型 → 维护。
// backlog 读失败直接返回错误（整行不注入）；单项信号读失败只跳过该项（宁缺勿假）。
func collectTouchItems(st *store.Store, now int64, auditFile string) ([]touchItem, error) {
	bl, err := st.Backlog(now)
	if err != nil {
		return nil, err
	}
	var items []touchItem
	if n := bl.CandidatesIsolated + bl.BundlesIsolated; n > 0 {
		items = append(items, touchItem{kind: "isolated", label: fmt.Sprintf("隔离 %d", n)})
	}
	if bl.PendingCount > 0 {
		age := int64(0)
		if bl.OldestPendingAge != nil {
			age = *bl.OldestPendingAge
		}
		items = append(items, touchItem{kind: "unreviewed", label: fmt.Sprintf("未审核最老 %d 天", ceilDays(age))})
	}
	if modelUnavailable(auditFile) {
		items = append(items, touchItem{kind: "model", label: "模型不可用"})
	}
	if maintainFailed(st, now) {
		items = append(items, touchItem{kind: "maintain", label: "维护失败"})
	}
	return items, nil
}

// renderTouchLine 组装并做预算截断。截断只丢整项：行内每项都是完整词组，
// 不允许出现「隔离 3 ｜ 未审」这种半个词。note 是 [待裁决] 指引，整体可丢。
func renderTouchLine(items []touchItem, note string, budget int) string {
	if budget <= 0 || len(items) == 0 {
		return ""
	}
	build := func(kept []touchItem, withNote bool) string {
		if len(kept) == 0 {
			return ""
		}
		labels := make([]string, len(kept))
		for i, it := range kept {
			labels[i] = it.label
		}
		s := fmt.Sprintf("待人工 %d 项（%s）", len(kept), strings.Join(labels, " ｜ "))
		if withNote && note != "" {
			s += "｜" + note
		}
		return s + "\n"
	}

	if full := build(items, note != ""); runeLen(full) <= budget {
		return full
	}
	if core := build(items, false); runeLen(core) <= budget {
		return core
	}
	// 按固定顺序从尾部丢项：维护 → 模型 → 未审核，最后只剩隔离。
	for n := len(items) - 1; n >= 1; n-- {
		if s := build(items[:n], false); runeLen(s) <= budget {
			return s
		}
	}
	return ""
}

// ceilDays 把秒向上取整成天：0 秒算 0 天，1 秒算 1 天。负数（时钟漂移）夹 0。
func ceilDays(sec int64) int64 {
	if sec <= 0 {
		return 0
	}
	return (sec + 86400 - 1) / 86400
}

// maintainFailed 判断维护失败是否仍应点亮。
// 「已恢复」以 last_ok 追上 last_fail 为准，而不是只看 24h 窗口：
// 失败后立刻成功维护一次，就该熄灭。
func maintainFailed(st *store.Store, now int64) bool {
	v, ok, err := st.MetaGet(store.MetaMaintainLastFailAt)
	if err != nil || !ok {
		return false
	}
	failAt, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if perr != nil {
		// 坏值当「没有信号」：不能把一条写坏的元数据展示成人需要处理的故障。
		return false
	}
	if failAt > now {
		failAt = now // 时钟漂移：未来时间按「刚刚失败」处理，不让它永久挂住
	}
	if now-failAt > touchMaintainFailWindow {
		return false
	}
	if okV, okOK, err := st.MetaGet(store.MetaMaintainLastOK); err == nil && okOK {
		if okAt, perr := strconv.ParseInt(strings.TrimSpace(okV), 10, 64); perr == nil && okAt >= failAt {
			return false
		}
	}
	return true
}

// modelUnavailable 读取审计里最近一条 proposal distill，判断是否因无 provider 停在 backlog。
// 空路径（直接调用渲染函数、未走 cmdHook）与 "-"（审计关闭）都不做判定。
func modelUnavailable(auditFile string) bool {
	rec, ok := lastProposalDistill(auditFile)
	if !ok {
		return false
	}
	return rec.Mode == "backlog" && strings.TrimSpace(rec.Provider) == ""
}

// lastProposalDistill 只读审计文件尾部 64KB 找最后一条 Event=distill / Source=proposal。
// 尾部起点落在半行中间时丢掉第一段；读不到或没有记录返回 false。
func lastProposalDistill(path string) (audit.Record, bool) {
	path = strings.TrimSpace(path)
	if path == "" || path == "-" {
		return audit.Record{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return audit.Record{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return audit.Record{}, false
	}
	size := fi.Size()
	off := size - touchAuditTailBytes
	if off < 0 {
		off = 0
	}
	buf := make([]byte, size-off)
	n, err := f.ReadAt(buf, off)
	if n == 0 {
		return audit.Record{}, false
	}
	if err != nil && n < len(buf) {
		buf = buf[:n]
	}
	lines := strings.Split(string(buf), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // 第一段可能是被截断的半行
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var rec audit.Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Event == "distill" && rec.Source == "proposal" {
			return rec, true
		}
	}
	return audit.Record{}, false
}
