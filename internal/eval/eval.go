// Package eval 计算记忆检索的评测指标。
//
// 这里是**纯函数**：只做「金标准 + 检索结果 → 指标」的计算，不碰数据库，
// 因此口径可以被独立测试固化。口径必须先定死再跑数，否则会像任何评测体系一样
// 事后按结果调口径。
package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// GoldCase 是一条金标准样本。Gold 用 content_hash 前缀表达 ——
// 用哈希而不是文本包含匹配，避免「同一事实换个说法就算命中」的自欺。
type GoldCase struct {
	Query string   `json:"query"`
	Gold  []string `json:"gold"`
	Note  string   `json:"note,omitempty"`
}

// CaseResult 是单条样本的结果。
type CaseResult struct {
	Query     string `json:"query"`
	Gold      int    `json:"gold"`
	Hit       int    `json:"hit"`
	FirstRank int    `json:"first_rank"` // 0 表示未命中
}

// Report 是一次评测的结果。
type Report struct {
	K         int          `json:"k"`
	Cases     int          `json:"cases"` // 有效样本数（分母）
	RecallAtK float64      `json:"recall_at_k"`
	MRR       float64      `json:"mrr"`
	EmptyRate float64      `json:"empty_rate"` // 检索完全无结果的样本比例
	Results   []CaseResult `json:"results,omitempty"`
}

// Score 计算指标。
//
// 口径（红线，改动须同步文档）：
//   - Recall@k **宏平均**：每个样本等权 —— Σ(命中 gold 数 / 该样本 gold 数) / N。
//     不用 micro 平均，否则 gold 多的样本会主导指标。
//   - 指标只看前 k 条，k 必须显式给出。
//   - 命中判定用哈希前缀匹配；gold 前缀长度由调用方保证（建议 ≥8）。
//   - N 是**有效样本数**；gold 不在库中的样本必须先由 Validate 剔除，
//     且剔除数必须上报 —— 静默剔除等于篡改分母。
func Score(cases []GoldCase, retrieved [][]string, k int) *Report {
	rep := &Report{K: k, Cases: len(cases)}
	if len(cases) == 0 {
		return rep
	}
	for i, c := range cases {
		top := []string{}
		if i < len(retrieved) {
			top = retrieved[i]
			if len(top) > k {
				top = top[:k]
			}
		}
		res := CaseResult{Query: c.Query, Gold: len(c.Gold)}
		if len(top) == 0 {
			rep.EmptyRate++
		}
		for rank, h := range top {
			if matchesGold(h, c.Gold) {
				res.Hit++
				if res.FirstRank == 0 {
					res.FirstRank = rank + 1
				}
			}
		}
		if c.Gold != nil {
			rep.RecallAtK += float64(res.Hit) / float64(len(c.Gold))
		}
		if res.FirstRank > 0 {
			rep.MRR += 1 / float64(res.FirstRank)
		}
		rep.Results = append(rep.Results, res)
	}
	n := float64(len(cases))
	rep.RecallAtK /= n
	rep.MRR /= n
	rep.EmptyRate /= n
	return rep
}

func matchesGold(hash string, gold []string) bool {
	for _, g := range gold {
		if g != "" && strings.HasPrefix(hash, g) {
			return true
		}
	}
	return false
}

// Validate 剔除 gold 不在库中的样本，并返回被剔除的原因。
// 被剔除数必须打印出来 —— 否则「gold 写错了」会表现成「Recall 低」，误导结论。
func Validate(cases []GoldCase, known map[string]bool) (valid []GoldCase, invalid []string) {
	for _, c := range cases {
		if len(c.Gold) == 0 {
			invalid = append(invalid, fmt.Sprintf("%q：gold 为空", c.Query))
			continue
		}
		missing := []string{}
		for _, g := range c.Gold {
			if !hasPrefixKey(known, g) {
				missing = append(missing, g)
			}
		}
		if len(missing) > 0 {
			invalid = append(invalid, fmt.Sprintf("%q：gold 不在库中 %v", c.Query, missing))
			continue
		}
		valid = append(valid, c)
	}
	return valid, invalid
}

func hasPrefixKey(known map[string]bool, prefix string) bool {
	for h := range known {
		if strings.HasPrefix(h, prefix) {
			return true
		}
	}
	return false
}

// LoadGold 读 JSONL 金标准，每行一个 GoldCase。坏行报错并指出行号。
func LoadGold(path string) ([]GoldCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []GoldCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var c GoldCase
		if err := json.Unmarshal([]byte(text), &c); err != nil {
			return nil, fmt.Errorf("第 %d 行不是合法 JSON: %w", line, err)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// SortByRecall 按「召回率升序」排序结果，方便先看最差的那批。
func SortByRecall(rs []CaseResult) []CaseResult {
	out := append([]CaseResult(nil), rs...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := ratio(out[i]), ratio(out[j])
		return ri < rj
	})
	return out
}

func ratio(r CaseResult) float64 {
	if r.Gold == 0 {
		return 0
	}
	return float64(r.Hit) / float64(r.Gold)
}
