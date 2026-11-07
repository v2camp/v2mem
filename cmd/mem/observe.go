package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/scrub"
	"github.com/v2camp/v2mem/internal/store"
	"github.com/v2camp/v2mem/internal/transcript"
)

// 观测缓冲的写路径（T1.2）。hooks 只许到这里，然后进 evidence（或 spool）。
// 本文件没有任何 store.Add。

const (
	// spoolDrainMax 是 SessionStart 补写的上限。
	// 有界是为了不让一次启动把宿主会话拖住；剩下的留到下一次。
	spoolDrainMax = 200
	// observeRawMaxRunes 是单条原始证据的正文上限。它只防「一条事件写爆磁盘」，
	// 不承担摘要职责：长工具输出在这里必须保住（旧实现截到 120 字）。
	observeRawMaxRunes = 32000
	// evidenceCaptureAttempts 是并发写者抢到同一序号时的重试次数。
	evidenceCaptureAttempts = 5
	// transcriptStopMaxRecords 是单次 Stop 最多补写的转录事件数。
	// 只是防病态大文件把钩子拖住；正常一轮增量远小于它。
	transcriptStopMaxRecords = 5000
	// transcriptToolMemory 是游标里保留的 tool_use id → 工具名条数，
	// 供跨次识别 tool_result 属于哪个工具（决定 ProposalEligible）。
	transcriptToolMemory = 128
)

// spoolMaxSegments 是单个会话的 spool 段数上限。旧实现按单文件体量设限；
// 段式 spool 的风险变成「文件数无限增长」，所以按段数守。
// 变量而非常量只为让测试能把上限压到可测规模。
var spoolMaxSegments = 4096

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

// rawField 取一个原始 JSON 字段的完整正文：折叠只在展示层做，这里保结构。
func rawField(raw json.RawMessage, noScrub bool) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		s = asString
	}
	return cutRunes(scrub.Apply(s, noScrub), observeRawMaxRunes)
}

// cutRunes 按 rune 硬截断，不加省略号 —— 用于证据正文，不用于展示摘要。
func cutRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

// observationText 是一条观测的完整正文（已打码、未按摘要截断）及其保真度。
//
// 归档层把它原样写进 Raw：摘要只是它的有界投影，证据不能被摘要替代。
// Fidelity 按「是否存在真实正文」标注，只控提案准入，不参与证据等级判定。
// 拿不到内容时返回空正文 + FidelityNone，绝不回退成 "stop" / "session-end"
// 这类事件名字面量——事件名不是内容（design-capture §0/§4.3）。
//
// hname 是可选的宿主名：生产路径由 appendObservationFailOpen 传入，字段来源
// 按 adapterFor(hname) 查表；不传时（既有直接调用）走保守默认，保持重构前行为。
// 主体 Fidelity 判定分支对所有宿主一致、不随表变化 —— 新增宿主只加表项。
func observationText(event harness.Event, in hookInput, noScrub bool, hname ...string) (string, evidence.Fidelity) {
	host := ""
	if len(hname) > 0 {
		host = hname[0]
	}
	cfg := adapterFor(host)

	var b strings.Builder
	fidelity := evidence.FidelityNone
	switch event {
	case harness.EventPostToolUse:
		name := strings.TrimSpace(in.ToolName)
		if name == "" {
			name = "tool"
		}
		b.WriteString("tool:" + name)
		toolIn := rawField(adapterRawField(cfg.toolInput, in.ToolInput, in), noScrub)
		toolOut := rawField(adapterRawField(cfg.toolOutput, in.ToolOutput, in), noScrub)
		if toolIn != "" {
			b.WriteString("\n" + toolIn)
		}
		if toolOut != "" {
			b.WriteString("\n→ " + toolOut)
		}
		if toolIn != "" || toolOut != "" {
			fidelity = evidence.FidelityFull
		} else {
			// 只有工具名这行摘要，没有真实正文。
			fidelity = evidence.FidelitySummary
		}
	case harness.EventStop, harness.EventSessionEnd:
		// 按表里的优先级列表取最终回复：命中第一个非空字段即作为真实正文。
		for _, sel := range cfg.stopReply {
			if v := strings.TrimSpace(sel(in)); v != "" {
				b.WriteString(v)
				fidelity = evidence.FidelityFull
				break
			}
		}
		// 没拿到正文时才考虑 status / reason 作结构化摘要，是否允许由表决定。
		if fidelity == evidence.FidelityNone && cfg.allowMetaSummary {
			var meta []string
			if s := strings.TrimSpace(in.Status); s != "" {
				meta = append(meta, "status="+s)
			}
			if s := strings.TrimSpace(in.Reason); s != "" {
				meta = append(meta, "reason="+s)
			}
			if len(meta) > 0 {
				b.WriteString(strings.Join(meta, " "))
				fidelity = evidence.FidelitySummary
			}
		}
		// 否则 b 为空、fidelity 保持 None：不伪造事件名字面量。
	default:
		b.WriteString(string(event))
	}
	return truncateRunes(scrub.Apply(b.String(), noScrub), observeRawMaxRunes), fidelity
}

