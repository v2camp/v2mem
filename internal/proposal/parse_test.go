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

// ---- Fidelity 驱动的提案准入 + ProposalPayloadRunes 有界投影 ----

func TestProposalEligible(t *testing.T) {
	cases := []struct {
		name     string
		eligible bool
		fidelity evidence.Fidelity
		want     bool
	}{
		{"full 可提案", true, evidence.FidelityFull, true},
		{"summary 可提案", true, evidence.FidelitySummary, true},
		{"空 Fidelity 按 full 兼容", true, "", true},
		{"pointer 不可提案", true, evidence.FidelityPointer, false},
		{"none 不可提案", true, evidence.FidelityNone, false},
		{"未知 Fidelity 不可提案", true, evidence.Fidelity("weird"), false},
		{"总开关关闭 full 也不可", false, evidence.FidelityFull, false},
		{"总开关关闭 空 Fidelity 也不可", false, "", false},
	}
	for _, tc := range cases {
		r := evidence.Ref{ProposalEligible: tc.eligible, Fidelity: tc.fidelity}
		if got := proposalEligible(r); got != tc.want {
			t.Errorf("%s: proposalEligible=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildPromptFidelityGate(t *testing.T) {
	refs := []evidence.Ref{
		{Summary: "FULL进", EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelityFull},
		{Summary: "SUMMARY进", EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelitySummary},
		{Summary: "POINTER挡", EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelityPointer},
		{Summary: "NONE挡", EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelityNone},
		{Summary: "空FID兼容进", EventType: "stop", SessionID: "s", ProposalEligible: true},
		{Summary: "总开关关挡", EventType: "stop", SessionID: "s", ProposalEligible: false, Fidelity: evidence.FidelityFull},
	}
	p := BuildPrompt(refs)
	for _, body := range []string{"FULL进", "SUMMARY进", "空FID兼容进"} {
		if !strings.Contains(p, body) {
			t.Errorf("可提案正文应进 prompt: %q\n%s", body, p)
		}
	}
	for _, body := range []string{"POINTER挡", "NONE挡", "总开关关挡"} {
		if strings.Contains(p, body) {
			t.Errorf("不可提案正文不应进 prompt: %q\n%s", body, p)
		}
	}
}

// 关键不变量：被 continue 跳过的条目不重编号，evidence_idx ordinal 仍对齐 refs 下标。
func TestBuildPromptOrdinalsWithFidelitySkips(t *testing.T) {
	// 第 2、3 条因 Fidelity 被跳过，第 5 条因总开关被跳过；
	// 第 4 条仍必须是 [4]、第 6 条仍是 [6]，绝不重排成 [2]/[3]。
	refs := []evidence.Ref{
		{Summary: "A-one", EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelityFull},
		{Summary: "B-skip", EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelityPointer},
		{Summary: "C-skip", EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelityNone},
		{Summary: "D-four", EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelitySummary},
		{Summary: "E-five-skip", EventType: "stop", SessionID: "s", ProposalEligible: false},
		{Summary: "F-six-空FID", EventType: "stop", SessionID: "s", ProposalEligible: true},
	}
	p := BuildPrompt(refs)
	if !strings.Contains(p, "[1] (stop/s) A-one") {
		t.Fatalf("序号 1 错位: %s", p)
	}
	if !strings.Contains(p, "[4] (stop/s) D-four") {
		t.Fatalf("跳过条目后序号 4 不能重排: %s", p)
	}
	if !strings.Contains(p, "[6] (stop/s) F-six-空FID") {
		t.Fatalf("序号 6 错位（空 Fidelity 兼容）: %s", p)
	}
	for _, absent := range []string{"[2] ", "[3] ", "[5] "} {
		if strings.Contains(p, absent) {
			t.Errorf("被跳过的序号不应渲染: %q\n%s", absent, p)
		}
	}
	for _, body := range []string{"B-skip", "C-skip", "E-five-skip"} {
		if strings.Contains(p, body) {
			t.Errorf("不可提案正文不应进 prompt: %q\n%s", body, p)
		}
	}
}

func TestBuildPromptPayloadBounded(t *testing.T) {
	// 纯中文 520 rune：oneLine / scrub 不改变其内容，便于精确验证截断边界。
	long := strings.Repeat("甲", evidence.ProposalPayloadRunes+120)
	refs := []evidence.Ref{
		{Summary: long, EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelityFull},
	}
	before := refs[0].Summary
	p := BuildPrompt(refs)
	if strings.Contains(p, long) {
		t.Fatal("长正文未截断")
	}
	// 省略标记计入预算：399 rune 正文 + "…" = 严格 400 rune。
	want := strings.Repeat("甲", evidence.ProposalPayloadRunes-1) + "…"
	if !strings.Contains(p, want) {
		t.Fatalf("截断边界不对（应截到 %d rune 加省略标记）:\n%s", evidence.ProposalPayloadRunes-1, p)
	}
	if strings.Contains(p, strings.Repeat("甲", evidence.ProposalPayloadRunes)) {
		t.Fatalf("投影正文超过 %d rune", evidence.ProposalPayloadRunes)
	}
	// 投影是纯文本操作：不改传入 refs，等价于不改归档 Raw / 不回写 store。
	if refs[0].Summary != before {
		t.Fatal("BuildPrompt 不应修改传入 ref 的 Summary")
	}
}

func TestBuildPromptPayloadShortAndScrubUnchanged(t *testing.T) {
	// 短文本不追加省略标记；脱敏仍在投影内生效。
	refs := []evidence.Ref{
		{Summary: "token=supersecret\n换行", EventType: "stop", SessionID: "s", ProposalEligible: true, Fidelity: evidence.FidelitySummary},
	}
	p := BuildPrompt(refs)
	if !strings.Contains(p, "[redacted]") || strings.Contains(p, "supersecret") {
		t.Fatalf("投影必须保留脱敏: %s", p)
	}
	if strings.Contains(p, "…") {
		t.Fatalf("短文本不应加省略标记: %s", p)
	}
}

// Eligible 是旧规则蒸馏路径复用的导出入口，必须与内部判定逐项同值，
// 否则会出现「两条路径对同一条证据给出不同准入结论」。
func TestEligibleExportedContract(t *testing.T) {
	cases := []struct {
		name string
		ref  evidence.Ref
		want bool
	}{
		{"full", evidence.Ref{ProposalEligible: true, Fidelity: evidence.FidelityFull}, true},
		{"summary", evidence.Ref{ProposalEligible: true, Fidelity: evidence.FidelitySummary}, true},
		{"legacy-empty-fidelity", evidence.Ref{ProposalEligible: true}, true},
		{"pointer", evidence.Ref{ProposalEligible: true, Fidelity: evidence.FidelityPointer}, false},
		{"none", evidence.Ref{ProposalEligible: true, Fidelity: evidence.FidelityNone}, false},
		{"unknown-fail-closed", evidence.Ref{ProposalEligible: true, Fidelity: evidence.Fidelity("bogus")}, false},
		{"switch-off", evidence.Ref{ProposalEligible: false, Fidelity: evidence.FidelityFull}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Eligible(c.ref)
			if got != c.want {
				t.Fatalf("Eligible(%+v)=%v want %v", c.ref, got, c.want)
			}
			if inner := proposalEligible(c.ref); inner != got {
				t.Fatalf("导出入口 %v 与内部判定 %v 不一致", got, inner)
			}
		})
	}
}
