package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/wanghui/v2mem/internal/store"
)

// 二期同义词提取管线。设计见 docs/design-synonym-library.md §5。
// 步骤：采集该工程记忆正文的候选词 → 用 store.Search 当「行为 oracle」算每词命中集
// → 两两 Jaccard ≥ θ 判定同义并聚簇 → 簇内选标准词 → 写入 synonyms.txt 的 [project] 段。
// θ/k 默认值来自 §5.4 校准实验。

const (
	lexiconDefaultThreshold = 0.5  // Jaccard 阈值，精度优先
	lexiconDefaultK         = 8    // 命中集 top-k
	lexiconDefaultMinFreq   = 2    // 候选词最少出现次数
	lexiconDefaultMaxTerms  = 200  // 候选词上限（避免超大库 C² 配对失控）
	lexiconGenericRatio     = 0.30 // hit 广度护栏：命中 > 内存总量 30% 视为泛词
	lexiconReviewLo         = 0.40 // 复核区间下限 [0.40, 阈值) 不强采纳
)

// genericStopwords 语言中性候选滤除词（含中文助词/虚词与少量英文泛词）。
var genericStopwords = map[string]bool{
	"用于": true, "文件": true, "内容": true, "相关": true, "机制": true, "方面": true,
	"一个": true, "进行": true, "我们": true, "这个": true, "那个": true, "以及": true,
	"可以": true, "需要": true, "里面": true, "时候": true, "放在": true, "输出": true,
	"根据": true, "能够": true, "完成": true, "处理": true, "就是": true, "怎么": true,
	"the": true, "and": true, "for": true, "with": true, "from": true, "into": true,
	"not": true, "are": true, "this": true, "that": true, "will": true, "have": true,
}

// cmdLexicon 是 subcommand 入口。
func cmdLexicon(args []string) error {
	if len(args) >= 1 && args[0] == "init" {
		return cmdLexiconInit(args[1:])
	}
	return errors.New("用法: mem lexicon init --scan --project <X>（从该工程记忆提取同义词写入 synonyms.txt）")
}

