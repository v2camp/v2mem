package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScoreComputesMacroRecallAndMRR(t *testing.T) {
	cases := []GoldCase{
		{Query: "a", Gold: []string{"aaaa1111"}},             // 命中第 1 位
		{Query: "b", Gold: []string{"bbbb2222", "bbbb3333"}}, // 命中 1/2，第 2 位
		{Query: "c", Gold: []string{"cccc4444"}},             // 未命中
	}
	retrieved := [][]string{
		{"aaaa1111", "xxxx"},
		{"yyyy", "bbbb3333"},
		{}, // 检索完全无结果 → 计入 EmptyRate
	}
	rep := Score(cases, retrieved, 5)
	if rep.Cases != 3 {
		t.Fatalf("样本数应为 3，got %d", rep.Cases)
	}
	// (1 + 0.5 + 0) / 3 = 0.5
	if got := rep.RecallAtK; got < 0.499 || got > 0.501 {
		t.Errorf("Recall@5 应=0.5（宏平均），got %v", got)
	}
	// (1/1 + 1/2 + 0) / 3 = 0.5
	if got := rep.MRR; got < 0.499 || got > 0.501 {
		t.Errorf("MRR 应=0.5，got %v", got)
	}
	// 3 条里 1 条检索完全无结果 → 1/3
	if got := rep.EmptyRate; got < 0.333 || got > 0.334 {
		t.Errorf("EmptyRate 应=1/3，got %v", got)
	}
}

// 口径区分：「检索有结果但没命中 gold」不等于「检索完全无结果」。
// 两者混为一谈会把「索引挂了」和「排序不好」算成同一个数，归因就废了。
func TestEmptyRateOnlyCountsZeroResults(t *testing.T) {
	cases := []GoldCase{
		{Query: "有结果但没命中", Gold: []string{"aaaa1111"}},
		{Query: "完全无结果", Gold: []string{"bbbb2222"}},
	}
	retrieved := [][]string{{"zzz", "yyy"}, {}}
	rep := Score(cases, retrieved, 5)
	if got := rep.EmptyRate; got != 0.5 {
		t.Errorf("EmptyRate 应=0.5（只有一条真的空），got %v", got)
	}
	if got := rep.RecallAtK; got != 0 {
		t.Errorf("两条都没命中，Recall 应=0，got %v", got)
	}
}

// k 必须真的截断：gold 排在第 k+1 位时不应算命中。
func TestScoreRespectsK(t *testing.T) {
	cases := []GoldCase{{Query: "a", Gold: []string{"aaaa1111"}}}
	retrieved := [][]string{{"x1", "x2", "x3", "x4", "aaaa1111"}}
	if got := Score(cases, retrieved, 3).RecallAtK; got != 0 {
		t.Errorf("k=3 时第 5 位不应算命中，got %v", got)
	}
	if got := Score(cases, retrieved, 5).RecallAtK; got != 1 {
		t.Errorf("k=5 时应命中，got %v", got)
	}
}

// 宏平均而非 micro：gold 多的样本不得主导指标。
func TestScoreUsesMacroAverage(t *testing.T) {
	cases := []GoldCase{
		{Query: "少", Gold: []string{"aaaa1111"}},             // 全中 → 1.0
		{Query: "多", Gold: []string{"b1", "b2", "b3", "b4"}}, // 0 中 → 0.0
	}
	retrieved := [][]string{{"aaaa1111"}, {"zzz"}}
	got := Score(cases, retrieved, 5).RecallAtK
	if got != 0.5 {
		t.Errorf("宏平均应=0.5（micro 会是 1/5=0.2），got %v", got)
	}
}

func TestScoreOnEmptyInputs(t *testing.T) {
	rep := Score(nil, nil, 5)
	if rep.Cases != 0 || rep.RecallAtK != 0 || rep.MRR != 0 {
		t.Errorf("空输入应得零值报告，got %+v", rep)
	}
}

func TestValidateReportsMissingGoldInsteadOfSilentlyDropping(t *testing.T) {
	cases := []GoldCase{
		{Query: "好", Gold: []string{"aaaa1111"}},
		{Query: "坏", Gold: []string{"deadbeef"}},
		{Query: "空", Gold: nil},
	}
	known := map[string]bool{"aaaa1111bbbb": true}
	valid, invalid := Validate(cases, known)
	if len(valid) != 1 || valid[0].Query != "好" {
		t.Errorf("应只剩 1 条有效样本，got %+v", valid)
	}
	if len(invalid) != 2 {
		t.Fatalf("应报告 2 条无效样本，got %d: %v", len(invalid), invalid)
	}
	joined := invalid[0] + "|" + invalid[1]
	for _, want := range []string{"deadbeef", "gold 为空"} {
		if !contains(joined, want) {
			t.Errorf("无效原因应含 %q，got %v", want, invalid)
		}
	}
}

func TestLoadGoldSkipsCommentsAndReportsBadLines(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "gold.jsonl")
	body := "# 注释行\n\n{\"query\":\"q1\",\"gold\":[\"aaaa1111\"]}\n"
	if err := os.WriteFile(good, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cases, err := LoadGold(good)
	if err != nil {
		t.Fatalf("LoadGold: %v", err)
	}
	if len(cases) != 1 {
		t.Errorf("应读到 1 条，got %d", len(cases))
	}

	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte("{\"query\":\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadGold(bad); err == nil {
		t.Error("坏行应报错并指出行号")
	}
}

func TestSortByRecallShowsWorstFirst(t *testing.T) {
	rs := []CaseResult{
		{Query: "好", Gold: 1, Hit: 1},
		{Query: "差", Gold: 1, Hit: 0},
		{Query: "中", Gold: 2, Hit: 1},
	}
	got := SortByRecall(rs)
	if got[0].Query != "差" || got[2].Query != "好" {
		t.Errorf("应按召回率升序（最差在前），got %+v", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
