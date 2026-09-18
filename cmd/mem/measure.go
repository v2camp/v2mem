// 评测：回答「这套记忆系统到底准不准」，而不是靠印象。
//
// 三个族（用户提出）：
//   - 用户会话 → mem 查询 → 命中是否准确   : mem eval recall（金标准）／audit（真实样本）
//   - 用户会话 → mem 记录 → 记录是否准确   : mem eval write
//   - 一段时间内的读/写活动与空命中        : mem eval activity（用量视图，兼任务收尾结论）
//
// 口径先定死再跑数，且**自动模式必须标注为下限测试** —— 不能让「自召回接近 100%」
// 被当成「真实检索质量好」。这是本仓库评测纪律的老坑。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wanghui/v2mem/internal/audit"
	"github.com/wanghui/v2mem/internal/eval"
	"github.com/wanghui/v2mem/internal/store"
)

const (
	// defaultEvalK 是默认的 top-k。与 hook 默认注入条数（3）不同：
	// 评测看的是「检索能力上限」，hook 看的是「注入预算」，两者不该混用同一个 k。
	defaultEvalK = 5
	// goldHashPrefix 是金标准里哈希前缀的长度。太短会撞（4 位 16^4=65536 有风险），
	// 太长则写起来繁琐。8 位十六进制在个人库规模下足够唯一。
	goldHashPrefix = 8
)

// ---------- mem audit ----------

func cmdAudit(args []string) error {
	var c common
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	c.register(fs)
	path := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl）")
	statsOnly := fs.Bool("stats", false, "只打印汇总")
	tail := fs.Int("tail", 10, "列出最近多少条（0=不列）")
	showHits := fs.Bool("hits", true, "列出每条记录实际涉及的记忆内容（可用 --hits=false 关闭）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := *path
	if p == "" {
		p = audit.Path()
	}
	rs, err := audit.Read(p, 0)
	if err != nil {
		return err
	}
	s := audit.Summarize(rs)

	if c.json {
		return printJSON(map[string]any{"path": p, "summary": s, "records": rs})
	}
	fmt.Printf("审计日志  : %s\n", p)
	fmt.Printf("总激活次数: %d\n", s.Total)
	fmt.Printf("空注入    : %d（%.1f%%）\n", s.Empty, s.EmptyRate*100)
	fmt.Printf("平均耗时  : %.1f ms\n", s.AvgMS)
	if s.Suppressed > 0 {
		fmt.Printf("重复触发  : 已抑制 %d 次\n", s.Suppressed)
	}
	fmt.Printf("可评测样本: %d（带 query 的读侧次数 —— 检索评测的分母来源）\n", s.WithQuery)
	// 读/写分开报：只记不查或只查不记，都会在这两个数上立刻显形
	ratio := "—"
	if s.Writes > 0 {
		ratio = fmt.Sprintf("%.0f%%", float64(s.Retrievals)/float64(s.Writes)*100)
	}
	fmt.Printf("读侧/写侧 : %d / %d 次（读/写比 %s）\n", s.Retrievals, s.Writes, ratio)
	if s.Writes > 0 {
		fmt.Printf("写侧构成  : 新增 %d，命中覆盖 %d（重复记录率 %.0f%%）\n",
			s.Creates, s.Overwrites,
			float64(s.Overwrites)/float64(s.Writes)*100)
	}
	if s.Suppressed > 0 {
		// 抑制数 >0 是「同一事件被多处配置触发」的直接证据 —— 提示可清理冗余配置
		fmt.Printf("重复触发  : 已抑制 %d 次（同一事件被多处配置并行触发；可清理冗余钩子配置）\n", s.Suppressed)
	}
	if len(s.ByEvent) > 0 {
		fmt.Printf("按事件    : %s\n", joinCounts(s.ByEvent))
	}
	if len(s.ByHarness) > 0 {
		fmt.Printf("按工具    : %s\n", joinCounts(s.ByHarness))
	}
	if *statsOnly || *tail <= 0 || len(rs) == 0 {
		if s.WithQuery < 20 {
			fmt.Printf("\n⚠️ 可评测样本仅 %d 条（建议 ≥20 再下结论）：真实检索评测需要积累。\n", s.WithQuery)
		}
		return nil
	}

	// 需要还原内容时，把窗口内所有引用一次性批量取回（读侧存本地 id，写侧存 content_hash）
	var contents map[string]string
	if *showHits {
		var refs []string
		seen := map[string]bool{}
		for i := len(rs) - 1; i >= 0 && i > len(rs)-1-*tail; i-- {
			for _, h := range rs[i].Hashes {
				if !seen[h] {
					seen[h] = true
					refs = append(refs, h)
				}
			}
		}
		if st, err := store.Open(c.db); err == nil {
			contents, _ = st.ContentsByRefs(refs)
			st.Close()
		}
	}

	fmt.Println("\n最近记录（新→旧）：")
	start := len(rs) - 1
	for i := start; i >= 0 && i > start-*tail; i-- {
		r := rs[i]
		q := r.Query
		if q == "" {
			q = "（无 query）"
		}
		marks := ""
		if r.Suppressed {
			marks += "  [已抑制]"
		}
		if r.Mode != "" {
			marks += "  [" + r.Mode + "]"
		}
		fmt.Printf("  %s  %-14s %-11s %s%s\n",
			formatAuditTime(r.TS), r.Event, r.Harness, truncateRunes(oneLine(q), 42), marks)
		if !*showHits {
			continue
		}
		for _, ref := range r.Hashes {
			if c, ok := contents[ref]; ok {
				fmt.Printf("        ↳ %s\n", truncateRunes(oneLine(c), 72))
			} else {
				fmt.Printf("        ↳ %s（内容不可用：可能已被删除或取代）\n", shortID(ref))
			}
		}
	}
	return nil
}

// formatAuditTime 按「今天只显时刻、跨天带日期」渲染，兼顾可读与省位。
func formatAuditTime(ts int64) string {
	t := time.Unix(ts, 0)
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04:05")
	}
	return t.Format("01-02 15:04")
}

