// Package transcript 把外部会话 JSONL 收成可写入观测的行。
//
// 未知行跳过。解析失败不让整份文件作废。
package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// Line 是从 transcript 抽出的一句有界文本。
type Line struct {
	SessionID string
	Role      string
	Text      string
	At        int64
	Project   string
}

// ParseClaudeJSONL 读 Claude Code 风格的 JSONL。
// 坏行跳过。只有读取本身失败才返回错误。
func ParseClaudeJSONL(r io.Reader) ([]Line, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []Line
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			continue
		}
		line, ok := claudeLine(raw)
		if !ok {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

func claudeLine(raw map[string]json.RawMessage) (Line, bool) {
	var line Line
	line.SessionID = jsonString(raw, "sessionId", "session_id")
	line.At = jsonTime(raw["timestamp"])
	if cwd := jsonString(raw, "cwd"); cwd != "" {
		line.Project = filepath.Base(cwd)
	}
	msg := raw["message"]
	if len(msg) == 0 {
		if s := jsonString(raw, "summary"); s != "" {
			line.Role = "summary"
			line.Text = s
			return line, true
		}
		return Line{}, false
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(msg, &message); err != nil {
		return Line{}, false
	}
	line.Role = jsonString(message, "role")
	if line.Role == "" {
		line.Role = jsonString(raw, "type")
	}
	text := contentText(message["content"])
	if strings.TrimSpace(text) == "" {
		return Line{}, false
	}
	line.Text = text
	if line.SessionID == "" {
		line.SessionID = "import"
	}
	return line, true
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		if typ != "" && typ != "text" {
			continue
		}
		if text, ok := b["text"].(string); ok && strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func jsonString(raw map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return s
		}
	}
	return ""
}

func jsonTime(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Unix()
		}
		return 0
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		if n > 1e12 {
			return int64(n / 1000)
		}
		return int64(n)
	}
	return 0
}
