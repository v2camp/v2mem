package similarity

import "testing"

// ---------- 模糊检索护栏 ----------

// EnoughShingles 是「是否值得做模糊估计」的判据（检索侧复用归并护栏 minShingles）。
func TestEnoughShingles(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"", false},
		{"日志", false},
		{"搬进记忆库", false},          // 5 字 → 3 个 trigram
		{"日志怎么放进记忆库里", true}, // 10 字 → 8 个 trigram
		{"日志怎么搬进记忆库里", true}, // 10 字 → 8 个 trigram
	}
	for _, c := range cases {
		if got := EnoughShingles(c.text); got != c.want {
			t.Errorf("EnoughShingles(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// ---------- 相似度估计 ----------
//
// 用字符级 3-gram + MinHash 估 Jaccard，不依赖分词器与任何模型。
// 中文没有词边界，字符 n-gram 是最省事且语言无关的选择。

func TestIdenticalTextsAreFullySimilar(t *testing.T) {
	if got := Similarity("记忆库数据放在 ~/.v2mem", "记忆库数据放在 ~/.v2mem"); got != 1.0 {
		t.Errorf("完全相同的文本相似度应为 1.0，got %v", got)
	}
}

// 只差一两个字的句子必须被判为高度相似 —— 这是「相似知识归并」要抓的主要形态。
func TestNearDuplicateTextsScoreHigh(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		min  float64
	}{
		{"尾部多一句", "记忆库数据必须放在 ~/.v2mem 目录", "记忆库数据必须放在 ~/.v2mem 目录下", 0.7},
		{"同义改写少量字", "活库不能放进 iCloud 同步目录，会损坏 SQLite", "活库不能放进 iCloud 同步目录，会损坏 sqlite 库", 0.7},
		{"中英混排", "用 CGO_ENABLED=0 构建，产物是单文件", "用 CGO_ENABLED=0 构建，产物是单个文件", 0.7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Similarity(c.a, c.b)
			if got < c.min {
				t.Errorf("相似度应 >= %v，got %v\n  a=%q\n  b=%q", c.min, got, c.a, c.b)
			}
		})
	}
}

// 负对照：不相干的句子必须低分，否则归并会大量误伤。
func TestUnrelatedTextsScoreLow(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{"主题无关", "记忆库数据必须放在 ~/.v2mem", "今天下午三点开评审会"},
		{"结构相近但主题不同", "记忆库不能放同步目录", "部署脚本不能放共享盘"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Similarity(c.a, c.b); got >= 0.5 {
				t.Errorf("无关文本相似度应 < 0.5，got %v", got)
			}
		})
	}
}

// 同一进程内重复调用必须给出完全相同的值（不含随机性）。
func TestSimilarityIsDeterministic(t *testing.T) {
	a, b := "记忆库数据必须放在 ~/.v2mem 目录", "记忆库数据必须放在 ~/.v2mem 目录下"
	first := Similarity(a, b)
	for i := 0; i < 20; i++ {
		if got := Similarity(a, b); got != first {
			t.Fatalf("第 %d 次调用结果不同：%v vs %v", i+1, got, first)
		}
	}
}

// 跨设备要能独立算出同一个值，签名不能依赖进程内随机数或运行次数。
// 这里用「把签名序列化后再比」来固化：同样的文本必须得到同样的签名。
func TestSignatureIsStableAcrossRecomputation(t *testing.T) {
	text := "记忆库数据必须放在 ~/.v2mem 目录"
	a := Sign(text)
	b := Sign(text)
	if len(a) != len(b) {
		t.Fatalf("签名长度应一致：%d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("签名第 %d 位不同：%d vs %d", i, a[i], b[i])
		}
	}
}

// 空文本与超短文本不能 panic，也不能虚假满分。
func TestShortAndEmptyTexts(t *testing.T) {
	if got := Similarity("", ""); got != 1.0 {
		t.Errorf("两个空串视为相同，got %v", got)
	}
	if got := Similarity("", "记忆库"); got != 0.0 {
		t.Errorf("空串与非空串相似度应为 0，got %v", got)
	}
	if got := Similarity("库", "库"); got != 1.0 {
		t.Errorf("单字相同应为 1.0，got %v", got)
	}
	if got := Similarity("库", "表"); got >= 0.5 {
		t.Errorf("单字不同不应判为相似，got %v", got)
	}
}

