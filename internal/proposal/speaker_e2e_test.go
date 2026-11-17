package proposal

import (
	"context"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
)

func captureTyped(t *testing.T, e *env, uid, eventType, summary string, eligible bool) {
	t.Helper()
	_, created, err := e.st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-t", SourceKind: "hook", Generation: 1,
		OccurredAtNS: 1700000000000000000, SessionID: "s-speaker", Project: "proj",
		Harness: "workbuddy", EventType: eventType, ParserVersion: "evidence/v1",
		ProposalEligible: eligible, Summary: summary, Fidelity: string(evidence.FidelityFull),
		ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
		RawRecordHash: "rh-" + uid, ByteSize: 16, RawCount: 1,
	})
	if err != nil || !created {
		t.Fatalf("CaptureEvidence(%s): created=%v err=%v", uid, created, err)
	}
}

// 端到端：从「用户提问 + 助手结论 + 工具载荷」三条真实形态证据，经分源
// BuildPrompt → 假执行器 → Runner 落库，候选必须带正确的 attributed_to。
func TestRunnerAttributionEndToEnd(t *testing.T) {
	e := newEnv(t)
	captureTyped(t, e, "u1", "user-prompt", "偏好：候选要标出是谁说的", true)
	captureTyped(t, e, "a1", "assistant-text", "踩坑：根因在 transcript 解析器", true)
	captureTyped(t, e, "t1", "post-tool-use", `{"callId":"c1"}`, false)
	b := e.bundle(t)
	if len(b.EvidenceIDs) != 3 {
		t.Fatalf("Bundle 应有 3 条证据，got %d", len(b.EvidenceIDs))
	}

	var now int64 = 1000
	g, _ := gen("fake", func(req GenerateRequest, _ int) (string, error) {
		for _, want := range []string{
			"=== USER ===", "=== ASSISTANT ===", "No Echo Extraction",
			"[1] (user-prompt/s-speaker) 偏好：候选要标出是谁说的",
			"[2] (assistant-text/s-speaker) 踩坑：根因在 transcript 解析器",
		} {
			if !strings.Contains(req.Prompt, want) {
				t.Errorf("提案提示词缺少 %q:\n%s", want, req.Prompt)
			}
		}
		if strings.Contains(req.Prompt, "callId") {
			t.Errorf("不可提案的工具载荷不应进 prompt:\n%s", req.Prompt)
		}
		// 第二条故意让模型把 attributed_to 填错，落库必须按证据纠正。
		return `{"kind":"preference","scope":"project","content":"候选要标出是谁说的","attributed_to":"user","evidence_idx":[1]}` + "\n" +
			`{"kind":"pitfall","scope":"project","content":"根因在 transcript 解析器","attributed_to":"user","evidence_idx":[2]}`, nil
	})
	r := newRunner(e, []Executor{NewExecutor(e.ev, g)}, &now)
	out, err := r.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusOK || out.Inserted != 2 {
		t.Fatalf("运行结果不对: %+v", out)
	}

	list, err := e.st.ListCandidates("pending", "s-speaker", 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("应落 2 条 pending: %+v err=%v", list, err)
	}
	byKind := map[string]string{}
	for _, c := range list {
		byKind[c.Kind] = c.AttributedTo
	}
	if byKind["preference"] != SpeakerUser || byKind["pitfall"] != SpeakerAssistant {
		t.Fatalf("候选 attributed_to 应以被引证据为准，got %+v", byKind)
	}
}