func cmdLexiconInit(args []string) error {
	var c common
	fs := flag.NewFlagSet("lexicon init", flag.ContinueOnError)
	c.register(fs)
	project := fs.String("project", "", "工程标记（必填，从该工程记忆提取）")
	scan := fs.Bool("scan", false, "执行扫描提取（本命令默认即扫描，仅供对齐语义）")
	threshold := fs.Float64("threshold", lexiconDefaultThreshold, "命中集 Jaccard 阈值")
	k := fs.Int("k", lexiconDefaultK, "每词命中集 top-k")
	terms := fs.String("terms", "", "候选词（逗号/空格分隔的真实查询词；最准确，校准依据此路径）")
	autoExtract := fs.Bool("auto", false, "实验性：从记忆正文自动提取 bigram 作候选（需跨≥2 条不同记忆；精度低于 --terms，需复核）")
	minFreq := fs.Int("min-freq", lexiconDefaultMinFreq, "候选词最少出现次数（--auto 时)）")
	out := fs.String("out", "", "写入的 synonyms.txt 路径（默认 <dbdir>/synonyms.txt）")
	dryRun := fs.Bool("dry-run", false, "只预览，不写文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*project) == "" {
		return errors.New("缺少 --project <X>：必须指定要提取的工程")
	}
	if *k < 2 {
		*k = 2
	}
	_ = scan

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	// Step A: 选取候选词
	var freq map[string]int
	var termsSrc []string
	switch {
	case strings.TrimSpace(*terms) != "":
		// 主路径：真实查询词（来自 gold/audit 或用户/agent 指定），校准实验即此路径。
		freq = map[string]int{}
		for _, t := range strings.FieldsFunc(*terms, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\t' }) {
			t = strings.TrimSpace(t)
			if t == "" || len([]rune(t)) < 2 || genericStopwords[t] {
				continue
			}
			freq[t]++
			termsSrc = append(termsSrc, t)
		}
		if len(termsSrc) == 0 {
			return errors.New("--terms 没有可用的候选词（长度≥2 且非停用词）")
		}
	case *autoExtract:
		// 实验性：从记忆正文提取 bigram。裸 bigram 会把同一记忆里相邻的字串
		// （入库/库动/动作）互为命中，故要求跨 ≥2 条不同记忆才保留，压低碎片与假簇。
		// 精度仍低于 --terms，输出必须走复核。
		corpus, err := st.List(store.ListQuery{Project: *project, Scope: "project", Limit: 100000})
		if err != nil {
			return fmt.Errorf("读取工程记忆: %w", err)
		}
		freq = candidateDistinctFreq(corpus)
		termsSrc = topCandidates(freq, *minFreq)
		if len(termsSrc) == 0 {
			return errors.New("--auto 没有提取到跨 ≥2 条记忆的候选词；请改用 --terms 提供真实查询词")
		}
	default:
		return errors.New("需要提供候选词：--terms \"词A,词B,...\"（真实查询词，最准确）；或 --auto（实验性 bigram 自动提取）")
	}

	// Step B: 每词命中集（行为 oracle）+ 泛词广度护栏
	total := projectMemoryCount(st, *project)
	broadGuard := total >= 20 // 小库什么都显得"广"，护栏只在库足够大时启用
	items := []string{}
	sets := map[string]map[string]bool{}
	for _, w := range termsSrc {
		hits, err := st.Search(store.SearchQuery{Query: w, Project: *project, Limit: *k})
		if err != nil {
			continue
		}
		if broadGuard && len(hits) > int(float64(total)*lexiconGenericRatio) {
			continue // 泛词/歧义词：命中面太广，不具区分度
		}
		m := map[string]bool{}
		for _, h := range hits {
			m[h.ID] = true
		}
		sets[w] = m
		items = append(items, w)
	}

	// Step C: 两两 Jaccard 聚簇 + 复核清单
	parent := map[string]string{}
	var find func(x string) string
	find = func(x string) string {
		if parent[x] == "" {
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	review := [][2]string{}
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			a, b := items[i], items[j]
			sim := hitJaccard(sets[a], sets[b])
			if sim < lexiconReviewLo {
				continue
			}
			if sim >= *threshold {
				union(a, b)
			} else {
				review = append(review, [2]string{a, b})
			}
		}
	}

	clusters := clusterGroups(items, parent)
	target := *out
	if target == "" {
		target = synonymPathFor(st)
	}

	if *dryRun {
		printLexicon(items, clusters, freq, review)
		fmt.Printf("\n（dry-run 未写入 %s）\n", target)
		return nil
	}
	if err := writeLexicon(target, *project, clusters, freq); err != nil {
		return err
	}
	printLexicon(items, clusters, freq, review)
	fmt.Printf("\n已写入: %s\n", target)
	return nil
}

// synonymPathFor 取该库对应的 synonyms.txt 路径（与 store.SynonymPath 一致）。
func synonymPathFor(st *store.Store) string {
	path := filepath.Join(filepath.Dir(st.Path()), "synonyms.txt")
	if p := os.Getenv("MEM_SYNONYMS"); p != "" {
		path = p
	}
	return path
}

func printLexicon(items []string, clusters [][]string, freq map[string]int, review [][2]string) {
	fmt.Printf("候选词: %d  同义簇: %d\n", len(items), len(clusters))
	for _, grp := range clusters {
		std := pickStandard(grp, freq)
		aliases := []string{}
		for _, w := range grp {
			if w != std {
				aliases = append(aliases, w)
			}
		}
		if len(aliases) == 0 {
			continue
		}
		fmt.Printf("  %s = %s\n", std, strings.Join(aliases, ", "))
	}
	if len(review) > 0 {
		fmt.Printf("\n复核清单（Jaccard 在 [%.2f, %.2f)，未自动采纳）:\n", lexiconReviewLo, lexiconDefaultThreshold)
		for _, p := range review {
			fmt.Printf("  %s ↔ %s\n", p[0], p[1])
		}
	}
}