// 估算值必须落在 [0,1]，且对称。
func TestSimilarityIsBoundedAndSymmetric(t *testing.T) {
	a, b := "记忆库数据必须放在 ~/.v2mem 目录", "记忆库数据不能放在代码目录"
	ab, ba := Similarity(a, b), Similarity(b, a)
	if ab < 0 || ab > 1 {
		t.Errorf("相似度必须落在 [0,1]，got %v", ab)
	}
	// MinHash 估计本身对称（同一对签名逐位比较）
	if ab != ba {
		t.Errorf("相似度应对称：a→b=%v  b→a=%v", ab, ba)
	}
}

// ---------- 归并前置护栏 ----------
//
// 纯阈值的两个致命问题（2026-09-17 实测，见 DESIGN.md §11.10）：
//  1. "Go 构建必须用 CGO_ENABLED=0" vs "...=1" 只差一个字符，相似度却高达 0.906。
//     字符 n-gram 对「一个字符导致语义反转」完全无感 —— 若只卡阈值，这类会被误归并。
//  2. 短文本 shingle 集小，Jaccard 估计方差极大："库不放同步目录" vs
//     "库不放共享目录" 只差一字，相似度仅 0.078，永远够不到阈值。
//
// 结论：准确性交给护栏，阈值只负责提召回。

// 校准矩阵：把实测分布固化成断言，任何调参导致的漂移都会在此报警。
func TestCalibrationMatrix(t *testing.T) {
	const defThreshold = 0.7
	cases := []struct {
		name      string
		a, b      string
		simRange  [2]float64 // [下界, 上界]
		wantMerge bool
	}{
		{"完全相同", "记忆库数据必须放在 ~/.v2mem 目录", "记忆库数据必须放在 ~/.v2mem 目录", [2]float64{1, 1}, true},
		{"仅差尾字", "记忆库数据必须放在 ~/.v2mem 目录", "记忆库数据必须放在 ~/.v2mem 目录下", [2]float64{0.9, 1}, true},
		{"差两字", "记忆库数据必须放在 ~/.v2mem 目录", "记忆库数据必须放在 ~/.v2mem 路径", [2]float64{0.6, 0.85}, true},
		{"同义改写", "活库不能放进 iCloud 同步目录，会损坏 SQLite", "活库不能放进 iCloud 同步目录，会损坏 sqlite 库", [2]float64{0.85, 1}, true},
		{"补一句", "记忆库数据必须放在 ~/.v2mem 目录", "记忆库数据必须放在 ~/.v2mem 目录，方便统一回收", [2]float64{0.7, 0.9}, true},
		// 🔴 关键负例：相似度极高但语义相反，必须被护栏拦下
		{"数字反转", "Go 构建必须用 CGO_ENABLED=0", "Go 构建必须用 CGO_ENABLED=1", [2]float64{0.85, 1}, false},
		// 🔴 关键负例：否定反转
		{"否定反转", "记忆库可以放进同步目录", "记忆库不可以放进同步目录", [2]float64{0.6, 1}, false},
		// 短文本因 shingle 过少而不参与归并
		{"短句差一字", "库不放同步目录", "库不放共享目录", [2]float64{0, 0.3}, false},
		{"同主题不同细节", "记忆库数据必须放在 ~/.v2mem 目录", "记忆库的导出文件放在 ~/.v2mem/sync 目录", [2]float64{0.2, 0.6}, false},
		{"主题无关", "记忆库数据必须放在 ~/.v2mem", "今天下午三点开评审会", [2]float64{0, 0.2}, false},
		{"结构相近主题不同", "记忆库不能放同步目录", "部署脚本不能放共享盘", [2]float64{0, 0.2}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sim := Similarity(c.a, c.b)
			if sim < c.simRange[0] || sim > c.simRange[1] {
				t.Errorf("相似度应落在 [%v, %v]，got %v", c.simRange[0], c.simRange[1], sim)
			}
			got := Judge(c.a, c.b, defThreshold)
			if got.Allowed != c.wantMerge {
				t.Errorf("归并裁定应为 %v，got %v（相似度 %v，理由 %q）",
					c.wantMerge, got.Allowed, got.Similarity, got.Reason)
			}
		})
	}
}

