package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// 证据级事件类型。与钩子的 post-tool-use / stop / session-end 刻意分开：
// 前者描述“宿主钩子触发了什么”，后者描述“transcript 里原生有什么”。
const (
	EventUserPrompt    = "user-prompt"
	EventAssistantText = "assistant-text"
	EventToolUse       = "tool-use"
	EventToolResult    = "tool-result"
	EventSummary       = "summary"
	// EventUnparsed 是无法按 Claude 格式解析、但可能含真实用户/助手内容的行。
	// 它按原文归档、不可用于 Proposal；Codex/Qoder/QoderWork 等宿主的
	// transcript 会落到这一类，绝不能静默丢弃后还把游标推过去。
	EventUnparsed = "unparsed"
)

// metadataTypes 是宿主自己产生的控制行，明确不是用户内容，可解释地跳过。
// 新增宿主元数据类型时加到这里；**不要**把「看不懂的行」加进来。
var metadataTypes = map[string]bool{
	"queue-operation":       true,
	"file-history-snapshot": true,
	"progress":              true,
}

// Record 是 transcript 里的一条事件。
//
// 与 Line 的区别：Line 是“一行一段可入库文本”，丢弃了块粒度与身份；Record
// 保留原生行序（Start/End 是行在文件内的字节偏移，严格递增）与事件身份
// （HostUID 来自宿主 uuid / tool_use id），同一行里的多个内容块各成一条。
//
// Text 未截断、未打码：截断与打码属于写入策略，由调用方决定，解析层不做有损处理。
type Record struct {
	SessionID string
	Project   string
	Role      string
	EventType string
	// HostUID 是宿主给出的事件身份；为空表示宿主没给，由调用方合成。
	HostUID  string
	ToolID   string
	ToolName string
	Text     string
	At       int64
	// Start/End 是该行在输入流里的起止字节偏移（End 含换行）。
	Start int64
	End   int64
	// Block 是同一行内的内容块序号（0 起）。单行单事件时为 0。
	Block int
	// Unparsed 表示这行无法按宿主格式解析，Text 是原始行文本。
	// 调用方必须保留原文并禁止它进入 Proposal；不能当作普通事实引用。
	Unparsed bool
}

// Reader 逐行解析 Claude 风格的 transcript。
//
// 只消费**完整行**（以换行结束）：末尾半行（宿主正在写）留到下一次，
// 否则 Stop 恰好撞上写入时会把半条 JSON 吞掉。
type Reader struct {
	src  *bufio.Reader
	off  int64
	tail []byte
}

func NewReader(r io.Reader) *Reader { return NewReaderAt(r, 0) }

// NewReaderAt 从输入流的 start 偏移开始计数。调用方把文件 Seek 到该位置后必须
// 传同一个 start：Offset 返回的始终是**流内绝对偏移**，这样才能直接当增量游标。
func NewReaderAt(r io.Reader, start int64) *Reader {
	return &Reader{src: bufio.NewReaderSize(r, 64<<10), off: start}
}

// Offset 返回已消费的字节数。调用方据此推进增量游标。
func (r *Reader) Offset() int64 { return r.off }

// Read 返回下一行的记录。该行没有任何可归档事件时返回空切片与 nil。
// 读到末尾（含不完整尾行）返回 io.EOF。
func (r *Reader) Read() ([]Record, error) {
	var line []byte
	var err error
	if len(r.tail) > 0 {
		var rest []byte
		rest, err = r.src.ReadBytes('\n')
		line = append(r.tail, rest...)
		r.tail = nil
	} else {
		line, err = r.src.ReadBytes('\n')
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(line) == 0 {
		return nil, io.EOF
	}
	if !bytes.HasSuffix(line, []byte("\n")) {
		// 没有换行结尾：文件到这里就结束了（很多导出/夹具不带尾换行），
		// 或者是宿主正写到一半。能解析成 JSON 就是完整行，否则留到下一次。
		if errors.Is(err, io.EOF) && json.Valid(bytes.TrimSpace(line)) {
			start := r.off
			r.off += int64(len(line))
			return parseLine(line, start), nil
		}
		r.tail = line
		return nil, io.EOF
	}
	start := r.off
	r.off += int64(len(line))
	return parseLine(line, start), nil
}

// ReadAll 读到底，返回全部记录。仅用于整份导入；增量场景请用 Read。
func (r *Reader) ReadAll() ([]Record, error) {
	var out []Record
	for {
		recs, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, recs...)
	}
}

