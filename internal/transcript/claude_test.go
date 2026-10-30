package transcript

import (
	"strings"
	"testing"
)

func TestParseClaudeJSONLSkipsUnknownAndKeepsText(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"user","sessionId":"sess","timestamp":"2026-10-01T00:00:00Z","cwd":"/work/demo","message":{"role":"user","content":"决策：用 WAL"}}`,
		`{"type":"assistant","sessionId":"sess","message":{"role":"assistant","content":[{"type":"text","text":"报错时先看 schema"},{"type":"tool_use","name":"bash","input":{"cmd":"rm -rf /"}}]}}`,
		`not json at all`,
		`{"type":"queue-operation","content":"skip"}`,
		`{"type":"summary","summary":"这轮定了 WAL"}`,
		``,
	}, "\n")
	lines, err := ParseClaudeJSONL(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("应留下 3 行，got %+v", lines)
	}
	if lines[0].SessionID != "sess" || lines[0].Project != "demo" || lines[0].At == 0 {
		t.Fatalf("首行元数据不对: %+v", lines[0])
	}
	if !strings.Contains(lines[0].Text, "WAL") {
		t.Fatal(lines[0].Text)
	}
	if strings.Contains(lines[1].Text, "rm -rf") {
		t.Fatal("工具调用块不应进正文")
	}
	if lines[2].Role != "summary" || !strings.Contains(lines[2].Text, "WAL") {
		t.Fatalf("summary 行: %+v", lines[2])
	}
}

func TestJSONTimeUnixSecondsAndMillis(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"user","sessionId":"s","timestamp":1700000000,"message":{"role":"user","content":"秒"}}`,
		`{"type":"user","sessionId":"s","timestamp":1700000000000,"message":{"role":"user","content":"毫秒"}}`,
		`{"type":"user","sessionId":"s","timestamp":"not-a-time","message":{"role":"user","content":"坏时间"}}`,
	}, "\n")
	lines, err := ParseClaudeJSONL(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("got %+v", lines)
	}
	if lines[0].At != 1700000000 || lines[1].At != 1700000000 {
		t.Fatalf("时间戳解析不对: %d %d", lines[0].At, lines[1].At)
	}
	if lines[2].At != 0 {
		t.Fatalf("坏时间应为 0，got %d", lines[2].At)
	}
}
