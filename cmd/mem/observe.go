package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/scrub"
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
	// spoolMaxBytes 是单个 spool 文件的上限。
	// 无上限时，库长期不可写会让 spool 无限增长，把磁盘写满 ——
	// 本来是「兜底」的机制反而成了新的故障源。
	spoolMaxBytes = 8 << 20
)

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

func scrubSecrets(s string) string { return scrub.Text(s) }

func compactRaw(raw json.RawMessage, max int, noScrub bool) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		s = asString
	}
	s = scrub.Apply(s, noScrub)
	return truncateRunes(strings.Join(strings.Fields(s), " "), max)
}

func buildObsSummary(event harness.Event, in hookInput, noScrub bool) string {
	var b strings.Builder
	switch event {
	case harness.EventPostToolUse:
		name := strings.TrimSpace(in.ToolName)
		if name == "" {
			name = "tool"
		}
		b.WriteString(name)
		if s := compactRaw(in.ToolInput, 180, noScrub); s != "" {
			b.WriteString(" ")
			b.WriteString(s)
		}
		if s := compactRaw(in.ToolOutput, 120, noScrub); s != "" {
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
	return truncateRunes(scrub.Apply(b.String(), noScrub), observeSummaryMaxRunes)
}

// observeEventID 取宿主事件号。显式 event_id 优先；PostToolUse 退到 tool_use_id。
// 没有稳定 id 时返回空，只靠 (session, content_hash) 去重。
func observeEventID(in hookInput, event harness.Event) string {
	if id := strings.TrimSpace(in.EventID); id != "" {
		return id
	}
	if event == harness.EventPostToolUse {
		if id := strings.TrimSpace(in.ToolUseID); id != "" {
			return "tool:" + in.sessionID() + ":" + id
		}
	}
	return ""
}

func observeChannel(event harness.Event, explicit string) string {
	ch, err := store.DefaultObsChannel(string(event), explicit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: channel 无效，改用默认:", err)
		ch, _ = store.DefaultObsChannel(string(event), "")
	}
	return ch
}

func appendObservationFailOpen(c common, harnessFlag string, event harness.Event, project string, in hookInput, auditPath string, started time.Time, noScrub bool) {
	hname := harnessName(harnessFlag, in)
	obs := store.ObsInput{
		SessionID: in.sessionID(),
		Project:   project,
		Harness:   hname,
		Event:     string(event),
		ToolName:  strings.TrimSpace(in.ToolName),
		Summary:   buildObsSummary(event, in, noScrub),
		MetaJSON:  observeSourceMeta,
		Channel:   observeChannel(event, in.Channel),
		EventID:   observeEventID(in, event),
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
	// 先看体量再写：超限就明确报错，让 hook 把「观测彻底丢了」打到 stderr，
	// 而不是默默继续追加到把盘写满。
	if fi, serr := f.Stat(); serr == nil && fi.Size() >= spoolMaxBytes {
		return fmt.Errorf("spool 文件已达上限 %d 字节，拒绝继续写: %s", spoolMaxBytes, path)
	}
	b, err := json.Marshal(obs)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	// fsync 而不是只 Write：spool 的全部意义就是「宿主崩溃后还能补写」。
	// 只写页缓存的话，进程被强杀/掉电时这条兜底记录一起消失，等于没兜住。
	// 代价是一次 fsync，但这条路径本来就只在写库失败时才走。
	return f.Sync()
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
		n, dropped, derr := drainOneSpool(st, filepath.Join(dir, e.Name()), auditPath, left, deadline)
		if dropped > 0 {
			// 坏行丢掉是对的（不挡后面的），但必须让人知道丢了——
			// 静默丢弃等于把「观测丢失」伪装成「本来就少」。
			fmt.Fprintf(os.Stderr, "v2mem hook: spool %s 丢弃 %d 行无法解析的数据\n", e.Name(), dropped)
		}
		if derr != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: spool drain 失败，已跳过:", derr)
			return
		}
		left -= n
	}
}

func drainOneSpool(st *store.Store, path, auditPath string, left int, deadline time.Time) (done, dropped int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var kept []string
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
			dropped++ // 坏行丢掉，不挡后面的；但要计数上报
			continue
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
		return done, dropped, os.Remove(path)
	}
	body := strings.Join(kept, "\n") + "\n"
	return done, dropped, os.WriteFile(path, []byte(body), 0o600)
}
