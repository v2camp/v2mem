package transcript

import (
	"strings"
	"testing"
)

// 活库 2026-11-23 实证：393 条 unparsed 里 250 条是宿主的工具事件
// （function_call 133 / function_call_result 117），85 条是 reasoning，
// 23 条是 session-meta / ai-title。它们都不是「看不懂的行」，
// 而是「本解析器没认出来的原生事件」。
//
// 教训：unparsed 一旦成为垃圾桶，它就不再是「解析失败」的信号。
const hostEventTranscript = `{"type":"function_call","id":"01a1d32c-0959","parentId":"01a1d32b","timestamp":"1794544240985","callId":"call_00_s3XjFsvoMYxmQpXHcubL7037","name":"Bash","arguments":"{\"command\": \"git status\"}","sessionId":"s1","cwd":"/work/demo"}
{"type":"function_call_result","id":"01a1d32c-0dde","parentId":"01a1d32c-0959","timestamp":"1794544242142","name":"Bash","callId":"call_00_s3XjFsvoMYxmQpXHcubL7037","status":"completed","output":{"type":"text","text":"Stdout: on branch main"},"sessionId":"s1","cwd":"/work/demo"}
{"type":"reasoning","id":"01a1d32b-fd3a","timestamp":"1794544240925","content":"[]","rawContent":[{"type":"reasoning_text","text":"用户想让我拉代码"}],"sessionId":"s1","cwd":"/work/demo"}
{"type":"session-meta","id":"01a1d32b-e68f","timestamp":"1794544232079","sessionId":"s1","meta":{"codebuddy.ai/hostKind":"unopted"}}
{"type":"ai-title","id":"1bdf7f9a-22ef","timestamp":"1794544239229","aiTitle":"拉取代码并本地构建","sessionId":"s1","cwd":"/work/demo"}
not json at all
`

func TestHostToolEventsParseAsToolEventsNotUnparsed(t *testing.T) {
	recs, err := NewReader(strings.NewReader(hostEventTranscript)).ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	var use, result *Record
	for i := range recs {
		switch recs[i].EventType {
		case EventToolUse:
			use = &recs[i]
		case EventToolResult:
			result = &recs[i]
		}
	}
	if use == nil {
		t.Fatalf("function_call 应解析为 %s，实际事件序列: %v", EventToolUse, eventTypes(recs))
	}
	if use.Unparsed {
		t.Fatal("function_call 不是「看不懂的行」，不该标 Unparsed")
	}
	if use.ToolName != "Bash" {
		t.Errorf("ToolName = %q，期望 Bash", use.ToolName)
	}
	if use.ToolID != "call_00_s3XjFsvoMYxmQpXHcubL7037" {
		t.Errorf("ToolID = %q，期望宿主的 callId", use.ToolID)
	}
	if use.HostUID != "tool_use:call_00_s3XjFsvoMYxmQpXHcubL7037" {
		t.Errorf("HostUID = %q，期望 tool_use:<callId>（与 Claude 块同形，重放才能幂等）", use.HostUID)
	}
	if !strings.Contains(use.Text, "tool:Bash") || !strings.Contains(use.Text, "git status") {
		t.Errorf("Text 应含工具名与实参，实际 %q", use.Text)
	}

	if result == nil {
		t.Fatalf("function_call_result 应解析为 %s，实际事件序列: %v", EventToolResult, eventTypes(recs))
	}
	if result.Unparsed {
		t.Fatal("function_call_result 不是「看不懂的行」，不该标 Unparsed")
	}
	if result.ToolID != use.ToolID {
		t.Errorf("工具结果必须与被调用的 ToolID 对上: result=%q use=%q", result.ToolID, use.ToolID)
	}
	if !strings.HasPrefix(result.Text, "tool_result") {
		t.Errorf("Text 应以 tool_result 开头（与 Claude 块同形），实际 %q", result.Text)
	}
	if !strings.Contains(result.Text, "on branch main") {
		t.Errorf("Text 应含工具输出正文，实际 %q", result.Text)
	}
}

// reasoning 是模型中间态、session-meta/ai-title 是宿主元数据：
// 与 thinking 块同处理——可解释地跳过，不占 unparsed 名额。
func TestHostIntermediateAndMetadataLinesAreSkipped(t *testing.T) {
	recs, err := NewReader(strings.NewReader(hostEventTranscript)).ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for _, r := range recs {
		switch r.EventType {
		case "reasoning", "session-meta", "ai-title":
			t.Errorf("%s 应被跳过，却产出了记录: %+v", r.EventType, r)
		}
	}
}

// 反向约束：真的解析不了的行仍必须落 unparsed —— 修这个 bug 不能把
// 「看不懂就丢」重新引回来（计划 §7 停止条件第 1 条）。
func TestTrulyUnknownLineStillUnparsed(t *testing.T) {
	recs, err := NewReader(strings.NewReader(hostEventTranscript)).ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var unparsed int
	for _, r := range recs {
		if r.EventType == EventUnparsed {
			unparsed++
			if !r.Unparsed {
				t.Error("EventUnparsed 的记录必须置 Unparsed=true")
			}
		}
	}
	if unparsed != 1 {
		t.Errorf("unparsed = %d，期望 1（只有非 JSON 那行）", unparsed)
	}
}

func eventTypes(recs []Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.EventType)
	}
	return out
}
