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

	"github.com/v2camp/v2mem/internal/store"
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

// notesBudgetRunes 是类码表与硬规则小字条的合计上限。
// 类码是常数，硬规则是可变部分；超预算时只截硬规则，不截类码。
const notesBudgetRunes = 2048

// notesView 把两段分开，供 --json 结构化输出。不得靠解析文本切分。
type notesView struct {
	Codes []kindCode `json:"codes"`
	Rules []string   `json:"rules"`
	Text  string     `json:"-"`
}

// buildNotes 先类码表、再硬规则。两段都按稳定键，多次运行逐字节一致。
func buildNotes(st *store.Store) (notesView, error) {
	rules, err := collectRules(st)
	if err != nil {
		return notesView{}, err
	}
	var sb strings.Builder
	sb.WriteString(renderKindCodes())
	used := runeLen(sb.String())
	var kept []string
	for _, r := range rules {
		line := oneLine(r.Content)
		if line == "" {
			continue
		}
		next := line + "\n"
		if used+runeLen(next) > notesBudgetRunes {
			break
		}
		sb.WriteString(next)
		used += runeLen(next)
		kept = append(kept, line)
	}
	if kept == nil {
		kept = []string{}
	}
	return notesView{Codes: append([]kindCode(nil), kindCodes...), Rules: kept, Text: sb.String()}, nil
}

// renderNotes 生成 notes 的纯文本：先类码表，再一行一条硬规则。
func renderNotes(st *store.Store) (string, error) {
	view, err := buildNotes(st)
	if err != nil {
		return "", err
	}
	return view.Text, nil
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

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	view, err := buildNotes(st)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(struct {
			Codes []kindCode `json:"codes"`
			Rules []string   `json:"rules"`
		}{Codes: view.Codes, Rules: view.Rules})
	}
	fmt.Print(view.Text)
	if len(view.Rules) == 0 {
		fmt.Println("（没有硬规则小字条）")
	}
	return nil
}