func observeSourceID(hname, sessionID string) string {
	return "hook:" + orUnknown(hname, "unknown") + ":" + orUnknown(sessionID, "unknown")
}

func transcriptSourceID(hname, sessionID string) string {
	return "transcript:" + orUnknown(hname, "unknown") + ":" + orUnknown(sessionID, "unknown")
}

func orUnknown(s, fallback string) string {
	if v := strings.TrimSpace(s); v != "" {
		return v
	}
	return fallback
}

// observeHostUID 用宿主事件号做身份。没有就返回空，交给 capture 合成。
func observeHostUID(in hookInput, event harness.Event, sourceID string) string {
	if id := strings.TrimSpace(in.EventID); id != "" {
		return sourceID + ":event:" + id
	}
	if event == harness.EventPostToolUse {
		if id := strings.TrimSpace(in.ToolUseID); id != "" {
			return sourceID + ":tool:" + id
		}
	}
	return ""
}

// synthEvidenceUID 合成身份：source + generation + seq + 原始正文 hash。
//
// 只有宿主没给 id 时才走这里。它保证「同一位置重放 = 同一身份」，
// 而不同位置的两条同摘要事件（seq 不同）不会互相吞并。
func synthEvidenceUID(sourceID string, gen, seq int, raw []byte) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\x00%d\x00%d\x00", sourceID, gen, seq)
	sum.Write(raw)
	return "evt:" + hex.EncodeToString(sum.Sum(nil))[:32]
}

// evidenceSession 把 Store 与 evidence.Engine 绑在一起。
// Engine 的归档目录与 spool 根都落在库文件旁，不会污染别处。
type evidenceSession struct {
	engine *evidence.Engine
	store  *store.Store
}

func openEvidenceSession(db string) (*evidenceSession, error) {
	st, err := store.Open(db)
	if err != nil {
		return nil, err
	}
	return &evidenceSession{engine: evidence.New(st), store: st}, nil
}

func (s *evidenceSession) Close() error { return s.engine.Close() }

// captureEvidence 写入一条证据，返回引用与「是否新建」。
//
// 身份不全时先读证据源水位再合成（source+generation+seq+raw hash）；
// 并发写者抢到同一序号时 store 会返回 ErrEvidenceSeqGap，重取水位重试。
// DB 写失败时 evidence.Engine 自己会保留归档并落 spool segment。
func captureEvidence(s *evidenceSession, ev evidence.RawEvent) (evidence.Ref, bool, error) {
	hostUID := ev.EventUID
	var lastErr error
	for attempt := 0; attempt < evidenceCaptureAttempts; attempt++ {
		if ev.Generation <= 0 || ev.Seq <= 0 {
			head, _, err := s.store.EvidenceSourceHead(ev.SourceID)
			if err != nil {
				return evidence.Ref{}, false, fmt.Errorf("读证据源水位失败: %w", err)
			}
			ev.Generation = head.Generation
			ev.Seq = head.LastSeq + 1
			if hostUID == "" {
				ev.EventUID = synthEvidenceUID(ev.SourceID, ev.Generation, ev.Seq, ev.Raw)
			}
		}
		_, existed, err := s.store.LookupEvidence(ev.EventUID, ev.SourceID, ev.Generation, ev.Seq)
		if err != nil && !isTransientDBError(err) {
			return evidence.Ref{}, false, err
		}
		ref, cerr := s.engine.Capture(ev)
		if cerr == nil {
			return ref, !existed, nil
		}
		lastErr = cerr
		if !errors.Is(cerr, store.ErrEvidenceSeqGap) && !isTransientDBError(cerr) {
			return evidence.Ref{}, false, cerr
		}
		ev.Generation, ev.Seq = 0, 0
		ev.EventUID = hostUID
		time.Sleep(time.Duration(attempt+1) * 2 * time.Millisecond)
	}
	return evidence.Ref{}, false, fmt.Errorf("证据写入重试 %d 次仍失败: %w", evidenceCaptureAttempts, lastErr)
}

