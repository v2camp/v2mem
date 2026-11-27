// mem review —— 一屏四态（ls）＋人工后审核动作面（confirm/correct/withdraw/
// restore）＋按版本回滚（rollback）＋标定统计（stats）。
//
// 为什么四态不是一张「待办清单」：例行审核队列 = 把「没人点按钮」的病换个位置复发
// （docs/design-data-lifecycle.md §6.4）。默认信任自动入库，复核是出错时的保险；
// 动作面只在出事时用：撤回/修正/回滚都不删行，证据与裁决历史一律保留。
package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/store"
)

// reviewStateLabels 是四段的中文标题，顺序与 store.ReviewQueueStates() 一致。
var reviewStateLabels = map[string]string{
	store.ReviewQueuePending:     "待判定",
	store.ReviewQueueSummary:     "未审核摘要",
	store.ReviewQueueQuarantined: "隔离",
	store.ReviewQueueFailed:      "失败",
}

func cmdReview(args []string) error {
	if len(args) == 0 {
		return errors.New("缺少子命令，用法: mem review ls [选项]；动作面见 mem review help")
	}
	switch args[0] {
	case "ls":
		return cmdReviewLs(args[1:])
	case store.ReviewActionConfirm, store.ReviewActionCorrect,
		store.ReviewActionWithdraw, store.ReviewActionRestore:
		return cmdReviewAction(args[0], args[1:])
	case "rollback":
		return cmdReviewRollback(args[1:])
	case "stats":
		return cmdReviewStats(args[1:])
	case "help", "-h", "--help":
		fmt.Println("用法: mem review <子命令> [选项]")
		fmt.Println("  ls                                    四态复核队列（只读）")
		fmt.Println("  confirm <memoryID>                    人工确认")
		fmt.Println("  correct <memoryID> --content TEXT     人工修正正文（保留模型原提案）")
		fmt.Println("  withdraw <memoryID> [--reason TEXT]  撤回（退出检索，行与证据保留）")
		fmt.Println("  restore <memoryID>                    撤回的逆动作")
		fmt.Println("  rollback --model-tag TAG [--dry-run]  按版本回滚自动记忆")
		fmt.Println("  stats [--judge]                      按版本标定统计（只读，阈值只提示；--judge 看判定器维度）")
		return nil
	default:
		return fmt.Errorf("未知子命令 %q；只读入口 mem review ls，动作面见 mem review help", args[0])
	}
}

// splitLeadingArgs 把开头连续的位置参数摘出来，剩下的交给 flag 包。
//
// flag 包遇到第一个非 flag token 就停止解析，所以 `mem review confirm <id> --json`
// 必须先把 <id> 摘走；`mem review confirm --json <id>` 则由 fs.Args() 兜住。
// flag 的值由 flag 包自己消费，这里只动开头那几个 token。
func splitLeadingArgs(args []string) (positional, rest []string) {
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") {
		positional = append(positional, args[i])
		i++
	}
	return positional, args[i:]
}

// cmdReviewAction 是四个记忆级动作的共用入口。状态迁移、候选终态恢复、
// review_action 裁决账全部在 store.ApplyMemoryReview 的单事务里完成，CLI 只做
// 参数解析与渲染（未来 MCP 复用同一 store API，不另写一套判定）。
func cmdReviewAction(action string, args []string) error {
	var c common
	fs := flag.NewFlagSet("review "+action, flag.ContinueOnError)
	c.register(fs)
	content := fs.String("content", "", "correct：人工修订后的正文")
	reason := fs.String("reason", "", "withdraw：撤回原因")
	modelTag := fs.String("model-tag", "", "归因 model@promptver（可选，默认沿用产生该记忆的版本）")
	positional, flagArgs := splitLeadingArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	positional = append(positional, fs.Args()...)
	id := strings.TrimSpace(strings.Join(positional, " "))
	if id == "" {
		return fmt.Errorf("缺少记忆 id，用法: mem review %s <memoryID> [选项]", action)
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := st.ApplyMemoryReview(store.MemoryReviewInput{
		MemoryID: id, Action: action, Content: *content, Reason: *reason,
		ModelTag: *modelTag, Actor: "human",
	})
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(rep)
	}
	fmt.Printf("已 %s 记忆 %s：%s → %s\n", action, shortID(rep.MemoryID),
		reviewStateText(rep.Before.ReviewState), reviewStateText(rep.After.ReviewState))
	if len(rep.AffectedCandidates) > 0 {
		fmt.Printf("  受影响候选: %s\n", joinShort(rep.AffectedCandidates))
	}
	if rep.Reason != "" {
		fmt.Printf("  原因: %s\n", rep.Reason)
	}
	return nil
}

