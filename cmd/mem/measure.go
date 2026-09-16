// 评测：回答「这套记忆系统到底准不准」，而不是靠印象。
//
// 两个方向都要测（用户提出）：
//   - 用户会话 → mem 查询 → 命中是否准确   : mem eval recall（金标准）／audit（真实样本）
//   - 用户会话 → mem 记录 → 记录是否准确   : mem eval write
//
// 口径先定死再跑数，且**自动模式必须标注为下限测试** —— 不能让「自召回接近 100%」
// 被当成「真实检索质量好」。这是本仓库评测纪律的老坑。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

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

	fmt.Println("\n最近记录（新→旧）：")
	start := len(rs) - 1
	for i := start; i >= 0 && i > start-*tail; i-- {
		r := rs[i]
		q := r.Query
		if q == "" {
			q = "（无 query）"
		}
		fmt.Printf("  %s  %-14s 命中%2d 空=%-5v %4dms  %s\n",
			r.Event, r.Harness, len(r.Hashes), r.Empty, r.MS, truncateRunes(q, 40))
	}
	return nil
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
		return errors.New("用法: mem eval recall|write（见 mem help）")
	}
	switch args[0] {
	case "recall":
		return cmdEvalRecall(args[1:])
	case "write":
		return cmdEvalWrite(args[1:])
	default:
		return fmt.Errorf("未知子命令 %q；可选 recall | write", args[0])
	}
}

func cmdEvalRecall(args []string) error {
	var c common
	fs := flag.NewFlagSet("eval recall", flag.ContinueOnError)
	c.register(fs)
	goldPath := fs.String("gold", "", "金标准 JSONL（每行 {\"query\":..,\"gold\":[\"<hash前缀>\"..]}）")
	auto := fs.Int("auto", 0, "免标注自召回下限测试：抽样 N 条记忆当金标准")
	k := fs.Int("k", defaultEvalK, "top-k")
	project := fs.String("project", "", "限定工程（--auto 时按该工程抽样）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *k <= 0 {
		return fmt.Errorf("--k 应为正数，got %d", *k)
	}
	if *goldPath == "" && *auto <= 0 {
		return errors.New("需要 --gold <文件> 或 --auto <N>；真实评测请用 --gold")
	}
	if *goldPath != "" && *auto > 0 {
		return errors.New("--gold 与 --auto 不可同时使用")
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	if *auto > 0 {
		return runAutoRecall(st, c, *auto, *k, *project)
	}
	return runGoldRecall(st, c, *goldPath, *k)
}

// runGoldRecall 跑真实金标准。这是**唯一能支撑「替换」决策**的指标来源。
func runGoldRecall(st *store.Store, c common, goldPath string, k int) error {
	cases, err := eval.LoadGold(goldPath)
	if err != nil {
		return err
	}
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
		return printJSON(map[string]any{"report": rep, "invalid": invalid, "gold_file": goldPath})
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
		return errors.New("需要 --session <文件>")
	}
	cands, lines, err := extractCandidates(*session, *minRunes)
	if err != nil {
		return err
	}

	res := map[string]any{
		"session": *session, "lines": lines, "candidates": len(cands),
		"fragments": countFragments(cands), "avg_runes": avgRunes(cands),
	}
	if *goldPath != "" {
		gold, err := readGoldPhrases(*goldPath)
		if err != nil {
			return err
		}
		missed, extra := diffAgainstGold(cands, gold)
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
