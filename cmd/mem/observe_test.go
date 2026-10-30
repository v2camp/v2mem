package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/store"
)

func hookJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}

func TestCmdHookObserveWritesBufferNotMemories(t *testing.T) {
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
		if !strings.Contains(o.MetaJSON, "hook-observe") {
			t.Errorf("观测 source 应为 hook-observe，got %s", o.MetaJSON)
		}
		switch o.Event {
		case "post-tool-use":
			sawPost = true
			if strings.Contains(o.Summary, "supersecret") {
				t.Errorf("摘要应抹掉密钥，got %s", o.Summary)
			}
			if strings.Contains(o.Summary, "[redacted]") {
				sawRedacted = true
			}
		case "stop":
			sawStop = true
		case "session-end":
			sawEnd = true
		}
	}
	if !sawPost || !sawStop || !sawEnd || !sawRedacted {
		t.Fatalf("观测事件不全 post=%v stop=%v end=%v redacted=%v", sawPost, sawStop, sawEnd, sawRedacted)
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

func TestCmdHookStopDedupesBySessionContent(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	in := hookJSON(t, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": "same", "status": "completed",
	})
	for i := 0; i < 2; i++ {
		if _, err := withStdin(t, in, func() error {
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
	if len(obs) != 1 {
		t.Fatalf("同一会话相同 Stop 应只留 1 条，got %d", len(obs))
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

func TestCmdHookSpoolDrainsOnSessionStart(t *testing.T) {
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
	matches, err := filepath.Glob(filepath.Join(dir, "spool", "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("应落下 spool 文件，got %v err %v", matches, err)
	}
	fi, err := os.Stat(matches[0])
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("spool 文件权限应为 0600，got %o", fi.Mode().Perm())
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