func joinCounts(m map[string]int) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	sortStrings(parts)
	return strings.Join(parts, " ")
}

// prefixOrEmpty 取哈希前缀，长度不足时原样返回（避免 panic）。
func prefixOrEmpty(h string, n int) string {
	if len(h) < n {
		return h
	}
	return h[:n]
}

func sortStrings(xs []string) {
	for i := 0; i < len(xs); i++ {
		for j := i + 1; j < len(xs); j++ {
			if xs[j] < xs[i] {
				xs[i], xs[j] = xs[j], xs[i]
			}
		}
	}
}

// ---------- mem eval ----------

func cmdEval(args []string) error {
	if len(args) == 0 {
		// 一键默认：最常用、且无需任何输入的是活动视图（读/写/空命中）。
		// 给 24h 窗口，比 8h 更贴「最近一天有没有用起来」的直觉。
		// 打印一行引导，告诉用户完整的等价指令（裸用一定是非 JSON 的人读模式）。
		fmt.Println("→ 简化调用已自动补默认参数。完整指令：mem eval activity --since 24h")
		fmt.Println()
		return cmdEvalActivity([]string{"--since", "24h"})
	}
	switch args[0] {
	case "recall":
		return cmdEvalRecall(args[1:])
	case "write":
		return cmdEvalWrite(args[1:])
	case "activity":
		return cmdEvalActivity(args[1:])
	}
	// 第一个 token 是 flag（以 - 开头）而非子命令 → 把它当成在默认 activity 上加参数，
	// 例如 `mem eval --db X` / `mem eval --since 2h`。
	if strings.HasPrefix(args[0], "-") {
		return cmdEvalActivity(args)
	}
	return fmt.Errorf("未知子命令 %q；可选 recall | write | activity（不带子命令时默认跑 activity 24h）", args[0])
}

// defaultAutoSample 是 `--auto` 裸用时的默认抽样条数。
const defaultAutoSample = 20

// optionalInt 让 `--auto` 可裸用（取 default），也接受 `--auto=N` 与 `--auto N`。
//
// Go 标准 flag 的 int 必须给值，`--auto` 直接报"flag needs an argument"。
// 这里用自定义 Value 并声明 IsBoolFlag：让 `--auto`（裸用）吃到 "true" 并取默认，
// `--auto=50` / `--auto 50` 正常取数值；三种写法一致可用。
type optionalInt struct {
	set   bool // 用户在命令行写过 --auto
	bare  bool // 裸用（未给数值，吃默认）
	value int
	def   int
}

