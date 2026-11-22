package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
	"github.com/v2camp/v2mem/internal/transcript"
)

// UserPromptSubmit 过去只做检索注入、完全不写库；本测试守住「用户原话必须
// 以 user-prompt 证据进链路」这条主来源约束，并确认它不写 memories。
func TestPromptSubmitWritesUserPromptEvidence(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	sid := "sess-speaker"
	in := hookJSON(t, map[string]any{
		"hook_event_name": "UserPromptSubmit", "cwd": proj, "session_id": sid,
		"prompt": "偏好：候选要标出是谁说的", "event_id": "evt-user-1",
	})
	args := []string{"--db", db, "--harness", "workbuddy", "--audit", "-", "--dedup-window", "0"}
	for i := 0; i < 2; i++ {
		if _, err := withStdin(t, in, func() error { return cmdHook(args) }); err != nil {
			t.Fatalf("第 %d 次 prompt-submit: %v", i+1, err)
		}
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	obs, err := st.ListObservations(sid, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 {
		t.Fatalf("同一事件重放应幂等，got %d 条: %+v", len(obs), obs)
	}
	o := obs[0]
	if o.Event != "user-prompt" {
		t.Fatalf("事件类型应为 user-prompt，got %q", o.Event)
	}
	if o.Channel != store.ChannelUser {
		t.Fatalf("用户提问 channel 应为 user，got %q", o.Channel)
	}
	if !strings.Contains(o.MetaJSON, `"proposal_eligible":true`) ||
		!strings.Contains(o.MetaJSON, `"fidelity":"full"`) {
		t.Fatalf("用户提问必须可提案且 full，got %s", o.MetaJSON)
	}
	if !strings.Contains(o.Summary, "候选要标出是谁说的") {
		t.Fatalf("摘要应是用户原话，got %q", o.Summary)
	}
	mems, err := st.List(store.ListQuery{Scope: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 0 {
		t.Fatalf("prompt-submit 绝不能写 memories，got %d", len(mems))
	}
}

// 宿主不保证 UserPromptSubmit 的 prompt 是用户说的：后台任务完成通知会整段
// 以 <task-notification> 形态送进来。归档必须保真，但这类消息既不是 full 正文，
// 也不得进提案——否则 36 条/283 条的用户证据是宿主噪音，R9 准入会被撑开。
func TestPromptSubmitHostNoticeIsNotUserSpeech(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	sid := "sess-host-notice"
	in := hookJSON(t, map[string]any{
		"hook_event_name": "UserPromptSubmit", "cwd": proj, "session_id": sid,
		"prompt": "<task-notification><task-id>iKjIwS</task-id><tool-use-id>call_01</tool-use-id>" +
			"<status>completed</status><summary>Background command done</summary></task-notification>",
		"event_id": "evt-notice-1",
	})
	if _, err := withStdin(t, in, func() error {
		return cmdHook([]string{"--db", db, "--harness", "workbuddy", "--audit", "-", "--dedup-window", "0"})
	}); err != nil {
		t.Fatalf("prompt-submit 通知: %v", err)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	obs, err := st.ListObservations(sid, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 {
		t.Fatalf("通知仍须归档一行证据，got %d 条", len(obs))
	}
	o := obs[0]
	if !strings.Contains(o.MetaJSON, `"proposal_eligible":false`) ||
		!strings.Contains(o.MetaJSON, `"fidelity":"none"`) {
		t.Fatalf("宿主通知不得标 full/可提案，got %s", o.MetaJSON)
	}
	if o.ArchiveHash == "" || o.EventUID == "" {
		t.Fatalf("归档仍须落盘（原文保真），got archive=%q uid=%q", o.ArchiveHash, o.EventUID)
	}
	// 摘要仍回退到归档原文（eventSummary 的设计：无 override 就用 Raw），
	// 所以这里不要求 summary 为空；判「不是用户的话」只看 fidelity / eligible。
	if o.Channel != store.ChannelUser {
		t.Fatalf("通道仍是 user（宿主从 user 通道送来），got %q", o.Channel)
	}
}

// 归档保真 vs 查询源清洗：Raw 仍含注入上下文，Summary 只留 <user_query>。
func TestRecordEvidenceUserPromptSummaryOverride(t *testing.T) {
	full := "<system-reminder data-role=\"user-context\">OS: darwin</system-reminder>\n" +
		"<user_query>按 B 来推进，完成后验证再合入</user_query>"
	rec := transcript.Record{
		SessionID: "s", Role: "user", EventType: transcript.EventUserPrompt,
		Text: full, HostUID: "wb-u1#0",
	}
	ev := recordEvidence("transcript:workbuddy:s", "s", "demo", "workbuddy", "/tmp/tx.jsonl", rec, nil, false)
	if string(ev.Raw) != full {
		t.Fatalf("归档 Raw 必须保留完整原文，got %q", ev.Raw)
	}
	if ev.SummaryOverride != "按 B 来推进，完成后验证再合入" {
		t.Fatalf("查询源应只剩用户原话，got %q", ev.SummaryOverride)
	}
	if !ev.ProposalEligible || ev.Fidelity != evidence.FidelityFull {
		t.Fatalf("应准入且 full，got eligible=%v fidelity=%q", ev.ProposalEligible, ev.Fidelity)
	}

	onlyCtx := transcript.Record{
		SessionID: "s", Role: "user", EventType: transcript.EventUserPrompt,
		Text:    "<system-reminder data-role=\"craft_mode\">Agent mode</system-reminder>",
		HostUID: "wb-u2#0",
	}
	ev2 := recordEvidence("transcript:workbuddy:s", "s", "demo", "workbuddy", "/tmp/tx.jsonl", onlyCtx, nil, false)
	if ev2.ProposalEligible || ev2.Fidelity != evidence.FidelityNone || ev2.SummaryOverride != "" {
		t.Fatalf("整条都是注入上下文时不得进提案，got %+v", ev2)
	}
}

// 真实缺陷的回归：WorkBuddy transcript 的用户/助手消息过去全落 unparsed（pointer，
// 不可提案）。现在必须成为 user-prompt / assistant-text 两条可提案证据，且用户
// 那条摘要只有真正的提问。
func TestObserveStopCapturesNativeWorkBuddyTranscript(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	path := filepath.Join(t.TempDir(), "wb.jsonl")
	sid := "wb-native-sess"
	writeClaudeTranscript(t, path,
		`{"id":"wb-u1","timestamp":1793583268633,"type":"message","role":"user","sessionId":"`+sid+`","cwd":"/work/demo","content":[{"type":"input_text","text":"<system-reminder data-role=\"user-context\">OS: darwin</system-reminder>\n<user_query>按 B 来推进</user_query>"}]}`,
		`{"id":"wb-a1","timestamp":1793583599512,"type":"message","role":"assistant","sessionId":"`+sid+`","cwd":"/work/demo","message":{"usage":{"output_tokens":12}},"content":[{"type":"output_text","text":"结论：归档双方都记，提案按说话人分组"}]}`,
		`{"id":"wb-t1","type":"function_call","name":"Bash","callId":"c1","sessionId":"`+sid+`"}`,
	)
	stop := hookJSON(t, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
		"status": "completed", "transcript_path": path,
	})
	if _, err := withStdin(t, stop, func() error {
		return cmdHook([]string{"--db", db, "--harness", "workbuddy", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := transcriptSourceID("workbuddy", sid)
	u, ok, err := st.LookupEvidence("tx:wb-u1#0", src, 0, 0)
	if err != nil || !ok {
		t.Fatalf("用户消息证据缺失: ok=%v err=%v", ok, err)
	}
	if u.EventType != "user-prompt" || !u.ProposalEligible {
		t.Fatalf("用户消息应可提案，got %+v", u)
	}
	if u.Summary != "按 B 来推进" || strings.Contains(u.Summary, "system-reminder") {
		t.Fatalf("提案输入必须只有用户原话，got %q", u.Summary)
	}
	a, ok, err := st.LookupEvidence("tx:wb-a1#0", src, 0, 0)
	if err != nil || !ok {
		t.Fatalf("助手消息证据缺失: ok=%v err=%v", ok, err)
	}
	if a.EventType != "assistant-text" || !a.ProposalEligible ||
		!strings.Contains(a.Summary, "按说话人分组") {
		t.Fatalf("助手消息应可提案且带结论，got %+v", a)
	}
	obs, err := st.ListObservations(sid, 20)
	if err != nil {
		t.Fatal(err)
	}
	unparsed := 0
	for _, o := range obs {
		if o.Event == "unparsed" {
			unparsed++
			if !strings.Contains(o.Summary, "function_call") {
				t.Fatalf("工具行仍须按原文归档，got %q", o.Summary)
			}
		}
	}
	if unparsed != 1 {
		t.Fatalf("未识别的工具行应恰好 1 条（原文保真），got %d", unparsed)
	}
}