// reviewStateText 让空状态在文本里可读（JSON 里仍保持 ""）。
func reviewStateText(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未审核"
	}
	return s
}

func joinShort(ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, shortID(id))
	}
	return strings.Join(parts, ", ")
}

// cmdReviewRollback 按 model@promptver 撤回自动记忆。只撤回、不删行；跳过人工
// 已确认/修正的记忆；dry-run 零写入；幂等由 store 侧「已是 withdrawn 计 already」保证。
func cmdReviewRollback(args []string) error {
	var c common
	fs := flag.NewFlagSet("review rollback", flag.ContinueOnError)
	c.register(fs)
	modelTag := fs.String("model-tag", "", "要回滚的 model@promptver")
	dryRun := fs.Bool("dry-run", false, "只报计划动作，零写入")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*modelTag) == "" {
		return errors.New("缺少 --model-tag，用法: mem review rollback --model-tag <model@promptver>")
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := st.RollbackModelTag(*modelTag, *dryRun, 0)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(rep)
	}
	dry := ""
	if rep.DryRun {
		dry = "（dry-run，未写库）"
	}
	fmt.Printf("回滚 %s%s：matched %d / withdrawn %d / skipped %d / already %d / reopened %d\n",
		rep.ModelTag, dry, rep.Matched, rep.Withdrawn, rep.Skipped, rep.Already, rep.Reopened)
	if rep.JudgeReopened > 0 || rep.JudgeSkipped > 0 {
		fmt.Printf("  Judge 驳回：回 pending %d / 跳过 %d\n", rep.JudgeReopened, rep.JudgeSkipped)
	}
	if len(rep.SkippedIDs) > 0 {
		fmt.Printf("  跳过（人工已确认/修正）: %s\n", joinShort(rep.SkippedIDs))
	}
	if len(rep.WithdrawnIDs) > 0 {
		fmt.Printf("  撤回: %s\n", joinShort(rep.WithdrawnIDs))
	}
	if len(rep.JudgeReopenedIDs) > 0 {
		fmt.Printf("  Judge 驳回回 pending: %s\n", joinShort(rep.JudgeReopenedIDs))
	}
	if len(rep.JudgeSkippedIDs) > 0 {
		fmt.Printf("  Judge 跳过（人工已接手）: %s\n", joinShort(rep.JudgeSkippedIDs))
	}
	return nil
}

// cmdReviewStats 按 model_tag 汇总后审核标定率。阈值只提示 WARN：
// 改提示词是人的活，stats 本身严格只读（docs/design-data-lifecycle.md §6.2）。
func cmdReviewStats(args []string) error {
	var c common
	fs := flag.NewFlagSet("review stats", flag.ContinueOnError)
	c.register(fs)
	judge := fs.Bool("judge", false, "按 judge_model 输出判定器标定（只读）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	if *judge {
		return renderJudgeStats(st, c.json)
	}
	rows, err := st.ReviewStats()
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Println("（暂无按版本归因的样本）")
		return nil
	}
	fmt.Println("版本                          样本  确认  修正  撤回  确认率  修正率  撤回率  回捞")
	for _, r := range rows {
		warn := ""
		if r.Warn {
			warn = "  WARN（阈值只提示：请人工复核提示词，不改提示词、不自动动作）"
		}
		fmt.Printf("%-30s %4d %5d %5d %5d  %5.0f%%  %5.0f%%  %5.0f%% %5d%s\n",
			r.ModelTag, r.Samples, r.Confirmed, r.Corrected, r.Withdrawn,
			r.ConfirmRate*100, r.CorrectRate*100, r.WithdrawRate*100, r.Recovered, warn)
	}
	return nil
}