func isTransientDBError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "busy")
}

func appendObservationFailOpen(c common, harnessFlag string, event harness.Event, project string, in hookInput, auditPath string, started time.Time, noScrub bool) {
	hname := harnessName(harnessFlag, in)
	sid := in.sessionID()
	source := observeSourceID(hname, sid)
	text, fidelity := observationText(event, in, noScrub, hname)
	ev := evidence.RawEvent{
		SourceID:     source,
		SessionID:    sid,
		Project:      project,
		Harness:      hname,
		EventType:    string(event),
		OccurredAtNS: started.UnixNano(),
		// 工具输出是 artifact，不是事实陈述；事实来自用户提问与助手回复。
		// 所以 PostToolUse 一律不进提案（不再按工具名区分）——R9 巡检里 20 条
		// pending 候选都是 transcript 原始 JSON，根因就是这里放行了普通工具载荷。
		ProposalEligible: event != harness.EventPostToolUse,
		EventUID:         observeHostUID(in, event, source),
		Fidelity:         fidelity,
		Raw:              []byte(text),
	}

	// Stop 仍是落盘标记；正文（用户/助手/工具事件）从 transcript 增量补在其前面，
	// 这样 Bundle 的序号顺序与真实时间顺序一致：… → 最后一条助手结论 → stop。
	if event == harness.EventStop {
		captureTranscriptFailOpen(c.db, hname, project, in, noScrub)
	}

	createdUID := ""
	es, err := openEvidenceSession(c.db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 打开记忆库失败，观测改写入 spool:", err)
		ev.EventUID = orSpoolUID(ev)
		if spoolErr := spoolEvidence(c.db, ev); spoolErr != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: spool 也失败，已放过:", spoolErr)
		}
	} else {
		ref, created, cerr := captureEvidence(es, ev)
		es.Close()
		if cerr != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: 写观测失败，引擎已保留归档/spool:", cerr)
		} else if created {
			createdUID = ref.EventUID
		}
	}

	if auditPath == "-" {
		return
	}
	// 触发事件（post-tool-use / stop / session-end）始终留痕，方便 mem audit 按事件查看。
	// 观测次数只认下面的 obs-append：成功插入一行才写，避免与触发事件双计。
	rec := audit.Record{
		TS: started.Unix(), Event: string(event), Harness: hname,
		Project: project, SessionID: sid,
		Source: "hook-observe", MS: time.Since(started).Milliseconds(),
	}
	_ = audit.Append(auditPath, rec)
	if createdUID != "" {
		rec.Event = "obs-append"
		rec.Hashes = []string{createdUID}
		_ = audit.Append(auditPath, rec)
	}
}

