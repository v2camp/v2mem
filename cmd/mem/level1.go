// Level 1 文件（每轮注入的会话级文件）的运维：机械化搬运与体量守卫。
//
// 要解决的是什么：AGENTS.md / MEMORY.md 每轮注入且有容量上限，灌满即被截断，
// 于是要**反复人工压缩**。根治办法是把细节搬到 Level 2，而「搬」这个动作
// 必须能机械化 —— 否则迁移本身又变成一次人工压缩。
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wanghui/v2mem/internal/store"
)

const (
	// ingestMinRunes 是候选事实的最小长度。太短的一行不是事实，是碎片。
	ingestMinRunes = 8
	// ingestMaxRunes 是单条上限。超长的往往是整段论述而非原子事实，
	// 硬收进来会污染检索精度（DESIGN.md §9.3：库里存原子事实，不存文档正文）。
	ingestMaxRunes = 500
)

// ingestStats 汇总一次搬运。
type ingestStats struct {
	Files     int `json:"files"`
	Lines     int `json:"lines"`
	Candidate int `json:"candidate"`
	Skipped   int `json:"skipped"`
	Inserted  int `json:"inserted"`
	Merged    int `json:"merged"` // 命中「相同知识覆盖」，即已存在
}

func cmdIngest(args []string) error {
	var c common
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	c.register(fs)
	project := fs.String("project", "", "工程标记（默认取当前 git 仓库名）")
	kind := fs.String("kind", "fact", "记忆类型")
	device := fs.String("device", "", "来源设备")
	salience := fs.Float64("salience", 0.5, "重要性 0..1")
	dryRun := fs.Bool("dry-run", false, "只列出候选项，不写库")
	minRunes := fs.Int("min-runes", ingestMinRunes, "候选事实的最小字符数")
	var tags stringSlice
	fs.Var(&tags, "tag", "标记 k=v，可重复")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		return errors.New("缺少文件，用法: mem ingest <文件.md> [<文件2.md>...]")
	}
	proj := *project
	if proj == "" {
		proj = detectProject()
	}
	tagMap, err := parseTags(tags)
	if err != nil {
		return err
	}

	var st *store.Store
	if !*dryRun {
		st, err = store.Open(c.db)
		if err != nil {
			return err
		}
		defer st.Close()
	}

	res := &ingestStats{}
	for _, p := range paths {
		cands, lines, err := extractCandidates(p, *minRunes)
		if err != nil {
			return err
		}
		res.Files++
		res.Lines += lines

		for _, cand := range cands {
			res.Candidate++
			if *dryRun {
				if c.json {
					continue
				}
				fmt.Printf("  + %s\n", cand)
				continue
			}
			r, err := st.Add(store.AddInput{
				Content: cand, Kind: *kind, Project: proj,
				Device: *device, Tags: tagMap, Salience: *salience,
			})
			if err != nil {
				return err
			}
			if r.Created {
				res.Inserted++
			} else {
				// 已存在 —— 这正是「同一份文件重复 ingest 幂等」的来源
				res.Merged++
			}
		}
	}

	if *dryRun {
		if c.json {
			return printJSON(res)
		}
		fmt.Printf("\n（--dry-run）%d 个文件 / %d 行 → 候选 %d 条，未写库\n",
			res.Files, res.Lines, res.Candidate)
		return nil
	}
	if c.json {
		return printJSON(res)
	}
	fmt.Printf("已搬运 %d 个文件 / %d 行 → 候选 %d 条：新增 %d，已存在 %d（[%s/%s]）\n",
		res.Files, res.Lines, res.Candidate, res.Inserted, res.Merged, *kind, proj)
	return nil
}

