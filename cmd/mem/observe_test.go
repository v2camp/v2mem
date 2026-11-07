package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
	"github.com/v2camp/v2mem/internal/transcript"
)

func hookJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}

func TestObserveHookWritesBufferNotMemories(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", name, "--kind", "fact", "唯一的永久记忆"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	ap := filepath.Join(t.TempDir(), "audit.jsonl")
	sid := "sess-observe"
	inputs := []string{
		hookJSON(t, map[string]any{
			"hook_event_name": "postToolUse", "cwd": proj, "session_id": sid,
			"tool_name": "Shell", "tool_use_id": "tu-1",
			"tool_input": map[string]any{"command": "echo 观测标记香蕉 api_key=supersecret"},
		}),
		hookJSON(t, map[string]any{
			"hook_event_name": "Stop", "cwd": proj, "session_id": sid, "status": "completed",
		}),
		hookJSON(t, map[string]any{
			"hook_event_name": "sessionEnd", "cwd": proj, "session_id": sid, "reason": "completed",
		}),
	}
	for _, in := range inputs {
		out, err := withStdin(t, in, func() error {
			return cmdHook([]string{"--db", db, "--harness", "cursor", "--audit", ap})
		})
		if err != nil {
			t.Fatalf("hook: %v", err)
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("观测钩子不应向 stdout 注入，got %q", out)
		}
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	mems, err := st.List(store.ListQuery{Scope: "all"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mems) != 1 || !strings.Contains(mems[0].Content, "永久记忆") {
		t.Fatalf("hooks 不得写入 memories，got %+v", mems)
	}
	obs, err := st.ListObservations(sid, 20)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	if len(obs) != 3 {
		t.Fatalf("未手调 add 也应有 3 条观测，got %d", len(obs))
	}
	var sawPost, sawStop, sawEnd, sawRedacted bool
	for _, o := range obs {
		// T1.2 起观测是 canonical 证据行：meta 由 evidence 写 proposal_eligible，
		// 不再带 hook-observe 标记；来源改由 event_uid/source_id 表达。
		if !strings.Contains(o.MetaJSON, "proposal_eligible") {
			t.Errorf("证据行 meta 应带 proposal_eligible，got %s", o.MetaJSON)
		}
		switch o.Event {
		case "post-tool-use":
			sawPost = true
			if o.Channel != store.ChannelTool {
				t.Errorf("PostToolUse 默认 channel=tool，got %s", o.Channel)
			}
			if o.EventID != "" {
				t.Errorf("证据行不再写 event_id（身份在 event_uid），got %s", o.EventID)
			}
			if strings.Contains(o.Summary, "supersecret") {
				t.Errorf("摘要应抹掉密钥，got %s", o.Summary)
			}
			if strings.Contains(o.Summary, "[redacted]") {
				sawRedacted = true
			}
		case "stop":
			sawStop = true
			if o.Channel != store.ChannelAgent {
				t.Errorf("Stop 默认 channel=agent，got %s", o.Channel)
			}
		case "session-end":
			sawEnd = true
			if o.Channel != store.ChannelAgent {
				t.Errorf("SessionEnd 默认 channel=agent，got %s", o.Channel)
			}
		}
	}
	if !sawPost || !sawStop || !sawEnd || !sawRedacted {
		t.Fatalf("观测事件不全 post=%v stop=%v end=%v redacted=%v", sawPost, sawStop, sawEnd, sawRedacted)
	}
	// 宿主事件号成为证据身份：同一 tool_use_id 重放不会再插一行。
	src := observeSourceID("cursor", sid)
	if _, ok, err := st.LookupEvidence(src+":tool:tu-1", src, 0, 0); err != nil || !ok {
		t.Fatalf("PostToolUse 应以 tool_use_id 作证据身份，got ok=%v err=%v", ok, err)
	}

	searchOut, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "观测标记香蕉"})
	})
	if err != nil {
		t.Fatalf("cmdSearch: %v", err)
	}
	if !strings.Contains(searchOut, "无结果") {
		t.Errorf("mem search 不应搜到观测，got:\n%s", searchOut)
	}
	layered, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--layer", "observation", "观测标记香蕉"})
	})
	if err != nil {
		t.Fatalf("cmdSearch observation: %v", err)
	}
	if !strings.Contains(layered, "观测标记香蕉") || !strings.Contains(layered, "observation:") {
		t.Errorf("痕迹层应搜到观测，got:\n%s", layered)
	}
	injected, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "观测标记香蕉"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "cursor"})
	})
	if err != nil {
		t.Fatalf("prompt hook: %v", err)
	}
	if strings.Contains(injected, "观测标记香蕉") {
		t.Errorf("随问注入应只带已定记忆，got:\n%s", injected)
	}

	rs, err := audit.Read(ap, 0)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rs {
		got[r.Event] = true
		if r.Event == "manual-add" || r.Event == "manual-forget" {
			t.Errorf("观测不得产生写侧审计 %+v", r)
		}
		if r.Event == "obs-append" && r.Source != "hook-observe" {
			t.Errorf("obs-append 应带来源 hook-observe，got %+v", r)
		}
	}
	for _, ev := range []string{"post-tool-use", "session-end", "obs-append", "stop"} {
		if !got[ev] {
			t.Errorf("审计应见到 %s，got %+v", ev, rs)
		}
	}
	sum := audit.Summarize(rs)
	appends := 0
	for _, r := range rs {
		if r.Event == "obs-append" {
			appends++
		}
	}
	if sum.Observations != appends || sum.Writes != 0 {
		t.Fatalf("观测次数应等于 obs-append（%d），不得把触发事件再算进去，got obs=%d writes=%d",
			appends, sum.Observations, sum.Writes)
	}
}