// captureTranscriptFailOpen 从 transcript 增量解析用户/助手/工具事件。
//
// 增量靠游标（库旁 evidence/transcripts/<session>.cursor）；游标丢失或文件被截断
// 时从头再读也不会重复写：身份要么来自宿主 uuid，要么由「文件路径+偏移+块序号」
// 合成，重放稳定命中已有证据。
func captureTranscriptFailOpen(db, hname, project string, in hookInput, noScrub bool) {
	path := strings.TrimSpace(in.TranscriptPath)
	sid := in.sessionID()
	if path == "" || sid == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: transcript 路径无效，已跳过:", err)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		fmt.Fprintln(os.Stderr, "v2mem hook: transcript 不可读，已跳过:", abs)
		return
	}

	cursorPath := transcriptCursorPath(db, sid)
	cur := readTranscriptCursor(cursorPath)
	offset := cur.Offset
	tools := cur.toolMap()
	reset := false
	if cur.Path != abs || offset > fi.Size() {
		// 换了文件或被截断：从头读，旧映射作废。
		offset, tools, reset = 0, map[string]string{}, true
	}
	if offset >= fi.Size() {
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: transcript 打开失败，已跳过:", err)
		return
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: transcript 定位失败，已跳过:", err)
		return
	}

	es, err := openEvidenceSession(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: 打开记忆库失败，transcript 已跳过:", err)
		return
	}
	defer es.Close()

	source := transcriptSourceID(hname, sid)
	rd := transcript.NewReaderAt(f, offset)
	lastGood := offset
	seen := 0
	unparsed := 0
	for seen < transcriptStopMaxRecords {
		recs, rerr := rd.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "v2mem hook: transcript 读取失败，已停在此行:", rerr)
			break
		}
		if len(recs) == 0 {
			lastGood = rd.Offset()
			continue
		}
		failed := false
		for _, rec := range recs {
			if rec.Unparsed {
				unparsed++
			}
			ev := recordEvidence(source, sid, project, hname, abs, rec, tools, noScrub)
			if _, _, cerr := captureEvidence(es, ev); cerr != nil {
				fmt.Fprintln(os.Stderr, "v2mem hook: transcript 证据写入失败，已停在此行:", cerr)
				failed = true
				break
			}
			seen++
		}
		if failed {
			break
		}
		tools = rememberTools(tools, recs)
		lastGood = rd.Offset()
	}

	if reset || lastGood != cur.Offset {
		writeTranscriptCursor(cursorPath, transcriptCursor{
			Path: abs, Offset: lastGood, Tools: toolPairs(tools),
		})
	}
	// 非 Claude 格式的行已按原文归档，但语义没解析出来：明确告知，
	// 让「跳过」变成可发现、可排查的事件，而不是静默丢字节。
	if unparsed > 0 {
		fmt.Fprintf(os.Stderr,
			"v2mem hook: transcript 有 %d 行非本解析器格式，已按原文归档且不可用于提案\n", unparsed)
	}
}

// recordEvidence 把一条 transcript 事件翻成待归档证据。
func recordEvidence(source, sid, project, hname, path string, rec transcript.Record, tools map[string]string, noScrub bool) evidence.RawEvent {
	name := strings.TrimSpace(rec.ToolName)
	if name == "" && rec.ToolID != "" {
		name = tools[rec.ToolID]
	}
	text := rec.Text
	if rec.EventType == transcript.EventToolResult && name != "" {
		text = "tool_result:" + name + strings.TrimPrefix(text, "tool_result")
	}
	text = truncateRunes(scrub.Apply(text, noScrub), observeRawMaxRunes)

	uid := ""
	if rec.HostUID != "" {
		uid = "tx:" + rec.HostUID
	} else {
		// 身份用**打码前**的正文：切 --no-scrub 不该让整份历史换身份。
		uid = transcriptFallbackUID(path, rec, rec.Text)
	}
	// unparsed 行只保住原文、语义未解析：标 pointer 且不进 Proposal；
	// 正常解析的 transcript 行带真实正文，标 full。
	fidelity := evidence.FidelityFull
	if rec.Unparsed {
		fidelity = evidence.FidelityPointer
	}
	// 工具输入/输出是 artifact，不是事实陈述；事实只来自用户提问与助手回复。
	// tool-use / tool-result 行一律不进提案（R9 残留的 transcript 原始 JSON
	// 就是这两类行，根因在准入而不在蒸馏）；unparsed 行语义未解析，同样不进。
	eligible := rec.EventType != transcript.EventToolUse &&
		rec.EventType != transcript.EventToolResult && !rec.Unparsed
	return evidence.RawEvent{
		EventUID:         uid,
		SourceID:         source,
		SessionID:        sid,
		Project:          project,
		Harness:          hname,
		EventType:        rec.EventType,
		OccurredAtNS:     rec.OccurredAtNS(),
		ProposalEligible: eligible,
		Fidelity:         fidelity,
		Raw:              []byte(text),
	}
}

