package proposal

import (
	"context"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
)

// captureEvent 是 R5/R6 需要的「指定事件类型 + Fidelity」捕获；
// runner_test.go 的 capture 固定 stop/full，规则抽取的说话人分源用不了。
func (e *env) captureEvent(t *testing.T, uid, session, event, summary string, eligible bool, fidelity evidence.Fidelity) {
	t.Helper()
	_, created, err := e.st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-t", SourceKind: "hook", Generation: 1,
		OccurredAtNS: 1700000000000000000, SessionID: session, Project: "proj",
		Harness: "workbuddy", EventType: event, ParserVersion: "evidence/v1",
		ProposalEligible: eligible, Summary: summary, Fidelity: string(fidelity),
		ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
		RawRecordHash: "rh-" + uid, ByteSize: 16, RawCount: 1,
	})
	if err != nil {
		t.Fatalf("CaptureEvidence(%s): %v", uid, err)
	}
	if !created {
		t.Fatalf("CaptureEvidence(%s) 应首次写入", uid)
	}
}

// R1：user/assistant 的 decision/task 摘要 → 合法草案，evidence_idx 对齐全量切片。
func TestRulesGeneratorExtractsDurableFacts(t *testing.T) {
	refs := []evidence.Ref{
		// 首位放 pointer：规则输出的序号必须是 req.Evidence 的 1 起下标，
		// 不能是「可提案观测」的紧凑序号 —— 否则会引用到另一条证据。
		{EventUID: "u0", SessionID: "s1", Project: "p", EventType: "user-prompt",
			Summary: "决定：指针证据不参与提案", ProposalEligible: true, Fidelity: evidence.FidelityPointer},
		{EventUID: "u1", SessionID: "s1", Project: "p", EventType: "user-prompt",
			Summary: "决定：提案链新增规则兜底，模型不可用时仍产候选", ProposalEligible: true},
		{EventUID: "u2", SessionID: "s1", Project: "p", EventType: "assistant-text",
			Summary: "决策：候选统一由 Bundle 消费，根因是双写导致重复", ProposalEligible: true, Fidelity: evidence.FidelityFull},
		{EventUID: "u3", SessionID: "s1", Project: "p", EventType: "user-prompt",
			Summary: "待办：把 rules executor 接进链尾", ProposalEligible: true, Fidelity: evidence.FidelitySummary},
	}

	g := NewRulesGenerator()
	if g.Name() != "rules" {
		t.Fatalf("executor 名应为 rules，got %q", g.Name())
	}
	raw, err := g.Generate(context.Background(), GenerateRequest{Evidence: refs})
	if err != nil {
		t.Fatalf("规则生成不应报错: %v", err)
	}
	parsed, err := ParseDrafts(raw, len(refs))
	if err != nil {
		t.Fatalf("规则输出必须能被 ParseDrafts 解析: %v\n%s", err, raw)
	}
	if len(parsed.Drafts) != 3 {
		t.Fatalf("三条精简摘要应产出 3 条草案，got %d (%s)", len(parsed.Drafts), raw)
	}

	wantUID := map[string]string{
		refs[1].Summary: "u1",
		refs[2].Summary: "u2",
		refs[3].Summary: "u3",
	}
	legalKind := map[string]bool{"preference": true, "decision": true, "pitfall": true, "task": true, "fact": true}
	seenContent := map[string]bool{}
	for _, d := range parsed.Drafts {
		if !legalKind[d.Kind] {
			t.Errorf("kind 不合法: %q", d.Kind)
		}
		if len(d.EvidenceIdx) == 0 {
			t.Errorf("每条草案至少引用一条证据: %+v", d)
			continue
		}
		uid, ok := wantUID[d.Content]
		if !ok {
			t.Errorf("草案正文不是规则从摘要抽出的内容: %q", d.Content)
			continue
		}
		for _, idx := range d.EvidenceIdx {
			if idx < 1 || idx > len(refs) {
				t.Errorf("evidence_idx %d 越界 1..%d", idx, len(refs))
				continue
			}
			if idx == 1 {
				t.Errorf("不可提案的 pointer 证据不应被引用: %+v", d)
			}
			if got := refs[idx-1].EventUID; got != uid {
				t.Errorf("evidence_idx %d 指向 %s，应指向 %s", idx, got, uid)
			}
		}
		seenContent[d.Content] = true
	}
	for content, uid := range wantUID {
		if !seenContent[content] {
			t.Errorf("%s 的可复用摘要未产出草案: %q", uid, content)
		}
	}
}

