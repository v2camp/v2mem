package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
	"github.com/v2camp/v2mem/internal/transcript"
)

// 本文件锁住 T4 的 Trae 字段映射与捕获层保真度标注。
//
// 注意 W0 的边界：internal/evidence/api.go 只冻结了 RawEvent.Fidelity /
// Ref.Fidelity 字段，Engine.Capture 到 T2 才透传、store 的 meta_json 到 T1
// 才写 Fidelity。故本分支的 Fidelity 断言落在「evidence.RawEvent 构造层」——
// 即 observationText / recordEvidence 的返回值；正文是否写进归档则通过
// ListObservations 的摘要端到端验证。这样不与其它波次的独占文件耦合。

// TestTraeStopCapturesLastAssistantMessage 复现 design-capture §1 的写侧断点：
// Trae Stop 的 last_assistant_message 必须成为 observation 正文，而不是字面量 "stop"。
func TestTraeStopCapturesLastAssistantMessage(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	sid := "trae-stop-sess"
	const (
		bodyA = "结论甲：npx 缓存根因在权限位，不是网络"
		bodyB = "结论乙：MCP 与 hooks 不冲突，可并行注册"
	)

	// 构造层：两次不同正文都应标 full，正文原样取 last_assistant_message。
	for _, body := range []string{bodyA, bodyB} {
		text, fid := observationText(harness.EventStop,
			hookInput{LastAssistantMessage: body}, false)
		if fid != evidence.FidelityFull {
			t.Fatalf("Trae Stop 带 last_assistant_message 应为 full，got %q", fid)
		}
		if text != body {
			t.Fatalf("正文应取 last_assistant_message，got %q", text)
		}
	}

	// 端到端：同一会话两次不同 Stop（关闭去重窗口）→ 两条不同 observation。
	for _, body := range []string{bodyA, bodyB} {
		in := hookJSON(t, map[string]any{
			"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
			"last_assistant_message": body,
		})
		if _, err := withStdin(t, in, func() error {
			return cmdHook([]string{"--db", db, "--harness", "trae", "--dedup-window", "0", "--audit", "-"})
		}); err != nil {
			t.Fatalf("Stop hook: %v", err)
		}
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	obs, err := st.ListObservations(sid, 10)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("两次不同 Stop 应产生 2 条 observation，got %d: %+v", len(obs), obs)
	}
	if obs[0].Summary == obs[1].Summary {
		t.Fatalf("两次不同 Stop 的摘要不应相同: %+v", obs)
	}
	seen := map[string]bool{}
	for _, o := range obs {
		// 核心回归：摘要里不能再是裸事件名 "stop"。
		if strings.Contains(o.Summary, "stop") {
			t.Errorf("Trae Stop 摘要不得再退化成事件名 stop: %q", o.Summary)
		}
		if strings.Contains(o.Summary, bodyA) {
			seen[bodyA] = true
		}
		if strings.Contains(o.Summary, bodyB) {
			seen[bodyB] = true
		}
	}
	if !seen[bodyA] || !seen[bodyB] {
		t.Fatalf("两次 Stop 应各带自己的真实正文，got %+v", obs)
	}
}

