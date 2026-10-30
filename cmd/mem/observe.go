package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

// 观测缓冲的写路径。hooks 只许到这里，然后进 observations 或 spool。
// 本文件没有任何 store.Add。

const (
	// spoolDrainMax / spoolDrainBudget 是 SessionStart 补写的上限。
	// 有界是为了不让一次启动把宿主会话拖住；剩下的留到下一次。
	spoolDrainMax          = 200
	spoolDrainBudget       = 200 * time.Millisecond
	observeSourceMeta      = `{"source":"hook-observe"}`
	observeSummaryMaxRunes = 400
)

var secretScrubbers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|authorization|passwd)\s*[:=]\s*\S+`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}`),
}

func isObserveHook(event harness.Event) bool {
	switch event {
	case harness.EventPostToolUse, harness.EventStop, harness.EventSessionEnd:
		return true
	}
	return false
}

func positiveEnvInt(key string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func scrubSecrets(s string) string {
	for _, re := range secretScrubbers {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

func compactRaw(raw json.RawMessage, max int) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		s = asString
	}
	s = scrubSecrets(s)
	return truncateRunes(strings.Join(strings.Fields(s), " "), max)
}

func buildObsSummary(event harness.Event, in hookInput) string {
	var b strings.Builder
	switch event {
	case harness.EventPostToolUse:
		name := strings.TrimSpace(in.ToolName)
		if name == "" {
			name = "tool"
		}
		b.WriteString(name)
		if s := compactRaw(in.ToolInput, 180); s != "" {
			b.WriteString(" ")
			b.WriteString(s)
		}
		if s := compactRaw(in.ToolOutput, 120); s != "" {
			b.WriteString(" → ")
			b.WriteString(s)
		}
	case harness.EventStop:
		b.WriteString("stop")
		if s := strings.TrimSpace(in.Status); s != "" {
			b.WriteString(" status=" + s)
		}
	case harness.EventSessionEnd:
		b.WriteString("session-end")
		if s := strings.TrimSpace(in.Reason); s != "" {
			b.WriteString(" reason=" + s)
		}
	default:
		b.WriteString(string(event))
	}
	return truncateRunes(scrubSecrets(b.String()), observeSummaryMaxRunes)
}

func appendObservationFailOpen(c common, harnessFlag string, event harness.Event, project string, in hookInput, auditPath string, started time.Time) {
	hname := harnessName(harnessFlag, in)
	obs := store.ObsInput{
		SessionID: in.sessionID(),
		Project:   project,
		Harness:   hname,
		Event:     string(event),
		ToolName:  strings.TrimSpace(in.ToolName),
		Summary:   buildObsSummary(event, in),
		MetaJSON:  observeSourceMeta,
	}
	if event != harness.EventPostToolUse {
		obs.RawRef = strings.TrimSpace(in.TranscriptPath)
	}

	createdID := ""
	st, err := store.Open(c.db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 打开记忆库失败，观测改写入 spool:", err)
		if spoolErr := appendSpool(c.db, obs); spoolErr != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: spool 也失败，已放过:", spoolErr)
		}
	} else {
		res, aerr := st.AppendObservation(obs)
		st.Close()
		if aerr != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: 写观测失败，改写入 spool:", aerr)
			if spoolErr := appendSpool(c.db, obs); spoolErr != nil {
				fmt.Fprintln(os.Stderr, "v2mem hook: spool 也失败，已放过:", spoolErr)
			}
		} else if res.Created {
			createdID = res.ID
		}
	}

	if auditPath == "-" {
		return
	}
	// 触发事件（post-tool-use / stop / session-end）始终留痕，方便 mem audit 按事件查看。
	// 观测次数只认下面的 obs-append：成功插入一行才写，避免与触发事件双计。
	rec := audit.Record{
		TS: started.Unix(), Event: string(event), Harness: hname,
		Project: project, SessionID: in.sessionID(),
		Source: "hook-observe", MS: time.Since(started).Milliseconds(),
	}
	_ = audit.Append(auditPath, rec)
	if createdID != "" {
		rec.Event = "obs-append"
		rec.Hashes = []string{createdID}
		_ = audit.Append(auditPath, rec)
	}
}

func writeHookStdout(harnessFlag string, event harness.Event, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	_ = event
	if !cursorHookOutput(harnessFlag) {
		fmt.Print(text)
		return
	}
	b, err := json.Marshal(map[string]string{"additional_context": text})
	if err != nil {
		fmt.Print(text)
		return
	}
	fmt.Printf("%s\n", b)
}

func cursorHookOutput(harnessFlag string) bool {
	h, err := harness.Get(harnessFlag)
	if err != nil {
		return false
	}
	return h.HookStyle == harness.HookStyleCursor
}

func resolveDBPath(db string) string {
	if strings.TrimSpace(db) == "" {
		return store.DefaultPath()
	}
	return db
}

func spoolDir(db string) string {
	return filepath.Join(filepath.Dir(resolveDBPath(db)), "spool")
}

func spoolFile(db, session string) string {
	name := strings.TrimSpace(session)
	if name == "" {
		name = "unknown"
	}
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', 0:
			return '_'
		default:
			return r
		}
	}, name)
	return filepath.Join(spoolDir(db), name+".jsonl")
}

func appendSpool(db string, obs store.ObsInput) error {
	path := spoolFile(db, obs.SessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(obs)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// drainSpoolFailOpen 把上次写库失败留下的观测补进 observations。
// 只在 SessionStart 调用。失败只打 stderr，不影响随后的注入。
func drainSpoolFailOpen(db, auditPath string) {
	dir := spoolDir(db)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	st, err := store.Open(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: spool drain 打开库失败，已跳过:", err)
		return
	}
	defer st.Close()

	deadline := time.Now().Add(spoolDrainBudget)
	left := spoolDrainMax
	for _, e := range entries {
		if left <= 0 || !time.Now().Before(deadline) {
			return
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		n, derr := drainOneSpool(st, filepath.Join(dir, e.Name()), auditPath, left, deadline)
		if derr != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: spool drain 失败，已跳过:", derr)
			return
		}
		left -= n
	}
}

func drainOneSpool(st *store.Store, path, auditPath string, left int, deadline time.Time) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var kept []string
	done := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if done >= left || !time.Now().Before(deadline) {
			kept = append(kept, line)
			continue
		}
		var in store.ObsInput
		if err := json.Unmarshal([]byte(line), &in); err != nil {
			continue // 坏行丢掉，不挡后面的
		}
		res, err := st.AppendObservation(in)
		if err != nil {
			kept = append(kept, line)
			continue
		}
		done++
		if res.Created && auditPath != "-" {
			_ = audit.Append(auditPath, audit.Record{
				TS: time.Now().Unix(), Event: "obs-append", Harness: in.Harness,
				Project: in.Project, SessionID: in.SessionID, Source: "hook-observe",
				Hashes: []string{res.ID}, Mode: "spool-drain",
			})
		}
	}
	if len(kept) == 0 {
		return done, os.Remove(path)
	}
	body := strings.Join(kept, "\n") + "\n"
	return done, os.WriteFile(path, []byte(body), 0o600)
}