func parseLine(line []byte, start int64) []Record {
	text := bytes.TrimSpace(line)
	if len(text) == 0 {
		return nil
	}
	base := Record{Start: start, End: start + int64(len(line))}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(text, &raw); err != nil {
		return []Record{unparsedRecord(base, line)}
	}
	base.SessionID = jsonString(raw, "sessionId", "session_id")
	base.At = jsonTime(raw["timestamp"])
	if cwd := jsonString(raw, "cwd"); cwd != "" {
		base.Project = filepath.Base(cwd)
	}
	uuid := jsonString(raw, "uuid")
	lineType := jsonString(raw, "type")

	// 明确的宿主控制行才可解释地丢弃；其余「看不懂」的行必须留原文。
	// 之前这里一律返回 nil，cmd 层又把游标推到行末，导致非 Claude 格式
	// 的用户/助手原文永久丢失（计划 §7 停止条件第 1 条）。
	if metadataTypes[lineType] {
		return nil
	}

	msg := raw["message"]
	if len(msg) == 0 {
		if s := jsonString(raw, "summary"); strings.TrimSpace(s) != "" {
			r := base
			r.Role, r.EventType, r.HostUID, r.Text = "summary", EventSummary, uuid, s
			return []Record{r}
		}
		return []Record{unparsedRecord(base, line)}
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(msg, &message); err != nil {
		return []Record{unparsedRecord(base, line)}
	}
	base.Role = jsonString(message, "role")
	if base.Role == "" {
		base.Role = lineType
	}

	content := message["content"]
	if len(content) == 0 {
		return []Record{unparsedRecord(base, line)}
	}
	var whole string
	if err := json.Unmarshal(content, &whole); err == nil {
		if strings.TrimSpace(whole) == "" {
			return nil // 宿主明确写了空正文，可解释
		}
		r := base
		r.Text, r.HostUID = whole, uuid
		r.EventType = lineEventType(base.Role)
		return []Record{r}
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return []Record{unparsedRecord(base, line)}
	}
	var out []Record
	for i, rawBlock := range blocks {
		if rec, ok := parseBlock(base, rawBlock, i, uuid); ok {
			out = append(out, rec)
		}
	}
	// 合法 content 数组、但全是 thinking/空块：属于「模型中间态」的设计跳过，
	// 不造 unparsed，避免把宿主正常元数据当噪声归档。
	return out
}

// unparsedRecord 把看不懂的行整行保留下来，交给调用方按原文归档。
// 不在这里截断/打码/摘要，否则「原始字节」就丢了；写入策略仍由 cmd 层决定。
func unparsedRecord(base Record, line []byte) Record {
	r := base
	r.Role = "unknown"
	r.EventType = EventUnparsed
	r.Unparsed = true
	r.Text = strings.TrimRight(string(line), "\r\n")
	return r
}

func parseBlock(base Record, rawBlock json.RawMessage, idx int, uuid string) (Record, bool) {
	var b map[string]json.RawMessage
	if err := json.Unmarshal(rawBlock, &b); err != nil {
		return Record{}, false
	}
	r := base
	r.Block = idx
	switch jsonString(b, "type") {
	case "text":
		text := jsonString(b, "text")
		if strings.TrimSpace(text) == "" {
			return Record{}, false
		}
		r.Text, r.EventType, r.HostUID = text, lineEventType(base.Role), blockUID(uuid, idx)
	case "tool_use":
		id, name := jsonString(b, "id"), jsonString(b, "name")
		r.Text = "tool:" + name
		if input := strings.TrimSpace(string(b["input"])); input != "" && input != "null" {
			r.Text += "\n" + input
		}
		r.EventType, r.ToolID, r.ToolName = EventToolUse, id, name
		r.HostUID = blockUID(uuid, idx)
		if id != "" {
			r.HostUID = "tool_use:" + id
		}
	case "tool_result":
		id := jsonString(b, "tool_use_id")
		r.Text, r.EventType, r.ToolID = "tool_result", EventToolResult, id
		if out := strings.TrimSpace(contentText(b["content"])); out != "" {
			r.Text += "\n" + out
		}
		r.HostUID = blockUID(uuid, idx)
		if id != "" {
			r.HostUID = "tool_result:" + id
		}
	default:
		// thinking 等块不进证据：它们是模型的中间态，不是可引用的事实。
		return Record{}, false
	}
	return r, true
}

func lineEventType(role string) string {
	switch role {
	case "user":
		return EventUserPrompt
	case "assistant":
		return EventAssistantText
	}
	return strings.TrimSpace(role)
}

// blockUID 给“有 uuid 的行”里的块一个稳定身份；没有 uuid 时返回空，
// 由调用方按文件偏移合成。
func blockUID(uuid string, idx int) string {
	if strings.TrimSpace(uuid) == "" {
		return ""
	}
	return uuid + "#" + itoa(idx)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ParseTime 把宿主时间戳转成纳秒时间基准（unix 秒）；0 表示无法解析。
// 供调用方把 Record.At 换成 evidence 需要的 OccurredAtNS。
func (r Record) OccurredAtNS() int64 {
	if r.At <= 0 {
		return 0
	}
	return time.Unix(r.At, 0).UnixNano()
}