// T1.2 的身份语义：
//   - 同一宿主事件（有 event_id）重放 → 一行；
//   - 同摘要、无 id 的两次真实 Stop → 两行（内容去重不再吞并不同事件）。
func TestObserveStopKeepsSameSummaryEventsApart(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	noID := hookJSON(t, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": "same", "status": "completed",
	})
	for i := 0; i < 2; i++ {
		if _, err := withStdin(t, noID, func() error {
			return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
		}); err != nil {
			t.Fatalf("hook #%d: %v", i, err)
		}
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	obs, err := st.ListObservations("same", 10)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("同摘要不同 event_uid 必须共存，got %d", len(obs))
	}
	for _, o := range obs {
		// observationText 不再把 "stop" 事件名当正文（design-capture §0/§4.3）：
		// 只有 status 时保真度为 summary，摘要是结构化的 status 值。
		// 本用例的核心断言不变：两条摘要应相同，且不被内容去重吞并。
		if o.Summary != "status=completed" {
			t.Fatalf("两条摘要应相同（被内容去重吞并才算失败），got %q", o.Summary)
		}
	}

	// 宿主给了 id：同一 id 重放必须只留一行。
	withID := hookJSON(t, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": "same-id",
		"status": "completed", "event_id": "stop-1",
	})
	for i := 0; i < 2; i++ {
		if _, err := withStdin(t, withID, func() error {
			return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
		}); err != nil {
			t.Fatalf("hook 重放 #%d: %v", i, err)
		}
	}
	obs, err = st.ListObservations("same-id", 10)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("同一 event_id 重放应只留 1 条，got %d", len(obs))
	}
}

func TestCmdHookPreCompactInjectsAndHonoursBudget(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", name, "--kind", "fact", "预压缩应注入的工程记忆甲"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	for i := 0; i < 6; i++ {
		body := strings.Repeat("长记忆填充预算"+string(rune('甲'+i)), 8)
		if err := cmdAdd([]string{"--db", db, "--project", name, body}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}

	out, err := withStdin(t, hookStdin("PreCompact", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--limit", "20", "--max-chars", "4000", "--dedup-window", "0", "--audit", "-"})
	})
	if err != nil {
		t.Fatalf("PreCompact: %v", err)
	}
	if !strings.Contains(out, "预压缩应注入的工程记忆甲") || !strings.Contains(out, "search / add") {
		t.Fatalf("PreCompact 应走与 SessionStart 相同的注入，got:\n%s", out)
	}

	t.Setenv("MEM_PRECOMPACT_BUDGET", "160")
	small, err := withStdin(t, hookStdin("PreCompact", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--limit", "20", "--dedup-window", "0", "--audit", "-"})
	})
	if err != nil {
		t.Fatalf("PreCompact budget: %v", err)
	}
	if !strings.Contains(small, "search / add") {
		t.Fatalf("预算再小也要留下用法提示，got:\n%s", small)
	}
	if len([]rune(small)) >= len([]rune(out)) {
		t.Fatalf("MEM_PRECOMPACT_BUDGET 应缩短注入，small=%d full=%d", len([]rune(small)), len([]rune(out)))
	}

	cursorOut, err := withStdin(t, hookStdin("preCompact", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "cursor", "--limit", "1", "--dedup-window", "0", "--audit", "-"})
	})
	if err != nil {
		t.Fatalf("cursor PreCompact: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(cursorOut), &payload); err != nil {
		t.Fatalf("Cursor 注入应为 JSON，got %q err %v", cursorOut, err)
	}
	if !strings.Contains(payload["additional_context"], "search / add") {
		t.Fatalf("additional_context 应含注入正文，got %q", payload["additional_context"])
	}
}

