// Package similarity 用字符级 n-gram + MinHash 估计两段文本的 Jaccard 相似度。
//
// 为什么不用 embedding：相似知识归并是近重复检测，属经典算法问题而非语义理解问题。
// MinHash 几十行、微秒级、零依赖、结果可复现——这正是 v2mem 能保持
// 「零 CGO / 零外部服务 / 单文件分发」的原因（见 DESIGN.md §11.1）。
//
// 为什么用字符 n-gram 而非分词：中文没有词边界。FTS5 内置分词器实测均不适用
// （见 DESIGN.md §11.3），字符 n-gram 语言无关且无需词典。
package similarity

import (
	"hash/fnv"
	"strings"
)

const (
	// shingleN 是 n-gram 的窗口（按 rune 计）。
	// 取 3 是权衡：2-gram 对短文本过于宽松，4-gram 对少量改写过严。
	shingleN = 3
	// numHashes 是 MinHash 签名长度。64 位下标准差约 1/sqrt(64)=12.5%，
	// 对「阈值 0.8 判近重复」足够；加大到 128 只换来更稳的估计，代价线性。
	numHashes = 64
	// mersennePrime = 2^31-1，用于 (a*x+b) mod p 哈希族。
	mersennePrime = 2147483647
)

// mults/adds 是 MinHash 的哈希族参数。
//
// 必须完全确定：两台设备要独立算出同一个签名，否则跨设备归并结果不一致。
// 因此不用 math/rand（其序列虽对固定种子稳定，但仍绑定标准库实现），
// 改用自带的 xorshift64 与固定种子，显式且与 Go 版本无关。
var (
	mults [numHashes]uint64
	adds  [numHashes]uint64
)

func init() {
	x := uint64(0x9E3779B97F4A7C15)
	next := func() uint64 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		return x
	}
	for i := 0; i < numHashes; i++ {
		mults[i] = next()%(mersennePrime-1) + 1
		adds[i] = next() % mersennePrime
	}
}

// Signature 是文本的 MinHash 签名。
type Signature []uint64

// Shingles 把文本切成字符级 n-gram 并哈希。
// 文本（按 rune 计）短于 n 时，整串作为一个 shingle——保证单字/双字文本也有签名，
// 而不是退化成空集（那会让 Similarity 恒为 0 或 1）。
func Shingles(text string) []uint64 {
	runes := []rune(strings.ToLower(text))
	if len(runes) == 0 {
		return nil
	}
	if len(runes) <= shingleN {
		return []uint64{hashRunes(runes)}
	}
	out := make([]uint64, 0, len(runes)-shingleN+1)
	for i := 0; i+shingleN <= len(runes); i++ {
		out = append(out, hashRunes(runes[i:i+shingleN]))
	}
	return out
}

func hashRunes(rs []rune) uint64 {
	h := fnv.New64a()
	var buf [4]byte
	for _, r := range rs {
		buf[0] = byte(r)
		buf[1] = byte(r >> 8)
		buf[2] = byte(r >> 16)
		buf[3] = byte(r >> 24)
		_, _ = h.Write(buf[:])
	}
	return h.Sum64()
}

// Sign 计算文本的 MinHash 签名。
func Sign(text string) Signature {
	shingles := Shingles(text)
	sig := make(Signature, numHashes)
	if len(shingles) == 0 {
		return sig // 全 0：与任何非空文本都不匹配
	}
	for i := range sig {
		sig[i] = ^uint64(0)
	}
	for _, sh := range shingles {
		for i := 0; i < numHashes; i++ {
			v := (mults[i]*sh + adds[i]) % mersennePrime
			if v < sig[i] {
				sig[i] = v
			}
		}
	}
	return sig
}

// EnoughShingles 报告文本的 shingle 数是否达到统计推断的下限。
// 检索侧用它决定是否值得做模糊匹配：shingle 太少时 Jaccard 估计方差不可接受
// （与归并护栏 minShingles 同一判据，避免短查询被噪声淹没）。
func EnoughShingles(text string) bool {
	return len(Shingles(text)) >= minShingles
}

// Estimate 由两条签名估计 Jaccard 相似度，落在 [0,1]。
func Estimate(a, b Signature) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	same := 0
	for i := 0; i < n; i++ {
		if a[i] == b[i] {
			same++
		}
	}
	return float64(same) / float64(n)
}