// writeLexicon 保留原文件内容，追加 [project] 段的未重复标准行。
func writeLexicon(path, project string, clusters [][]string, freq map[string]int) error {
	// 已有别名 -> 标准词 映射（去重用）
	existing := map[string]string{}
	old := ""
	if b, err := os.ReadFile(path); err == nil {
		old = string(b)
		for _, ln := range strings.Split(old, "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "[") {
				continue
			}
			if eq := strings.IndexByte(ln, '='); eq > 0 {
				std := strings.TrimSpace(ln[:eq])
				for _, a := range strings.Split(ln[eq+1:], ",") {
					if a = strings.TrimSpace(a); a != "" {
						existing[a] = std
					}
				}
			}
		}
	}

	hadSection := false
	for _, ln := range strings.Split(old, "\n") {
		if strings.TrimSpace(ln) == "["+project+"]" {
			hadSection = true
		}
	}
	var sb strings.Builder
	sb.WriteString(old)
	if old != "" && !strings.HasSuffix(old, "\n") {
		sb.WriteByte('\n')
	}
	if !hadSection {
		sb.WriteString("[" + project + "]\n")
	}
	wrote := 0
	for _, grp := range clusters {
		std := pickStandard(grp, freq)
		var aliases []string
		for _, w := range grp {
			if w != std && existing[w] == "" { // 该别名从未被收录
				aliases = append(aliases, w)
			}
		}
		if len(aliases) == 0 {
			continue
		}
		sb.WriteString(std + " = " + strings.Join(aliases, ", ") + "\n")
		wrote++
	}
	if wrote == 0 {
		return nil // 无新词，不改文件
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// projectMemoryCount 统计工程内的记忆条数（供泛词护栏）。
func projectMemoryCount(st *store.Store, project string) int {
	hits, err := st.List(store.ListQuery{Project: project, Scope: "project", Limit: 100000})
	if err != nil {
		return 0
	}
	return len(hits)
}

// candidateDistinctFreq 统计「每个 bigram 出现在几条不同的记忆」。
// 以去重后的记忆数计数（而非出现次数），压掉同一记忆里相邻字串互为碎片的问题。
func candidateDistinctFreq(corpus []store.Hit) map[string]int {
	var han []rune
	bigrams := func() []string {
		var out []string
		for i := 0; i+1 < len(han); i++ {
			out = append(out, string(han[i:i+2]))
		}
		return out
	}
	freq := map[string]int{}
	for _, h := range corpus {
		han = nil
		tmp := []string{}
		for _, r := range h.Content {
			if unicode.Is(unicode.Han, r) {
				han = append(han, r)
				continue
			}
			tmp = append(tmp, bigrams()...)
			han = nil
		}
		tmp = append(tmp, bigrams()...)
		seen := map[string]bool{}
		for _, w := range tmp {
			if seen[w] || genericStopwords[w] {
				continue
			}
			seen[w] = true
			freq[w]++
		}
	}
	return freq
}

// topCandidates 筛选出现 ≥ minFreq 的词，频率降序、同频字典序，截到上限。
func topCandidates(freq map[string]int, minFreq int) []string {
	var terms []string
	for w, n := range freq {
		if n >= minFreq {
			terms = append(terms, w)
		}
	}
	sort.Slice(terms, func(i, j int) bool {
		if freq[terms[i]] != freq[terms[j]] {
			return freq[terms[i]] > freq[terms[j]]
		}
		return terms[i] < terms[j]
	})
	if len(terms) > lexiconDefaultMaxTerms {
		terms = terms[:lexiconDefaultMaxTerms]
	}
	return terms
}

func hitJaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for x := range a {
		if b[x] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// pickStandard 选簇内词频最高者为标准词；并列时长者优先（簇内同主题假设）。
func pickStandard(grp []string, freq map[string]int) string {
	best := grp[0]
	for _, w := range grp[1:] {
		bl := len([]rune(best))
		wl := len([]rune(w))
		if freq[w] > freq[best] || (freq[w] == freq[best] && wl > bl) {
			best = w
		}
	}
	return best
}

// clusterGroups 按并查集 parent 把 items 分成 ≥2 的连通簇，字典序稳定。
func clusterGroups(items []string, parent map[string]string) [][]string {
	var find func(x string) string
	find = func(x string) string {
		if parent[x] == "" {
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	groups := map[string][]string{}
	for _, w := range items {
		r := find(w)
		groups[r] = append(groups[r], w)
	}
	out := [][]string{}
	for _, g := range groups {
		if len(g) >= 2 {
			sort.Strings(g)
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}