func TestSpoolDrainsOnSessionStart(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	if err := os.MkdirAll(db, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	proj, _ := hookProjectDir(t, "demo-repo")
	in := hookJSON(t, map[string]any{
		"hook_event_name": "postToolUse", "cwd": proj, "session_id": "spool-sess",
		"tool_name": "Shell", "tool_use_id": "tu-spool",
		"tool_input": map[string]any{"command": "echo spool-marker-zeta"},
	})
	if _, err := withStdin(t, in, func() error {
		return cmdHook([]string{"--db", db, "--harness", "cursor", "--audit", "-"})
	}); err != nil {
		t.Fatalf("写库失败的 hook 仍应返回 nil: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "spool", "spool-sess", "seg-*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("应落下 spool/<session>/seg-*.jsonl，got %v err %v", matches, err)
	}
	fi, err := os.Stat(matches[0])
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("spool 文件权限应为 0600，got %o", fi.Mode().Perm())
	}
	if tmp, _ := filepath.Glob(filepath.Join(dir, "spool", "spool-sess", "*.tmp")); len(tmp) != 0 {
		t.Errorf("tmp 必须先 rename 掉，残留 %v", tmp)
	}

	if err := os.RemoveAll(db); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "cursor", "--audit", "-"})
	}); err != nil {
		t.Fatalf("SessionStart drain: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	obs, err := st.ListObservations("spool-sess", 10)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	if len(obs) != 1 || !strings.Contains(obs[0].Summary, "spool-marker-zeta") {
		t.Fatalf("SessionStart 应把 spool 灌进 observations，got %+v", obs)
	}
	if _, err := os.Stat(matches[0]); !os.IsNotExist(err) {
		t.Errorf("drain 成功后 spool 文件应删除")
	}
}

func TestCmdGCObsLeavesMemories(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "gc 不该删的记忆"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendObservation(store.ObsInput{SessionID: "s", Event: "stop", Summary: "一条观测"}); err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdGC([]string{"--db", db, "--obs", "--max-age", "720h"})
	})
	if err != nil {
		t.Fatalf("cmdGC --obs: %v", err)
	}
	if !strings.Contains(out, "未改动 memories") {
		t.Fatalf("应说明不改 memories，got:\n%s", out)
	}
	st, err = store.Open(db)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	mems, err := st.List(store.ListQuery{Scope: "all"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("mem gc --obs 不得删除记忆，got %d", len(mems))
	}
}