// IsBoolFlag 允许 bare 形式：裸 `--auto` 时 Go flag 调 Set("true")。
func (o *optionalInt) IsBoolFlag() bool { return true }

func (o *optionalInt) String() string { return strconv.Itoa(o.value) }

func (o *optionalInt) Set(s string) error {
	o.set = true
	if s == "" || s == "true" { // 裸 `--auto`：Go flag 传 "true"
		o.bare = true
		o.value = o.def
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	o.value = v
	return nil
}

// evalHint 打印一行「简化调用已自动补默认参数、完整指令是什么」的引导。
// 只在非 JSON 模式输出 —— JSON 不允许被任何非结构文本污染（LLM 解析会崩）。
func evalHint(c common, full string) {
	if c.json {
		return
	}
	fmt.Printf("→ 简化调用已自动补默认参数。完整指令：%s\n\n", full)
}

func cmdEvalRecall(args []string) error {
	var c common
	fs := flag.NewFlagSet("eval recall", flag.ContinueOnError)
	c.register(fs)
	goldPath := fs.String("gold", "", "金标准 JSONL（每行 {\"query\":..,\"gold\":[\"<hash前缀>\"..]}）")
	goldDir := fs.String("gold-dir", "", "金标准目录：合并同一目录下所有 *.gold.jsonl 为一份金标准（与 --gold 二选一）")
	auto := &optionalInt{def: defaultAutoSample}
	fs.Var(auto, "auto", fmt.Sprintf("免标注自召回下限测试：抽样 N 条记忆当金标准（裸用取默认 %d）", defaultAutoSample))
	k := fs.Int("k", defaultEvalK, "top-k")
	project := fs.String("project", "", "限定工程（--auto 时按该工程抽样）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 裸 `--auto` 时，紧跟的空格数值会被 Go flag 当成位置参数（bool flag 不消费下一 token）。
	// 这里拾回：`--auto 1` 应等价 `--auto=1`，且显式给数后不再算「简化调用」。
	if auto.set && auto.bare {
		if rest := fs.Args(); len(rest) > 0 {
			if v, err := strconv.Atoi(rest[0]); err == nil && v > 0 {
				auto.value = v
				auto.bare = false
			}
		}
	}
	if *k <= 0 {
		return fmt.Errorf("--k 应为正数，got %d", *k)
	}
	if *goldPath == "" && *goldDir == "" && !auto.set {
		return errors.New("需要 --gold <文件>、--gold-dir <目录> 或 --auto [N]（真实评测请用 --gold/--gold-dir）。\n" +
			"最短可用：\n" +
			"  mem eval recall --auto           # 免标注下限自查（默认抽样 20 条）\n" +
			"  mem eval recall --auto=50        # 指定抽样条数\n" +
			"  mem eval recall --gold <文件>    # 真实金标准评测（单文件）\n" +
			"  mem eval recall --gold-dir <目录> # 真实金标准评测（合并目录下全部 *.gold.jsonl）")
	}
	if *goldPath != "" && *goldDir != "" {
		return errors.New("--gold 与 --gold-dir 不可同时使用（--gold-dir 已合并目录下全部 *.gold.jsonl）")
	}
	if (*goldPath != "" || *goldDir != "") && auto.set {
		return errors.New("--gold/--gold-dir 与 --auto 不可同时使用")
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	if auto.set {
		if auto.bare {
			evalHint(c, fmt.Sprintf("mem eval recall --auto=%d", auto.value))
		}
		return runAutoRecall(st, c, auto.value, *k, *project)
	}
	if *goldDir != "" {
		cases, err := loadGoldDir(*goldDir)
		if err != nil {
			return err
		}
		return runGoldRecall(st, c, cases, *goldDir, *k)
	}
	// 回退现有 --gold（单文件）。
	cases, err := eval.LoadGold(*goldPath)
	if err != nil {
		return err
	}
	return runGoldRecall(st, c, cases, *goldPath, *k)
}

// loadGoldDir 读取同一目录下所有 *.gold.jsonl 并合并为一份金标准。
// 目录下没有任何 *.gold.jsonl 时显式报错（空金标准不是合法输入）。
func loadGoldDir(dir string) ([]eval.GoldCase, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.gold.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s 下没有 *.gold.jsonl（金标准目录为空）", dir)
	}
	var out []eval.GoldCase
	for _, p := range matches {
		cases, err := eval.LoadGold(p)
		if err != nil {
			return nil, err
		}
		out = append(out, cases...)
	}
	return out, nil
}

// runGoldRecall 跑真实金标准。这是**唯一能支撑「替换」决策**的指标来源。
func runGoldRecall(st *store.Store, c common, cases []eval.GoldCase, label string, k int) error {
	known, err := allHashes(st)
	if err != nil {
		return err
	}
	valid, invalid := eval.Validate(cases, known)

	retrieved := make([][]string, 0, len(valid))
	for _, gc := range valid {
		hits, err := st.Search(store.SearchQuery{Query: gc.Query, Scope: "all", Limit: k})
		if err != nil {
			return err
		}
		hashes := make([]string, 0, len(hits))
		for _, h := range hits {
			hashes = append(hashes, hashOrEmpty(st, h.ID))
		}
		retrieved = append(retrieved, hashes)
	}
	rep := eval.Score(valid, retrieved, k)

	if c.json {
		return printJSON(map[string]any{"report": rep, "invalid": invalid, "gold_file": label})
	}
	printRecallReport(rep)
	if len(invalid) > 0 {
		// 无效样本必须显式上报：否则「gold 写错了」会表现成「Recall 低」
		fmt.Printf("\n⚠️ 已剔除 %d 条无效样本（gold 不在库中或为空）：\n", len(invalid))
		for _, s := range invalid {
			fmt.Printf("  - %s\n", s)
		}
	}
	fmt.Println("\n最差样本：")
	for _, r := range eval.SortByRecall(rep.Results) {
		fmt.Printf("  %d/%d  rank=%d  %s\n", r.Hit, r.Gold, r.FirstRank, truncateRunes(r.Query, 46))
		if r.Hit == 0 {
			break
		}
	}
	return nil
}

// runAutoRecall 免标注的自召回下限测试。
//
// 它只能证明「索引与中文切分没坏」，**不能代表真实检索质量** ——
// 因为查询就是记忆内容本身，不存在「用户换个说法」这层困难。
// 输出里必须把这个局限说清楚，避免数字被误引用。
func runAutoRecall(st *store.Store, c common, n, k int, project string) error {
	scope := "all"
	if project != "" {
		scope = "project"
	}
	hits, err := st.List(store.ListQuery{Scope: scope, Project: project, Limit: 100000})
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return errors.New("库中没有记忆，无法做自召回测试")
	}
	// 均匀抽样，确定性（不用随机数，否则指标不可复现）
	step := len(hits) / n
	if step < 1 {
		step = 1
	}
	var sampled []store.Hit
	for i := 0; i < len(hits) && len(sampled) < n; i += step {
		sampled = append(sampled, hits[i])
	}

	forms := []struct {
		name string
		mk   func(string) string
	}{
		{"全文复述", func(s string) string { return s }},
		{"截断复述", func(s string) string {
			rs := []rune(s)
			if len(rs) <= 12 {
				return s
			}
			return string(rs[:len(rs)*3/5]) // 只记得前 60%
		}},
	}
	reports := map[string]*eval.Report{}
	for _, f := range forms {
		cases := make([]eval.GoldCase, 0, len(sampled))
		for _, h := range sampled {
			cases = append(cases, eval.GoldCase{Query: f.mk(h.Content), Gold: []string{prefixOrEmpty(hashOrEmpty(st, h.ID), goldHashPrefix)}})
		}
		retrieved := make([][]string, 0, len(cases))
		for _, gc := range cases {
			hs, err := st.Search(store.SearchQuery{Query: gc.Query, Scope: "all", Limit: k})
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(hs))
			for _, h := range hs {
				ids = append(ids, hashOrEmpty(st, h.ID))
			}
			retrieved = append(retrieved, ids)
		}
		reports[f.name] = eval.Score(cases, retrieved, k)
	}

	if c.json {
		return printJSON(map[string]any{"sampled": len(sampled), "library": len(hits), "k": k, "reports": reports})
	}
	fmt.Println("⚠️ 自召回下限测试（免标注）：查询就是记忆内容本身，")
	fmt.Println("   它只能证明「索引与中文切分没坏」，**不代表真实检索质量**。")
	fmt.Println("   真实评测请用 --gold 跑人工标注的、来自真实会话的查询。")
	fmt.Printf("\n库内 %d 条，抽样 %d 条，k=%d\n\n", len(hits), len(sampled), k)
	for _, f := range forms {
		fmt.Printf("[%s] Recall@%d=%.1f%%  MRR=%.3f  空结果率=%.1f%%\n",
			f.name, k, reports[f.name].RecallAtK*100, reports[f.name].MRR, reports[f.name].EmptyRate*100)
	}
	return nil
}