func TestJudgeRejectsWhenDigitsDiffer(t *testing.T) {
	v := Judge("端口固定为 6379", "端口固定为 6380", 0.5)
	if v.Allowed {
		t.Errorf("数字不同必须拒绝归并，got %+v", v)
	}
	if v.Reason != ReasonDigitsDiffer {
		t.Errorf("理由应为 %q，got %q", ReasonDigitsDiffer, v.Reason)
	}
}

func TestJudgeAllowsWhenDigitsMatch(t *testing.T) {
	v := Judge("端口固定为 6379", "端口固定为 6379 别改", 0.5)
	if !v.Allowed {
		t.Errorf("数字一致且足够相似时应允许归并，got %+v", v)
	}
}

func TestJudgeRejectsNegationFlip(t *testing.T) {
	v := Judge("记忆库可以放进同步目录", "记忆库不可以放进同步目录", 0.5)
	if v.Allowed {
		t.Errorf("否定词数量不同必须拒绝归并，got %+v", v)
	}
	if v.Reason != ReasonNegationDiffers {
		t.Errorf("理由应为 %q，got %q", ReasonNegationDiffers, v.Reason)
	}
}

func TestJudgeRejectsLengthOutlier(t *testing.T) {
	long := "记忆库数据必须放在 ~/.v2mem 目录，不能放在代码目录，因为代码目录会被 git 提交带走，" +
		"而且多机同步时会与 iCloud 冲突，导致 SQLite 库损坏，这是实测过的坑"
	v := Judge("记忆库数据必须放在 ~/.v2mem 目录", long, 0.3)
	if v.Allowed {
		t.Errorf("长度悬殊必须拒绝归并（防一句话吞掉整段），got %+v", v)
	}
	if v.Reason != ReasonLengthRatio {
		t.Errorf("理由应为 %q，got %q", ReasonLengthRatio, v.Reason)
	}
}

func TestJudgeRejectsTooShortTexts(t *testing.T) {
	// 短文本 shingle 太少，估计方差不可接受；完全相同的情况由 hash 覆盖机制处理，不走归并
	v := Judge("库不放同步目录", "库不放同部目录", 0.0)
	if v.Allowed {
		t.Errorf("过短文本不应参与归并，got %+v", v)
	}
	if v.Reason != ReasonTooShort {
		t.Errorf("理由应为 %q，got %q", ReasonTooShort, v.Reason)
	}
}

func TestJudgeReportsBelowThreshold(t *testing.T) {
	// 要测到阈值分支，必须先满足前面所有护栏，否则测的是别的分支：
	//   - 都不含数字（"~/.v2mem" 里的 2 也算数字，见下一条用例）
	//   - 长度都 >= minShingles+shingleN-1 = 10 个字符
	//   - 长度比在区间内、否定词计数相同
	const a = "记忆库数据必须放在固定目录里"
	const b = "今天下午集中开一次技术评审会"
	v := Judge(a, b, 0.7)
	if v.Allowed {
		t.Errorf("低于阈值不应归并，got %+v", v)
	}
	if v.Reason != ReasonBelowThreshold {
		t.Errorf("理由应为 %q，got %q", ReasonBelowThreshold, v.Reason)
	}
}

// 标识符里的数字同样参与比较：v2mem 与 v3mem、UTF-8 与 UTF-16 不是同一条知识。
func TestJudgeTreatsIdentifierDigitsAsMeaningful(t *testing.T) {
	v := Judge("数据统一放 ~/.v2mem 目录", "数据统一放 ~/.v3mem 目录", 0.5)
	if v.Allowed {
		t.Errorf("标识符版本号不同必须拒绝归并，got %+v", v)
	}
	if v.Reason != ReasonDigitsDiffer {
		t.Errorf("理由应为 %q，got %q", ReasonDigitsDiffer, v.Reason)
	}
}

// 数字抽取按出现顺序去重，故「顺序不同」也会被拦下——刻意的保守。
func TestDigitsAreOrderedAndDeduplicated(t *testing.T) {
	got := Digits("端口 6379 与 6380，还是 6379")
	want := []string{"6379", "6380"}
	if len(got) != len(want) {
		t.Fatalf("Digits 应去重，got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Digits 应保持出现顺序，got %v want %v", got, want)
		}
	}
	if d := Digits("没有数字的句子"); len(d) != 0 {
		t.Errorf("无数字时应返回空，got %v", d)
	}
}