// transcriptFallbackUID 给没有宿主 id 的记录一个稳定身份。
//
// 用「文件路径 + 行偏移 + 块序号 + 正文 hash」：同一行重放必然同 id，
// 追加写不会让已归档的行换身份（行偏移不变）。
func transcriptFallbackUID(path string, rec transcript.Record, text string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00", path, rec.Start, rec.Block)
	h.Write([]byte(text))
	return "tx:" + hex.EncodeToString(h.Sum(nil))[:32]
}

// transcriptCursor 是增量解析位置。Tools 保留最近的 tool_use id → 工具名，
// 让下一次运行也能判断 tool_result 是不是管道工具的产物。
type transcriptCursor struct {
	Path   string           `json:"path"`
	Offset int64            `json:"offset"`
	Tools  []transcriptTool `json:"tools,omitempty"`
}

type transcriptTool struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c transcriptCursor) toolMap() map[string]string {
	out := make(map[string]string, len(c.Tools))
	for _, t := range c.Tools {
		if t.ID != "" && t.Name != "" {
			out[t.ID] = t.Name
		}
	}
	return out
}

func toolPairs(tools map[string]string) []transcriptTool {
	if len(tools) == 0 {
		return nil
	}
	ids := make([]string, 0, len(tools))
	for id := range tools {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]transcriptTool, 0, len(ids))
	for _, id := range ids {
		out = append(out, transcriptTool{ID: id, Name: tools[id]})
	}
	if len(out) > transcriptToolMemory {
		out = out[len(out)-transcriptToolMemory:]
	}
	return out
}

func rememberTools(tools map[string]string, recs []transcript.Record) map[string]string {
	for _, rec := range recs {
		if rec.EventType == transcript.EventToolUse && rec.ToolID != "" && rec.ToolName != "" {
			tools[rec.ToolID] = rec.ToolName
		}
	}
	if len(tools) <= transcriptToolMemory {
		return tools
	}
	ids := make([]string, 0, len(tools))
	for id := range tools {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids[:len(ids)-transcriptToolMemory] {
		delete(tools, id)
	}
	return tools
}

func transcriptCursorPath(db, session string) string {
	dir := filepath.Join(filepath.Dir(resolveDBPath(db)), "evidence", "transcripts")
	return filepath.Join(dir, safeName(session)+".cursor")
}

func readTranscriptCursor(path string) transcriptCursor {
	var cur transcriptCursor
	b, err := os.ReadFile(path)
	if err != nil {
		return transcriptCursor{}
	}
	if err := json.Unmarshal(b, &cur); err != nil {
		// 坏游标按「没读过」处理：重放由证据身份兜住，不会重复。
		return transcriptCursor{}
	}
	return cur
}

func writeTranscriptCursor(path string, cur transcriptCursor) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: transcript 游标目录不可用，已跳过:", err)
		return
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: transcript 游标写入失败，已跳过:", err)
		return
	}
	// rename 保证读侧永远看不到半个游标；游标丢失最多让下次重放（幂等）。
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		fmt.Fprintln(os.Stderr, "v2mem hook: transcript 游标提交失败，已跳过:", err)
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

// spoolDir 与 evidence.Engine 的 SpoolRoot 同址：<db-dir>/spool。
func spoolDir(db string) string {
	return filepath.Join(filepath.Dir(resolveDBPath(db)), "spool")
}

func safeName(session string) string {
	name := strings.TrimSpace(session)
	if name == "" {
		return "unknown"
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/', r == '\\', r == ':', r == 0:
			return '_'
		case r < 0x20:
			return '_'
		default:
			return r
		}
	}, name)
	name = strings.Trim(name, ".")
	if name == "" {
		return "unknown"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}

// spoolRecord 逐字段镜像 internal/evidence 的 segmentRecord。
//
// 这里是**契约复制**，不是自由格式：drain 由 evidence.Engine 读，字段对不上就等于
// 丢证据。TestSpoolSegmentContractDrainsViaEngine 用真实 drain 锁住这份镜像。
type spoolRecord struct {
	Version          int    `json:"v"`
	EventUID         string `json:"event_uid"`
	SourceID         string `json:"source_id"`
	Generation       int    `json:"generation"`
	Seq              int    `json:"seq"`
	OccurredAtNS     int64  `json:"occurred_at_ns"`
	SessionID        string `json:"session_id,omitempty"`
	Project          string `json:"project,omitempty"`
	Harness          string `json:"harness,omitempty"`
	EventType        string `json:"event_type,omitempty"`
	ParserVersion    string `json:"parser_version,omitempty"`
	ProposalEligible bool   `json:"proposal_eligible,omitempty"`
	Raw              []byte `json:"raw"`
}