// renderJudgeStats 输出按 judge_model 分组的判定器标定。
// JSON 字段固定（judge_model/verdicts/reject/applied/reopened），空样本输出 []；
// 文本形态与旧表分开，方便与 ReviewStats 并列阅读。
func renderJudgeStats(st *store.Store, asJSON bool) error {
	rows, err := st.JudgeCalibrationStats()
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Println("（暂无 Judge 判定样本）")
		return nil
	}
	fmt.Println("判定器                              裁决  否决  落库  重开")
	for _, r := range rows {
		fmt.Printf("%-34s %4d %5d %5d %5d\n", r.JudgeModel, r.Verdicts, r.Reject, r.Applied, r.Reopened)
	}
	return nil
}

func cmdReviewLs(args []string) error {
	var c common
	fs := flag.NewFlagSet("review ls", flag.ContinueOnError)
	c.register(fs)
	state := fs.String("state", store.ReviewQueueAll,
		"只看某一态: all|pending|summary|quarantined|failed")
	limit := fs.Int("limit", 50, "每态最多列出多少条（计数不受影响）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit <= 0 {
		return errors.New("--limit 应为正数（每态上限；不截断计数）")
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	// 状态在 store 侧校验：CLI 与未来 MCP 复用同一份白名单，避免两处各写一遍。
	q, err := st.ListReviewQueue(*state, *limit)
	if err != nil {
		return err
	}
	// 候选态补算 reviewable：巡检 R8 的「现行判据下仍值得裁决」口径与写侧同源。
	// store 不 import distill（循环依赖），由命令层补这一层只读投影。
	for i := range q.States[store.ReviewQueuePending] {
		it := &q.States[store.ReviewQueuePending][i]
		ok := distill.Reviewable(it.Content, it.AttributedTo, it.Kind)
		it.Reviewable = &ok
	}
	if c.json {
		return printJSON(q)
	}
	renderReviewQueue(q, *limit)
	return nil
}

func renderReviewQueue(q *store.ReviewQueue, limit int) {
	fmt.Printf("复核队列（每态上限 %d）\n", limit)
	for _, st := range store.ReviewQueueStates() {
		fmt.Printf("\n%s %s：%d\n", st, reviewStateLabels[st], q.Counts[st])
		items := q.States[st]
		if len(items) == 0 {
			fmt.Println("  （无）")
			continue
		}
		for _, it := range items {
			fmt.Printf("  %s  %-11s %-12s %-8s%s\n",
				shortID(it.ID), "["+it.Kind+"]", reviewScope(it.Scope), ageText(it.AgeSeconds),
				reviewDecision(it))
		}
		if len(items) == limit {
			fmt.Println("  （已达本态上限；用 --limit 放宽，或用 --state 只看某一态）")
		}
	}
}

// reviewScope 空 scope 显示为「-」：legacy 行没有作用域，留空会让列错位。
func reviewScope(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return "-"
	}
	return scope
}

// reviewDecision 把「最近一次裁决」与「记忆 id」拼成一行尾巴，没有就什么都不加。
func reviewDecision(it store.ReviewItem) string {
	var parts []string
	if it.StatusReason != "" {
		parts = append(parts, "原因 "+it.StatusReason)
	}
	if it.Actor != "" || it.Action != "" {
		parts = append(parts, "裁决 "+it.Actor+"/"+it.Action)
	}
	if it.Status == store.ReviewQueueFailed && it.Attempt > 0 {
		parts = append(parts, fmt.Sprintf("attempt %d", it.Attempt))
	}
	if it.MemoryID != "" {
		parts = append(parts, "记忆 "+shortID(it.MemoryID))
	}
	if len(parts) == 0 {
		return ""
	}
	return "  " + strings.Join(parts, "  ")
}

// ageText 让年龄一眼可读。天数是主单位：复核关心的是「压了多久」，不是精确秒数。
func ageText(sec int64) string {
	switch {
	case sec <= 0:
		return "刚刚"
	case sec < 60:
		return "刚刚"
	case sec < 3600:
		return fmt.Sprintf("%d 分钟", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%d 小时", sec/3600)
	default:
		return fmt.Sprintf("%d 天", sec/86400)
	}
}