// R2：不可提案的 pointer 证据既不进 Observation，也不被草案引用。
func TestRulesGeneratorSkipsIneligibleRefs(t *testing.T) {
	mixed := []evidence.Ref{
		{EventUID: "e1", SessionID: "s1", Project: "p", EventType: "user-prompt",
			Summary: "决定：本地优先，不外发远端", ProposalEligible: true, Fidelity: evidence.FidelityFull},
		{EventUID: "p1", SessionID: "s1", Project: "p", EventType: "user-prompt",
			Summary: "决定：指针证据绝不进候选", ProposalEligible: true, Fidelity: evidence.FidelityPointer},
		{EventUID: "e2", SessionID: "s1", Project: "p", EventType: "user-prompt",
			Summary: "待办：补 rules 的测试", ProposalEligible: true, Fidelity: evidence.FidelitySummary},
	}
	raw, err := NewRulesGenerator().Generate(context.Background(), GenerateRequest{Evidence: mixed})
	if err != nil {
		t.Fatalf("规则生成不应报错: %v", err)
	}
	if strings.Contains(raw, mixed[1].Summary) {
		t.Fatalf("pointer 证据不应进 Observation/草案: %s", raw)
	}
	parsed, err := ParseDrafts(raw, len(mixed))
	if err != nil {
		t.Fatalf("ParseDrafts: %v", err)
	}
	if len(parsed.Drafts) != 2 {
		t.Fatalf("只有两条可提案证据，got %d (%s)", len(parsed.Drafts), raw)
	}
	for _, d := range parsed.Drafts {
		for _, idx := range d.EvidenceIdx {
			if idx == 2 {
				t.Fatalf("草案不得引用 pointer 证据: %+v", d)
			}
		}
	}

	// 全部不可提案：合法空结果（→ ParseDrafts 0 条 → noop）。
	onlyPointer := []evidence.Ref{
		{EventUID: "p1", SessionID: "s1", Project: "p", EventType: "user-prompt",
			Summary: "决定：指针证据绝不进候选", ProposalEligible: true, Fidelity: evidence.FidelityPointer},
		{EventUID: "p2", SessionID: "s1", Project: "p", EventType: "stop",
			Summary: "待办：也不该出现", ProposalEligible: false, Fidelity: evidence.FidelityFull},
	}
	raw, err = NewRulesGenerator().Generate(context.Background(), GenerateRequest{Evidence: onlyPointer})
	if err != nil || raw != "" {
		t.Fatalf("全不可提案应输出空串: %q err=%v", raw, err)
	}
	if parsed, err := ParseDrafts(raw, len(onlyPointer)); err != nil || len(parsed.Drafts) != 0 {
		t.Fatalf("空串应解析为 0 条: %+v err=%v", parsed, err)
	}
}

// R3：eligible 但抽不出（tool/unparsed）→ 空串 → 0 条，不是错误。
func TestRulesGeneratorNoExtractableIsEmpty(t *testing.T) {
	refs := []evidence.Ref{
		{EventUID: "t1", SessionID: "s1", Project: "p", EventType: "post-tool-use",
			Summary: "决定：工具输出不是事实陈述", ProposalEligible: true, Fidelity: evidence.FidelityFull},
		{EventUID: "t2", SessionID: "s1", Project: "p", EventType: "unparsed",
			Summary: "待办：解析失败的载荷也不该产事实", ProposalEligible: true, Fidelity: evidence.FidelityFull},
	}
	raw, err := NewRulesGenerator().Generate(context.Background(), GenerateRequest{Evidence: refs})
	if err != nil {
		t.Fatalf("抽不出事实不是错误: %v", err)
	}
	if raw != "" {
		t.Fatalf("抽不出事实应输出空串，got %q", raw)
	}
	parsed, err := ParseDrafts(raw, len(refs))
	if err != nil || len(parsed.Drafts) != 0 || parsed.Invalid != 0 {
		t.Fatalf("空串必须是合法 0 条: %+v err=%v", parsed, err)
	}
}

