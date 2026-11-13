package proposal

import (
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
)

func TestSpeakerOf(t *testing.T) {
	cases := map[string]string{
		"user-prompt":     SpeakerUser,
		"user.prompt":     SpeakerUser,
		"assistant-text":  SpeakerAssistant,
		"assistant.final": SpeakerAssistant,
		"stop":            SpeakerAssistant,
		"session-end":     SpeakerAssistant,
		"post-tool-use":   "",
		"tool-use":        "",
		"unparsed":        "",
		"":                "",
	}
	for in, want := range cases {
		if got := SpeakerOf(in); got != want {
			t.Errorf("SpeakerOf(%q)=%q want %q", in, got, want)
		}
	}
}

func eligible(event, summary string) evidence.Ref {
	return evidence.Ref{
		EventType: event, SessionID: "s", Summary: summary,
		ProposalEligible: true, Fidelity: evidence.FidelityFull,
	}
}

// 分源渲染 + 反回声规则 + 序号不重排，三者必须同时成立。
func TestBuildPromptGroupsBySpeakerAndKeepsOrdinals(t *testing.T) {
	refs := []evidence.Ref{
		eligible("stop", "助手先复述一遍"),
		eligible("user-prompt", "按 B 来推进"),
		{Summary: "跳过", EventType: "post-tool-use", SessionID: "s", ProposalEligible: false},
		eligible("assistant-text", "结论：归档双方都记"),
	}
	p := BuildPrompt(refs)
	for _, want := range []string{
		"=== USER ===", "=== ASSISTANT ===",
		"No Echo Extraction", "用户原话是主来源", "attributed_to",
		"[1] (stop/s) 助手先复述一遍",
		"[2] (user-prompt/s) 按 B 来推进",
		"[4] (assistant-text/s) 结论：归档双方都记",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt 缺少 %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "[3] ") {
		t.Error("被跳过的序号不应渲染")
	}
	// USER 段必须排在 ASSISTANT 段前面，才能落实「原话是主来源」。
	if strings.Index(p, "=== USER ===") > strings.Index(p, "=== ASSISTANT ===") {
		t.Fatalf("USER 段应在前:\n%s", p)
	}
}

// hook 与 transcript 会采集到同一句用户原话；不去掉重复，模型会把一条事实提两次。
func TestBuildPromptDropsDuplicatePayloadKeepsOrdinalGap(t *testing.T) {
	refs := []evidence.Ref{
		eligible("user-prompt", "按 B 来推进"),
		eligible("user-prompt", "按 B 来推进"),
		eligible("stop", "结论：已按 B 推进"),
	}
	p := BuildPrompt(refs)
	if strings.Count(p, "按 B 来推进") != 1 {
		t.Fatalf("重复正文只应保留首次出现:\n%s", p)
	}
	if !strings.Contains(p, "[3] (stop/s) 结论：已按 B 推进") {
		t.Fatalf("去重不能让后面的序号重排:\n%s", p)
	}
}

func TestParseDraftsAttributedTo(t *testing.T) {
	raw := `{"kind":"decision","content":"按 B 推进","attributed_to":"user","evidence_idx":[1]}` + "\n" +
		`{"kind":"pitfall","content":"根因在解析器","attributed_to":"assistant","evidence_idx":[2]}` + "\n" +
		`{"kind":"task","content":"来源自造","attributed_to":"system","evidence_idx":[3]}`
	got, err := ParseDrafts(raw, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Drafts) != 3 {
		t.Fatalf("应解析 3 条，got %d", len(got.Drafts))
	}
	for i, want := range []string{SpeakerUser, SpeakerAssistant, ""} {
		if got.Drafts[i].AttributedTo != want {
			t.Errorf("第 %d 条 attributed_to=%q want %q", i+1, got.Drafts[i].AttributedTo, want)
		}
	}
}

// 落库以被引证据为准，不信任模型回填；用户与助手混合时按 user 优先。
func TestCandidateInputDerivesAttributedToFromEvidence(t *testing.T) {
	r := &Runner{}
	b := &store.ProposalBundle{EvidenceIDs: []string{"e1", "e2", "e3", "e4"}}
	refs := []evidence.Ref{
		eligible("user-prompt", "用户原话"),
		eligible("stop", "助手结论"),
		eligible("assistant-text", "助手结论二"),
		eligible("unparsed", "来源不明"),
	}
	cases := []struct {
		name  string
		idx   []int
		claim string
		want  string
	}{
		{"用户优先", []int{2, 1}, SpeakerAssistant, SpeakerUser},
		{"单条助手", []int{2}, SpeakerUser, SpeakerAssistant},
		{"模型填错也纠正", []int{3}, SpeakerUser, SpeakerAssistant},
		{"认不出时退回模型", []int{4}, SpeakerAssistant, SpeakerAssistant},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Draft{Kind: "decision", Content: "x", AttributedTo: c.claim, EvidenceIdx: c.idx}
			in, err := r.candidateInput(b, refs, d, 1, "run_1")
			if err != nil {
				t.Fatal(err)
			}
			if in.AttributedTo != c.want {
				t.Fatalf("AttributedTo=%q want %q", in.AttributedTo, c.want)
			}
		})
	}
}
