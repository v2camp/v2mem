// mem notes —— 从库里抽出「硬规则小字条」。
//
// 为什么要它：跨会话的全局红线（commit 前必跑 make cover-gate 之类）散落在库里，
// 每次会话起始都要完整注入一整套硬规则会吃掉预算。notes 把这类「硬规则」压成
// 一行一条的稳定纯文本 —— 一行一规则，按稳定键排序，多次运行输出完全一致（幂等）。
// 读侧（hook）只注入少数几条最相关记忆，其余硬规则交给 notes 这个小字条覆盖。
package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/wanghui/v2mem/internal/store"
)

// notesSalienceThreshold 是「高重要性」下限，用于把全局 (project 空)
// 且重要性达到该值的记忆视为硬规则 —— 红线类约定通常以高 salience 标记。
const notesSalienceThreshold = 0.8

// notesMaxScan 是 notes 扫描全库时的 LIMIT 上限。
// List 的默认 Limit=10 无法覆盖全库，此处给一个在 CLI 记忆库量级下等效「全量」的值，
// 保证抽取不遗漏任何一条硬规则。
const notesMaxScan = 1_000_000

// isHardRule 判断一条记忆是否构成「硬规则小字条」：
//   - kind 显式为 rule（写侧用 --kind rule 标记），或
//   - project 为空（全局）且 salience 达到阈值（红线类约定）
func isHardRule(h store.Hit) bool {
	if h.Kind == "rule" {
		return true
	}
	return h.Project == "" && h.Salience >= notesSalienceThreshold
}

// collectRules 抽取全部硬规则小字条，并按稳定键排序。
// 排序用 (project, content, id)：content 不因时间、命中次数等不稳定信号变化，
// 保证多次运行输出一致（幂等）。
func collectRules(st *store.Store) ([]store.Hit, error) {
	hits, err := st.List(store.ListQuery{Scope: "all", Limit: notesMaxScan})
	if err != nil {
		return nil, err
	}
	var rules []store.Hit
	for _, h := range hits {
		if isHardRule(h) {
			rules = append(rules, h)
		}
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Project != rules[j].Project {
			return rules[i].Project < rules[j].Project
		}
		if rules[i].Content != rules[j].Content {
			return rules[i].Content < rules[j].Content
		}
		return rules[i].ID < rules[j].ID
	})
	return rules, nil
}

// renderNotes 生成 notes 的纯文本：一行一条硬规则。
func renderNotes(st *store.Store) (string, error) {
	rules, err := collectRules(st)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, r := range rules {
		sb.WriteString(oneLine(r.Content))
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

// runNotes 是 notes 的库级入口与测试接缝：打开库并渲染硬规则小字条。
func runNotes(db string) (string, error) {
	st, err := store.Open(db)
	if err != nil {
		return "", err
	}
	defer st.Close()
	return renderNotes(st)
}

// cmdNotes 是 `mem notes` 子命令：输出硬规则小字条（一行一条）。
func cmdNotes(args []string) error {
	var c common
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	text, err := runNotes(c.db)
	if err != nil {
		return err
	}
	if c.json {
		var lines []string
		for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		if lines == nil {
			lines = []string{}
		}
		return printJSON(lines)
	}
	if text == "" {
		fmt.Println("（没有硬规则小字条）")
		return nil
	}
	fmt.Print(text)
	return nil
}