func printRecallReport(rep *eval.Report) {
	fmt.Printf("有效样本: %d（分母）\n", rep.Cases)
	fmt.Printf("Recall@%d: %.1f%%（宏平均，每样本等权）\n", rep.K, rep.RecallAtK*100)
	fmt.Printf("MRR      : %.3f\n", rep.MRR)
	fmt.Printf("空结果率 : %.1f%%（检索完全无结果的样本占比）\n", rep.EmptyRate*100)
}

// ---------- mem eval write ----------

// cmdEvalWrite 评「用户会话 → mem 记录」。
//
// 有 gold 时给漏记/多记；无 gold 时只给可自动算的质量指标（并明确标注是启发式）。
func cmdEvalWrite(args []string) error {
	var c common
	fs := flag.NewFlagSet("eval write", flag.ContinueOnError)
	c.register(fs)
	session := fs.String("session", "", "要抽取的会话/日志文件")
	goldPath := fs.String("gold", "", "应当被记录的事实清单（每行一条关键词短语）；省略则只给启发式质量指标")
	minRunes := fs.Int("min-runes", ingestMinRunes, "与 mem ingest 一致的最小长度")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *session == "" {
		return errors.New("需要 --session <文件>（抽哪个会话/日志）。\n" +
			"最短可用：\n" +
			"  mem eval write --session <会话/日志文件>          # 只给可自动算的质量指标\n" +
			"  mem eval write --session <文件> --gold <清单>    # 附金标准，给漏记/多记")
	}
	cands, lines, err := extractCandidates(*session, *minRunes)
	if err != nil {
		return err
	}

	texts := make([]string, len(cands))
	for i, c := range cands {
		texts[i] = c.Text
	}

	res := map[string]any{
		"session": *session, "lines": lines, "candidates": len(cands),
		"fragments": countFragments(texts), "avg_runes": avgRunes(texts),
	}
	if *goldPath != "" {
		gold, err := readGoldPhrases(*goldPath)
		if err != nil {
			return err
		}
		missed, extra := diffAgainstGold(texts, gold)
		res["gold_total"] = len(gold)
		res["missed"] = missed
		res["extra_estimate"] = extra
	} else if c.json {
		return printJSON(res)
	}

	if c.json {
		return printJSON(res)
	}
	fmt.Printf("会话文件  : %s\n", *session)
	fmt.Printf("行数      : %d → 候选 %d 条\n", lines, len(cands))
	fmt.Printf("平均长度  : %.0f 字符\n", res["avg_runes"])
	fmt.Printf("疑似碎片  : %d 条（以逗号/顿号结尾 → 像是被硬换行切断）\n", res["fragments"])
	if *goldPath != "" {
		fmt.Printf("\n金标准    : %d 条应记事实\n", res["gold_total"])
		fmt.Printf("漏记      : %d 条（%.1f%%）\n", len(res["missed"].([]string)),
			float64(len(res["missed"].([]string)))/float64(maxInt(res["gold_total"].(int), 1))*100)
		fmt.Printf("多记(估)  : %d 条（保守下界：未匹配到 gold 的候选，可能对应未标注的真实事实）\n",
			res["extra_estimate"])
		if ms := res["missed"].([]string); len(ms) > 0 {
			fmt.Println("\n漏记清单：")
			for _, m := range ms {
				fmt.Printf("  - %s\n", m)
			}
		}
	} else {
		fmt.Println("\n未给 --gold：以上只是启发式质量指标，**不能当作准确率**。")
		fmt.Println("要评漏记/多记，需要人工标注 gold（每行一条应当被记录的事实短语）。")
	}
	return nil
}

