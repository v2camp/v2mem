// mem review ls —— 一屏四态：pending（模型没跑）/ summary（模型自动写库但没人看过）
// / quarantined / failed。
//
// 为什么是四态而不是一张「待办清单」：例行审核队列 = 把「没人点按钮」的病换个位置复发
// （docs/design-data-lifecycle.md §6.4）。默认信任自动入库，复核是出错时的保险；
// 这一屏只回答问题找人时那一眼：**现在有什么在等我，最老的多久了**。
//
// 本切片只做只读视图：withdraw/restore/rollback/model-auto 的动作面在 S10b。
package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

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
		return errors.New("缺少子命令，用法: mem review ls [选项]（动作面 withdraw/restore/rollback 见后续版本）")
	}
	switch args[0] {
	case "ls":
		return cmdReviewLs(args[1:])
	case "help", "-h", "--help":
		fmt.Println("用法: mem review ls [--state all|pending|summary|quarantined|failed] [--json] [--limit 50] [--db path]")
		return nil
	default:
		return fmt.Errorf("未知子命令 %q；当前只提供只读视图 mem review ls（动作面 withdraw/restore/rollback 见后续版本）", args[0])
	}
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