func TestCmdInitCursorRegistersSixEvents(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".cursor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "hooks.json")
	existing := `{
  "version": 1,
  "hooks": {
    "afterFileEdit": [{"command": "/usr/local/bin/format.sh"}]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "cursor", "--project", proj})
	})
	if err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	for _, ev := range []string{"sessionStart", "beforeSubmitPrompt", "postToolUse", "preCompact", "stop", "sessionEnd"} {
		if !strings.Contains(out, ev) {
			t.Errorf("摘要应包含 %s，got:\n%s", ev, out)
		}
	}

	cfg := readCfg(t, path)
	if cfg["version"] != float64(1) && cfg["version"] != 1 {
		// encoding/json 把数字解成 float64
		if v, ok := cfg["version"].(float64); !ok || v != 1 {
			t.Errorf("version 应为 1，got %#v", cfg["version"])
		}
	}
	hooks := cfg["hooks"].(map[string]any)
	if _, ok := hooks["afterFileEdit"]; !ok {
		t.Error("用户自己的 afterFileEdit 应保留")
	}
	for _, ev := range []string{"sessionStart", "beforeSubmitPrompt", "postToolUse", "preCompact", "stop", "sessionEnd"} {
		items, _ := hooks[ev].([]any)
		if len(items) != 1 {
			t.Fatalf("%s 应有 1 条 command，got %#v", ev, hooks[ev])
		}
		cmd, _ := items[0].(map[string]any)["command"].(string)
		if !strings.Contains(cmd, "hook") || !strings.Contains(cmd, "--harness cursor") || !strings.Contains(cmd, "# v2mem") {
			t.Errorf("%s 命令应为 mem hook --harness cursor，got %q", ev, cmd)
		}
		if !filepath.IsAbs(firstToken(cmd)) {
			t.Errorf("%s 应用绝对路径，got %q", ev, cmd)
		}
	}

	before, _ := os.ReadFile(path)
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "cursor", "--project", proj})
	}); err != nil {
		t.Fatalf("第二次 init: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Errorf("Cursor init 应幂等\n前:\n%s\n后:\n%s", before, after)
	}

	if _, err := captureStdout(t, func() error {
		return cmdUninstall([]string{"--harness", "cursor", "--project", proj})
	}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	cfg = readCfg(t, path)
	hooks = cfg["hooks"].(map[string]any)
	if _, ok := hooks["afterFileEdit"]; !ok {
		t.Error("卸载后应保留用户钩子")
	}
	if _, ok := hooks["sessionStart"]; ok {
		t.Errorf("卸载后不应留下 sessionStart，got %#v", hooks["sessionStart"])
	}
}

func TestCmdInitRegistersStopWhereDocumented(t *testing.T) {
	claudeProj := t.TempDir()
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "claude", "--project", claudeProj})
	}); err != nil {
		t.Fatalf("claude init: %v", err)
	}
	claudeCfg := readCfg(t, filepath.Join(claudeProj, ".claude", "settings.json"))
	ch := claudeCfg["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		if _, ok := ch[ev]; !ok {
			t.Errorf("claude 应注册 %s", ev)
		}
	}

	codexProj := t.TempDir()
	if _, err := captureStdout(t, func() error {
		return cmdInit([]string{"--harness", "codex", "--project", codexProj})
	}); err != nil {
		t.Fatalf("codex init: %v", err)
	}
	codexCfg := readCfg(t, filepath.Join(codexProj, ".codex", "hooks.json"))
	hooks := codexCfg["hooks"].(map[string]any)
	if _, ok := hooks["Stop"]; !ok {
		t.Error("codex 应注册 Stop")
	}
	if _, ok := hooks["SessionEnd"]; ok {
		t.Error("codex 官方事件表没有 SessionEnd，不应注册")
	}
}

func TestObserveEventIDReplayAndNoScrub(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	body := hookJSON(t, map[string]any{
		"hook_event_name": "postToolUse", "cwd": proj, "session_id": "evt-sess",
		"tool_name": "Shell", "event_id": "host-evt-9",
		"tool_input": map[string]any{"command": "echo api_key=supersecret"},
	})
	for i := 0; i < 2; i++ {
		if _, err := withStdin(t, body, func() error {
			return cmdHook([]string{"--db", db, "--harness", "cursor", "--dedup-window", "0", "--audit", "-"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	obs, err := st.ListObservations("evt-sess", 10)
	if err != nil {
		t.Fatal(err)
	}
	src := observeSourceID("cursor", "evt-sess")
	if len(obs) != 1 || strings.Contains(obs[0].Summary, "supersecret") {
		t.Fatalf("重放不得双写，且默认打码: %+v", obs)
	}
	if rec, ok, err := st.LookupEvidence(src+":event:host-evt-9", src, 0, 0); err != nil || !ok || rec.EventUID != src+":event:host-evt-9" {
		t.Fatalf("宿主 event_id 应成为证据身份，got ok=%v err=%v", ok, err)
	}

	raw := hookJSON(t, map[string]any{
		"hook_event_name": "postToolUse", "cwd": proj, "session_id": "evt-sess",
		"tool_name": "Shell", "event_id": "host-evt-raw",
		"tool_input": map[string]any{"command": "echo api_key=supersecret"},
	})
	if _, err := withStdin(t, raw, func() error {
		return cmdHook([]string{"--db", db, "--harness", "cursor", "--dedup-window", "0", "--no-scrub", "--audit", "-"})
	}); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := st.LookupEvidence(src+":event:host-evt-raw", src, 0, 0)
	if err != nil || !ok {
		t.Fatalf("--no-scrub 那条应可查到，ok=%v err=%v", ok, err)
	}
	if !strings.Contains(rec.Summary, "supersecret") {
		t.Fatalf("--no-scrub 应保留密钥: %+v", rec)
	}
}

// ---------- T1.2：transcript 增量、管道事件、spool 契约 ----------

func writeClaudeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("写 transcript: %v", err)
	}
}

// Stop 必须是 marker，同时把 transcript 的用户/助手/工具事件按原生顺序补成独立证据；
// 最后一条助手结论必须自己占一行（不能被塞进 stop 摘要）。
func TestObserveStopCapturesTranscriptIncrementally(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	sid := "tx-sess"
	writeClaudeTranscript(t, path,
		`{"type":"user","uuid":"u1","sessionId":"`+sid+`","timestamp":"2026-11-06T00:00:00Z","cwd":"/work/demo","message":{"role":"user","content":"需求：把证据写全"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"`+sid+`","timestamp":"2026-11-06T00:00:01Z","cwd":"/work/demo","message":{"role":"assistant","content":[{"type":"text","text":"先看代码"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","uuid":"u2","sessionId":"`+sid+`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a.txt"}]}}`,
		`{"type":"assistant","uuid":"a2","sessionId":"`+sid+`","timestamp":"2026-11-06T00:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"结论：T1.2 按事件身份归档"}]}}`,
	)
	stop := hookJSON(t, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
		"status": "completed", "transcript_path": path,
	})
	runStop := func() {
		t.Helper()
		if _, err := withStdin(t, stop, func() error {
			return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
		}); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
	runStop()

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := transcriptSourceID("claude", sid)
	order := []string{"tx:u1", "tx:a1#0", "tx:tool_use:toolu_1", "tx:tool_result:toolu_1", "tx:a2#0"}
	var lastSeq int
	for _, uid := range order {
		rec, ok, err := st.LookupEvidence(uid, src, 0, 0)
		if err != nil || !ok {
			t.Fatalf("证据 %s 缺失（ok=%v err=%v）", uid, ok, err)
		}
		if rec.Seq <= lastSeq {
			t.Fatalf("原生行序被破坏：%s seq=%d 不大于前一条 %d", uid, rec.Seq, lastSeq)
		}
		lastSeq = rec.Seq
	}
	final, ok, err := st.LookupEvidence("tx:a2#0", src, 0, 0)
	if err != nil || !ok {
		t.Fatalf("最后一条助手结论必须是独立证据: ok=%v err=%v", ok, err)
	}
	if final.Seq != lastSeq || !strings.Contains(final.Summary, "按事件身份归档") {
		t.Fatalf("最后一条应为该源的末条且带结论正文: %+v", final)
	}
	if final.EventType != "assistant-text" {
		t.Fatalf("助手结论事件类型应为 assistant-text，got %s", final.EventType)
	}

	// stop marker 自己一行，且不重复承载结论正文。
	markerSrc := observeSourceID("claude", sid)
	if _, ok, err := st.LookupEvidence(markerSrc+":syn", markerSrc, 0, 0); err != nil || ok {
		t.Fatalf("stop marker 身份不该被猜出来: ok=%v err=%v", ok, err)
	}
	obs, err := st.ListObservations(sid, 20)
	if err != nil {
		t.Fatal(err)
	}
	markers := 0
	for _, o := range obs {
		if o.Event == "stop" {
			markers++
			if strings.Contains(o.Summary, "按事件身份归档") {
				t.Errorf("stop marker 不该承载结论正文: %q", o.Summary)
			}
		}
	}
	if markers != 1 {
		t.Fatalf("Stop 应留下 1 条 marker，got %d", markers)
	}
	if len(obs) != 6 {
		t.Fatalf("首轮应有 5 条转录证据 + 1 条 marker，got %d", len(obs))
	}

	// 追加一行后再 Stop：只补新增的那条。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	extra := `{"type":"assistant","uuid":"a3","sessionId":"` + sid + `","message":{"role":"assistant","content":[{"type":"text","text":"补充：增量只读新行"}]}}` + "\n"
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	f.Close()
	runStop()
	obs, err = st.ListObservations(sid, 20)
	if err != nil {
		t.Fatal(err)
	}
	// 6 条转录 + 2 条 stop marker（两次真实 Stop 各一条）。
	if len(obs) != 8 {
		t.Fatalf("增量应只补 1 条转录 + 1 条 marker，got %d: %+v", len(obs), obs)
	}
	if _, ok, _ := st.LookupEvidence("tx:a3#0", src, 0, 0); !ok {
		t.Fatal("新增行应被增量解析到")
	}
}

// 回归计划 §7 停止条件第 1 条：非 Claude 格式的行曾被静默跳过、游标却前进，
// 下次 Stop 永不回看。现在必须按原文归档、标记不可用于提案，且游标不卡死。
func TestObserveStopKeepsUnparsedTranscriptLine(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	path := filepath.Join(t.TempDir(), "tx-mixed.jsonl")
	sid := "tx-mixed"
	const marker = "UNPARSED_MARKER_用户原文不丢"
	writeClaudeTranscript(t, path,
		`{"type":"user","uuid":"u1","sessionId":"`+sid+`","message":{"role":"user","content":"合法前一行"}}`,
		`{"role":"user","content":"`+marker+`","sessionId":"`+sid+`"}`,
		`{"type":"assistant","uuid":"a1","sessionId":"`+sid+`","message":{"role":"assistant","content":"合法后一行"}}`,
	)
	stop := hookJSON(t, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
		"status": "completed", "transcript_path": path,
	})
	runStop := func() {
		t.Helper()
		if _, err := withStdin(t, stop, func() error {
			return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
		}); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
	runStop()

	// 游标已消费整份文件：证明「留原文」没有把增量解析卡在未知行上。
	cur := readTranscriptCursor(transcriptCursorPath(db, sid))
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Offset != fi.Size() {
		t.Fatalf("游标应走到文件末尾，got %d want %d", cur.Offset, fi.Size())
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	check := func(wantUnparsed int) {
		t.Helper()
		obs, err := st.ListObservations(sid, 30)
		if err != nil {
			t.Fatal(err)
		}
		unparsed, events := 0, map[string]bool{}
		for _, o := range obs {
			events[o.Event] = true
			if o.Event != "unparsed" {
				continue
			}
			unparsed++
			if !strings.Contains(o.Summary, marker) {
				t.Fatalf("未知行的原文必须留在证据里，got %q", o.Summary)
			}
			if !strings.Contains(o.MetaJSON, `"proposal_eligible":false`) {
				t.Fatalf("未解析行不得进入 Proposal，meta=%q", o.MetaJSON)
			}
		}
		if unparsed != wantUnparsed {
			t.Fatalf("unparsed 证据应恰好 %d 条，got %d: %+v", wantUnparsed, unparsed, obs)
		}
		// 未知行前后的合法行都还在，游标没有「跳过一段」。
		if !events["user-prompt"] || !events["assistant-text"] {
			t.Fatalf("未知行前后的合法事件都应被归档，got %v", events)
		}
	}
	check(1)

	// 二次 Stop：游标不回看，原文不重复归档（身份由文件偏移+正文 hash 保证幂等）。
	runStop()
	check(1)
}

// 工具输出是 artifact，不是事实陈述：PostToolUse 一律不进提案（不按工具名区分）。
// 归档与保真度不变：有载荷 full、只有工具名 summary。R9 根因回归。
func TestObservePostToolUseNeverProposalEligible(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	cases := []struct {
		name     string
		tool     string
		input    map[string]any
		fidelity string
	}{
		{"普通工具带 JSON 载荷", "Bash", map[string]any{"command": "echo R9_PAYLOAD"}, string(evidence.FidelityFull)},
		{"管道工具带载荷", "mcp__v2mem__search", map[string]any{"query": "x"}, string(evidence.FidelityFull)},
		{"只有工具名", "ToolSearch", nil, string(evidence.FidelitySummary)},
	}
	for i, tc := range cases {
		fields := map[string]any{
			"hook_event_name": "postToolUse", "cwd": proj, "session_id": "pipe-sess",
			"tool_name": tc.tool, "event_id": "evt-" + strconv.Itoa(i),
		}
		if tc.input != nil {
			fields["tool_input"] = tc.input
		}
		in := hookJSON(t, fields)
		if _, err := withStdin(t, in, func() error {
			return cmdHook([]string{"--db", db, "--harness", "claude", "--dedup-window", "0", "--audit", "-"})
		}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := observeSourceID("claude", "pipe-sess")
	for i, tc := range cases {
		uid := src + ":event:evt-" + strconv.Itoa(i)
		rec, ok, err := st.LookupEvidence(uid, src, 0, 0)
		if err != nil || !ok {
			t.Fatalf("%s 应被归档: ok=%v err=%v", tc.name, ok, err)
		}
		if rec.ProposalEligible {
			t.Errorf("%s: 工具载荷不得进提案", tc.name)
		}
		if rec.Fidelity != tc.fidelity {
			t.Errorf("%s: Fidelity=%q want %q", tc.name, rec.Fidelity, tc.fidelity)
		}
	}
	obs, err := st.ListObservations("pipe-sess", 10)
	if err != nil || len(obs) != len(cases) {
		t.Fatalf("工具事件不得被丢弃，got %d err=%v", len(obs), err)
	}
}

// transcript 侧同口径：tool-result 行一律不进提案；用户提问与助手回复仍可提案
// （防止收窄过头把对话正文也挡掉）；unparsed 仍 pointer + 不可提案。
func TestObserveTranscriptProposalEligibility(t *testing.T) {
	const (
		src  = "transcript:claude:tx-admission"
		path = "/tmp/tx-admission.jsonl"
	)
	tools := map[string]string{"toolu_bash": "Bash"}
	cases := []struct {
		name     string
		rec      transcript.Record
		fidelity evidence.Fidelity
		eligible bool
	}{
		{
			name:     "tool-result/普通工具",
			rec:      transcript.Record{EventType: transcript.EventToolResult, ToolID: "toolu_bash", Text: "tool_result"},
			fidelity: evidence.FidelityFull,
			eligible: false,
		},
		{
			name:     "tool-use/普通工具（工具输入也是 artifact）",
			rec:      transcript.Record{EventType: transcript.EventToolUse, ToolID: "toolu_bash", ToolName: "Bash", Text: `tool:Bash\n{"command":"ls"}`},
			fidelity: evidence.FidelityFull,
			eligible: false,
		},
		{
			name:     "user prompt",
			rec:      transcript.Record{EventType: transcript.EventUserPrompt, Text: "需求：把证据写全"},
			fidelity: evidence.FidelityFull,
			eligible: true,
		},
		{
			name:     "assistant text",
			rec:      transcript.Record{EventType: transcript.EventAssistantText, Text: "结论：按事件类型收窄准入"},
			fidelity: evidence.FidelityFull,
			eligible: true,
		},
		{
			name:     "unparsed",
			rec:      transcript.Record{EventType: transcript.EventUnparsed, Text: "RAW_MARKER", Unparsed: true},
			fidelity: evidence.FidelityPointer,
			eligible: false,
		},
	}
	for _, tc := range cases {
		ev := recordEvidence(src, "tx-admission", "demo", "claude", path, tc.rec, tools, false)
		if ev.ProposalEligible != tc.eligible {
			t.Errorf("%s: ProposalEligible=%v want %v", tc.name, ev.ProposalEligible, tc.eligible)
		}
		if ev.Fidelity != tc.fidelity {
			t.Errorf("%s: Fidelity=%q want %q", tc.name, ev.Fidelity, tc.fidelity)
		}
		if len(ev.Raw) == 0 {
			t.Errorf("%s: 原文仍须归档", tc.name)
		}
	}
}

// 库打不开时 cmd/mem 自己写 spool；格式必须能被 evidence.Engine 的 drain 读懂。
func TestSpoolSegmentContractDrainsViaEngine(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	ev := evidence.RawEvent{
		EventUID: "evt:test:contract", SourceID: "hook:claude:s", SessionID: "sess-a",
		Project: "demo", Harness: "claude", EventType: "stop",
		OccurredAtNS: time.Now().UnixNano(), Raw: []byte("spool 契约正文"),
	}
	if err := spoolEvidence(db, ev); err != nil {
		t.Fatalf("spoolEvidence: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(spoolDir(db), "sess-a", "seg-*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("段路径应为 spool/<session>/seg-*.jsonl，got %v err %v", matches, err)
	}
	if tmp, _ := filepath.Glob(filepath.Join(spoolDir(db), "sess-a", "*.tmp")); len(tmp) != 0 {
		t.Fatalf("tmp 必须先 rename 掉，残留 %v", tmp)
	}

	e, err := evidence.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	res, err := e.DrainSpool(10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Drained != 1 || res.Files != 1 {
		t.Fatalf("drain 计数不对: %+v", res)
	}
	if _, err := os.Stat(matches[0]); !os.IsNotExist(err) {
		t.Fatal("成功 drain 后段应删除")
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ref, ok, err := st.LookupEvidence(ev.EventUID, ev.SourceID, 0, 0)
	if err != nil || !ok || !strings.Contains(ref.Summary, "spool 契约正文") {
		t.Fatalf("drain 后应可查到证据: %+v ok=%v err=%v", ref, ok, err)
	}
}

// 段数上限必须可见：兜底路径静默把磁盘写满比丢观测更糟。
func TestSpoolSegmentLimitIsVisible(t *testing.T) {
	old := spoolMaxSegments
	spoolMaxSegments = 2
	t.Cleanup(func() { spoolMaxSegments = old })

	db := filepath.Join(t.TempDir(), "mem.db")
	for i := 0; i < 2; i++ {
		if err := spoolEvidence(db, evidence.RawEvent{
			EventUID: "evt:limit:" + strconv.Itoa(i), SourceID: "s", SessionID: "lim",
			Raw: []byte("段"),
		}); err != nil {
			t.Fatalf("第 %d 段: %v", i, err)
		}
	}
	err := spoolEvidence(db, evidence.RawEvent{
		EventUID: "evt:limit:2", SourceID: "s", SessionID: "lim", Raw: []byte("段"),
	})
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超过段数上限必须报错，got %v", err)
	}
}

// drain 认领 .processing、成功删除、失败恢复，坏行计数上报。
func TestSpoolDrainClaimsAndReportsDropped(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	if err := spoolEvidence(db, evidence.RawEvent{
		EventUID: "evt:drain:1", SourceID: "s", SessionID: "dr", Raw: []byte("好行一"),
	}); err != nil {
		t.Fatal(err)
	}
	sessDir := filepath.Join(spoolDir(db), "dr")
	// 坏行 + 一条 stale .processing：坏行要计数，stale 要被认领。
	if err := os.WriteFile(filepath.Join(sessDir, "seg-bad.jsonl"), []byte("这不是 JSON\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := `{"v":1,"event_uid":"evt:drain:2","source_id":"s","occurred_at_ns":1,"session_id":"dr","event_type":"stop","raw":"` +
		"c3RhbGU=" + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessDir, "seg-stale.jsonl.processing"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	res := drainSpoolFailOpen(db, "-")
	if res.Drained != 2 || res.Dropped != 1 || res.Files != 3 || res.Restored != 0 {
		t.Fatalf("drain 计数不对: %+v", res)
	}
	if left, _ := filepath.Glob(filepath.Join(sessDir, "seg-*")); len(left) != 0 {
		t.Fatalf("成功后不应残留段: %v", left)
	}

	// 库不可用：段必须原样留着，不能删。
	if err := spoolEvidence(db, evidence.RawEvent{
		EventUID: "evt:drain:3", SourceID: "s", SessionID: "dr", Raw: []byte("留着"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(db); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(db, 0o755); err != nil { // 库路径变成目录 → 打不开
		t.Fatal(err)
	}
	drainSpoolFailOpen(db, "-")
	if left, _ := filepath.Glob(filepath.Join(sessDir, "seg-*.jsonl")); len(left) != 1 {
		t.Fatalf("打不开库时段必须保留，got %v", left)
	}
}

// 并发 append/drain 不丢行：段是原子落盘 + rename 认领，绝不读全文件重写。
func TestSpoolConcurrentAppendDrainKeepsEveryRow(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	const writers, perWriter = 4, 25
	sid := "conc"

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				ev := evidence.RawEvent{
					EventUID: "evt:conc:" + strconv.Itoa(w) + ":" + strconv.Itoa(i),
					SourceID: "hook:test:" + sid, SessionID: sid,
					EventType: "post-tool-use",
					Raw:       []byte("并发行 " + strconv.Itoa(w) + "-" + strconv.Itoa(i)),
				}
				if err := spoolEvidence(db, ev); err != nil {
					t.Errorf("spool 写失败: %v", err)
					return
				}
			}
		}(w)
	}

	// 两个 drain 进程同时抢；报错（锁冲突）就等下一轮，不放弃。
	var dg sync.WaitGroup
	stop := make(chan struct{})
	for d := 0; d < 2; d++ {
		dg.Add(1)
		go func() {
			defer dg.Done()
			e, err := evidence.Open(db)
			if err != nil {
				return
			}
			defer e.Close()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := e.DrainSpool(10); err != nil {
					time.Sleep(5 * time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	dg.Wait()

	// 收尾：把剩余段补完。
	e, err := evidence.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := e.DrainSpool(100)
		if err != nil {
			if time.Now().After(deadline) {
				t.Fatalf("收尾 drain 卡住: %v", err)
			}
			continue
		}
		if res.Drained == 0 && res.Files == 0 {
			break
		}
	}
	left, _ := filepath.Glob(filepath.Join(spoolDir(db), sid, "seg-*"))
	if len(left) != 0 {
		t.Fatalf("收尾后不应残留段: %v", left)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	obs, err := st.ListObservations(sid, writers*perWriter+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != writers*perWriter {
		t.Fatalf("并发 append/drain 丢行或重复：got %d want %d", len(obs), writers*perWriter)
	}
	seen := map[string]bool{}
	for _, o := range obs {
		if seen[o.Summary] {
			t.Fatalf("同一行被写了两次: %q", o.Summary)
		}
		seen[o.Summary] = true
	}
}