// TestTraeStopFallbackTextContent：last_assistant_message 缺失时回退 text_content。
func TestTraeStopFallbackTextContent(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	sid := "trae-text-stop"
	const body = "备用正文：transcript 缺失时仍有可沉淀事实"

	text, fid := observationText(harness.EventStop, hookInput{TextContent: body}, false)
	if fid != evidence.FidelityFull || text != body {
		t.Fatalf("text_content 应标 full 且原样取用，got fid=%q text=%q", fid, text)
	}

	in := hookJSON(t, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
		"text_content": body,
	})
	if _, err := withStdin(t, in, func() error {
		return cmdHook([]string{"--db", db, "--harness", "trae", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatalf("Stop hook: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	obs, err := st.ListObservations(sid, 10)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	if len(obs) != 1 || !strings.Contains(obs[0].Summary, body) {
		t.Fatalf("text_content 应成为 observation 正文，got %+v", obs)
	}
	if strings.Contains(obs[0].Summary, "stop") {
		t.Fatalf("不得退化成事件名 stop: %q", obs[0].Summary)
	}
}

// TestTraeStopNoPayloadIsNotFabricated：缺载荷 Stop 不伪造正文、标 none。
func TestTraeStopNoPayloadIsNotFabricated(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	sid := "trae-empty-stop"

	// 捕获层：拿不到内容就为空，绝不回退成 "stop" / "session-end" 事件名字面量。
	text, fid := observationText(harness.EventStop, hookInput{}, false)
	if fid != evidence.FidelityNone {
		t.Fatalf("缺载荷 Stop Fidelity 应为 none，got %q", fid)
	}
	if text != "" {
		t.Fatalf("缺载荷 Stop 不得伪造正文，got %q", text)
	}

	in := hookJSON(t, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
	})
	if _, err := withStdin(t, in, func() error {
		return cmdHook([]string{"--db", db, "--harness", "trae", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatalf("Stop hook: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	obs, err := st.ListObservations(sid, 10)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	// §4.3：none 只记事件，不生成摘要内容；事件本身仍必须留下。
	if len(obs) != 1 {
		t.Fatalf("fidelity=none 仍应留下 1 条事件记录，got %d", len(obs))
	}
	// 说明：展示摘要此时仍可能为 "stop"，那是 internal/evidence/engine.go 的
	// summarizeRaw 在 Raw 为空时按 eventType 兜底，属 T2 独占层，本波不触碰。
	// 本分支保证的是归档 Raw 为空 + RawEvent.Fidelity=none（提案准入据此拦截）。
}

// TestTraeObservationFidelityClassification 表驱动锁住 observationText 的全部分级分支：
// 有真实正文→full、只有 status/reason 或摘要→summary、无正文→none。
func TestTraeObservationFidelityClassification(t *testing.T) {
	cases := []struct {
		name     string
		event    harness.Event
		in       hookInput
		wantText string
		wantFid  evidence.Fidelity
	}{
		{
			name:     "Stop 优先 last_assistant_message",
			event:    harness.EventStop,
			in:       hookInput{LastAssistantMessage: "结论：根因是权限位", Status: "completed"},
			wantText: "结论：根因是权限位",
			wantFid:  evidence.FidelityFull,
		},
		{
			name:     "Stop 回退 text_content",
			event:    harness.EventStop,
			in:       hookInput{TextContent: "备用正文"},
			wantText: "备用正文",
			wantFid:  evidence.FidelityFull,
		},
		{
			name:     "Stop 只有 status → summary，不写 stop 字面量",
			event:    harness.EventStop,
			in:       hookInput{Status: "completed"},
			wantText: "status=completed",
			wantFid:  evidence.FidelitySummary,
		},
		{
			name:     "Stop 只有 reason → summary",
			event:    harness.EventStop,
			in:       hookInput{Reason: "max_turns"},
			wantText: "reason=max_turns",
			wantFid:  evidence.FidelitySummary,
		},
		{
			name:     "Stop 同时有 status 与 reason",
			event:    harness.EventStop,
			in:       hookInput{Status: "completed", Reason: "done"},
			wantText: "status=completed reason=done",
			wantFid:  evidence.FidelitySummary,
		},
		{
			name:     "Stop 无载荷 → none + 空正文",
			event:    harness.EventStop,
			in:       hookInput{},
			wantText: "",
			wantFid:  evidence.FidelityNone,
		},
		{
			name:     "SessionEnd 回退 reason → summary",
			event:    harness.EventSessionEnd,
			in:       hookInput{Reason: "completed"},
			wantText: "reason=completed",
			wantFid:  evidence.FidelitySummary,
		},
		{
			name:     "SessionEnd 带 last_assistant_message → full",
			event:    harness.EventSessionEnd,
			in:       hookInput{LastAssistantMessage: "会话总结正文"},
			wantText: "会话总结正文",
			wantFid:  evidence.FidelityFull,
		},
		{
			name:     "SessionEnd 无载荷 → none + 空正文",
			event:    harness.EventSessionEnd,
			in:       hookInput{},
			wantText: "",
			wantFid:  evidence.FidelityNone,
		},
		{
			name:     "PostToolUse 有 tool_input → full",
			event:    harness.EventPostToolUse,
			in:       hookInput{ToolName: "Bash", ToolInput: json.RawMessage(`"echo trae"`)},
			wantText: "tool:Bash\necho trae",
			wantFid:  evidence.FidelityFull,
		},
		{
			name:     "PostToolUse 只有工具名 → summary",
			event:    harness.EventPostToolUse,
			in:       hookInput{ToolName: "Bash"},
			wantText: "tool:Bash",
			wantFid:  evidence.FidelitySummary,
		},
		{
			name:     "其它事件 → none（default 分支）",
			event:    harness.Event("custom-event"),
			in:       hookInput{},
			wantText: "custom-event",
			wantFid:  evidence.FidelityNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, fid := observationText(tc.event, tc.in, false)
			if fid != tc.wantFid {
				t.Errorf("Fidelity=%q want %q", fid, tc.wantFid)
			}
			if text != tc.wantText {
				t.Errorf("text=%q want %q", text, tc.wantText)
			}
			if (tc.event == harness.EventStop || tc.event == harness.EventSessionEnd) &&
				tc.wantFid == evidence.FidelityNone && text != "" {
				t.Errorf("none 时不得伪造正文: %q", text)
			}
		})
	}
}

// TestTraeRecordEvidenceFidelity：transcript 的 unparsed 行标 pointer 且仍不可提案；
// 正常解析的行带真实正文，标 full。
func TestTraeRecordEvidenceFidelity(t *testing.T) {
	const (
		src  = "transcript:trae:s"
		path = "/tmp/trae-tx.jsonl"
	)
	tools := map[string]string{}

	parsed := transcript.Record{
		EventType: transcript.EventUserPrompt,
		Text:      "需求：把证据写全",
		HostUID:   "u1",
	}
	ev := recordEvidence(src, "s", "demo", "trae", path, parsed, tools, false)
	if ev.Fidelity != evidence.FidelityFull {
		t.Errorf("已解析 transcript 行应为 full，got %q", ev.Fidelity)
	}
	if !ev.ProposalEligible {
		t.Errorf("已解析行应可提案")
	}

	unparsed := transcript.Record{
		EventType: transcript.EventUnparsed,
		Text:      "UNPARSED_MARKER_原文不丢",
		Start:     10,
		Block:     0,
		Unparsed:  true,
	}
	ev = recordEvidence(src, "s", "demo", "trae", path, unparsed, tools, false)
	if ev.Fidelity != evidence.FidelityPointer {
		t.Errorf("unparsed 行应为 pointer，got %q", ev.Fidelity)
	}
	if ev.ProposalEligible {
		t.Errorf("unparsed 行保持 ProposalEligible=false")
	}
	if !strings.Contains(string(ev.Raw), "UNPARSED_MARKER_原文不丢") {
		t.Errorf("unparsed 行仍须按原文归档，got %q", string(ev.Raw))
	}
}

// TestTraePipelineToolStillIneligible：Trae 把 last_assistant_message 塞进
// PostToolUse 载荷时，管道工具仍 ProposalEligible=false（isPipelineTool 逻辑不动），
// 且未知字段被忽略而非丢事件。
func TestTraePipelineToolStillIneligible(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	sid := "trae-pipe-sess"
	in := hookJSON(t, map[string]any{
		"hook_event_name": "postToolUse", "cwd": proj, "session_id": sid,
		"tool_name": "mcp__v2mem__search", "event_id": "evt-trae-pipe",
		"tool_input":             map[string]any{"query": "缓存根因"},
		"last_assistant_message": "结论：不应进提案",
	})
	if _, err := withStdin(t, in, func() error {
		return cmdHook([]string{"--db", db, "--harness", "trae", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatalf("hook: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	// --harness trae 经 harness.Get 归一为 traecode。
	src := observeSourceID("traecode", sid)
	rec, ok, err := st.LookupEvidence(src+":event:evt-trae-pipe", src, 0, 0)
	if err != nil || !ok {
		t.Fatalf("管道事件仍须归档: ok=%v err=%v", ok, err)
	}
	if rec.ProposalEligible {
		t.Fatalf("管道工具仍不可提案")
	}
}
