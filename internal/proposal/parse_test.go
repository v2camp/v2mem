package proposal

import (
	"errors"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
)

func TestParseDraftsMatrix(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		evCount int
		valid   int
		invalid int
		bad     bool
	}{
		{"空输出", "   ", 2, 0, 0, false},
		{"空数组", "[]", 2, 0, 0, false},
		{"JSON 数组", `[{"kind":"decision","content":"a","evidence_idx":[1]}]`, 2, 1, 0, false},
		{"JSONL", "{\"kind\":\"pitfall\",\"content\":\"b\",\"evidence_idx\":[2]}\n{\"kind\":\"task\",\"content\":\"c\",\"evidence_idx\":[1,2]}", 2, 2, 0, false},
		{"部分无效", "{\"kind\":\"decision\",\"content\":\"a\",\"evidence_idx\":[1]}\n{\"kind\":\"nope\",\"content\":\"b\",\"evidence_idx\":[1]}", 2, 1, 1, false},
		{"坏 JSON", `{"kind":`, 2, 0, 0, true},
		{"纯散文", "这是一段解释，不是 JSON", 2, 0, 0, true},
		{"引用越界", `{"kind":"decision","content":"a","evidence_idx":[3]}`, 2, 0, 1, false},
		{"引用为零", `{"kind":"decision","content":"a","evidence_idx":[0]}`, 2, 0, 1, false},
		{"没有引用", `{"kind":"decision","content":"a"}`, 2, 0, 1, false},
		{"空内容", `{"kind":"decision","content":"  ","evidence_idx":[1]}`, 2, 0, 1, false},
		{"坏 scope", `{"kind":"decision","content":"a","scope":"universe","evidence_idx":[1]}`, 2, 0, 1, false},
		{"事实 kind", `{"kind":"fact","content":"a","evidence_idx":[1]}`, 2, 1, 0, false},
	}
	for _, tc := range cases {
		got, err := ParseDrafts(tc.raw, tc.evCount)
		if tc.bad {
			if !errors.Is(err, ErrBadOutput) {
				t.Errorf("%s: 应报坏输出，got %v", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 不应报错: %v", tc.name, err)
			continue
		}
		if len(got.Drafts) != tc.valid || got.Invalid != tc.invalid {
			t.Errorf("%s: valid=%d invalid=%d，want %d/%d", tc.name,
				len(got.Drafts), got.Invalid, tc.valid, tc.invalid)
		}
	}
}

func TestParseDraftsNormalizesScopeAndDedupsIndex(t *testing.T) {
	got, err := ParseDrafts(`{"kind":"Decision","content":"  x  ","scope":"","evidence_idx":[2,2,1]}`, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Drafts) != 1 {
		t.Fatalf("应有一条: %+v", got)
	}
	d := got.Drafts[0]
	if d.Kind != "decision" || d.Scope != "project" || d.Content != "x" {
		t.Fatalf("归一化不对: %+v", d)
	}
	if len(d.EvidenceIdx) != 2 || d.EvidenceIdx[0] != 1 || d.EvidenceIdx[1] != 2 {
		t.Fatalf("序号应去重并排序: %+v", d.EvidenceIdx)
	}
}

func TestEvidenceIDsFor(t *testing.T) {
	ids, err := evidenceIDsFor([]int{1, 3}, []string{"ev1", "ev2", "ev3"})
	if err != nil || len(ids) != 2 || ids[0] != "ev1" || ids[1] != "ev3" {
		t.Fatalf("映射不对: %+v err=%v", ids, err)
	}
	if _, err := evidenceIDsFor([]int{4}, []string{"ev1", "ev2", "ev3"}); err == nil {
		t.Fatal("越界应报错")
	}
}

func TestBuildPromptKeepsBundleOrdinalsAndScrubs(t *testing.T) {
	refs := []evidence.Ref{
		{Summary: "第一条", EventType: "stop", SessionID: "s", ProposalEligible: true},
		{Summary: "不可提案", EventType: "stop", SessionID: "s", ProposalEligible: false},
		{Summary: "token=supersecret", EventType: "post-tool-use", SessionID: "s", ProposalEligible: true},
	}
	p := BuildPrompt(refs)
	if !strings.Contains(p, "[1] (stop/s) 第一条") {
		t.Fatalf("应保留 Bundle 序号: %s", p)
	}
	if strings.Contains(p, "不可提案") {
		t.Fatalf("不可提案证据不应进 prompt: %s", p)
	}
	if !strings.Contains(p, "[3]") || !strings.Contains(p, "[redacted]") ||
		strings.Contains(p, "supersecret") {
		t.Fatalf("序号不能重排、密钥要打码: %s", p)
	}
}

func TestRepairPromptIsBounded(t *testing.T) {
	long := strings.Repeat("x", maxRawForRepair+100)
	p := RepairPrompt("base", long)
	if !strings.HasPrefix(p, "base") || !strings.Contains(p, "只输出 JSON") {
		t.Fatalf("修复提示不完整: %s", p)
	}
	if strings.Count(p, "x") != maxRawForRepair {
		t.Fatalf("修复提示必须截断，got %d", strings.Count(p, "x"))
	}
}

func TestBuildChainModes(t *testing.T) {
	host := FuncGenerator{Label: "host"}
	local := FuncGenerator{Label: "local"}
	byok := FuncGenerator{Label: "byok"}
	deps := Deps{Host: host, Local: local, BYOK: byok}

	cases := []struct {
		mode string
		want string
		err  bool
	}{
		{"", "local", false},
		{"auto", "local", false},
		{"local", "local", false},
		{"host", "host,local", false},
		{"byok", "byok,local", false},
		{"none", "", false},
		{"nope", "", true},
	}
	for _, tc := range cases {
		chain, err := BuildChain(tc.mode, nil, deps)
		if tc.err {
			if err == nil {
				t.Errorf("%s: 应报错", tc.mode)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.mode, err)
			continue
		}
		if got := strings.Join(ExecutorNames(chain), ","); got != tc.want {
			t.Errorf("%s: chain=%q want %q", tc.mode, got, tc.want)
		}
	}
}
