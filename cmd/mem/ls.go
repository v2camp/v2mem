// mem ls —— 不依赖查询词，直接列出库里有什么。
//
// 为什么需要它：`mem search` 要求先有查询词，`mem export` 是给机器读的 JSONL。
// 「我到底记了些什么」这个最朴素的问题此前没有对应命令 —— 自查能力缺了这一块。
package main

import (
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/wanghui/v2mem/internal/store"
)

func cmdLs(args []string) error {
	var c common
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	c.register(fs)
	project := fs.String("project", "", "限定工程")
	scope := fs.String("scope", "", "留空=全部（给了 --project 则按其过滤）/ current=当前工程+全局 / global=只要全局 / all=显式跨工程")
	kind := fs.String("kind", "", "限定类型：preference|decision|pitfall|task|fact")
	limit := fs.Int("limit", 50, "最多列出多少条")
	var tags stringSlice
	fs.Var(&tags, "tag", "标记 k=v，可重复（AND 语义）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateLsScope(*scope); err != nil {
		return err
	}
	// --scope all 与 --project 语义矛盾：all 就是「忽略工程」。
	// 放过去会让用户以为过滤生效了而实际没有 —— 静默失效比报错更糟。
	if *scope == "all" && strings.TrimSpace(*project) != "" {
		return errors.New("--scope all 与 --project 矛盾：all 表示跨工程、忽略工程过滤；要按工程过滤请去掉 --scope all")
	}
	tagMap, err := parseTags(tags)
	if err != nil {
		return err
	}
	// 工程：**只有显式给了 --project，或作用域本身需要工程时才推断**。
	// 默认不去猜当前目录 —— `ls` 的默认语义是「把库里有什么都列出来」，
	// 一上来就按 cwd 过滤会让「不带参数」看起来像空库（实测踩过）。
	proj := *project
	if proj == "" && (*scope == "current" || *scope == "project") {
		proj = detectProject()
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	hits, err := st.List(store.ListQuery{
		Scope: *scope, Project: proj, Kind: *kind, Tags: tagMap, Limit: *limit,
	})
	if err != nil {
		return err
	}
	if c.json {
		if hits == nil {
			hits = []store.Hit{}
		}
		return printJSON(hits)
	}
	if len(hits) == 0 {
		fmt.Println("（无匹配记忆）")
		return nil
	}

	// 分组展示更利于浏览：按工程聚合，组内保持 store 的排序（重要性降序）
	groups := map[string][]store.Hit{}
	var order []string
	for _, h := range hits {
		p := h.Project
		if p == "" {
			p = "(全局)"
		}
		if _, ok := groups[p]; !ok {
			order = append(order, p)
		}
		groups[p] = append(groups[p], h)
	}
	sort.Strings(order)

	fmt.Printf("共 %d 条（上限 %d）\n", len(hits), *limit)
	for _, p := range order {
		fmt.Printf("\n[%s] %d 条\n", p, len(groups[p]))
		for _, h := range groups[p] {
			fmt.Printf("  %s  %-11s %s\n", shortID(h.ID), "["+h.Kind+"]", truncateRunes(oneLine(h.Content), 78))
		}
	}
	if len(hits) == *limit {
		fmt.Printf("\n（已达上限；用 --limit 放宽，或用 --project/--kind/--tag 收窄）\n")
	}
	return nil
}

// oneLine 把内容压成单行，便于列表展示。
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// 保证 --scope 的取值可控，避免拼错时静默变成「不限」
func validateLsScope(scope string) error {
	switch scope {
	case "", "all", "current", "global":
		return nil
	}
	return errors.New("非法 --scope（可选留空|current|global|all）")
}