// extractCandidates 从一个 markdown 文件里抽出候选原子事实。
//
// 规则（刻意机械、可预期，不依赖模型）：
//   - 跳过标题行、表格行、标记块边界、代码围栏内的行、空行
//   - **缩进行并入上一条**：markdown 常把一条事实硬换行写，续行带缩进
//     （实测本仓库日志 28 处、details 23 处）。不合并就会把一个事实拆成碎片，
//     碎片进库后既检索不准也浪费条数。
//   - 列表项去掉 "- " / "* " / "1. " 前缀；顶格普通行各自成条
//   - 过短/过长丢弃
//
// 去重交给 store 的「相同知识覆盖」，不在这里做 —— 归一化规则只有一处。
func extractCandidates(path string, minRunes int) ([]string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var out []string
	seen := map[string]bool{}
	var cur []string
	lines := 0
	inFence := false

	flush := func() {
		if len(cur) == 0 {
			return
		}
		text := strings.Join(cur, " ")
		cur = nil
		if cand, ok := acceptable(text, minRunes); ok && !seen[cand] {
			seen[cand] = true
			out = append(out, cand)
		}
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines++
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)

		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			flush()
			continue
		}
		if inFence {
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		if strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "|") ||
			strings.HasPrefix(trimmed, "<!--") {
			flush()
			continue
		}
		// 缩进行是上一条的续行
		if len(cur) > 0 && (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) {
			cur = append(cur, trimmed)
			continue
		}
		flush()
		cur = []string{stripListMarker(trimmed)}
	}
	flush()

	if err := sc.Err(); err != nil {
		return nil, lines, err
	}
	return out, lines, nil
}

// linkRe 匹配 markdown 链接与图片；orderedRe 匹配有序列表前缀。
var (
	linkRe    = regexp.MustCompile(`!?\[[^\]]*\]\([^)]*\)`)
	orderedRe = regexp.MustCompile(`^\d+[.)]\s+`)
)

// stripListMarker 去掉无序/有序/引用前缀。
func stripListMarker(line string) string {
	for _, p := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, p) {
			line = line[len(p):]
			break
		}
	}
	line = orderedRe.ReplaceAllString(line, "")
	return strings.TrimSpace(strings.TrimLeft(line, ">"))
}

// acceptable 判断一条候选是否够格入库。
func acceptable(line string, minRunes int) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	// 去掉链接后必须自身也够长：否则「见 [某文档](url)」这类纯指引行会被误收
	// （实测漏过 —— 去掉链接只剩一个「见」字，却因为原文够长而通过）。
	if n := len([]rune(strings.TrimSpace(linkRe.ReplaceAllString(line, "")))); n < minRunes {
		return "", false
	}
	n := len([]rune(line))
	if n < minRunes || n > ingestMaxRunes {
		return "", false
	}
	return line, true
}

func cmdBudget(args []string) error {
	var c common
	fs := flag.NewFlagSet("budget", flag.ContinueOnError)
	c.register(fs)
	file := fs.String("file", "", "要检查的 Level 1 文件（如 AGENTS.md 或 MEMORY.md）")
	maxChars := fs.Int("max-chars", 7800, "该文件的注入预算（字符）")
	project := fs.String("project", "", "用于对照统计的工程名（默认取当前 git 仓库名）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*file) == "" {
		return errors.New("缺少 --file，用法: mem budget --file AGENTS.md [--max-chars 7800]")
	}
	if *maxChars <= 0 {
		return fmt.Errorf("--max-chars 应为正数，got %d", *maxChars)
	}

	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	size := len([]rune(string(raw)))

	proj := *project
	if proj == "" {
		proj = detectProject()
	}
	libCount := -1
	if st, err := store.Open(c.db); err == nil {
		if hits, err := st.List(store.ListQuery{Scope: "project", Project: proj, Limit: 100000}); err == nil {
			libCount = len(hits)
		}
		st.Close()
	}

	over := size - *maxChars
	verdict := "OK"
	if over > 0 {
		verdict = fmt.Sprintf("超预算 %d 字符", over)
	}

	if c.json {
		return printJSON(map[string]any{
			"file": filepath.Base(*file), "chars": size,
			"max_chars": *maxChars, "over": maxInt(over, 0),
			"library_memories": libCount, "project": proj,
		})
	}

	fmt.Printf("文件      : %s\n", *file)
	fmt.Printf("体量      : %d 字符（预算 %d）\n", size, *maxChars)
	fmt.Printf("判定      : %s\n", verdict)
	if libCount >= 0 {
		// 对照：这些内容已搬到 Level 2，因此不占注入预算
		fmt.Printf("库中记忆  : %d 条（[%s] 工程，代码强制 0 注入字符）\n", libCount, proj)
	}
	if over > 0 {
		fmt.Println("建议      : 把细节移到记忆库：mem ingest <文件> --project " + proj)
		return fmt.Errorf("%s 超出预算 %d 字符", filepath.Base(*file), over)
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