// countFragments 数「疑似被硬换行切断」的候选：以逗号/顿号/分号结尾。
func countFragments(cands []string) int {
	n := 0
	for _, c := range cands {
		t := strings.TrimSpace(c)
		if t == "" {
			continue
		}
		switch []rune(t)[len([]rune(t))-1] {
		case '，', '、', '；', ',', ';':
			n++
		}
	}
	return n
}

func avgRunes(xs []string) float64 {
	if len(xs) == 0 {
		return 0
	}
	total := 0
	for _, x := range xs {
		total += len([]rune(x))
	}
	return float64(total) / float64(len(xs))
}

func readGoldPhrases(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func diffAgainstGold(cands, gold []string) (missed []string, extra int) {
	matched := 0
	usedCand := make([]bool, len(cands))
	for _, g := range gold {
		hit := false
		for i, cd := range cands {
			if strings.Contains(cd, g) {
				hit = true
				usedCand[i] = true
			}
		}
		if !hit {
			missed = append(missed, g)
		}
	}
	for _, u := range usedCand {
		if u {
			matched++
		}
	}
	return missed, len(cands) - matched
}

// ---------- 小工具 ----------

func allHashes(st *store.Store) (map[string]bool, error) {
	hits, err := st.List(store.ListQuery{Scope: "all", Limit: 1000000})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(hits))
	for _, h := range hits {
		out[hashOrEmpty(st, h.ID)] = true
	}
	return out, nil
}