// spoolEvidence 在库根本打不开时兜底：每条事件一个独立段，
// 路径与 evidence.Engine 的契约一致（<db-dir>/spool/<session>/seg-*.jsonl），
// 所以库恢复后 DrainSpool 能直接补写。
func spoolEvidence(db string, ev evidence.RawEvent) error {
	dir := filepath.Join(spoolDir(db), safeName(ev.SessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 段式 spool 的单点风险从「单文件过大」变成「文件数无限」，按段数守上限。
	if n, err := countSpoolSegments(dir); err != nil {
		return err
	} else if n >= spoolMaxSegments {
		return fmt.Errorf("spool 段数已达上限 %d，拒绝继续写: %s", spoolMaxSegments, dir)
	}
	if ev.ParserVersion == "" {
		ev.ParserVersion = evidence.ParserVersion
	}
	body, err := json.Marshal(spoolRecord{
		Version: 1, EventUID: ev.EventUID, SourceID: ev.SourceID,
		Generation: ev.Generation, Seq: ev.Seq, OccurredAtNS: ev.OccurredAtNS,
		SessionID: ev.SessionID, Project: ev.Project, Harness: ev.Harness,
		EventType: ev.EventType, ParserVersion: ev.ParserVersion,
		ProposalEligible: ev.ProposalEligible, Raw: ev.Raw,
	})
	if err != nil {
		return err
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(dir, "seg-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(body); err != nil {
		cleanup()
		return err
	}
	// spool 的全部意义是宿主崩溃/库不可用后还能补写，必须 fsync 而不是只写页缓存。
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	final := strings.TrimSuffix(tmpName, ".tmp") + ".jsonl"
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return syncDirectory(dir)
}

// orSpoolUID 在库不可用、读不到 generation/seq 时给事件一个身份。
//
// 正常路径的身份是 source+generation+seq+raw hash；这里没有库水位可用，
// 退化成 source+raw hash+发生时刻，保证每次真实触发各成一条，不被互相吞并。
func orSpoolUID(ev evidence.RawEvent) string {
	if ev.EventUID != "" {
		return ev.EventUID
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\x00%d\x00", ev.SourceID, ev.OccurredAtNS)
	sum.Write(ev.Raw)
	return "evt:spool:" + hex.EncodeToString(sum.Sum(nil))[:32]
}

func countSpoolSegments(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "seg-") &&
			(strings.HasSuffix(e.Name(), ".jsonl") || strings.HasSuffix(e.Name(), ".jsonl.processing")) {
			n++
		}
	}
	return n, nil
}

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// drainSpoolFailOpen 把库不可用期间落下的段补进证据账。
// 只在 SessionStart 调用。认领/恢复/删除全部交给 evidence.Engine：
// 它按 .processing 原子认领，成功删除、失败恢复，从不重写整个文件。
func drainSpoolFailOpen(db, auditPath string) evidence.SpoolDrainResult {
	e, err := evidence.Open(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: spool drain 打开库失败，已跳过:", err)
		return evidence.SpoolDrainResult{}
	}
	defer e.Close()

	res, derr := e.DrainSpool(spoolDrainMax)
	if derr != nil {
		fmt.Fprintln(os.Stderr, "v2mem hook: spool drain 未全部完成，下次继续:", derr)
	}
	if res.Dropped > 0 {
		// 坏行丢掉是对的（不挡后面的），但必须让人知道丢了。
		fmt.Fprintf(os.Stderr, "v2mem hook: spool 丢弃 %d 行无法解析的数据\n", res.Dropped)
	}
	if auditPath != "-" && (res.Drained > 0 || res.Restored > 0 || res.Dropped > 0) {
		_ = audit.Append(auditPath, audit.Record{
			TS: time.Now().Unix(), Event: "obs-append", Source: "hook-observe",
			Mode: "spool-drain", Count: res.Drained, Failed: res.Restored + res.Dropped,
		})
	}
	return res
}
