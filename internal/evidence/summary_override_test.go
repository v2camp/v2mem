package evidence

import (
	"strings"
	"testing"
)

// SummaryOverride 只换查询源，不动归档 Raw：宿主把注入上下文和用户原话
// 拼在一条消息里时，两者必须分开，否则提案看到的全是 system-reminder。
func TestEventSummaryOverride(t *testing.T) {
	full := "<system-reminder>OS: darwin</system-reminder>\n<user_query>按 B 来推进</user_query>"
	ev := rawEvent("uid-summary")
	ev.EventType = "user-prompt"
	ev.Raw = []byte(full)

	if got := eventSummary(ev); !strings.Contains(got, "system-reminder") {
		t.Fatalf("无 override 时按 Raw 生成摘要，got %q", got)
	}
	ev.SummaryOverride = "按 B 来推进"
	if got := eventSummary(ev); got != "按 B 来推进" {
		t.Fatalf("override 应生效，got %q", got)
	}
	ev.SummaryOverride = "   "
	if got := eventSummary(ev); !strings.Contains(got, "system-reminder") {
		t.Fatalf("空白 override 应回落 Raw，got %q", got)
	}
}

func TestCaptureKeepsRawAndUsesOverrideForSummary(t *testing.T) {
	e, _ := newTestEngine(t)
	full := "<system-reminder data-role=\"user-context\">OS: darwin</system-reminder>\n<user_query>按 B 来推进</user_query>"
	ev := rawEvent("uid-override")
	ev.EventType = "user-prompt"
	ev.Raw = []byte(full)
	ev.SummaryOverride = "按 B 来推进"
	ev.Fidelity = FidelityFull
	ev.ProposalEligible = true

	ref, err := e.Capture(ev)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Summary != "按 B 来推进" || strings.Contains(ref.Summary, "system-reminder") {
		t.Fatalf("提案输入应只剩用户原话，got %q", ref.Summary)
	}
	path := strings.TrimSuffix(ref.RawLocator, "#1")
	rec, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.Raw) != full {
		t.Fatalf("归档必须保留完整原文（含注入上下文）: %q", string(rec.Raw))
	}
}

// DB 写失败走 spool 时，查询源清洗不能丢：segment 带 summary_override，
// drain 回放后 Summary 仍是用户原话，而不是 512 字的注入上下文。
func TestSpoolRoundTripKeepsSummaryOverride(t *testing.T) {
	e, _ := newTestEngine(t)
	full := "<system-reminder data-role=\"user-context\">OS: darwin</system-reminder>\n<user_query>按 B 来推进</user_query>"
	ev := rawEvent("uid-override-spool")
	ev.Generation, ev.Seq = 1, 1
	ev.EventType = "user-prompt"
	ev.Raw = []byte(full)
	ev.SummaryOverride = "按 B 来推进"
	ev.Fidelity = FidelityFull
	ev.ProposalEligible = true
	if _, err := e.writeSpool(ev); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DrainSpool(10); err != nil {
		t.Fatal(err)
	}
	b, err := e.NextBundle(10, 1<<20)
	if err != nil || b == nil {
		t.Fatalf("drain 后应可出窗: %+v err=%v", b, err)
	}
	refs, err := e.ListEvidence(b.ID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("证据回放失败: %+v err=%v", refs, err)
	}
	if refs[0].Summary != "按 B 来推进" || strings.Contains(refs[0].Summary, "system-reminder") {
		t.Fatalf("spool 回放后查询源被污染: %q", refs[0].Summary)
	}
}