// hashOrEmpty 取记忆的 content_hash，失败返回空串（评测路径不该因单条失败中断）。
// 金标准用哈希而非文本匹配，避免「同一事实换个说法就算命中」的自欺。
func hashOrEmpty(st *store.Store, id string) string {
	h, err := st.HashByID(id)
	if err != nil {
		return ""
	}
	return h
}

// ---------- mem eval activity：用量视图（任务收尾的 mem 评测结论） ----------

// cmdEvalActivity 给「每次任务结束输出 mem 评测结论」提供参数支持。
//
// 按时间窗（period / task）或会话（session）切出期间的读、写两侧活动，并直接给出
// 四态结论 —— 让「mem 到底起没起作用」在任务收尾时就有一个可写进交付物的判据，
// 而不是等攒够样本再回头分析。
//
// 它取代了被删除的 `mem report`：报与 eval 同级，不该有独立 CLI 入口（见
// DESIGN.md §16）。task 粒度没有 harness 事件可依赖，由调用方显式给出窗口
// （training-products 的 task-finish.sh 用分支首提交时间当起点）。
func cmdEvalActivity(args []string) error {
	var c common
	fs := flag.NewFlagSet("eval activity", flag.ContinueOnError)
	c.register(fs)
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl）")
	project := fs.String("project", "", "限定工程（默认不限）")
	scope := fs.String("scope", "period", "粒度: period|task|session")
	since := fs.String("since", "8h", "时间窗，如 30m / 2h / 24h（period/task 粒度）")
	session := fs.String("session", "", "限定会话 id（session 粒度）")
	topN := fs.Int("top", 5, "空命中查询最多列几条")
	includeBench := fs.Bool("include-bench", false, "计入 bench/test 来源的写侧（默认排除，避免程序化灌库污染统计）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *scope {
	case "period", "task", "session":
	default:
		return fmt.Errorf("未知 --scope: %q（可选 period|task|session）", *scope)
	}
	d, err := time.ParseDuration(*since)
	if err != nil || d <= 0 {
		return fmt.Errorf("非法 --since: %q（需为 30m / 2h / 24h 这类正时长）", *since)
	}
	if *scope == "session" && *session == "" {
		return errors.New("--scope session 需要 --session <会话 id>")
	}
	cutoff := time.Now().Add(-d).Unix()

	// 与 cmdAudit 保持一致：未指定时取默认审计日志位置。
	p := *auditFile
	if p == "" {
		p = audit.Path()
	}
	// 🔴 「读不到日志」与「窗口内无活动」是两回事，不能都报「未使用记忆库」。
	// 前者是环境/路径问题（比如路径写错、日志被清），后者才是真实结论。
	if !audit.Exists(p) {
		if c.json {
			return printJSON(map[string]any{"audit_file": p, "exists": false})
		}
		fmt.Printf("⚠️ 审计日志不存在：%s\n", p)
		fmt.Println("   这不是「未使用记忆库」，而是读不到日志 —— 请确认路径，或先用一次带钩子的工具生成记录。")
		return nil
	}

	all, err := audit.Read(p, 0)
	if err != nil {
		return err
	}
	var rs []audit.Record
	for _, r := range all {
		if *project != "" && r.Project != *project {
			continue
		}
		// 写侧治理：bench/test 属程序化灌库（基准/测试），默认不计入用量视图。
		// --include-bench 放开。读侧（batch/read 事件）无 source，不受影响。
		if !*includeBench && (r.Source == "bench" || r.Source == "test") {
			continue
		}
		if *scope == "session" {
			if r.SessionID != *session {
				continue
			}
		} else if r.TS < cutoff {
			continue
		}
		rs = append(rs, r)
	}
	s := audit.Summarize(rs)

	if c.json {
		return printJSON(map[string]any{
			// 带上读的是哪个日志：本次 bug 难发现的根因之一就是「报告没说它读了哪」，
			// 空路径被静默当成空日志，从输出上看不出任何异常。
			"audit_file": p, "exists": true,
			"scope": *scope, "since": *since, "project": *project, "session": *session,
			"include_bench": *includeBench,
			"summary":       s, "verdict": activityVerdict(s), "empty_queries": emptyQueries(rs, *topN),
		})
	}

	fmt.Printf("mem 评测结论（%s级", *scope)
	if *scope == "session" {
		fmt.Printf("，会话 %s", *session)
	} else {
		fmt.Printf("，窗口 %s", *since)
	}
	if *project != "" {
		fmt.Printf("，工程 %s", *project)
	}
	fmt.Printf("）\n")
	fmt.Printf("日志      : %s\n\n", p)
	fmt.Printf("活动      : 读侧 %d 次 / 写侧 %d 次\n", s.Retrievals, s.Writes)
	if *includeBench {
		fmt.Printf("（已含 bench/test 来源的写侧 —— 默认排除）\n")
	}
	fmt.Printf("空命中    : %d 次（%.1f%%）\n", s.Empty, s.EmptyRate*100)
	fmt.Printf("平均耗时  : %.1f ms\n", s.AvgMS)
	if s.Writes > 0 {
		fmt.Printf("写侧构成  : 新增 %d，命中覆盖 %d（重复记录率 %.0f%%）\n",
			s.Creates, s.Overwrites, float64(s.Overwrites)/float64(s.Writes)*100)
	}
	if eq := emptyQueries(rs, *topN); len(eq) > 0 {
		fmt.Println("空命中查询（标注金标准的候选样本）：")
		for _, q := range eq {
			fmt.Printf("  - %s\n", truncateRunes(q, 60))
		}
	}
	fmt.Printf("结论      : %s\n", activityVerdict(s))
	return nil
}