// Similarity 是 Sign + Estimate 的便捷入口。
//
// 边界约定：两个空串视为完全相同（1.0），空串与非空串视为完全不同（0.0）。
func Similarity(a, b string) float64 {
	ea, eb := strings.TrimSpace(a) == "", strings.TrimSpace(b) == ""
	switch {
	case ea && eb:
		return 1
	case ea || eb:
		return 0
	}
	return Estimate(Sign(a), Sign(b))
}

// ---------- 归并护栏 ----------
//
// 纯阈值不够：字符 n-gram 对「一个字符导致语义反转」无感。实测
// "Go 构建必须用 CGO_ENABLED=0" 与 "...=1" 相似度 0.906 —— 只卡阈值就会把
// 两条互斥的规则合成一条，比漏归并危险得多。因此归并裁定 = 阈值（提召回）+
// 护栏（保精度）。

// 拒绝归并的理由。
const (
	ReasonDigitsDiffer    = "digits-differ"    // 数字集合不同（0 vs 1 这类语义反转）
	ReasonNegationDiffers = "negation-differs" // 否定词数量不同（可以 vs 不可以）
	ReasonLengthRatio     = "length-ratio"     // 长度悬殊，防一句话吞掉整段
	ReasonTooShort        = "too-short"        // shingle 太少，估计方差不可接受
	ReasonBelowThreshold  = "below-threshold"  // 相似度不足
)

const (
	// defaultLengthRatio 是两条文本长度比的允许区间 [1/r, r]。
	// 3-gram 覆盖率会让「短句被长句包含」也得到不低的相似度，需要显式挡住。
	defaultLengthRatio = 3.0
	// minShingles 是参与归并的最小 shingle 数（约 10 个字符以上）。
	// 短文本的 Jaccard 估计方差极大（实测一字之差可从 0.9 掉到 0.08），
	// 宁可交给「相同内容覆盖」机制处理，也不做统计推断。
	minShingles = 8
)

// Verdict 是一次归并裁定的结果。
type Verdict struct {
	Similarity float64 // MinHash 估计的 Jaccard
	Allowed    bool
	Reason     string // 拒绝理由；允许时为空串
}

// Digits 抽出文本中的所有数字串（按出现顺序，去重）。
// 用于识别「只差一个数字但语义反转」的情形，如端口 6379/6380、开关 0/1。
func Digits(text string) []string {
	var out []string
	seen := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		s := cur.String()
		cur.Reset()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, r := range text {
		if r >= '0' && r <= '9' {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// negationWords 是判定「否定反转」的词表。
// 只做计数比较而非语义分析：数量不同即拒绝归并。这是刻意保守的选择——
// 漏归并只是少省一点空间，误归并会让两条互斥规则合并成错的。
var negationWords = []string{
	"不能", "不得", "不要", "不应", "不该", "不可", "不会", "不用",
	"禁止", "避免", "切勿", "勿", "非", "无", "没",
	"not ", "never", "no ", "don't", "doesn't", "cannot",
}

// NegationCount 统计文本中命中否定词表的次数。
func NegationCount(text string) int {
	lower := strings.ToLower(text)
	n := 0
	for _, w := range negationWords {
		n += strings.Count(lower, w)
	}
	return n
}

// Judge 裁定两段文本是否允许归并。
//
// 守序：先看形式护栏（短文本 / 长度 / 数字 / 否定），再看相似度。
// 形式护栏与相似度无关，先判可以避免在明显不该合并的样本上浪费计算。
func Judge(a, b string, threshold float64) Verdict {
	shingleCount := func(s string) int { return len(Shingles(s)) }
	if shingleCount(a) < minShingles || shingleCount(b) < minShingles {
		return Verdict{Similarity: Similarity(a, b), Reason: ReasonTooShort}
	}

	la, lb := float64(len([]rune(a))), float64(len([]rune(b)))
	if la < lb {
		la, lb = lb, la
	}
	if lb == 0 || la/lb > defaultLengthRatio {
		return Verdict{Similarity: Similarity(a, b), Reason: ReasonLengthRatio}
	}

	if !sameStrings(Digits(a), Digits(b)) {
		return Verdict{Similarity: Similarity(a, b), Reason: ReasonDigitsDiffer}
	}
	if NegationCount(a) != NegationCount(b) {
		return Verdict{Similarity: Similarity(a, b), Reason: ReasonNegationDiffers}
	}

	sim := Similarity(a, b)
	if sim < threshold {
		return Verdict{Similarity: sim, Reason: ReasonBelowThreshold}
	}
	return Verdict{Similarity: sim, Allowed: true}
}

// sameStrings 比较两个字符串切片是否同序同内容。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