// R4：Rules 恒在链尾；none 仍为空链；Rules 为 nil 时链与旧语义完全一致。
func TestBuildChainAppendsRulesLast(t *testing.T) {
	host := FuncGenerator{Label: "host"}
	local := FuncGenerator{Label: "local"}
	byok := FuncGenerator{Label: "byok"}
	rules := NewRulesGenerator()
	deps := Deps{Host: host, Local: local, BYOK: byok, Rules: rules}

	cases := []struct {
		mode string
		want string
	}{
		{"", "local,rules"},
		{"auto", "local,rules"},
		{"local", "local,rules"},
		{"host", "host,local,rules"},
		{"byok", "byok,local,rules"},
		{"none", ""},
	}
	for _, tc := range cases {
		chain, err := BuildChain(tc.mode, nil, deps)
		if err != nil {
			t.Errorf("%s: %v", tc.mode, err)
			continue
		}
		if got := strings.Join(ExecutorNames(chain), ","); got != tc.want {
			t.Errorf("%s: chain=%q want %q", tc.mode, got, tc.want)
		}
	}

	// 只有 Rules 可用（模型全都不可用）：链就是 [rules]，不能因此变空链。
	onlyRules, err := BuildChain("auto", nil, Deps{Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ExecutorNames(onlyRules), ","); got != "rules" {
		t.Errorf("只有 rules 时应为 [rules]，got %q", got)
	}

	// 向后兼容：Rules 为 nil 时链路保持原样。
	legacy := Deps{Host: host, Local: local, BYOK: byok}
	legacyCases := []struct{ mode, want string }{
		{"auto", "local"},
		{"host", "host,local"},
		{"byok", "byok,local"},
		{"none", ""},
	}
	for _, tc := range legacyCases {
		chain, err := BuildChain(tc.mode, nil, legacy)
		if err != nil {
			t.Errorf("%s: %v", tc.mode, err)
			continue
		}
		if got := strings.Join(ExecutorNames(chain), ","); got != tc.want {
			t.Errorf("Rules=nil 时 %s: chain=%q want %q", tc.mode, got, tc.want)
		}
	}
}

// R5：模型不可用 → 规则兜底产出候选并消费窗口，而不是落到 backlog。
func TestRunnerUsesRulesWhenProvidersUnavailable(t *testing.T) {
	e := newEnv(t)
	e.captureEvent(t, "u1", "s1", "user-prompt", "决定：模型不可用时用规则兜底", true, evidence.FidelityFull)
	e.captureEvent(t, "u2", "s1", "user-prompt", "待办：把规则接入链尾", true, evidence.FidelityFull)
	b := e.bundle(t)

	chain, err := BuildChain("auto", e.ev, Deps{
		Local: UnavailableGenerator("local", ErrNoProvider),
		Rules: NewRulesGenerator(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ExecutorNames(chain), ","); got != "local,rules" {
		t.Fatalf("链应为 local,rules，got %q", got)
	}

	var now int64 = 1000
	r := newRunner(e, chain, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatalf("规则兜底不应报错: %v", err)
	}
	if out.Status != StatusOK || out.BundleStatus != "ok" {
		t.Fatalf("规则兜底应 ok 并消费窗口: %+v err=%v", out, out.Error)
	}
	if out.Executor != "rules" {
		t.Fatalf("实际执行者应为 rules，got %q", out.Executor)
	}
	if out.Inserted < 1 {
		t.Fatalf("应落库至少 1 条候选: %+v", out)
	}
	bundle, err := e.st.LoadProposalBundle(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Status != "ok" {
		t.Fatalf("Bundle 应为 ok，got %+v", bundle)
	}
	cands, err := e.st.ListCandidates("", "s1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != out.Inserted {
		t.Fatalf("落库候选数不一致: store=%d outcome=%d", len(cands), out.Inserted)
	}
	// 窗口已消费：再 Step 不会拿到同一个 Bundle。
	next, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != "skipped" || next.BundleID != "" {
		t.Fatalf("水位应已推进，第二次 Step 应 skipped: %+v", next)
	}
}

// R6：规则也抽不出事实 → noop（合法空结果），且水位照常推进。
func TestRunnerRulesNoFactsIsNoopAndAdvances(t *testing.T) {
	e := newEnv(t)
	e.captureEvent(t, "t1", "s1", "post-tool-use", "决定：工具载荷不产事实", true, evidence.FidelityFull)
	e.captureEvent(t, "t2", "s1", "unparsed", "待办：坏载荷不产事实", true, evidence.FidelityFull)
	b := e.bundle(t)

	chain, err := BuildChain("auto", e.ev, Deps{Rules: NewRulesGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ExecutorNames(chain), ","); got != "rules" {
		t.Fatalf("链应只有 rules，got %q", got)
	}

	var now int64 = 1000
	r := newRunner(e, chain, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatalf("空结果不是错误: %v", err)
	}
	if out.Status != StatusNoop || out.BundleStatus != "noop" || out.Executor != "rules" {
		t.Fatalf("抽不出事实应 noop/rules: %+v", out)
	}
	if out.Inserted != 0 {
		t.Fatalf("noop 不落候选: %+v", out)
	}
	bundle, err := e.st.LoadProposalBundle(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Status != "noop" {
		t.Fatalf("Bundle 应为 noop，got %+v", bundle)
	}

	next, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != "skipped" || next.BundleID == b.ID {
		t.Fatalf("水位已推进，不应重复消费同一窗: %+v", next)
	}
}