// activityVerdict 给出可直接写进交付物的一句话结论。
// 「只查未记」与「只记未查」是两种不同的故障，必须分开说。
func activityVerdict(s audit.Summary) string {
	switch {
	case s.Total == 0:
		return "本次任务未使用记忆库（窗口内既无检索也无记录活动）"
	case s.Retrievals == 0:
		return fmt.Sprintf("只记未查：记录 %d 条但一次未检索 —— 记忆被写入却没被用上", s.Writes)
	case s.Writes == 0:
		return fmt.Sprintf("只查未记：检索 %d 次但未沉淀任何新事实 —— 这次任务没有留下可复用的东西", s.Retrievals)
	}
	v := fmt.Sprintf("读 %d / 写 %d，两侧都有活动", s.Retrievals, s.Writes)
	if s.EmptyRate > 0.5 {
		v += fmt.Sprintf("；但空命中率 %.0f%% 偏高，检索质量可疑，建议把上面的空命中查询标注成金标准后跑 mem eval recall --gold",
			s.EmptyRate*100)
	}
	return v
}

// emptyQueries 列出空命中的查询原文（去重保序）。
func emptyQueries(rs []audit.Record, limit int) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rs {
		q := strings.TrimSpace(r.Query)
		if q == "" || !r.Empty || seen[q] {
			continue
		}
		seen[q] = true
		out = append(out, q)
		if len(out) >= limit {
			break
		}
	}
	return out
}
