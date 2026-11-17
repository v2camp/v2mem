package transcript

import (
	"strings"
	"testing"
)

// WorkBuddy / CodeBuddy 的 message 行：role / content 在顶层，块类型是
// input_text / output_text，assistant 行还另有一个 usage 用的 message 字段。
// 这些行过去全落 unparsed → pointer → 不可提案，用户请求永远进不了记忆链路。
const nativeUserLine = `{"id":"wb-u1","timestamp":1793583268633,"type":"message","role":"user","content":[{"type":"input_text","text":"<system-reminder data-role=\"user-context\">\n<user_info>OS: darwin</user_info>\n</system-reminder>\n<user_query>审查，我们当前记忆记录的是用户请求吗</user_query>"}],"sessionId":"wb-sess","cwd":"/work/v2mem"}`

const nativeAssistantLine = `{"id":"wb-a1","parentId":"wb-u1","timestamp":1793583599512,"type":"message","role":"assistant","message":{"usage":{"output_tokens":12}},"content":[{"type":"output_text","text":"结论：归档层双方都记，提案层只进了助手回复"}],"sessionId":"wb-sess","cwd":"/work/v2mem"}`

const nativeToolLine = `{"id":"wb-t1","type":"function_call","name":"Bash","callId":"c1","arguments":"{}","sessionId":"wb-sess","cwd":"/work/v2mem"}`

func TestParseNativeMessageUserAndAssistant(t *testing.T) {
	recs, err := NewReader(strings.NewReader(nativeUserLine + "\n" + nativeAssistantLine + "\n")).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("应解析出 2 条消息，got %d: %+v", len(recs), recs)
	}
	u, a := recs[0], recs[1]
	if u.EventType != EventUserPrompt || a.EventType != EventAssistantText {
		t.Fatalf("事件类型错: user=%q assistant=%q", u.EventType, a.EventType)
	}
	if u.Unparsed || a.Unparsed {
		t.Fatal("原生消息不应落 unparsed")
	}
	if u.Role != "user" || a.Role != "assistant" {
		t.Fatalf("角色错: %q / %q", u.Role, a.Role)
	}
	if !strings.Contains(u.Text, "<user_query>") || !strings.Contains(u.Text, "OS: darwin") {
		t.Fatalf("归档正文必须保真（含注入上下文），got %q", u.Text)
	}
	if !strings.Contains(a.Text, "结论：") {
		t.Fatalf("助手正文丢失: %q", a.Text)
	}
	if u.HostUID != "wb-u1#0" || a.HostUID != "wb-a1#0" {
		t.Fatalf("宿主 id 应成为稳定身份: %q / %q", u.HostUID, a.HostUID)
	}
	if u.SessionID != "wb-sess" || a.SessionID != "wb-sess" {
		t.Fatalf("sessionId 未读出: %q / %q", u.SessionID, a.SessionID)
	}
	if u.Project != "v2mem" || a.Project != "v2mem" {
		t.Fatalf("cwd 未转成 project: %q / %q", u.Project, a.Project)
	}
	// timestamp 是毫秒；jsonTime 除以 1000 后按秒转纳秒。
	if u.OccurredAtNS() != 1793583268000000000 || a.OccurredAtNS() == 0 {
		t.Fatalf("毫秒时间戳换算错: %d / %d", u.OccurredAtNS(), a.OccurredAtNS())
	}
	if u.Start >= a.Start || a.End <= a.Start {
		t.Fatalf("行序/起止偏移错: %+v / %+v", u, a)
	}
}

func TestParseNativeToolLineKeepsRawAsUnparsed(t *testing.T) {
	recs, err := NewReader(strings.NewReader(nativeToolLine + "\n")).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("工具行应归档成 1 条，got %d", len(recs))
	}
	r := recs[0]
	if r.EventType != EventUnparsed || !r.Unparsed || r.EventType == EventUserPrompt {
		t.Fatalf("未识别的工具行必须保留原文且不可提案，got %+v", r)
	}
	if !strings.Contains(r.Text, "function_call") || !strings.Contains(r.Text, "callId") {
		t.Fatalf("原文必须整行保住: %q", r.Text)
	}
}

func TestParseClaudeLineStillUsesMessageEnvelope(t *testing.T) {
	line := `{"type":"user","uuid":"cl-u1","sessionId":"c1","message":{"role":"user","content":"按 Claude 格式解析"}}`
	recs, err := NewReader(strings.NewReader(line + "\n")).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].EventType != EventUserPrompt || recs[0].Unparsed {
		t.Fatalf("Claude 格式回归: %+v", recs)
	}
	if recs[0].Text != "按 Claude 格式解析" || recs[0].HostUID != "cl-u1" {
		t.Fatalf("Claude 正文/身份错: %+v", recs[0])
	}
}

func TestUserQueryText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"优先取 user_query",
			"<system-reminder data-role=\"user-context\"><user_info>OS: darwin</user_info></system-reminder>\n<user_query>按 B 来推进</user_query>",
			"按 B 来推进",
		},
		{
			"多个标签合并",
			"<user_query>第一句</user_query>\n<system-reminder>x</system-reminder>\n<user_query>第二句</user_query>",
			"第一句\n第二句",
		},
		{
			"无标签只剥注入块",
			"<system-reminder data-role=\"craft_mode\">Agent mode</system-reminder>\n真实提问",
			"真实提问",
		},
		{
			"多段注入块全剥",
			"<system-reminder>a</system-reminder>\n<system-reminder>b</system-reminder>",
			"",
		},
		{"普通文本原样", "直接提问，不带标签", "直接提问，不带标签"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UserQueryText(c.in); got != c.want {
				t.Fatalf("UserQueryText=%q want %q", got, c.want)
			}
		})
	}
}

func TestParseNativeMessageBadContentFallsBackToRaw(t *testing.T) {
	line := `{"id":"wb-bad","type":"message","role":"user","content":"not-an-array","sessionId":"s"}`
	recs, err := NewReader(strings.NewReader(line + "\n")).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || !recs[0].Unparsed {
		t.Fatalf("content 结构异常时必须按原文归档，got %+v", recs)
	}
}

func TestParseNativeMessageSkipsNonTextBlock(t *testing.T) {
	line := `{"id":"wb-mix","type":"message","role":"assistant","content":[{"type":"reasoning","text":"内部推理"},{"type":"output_text","text":"可见结论"},{"type":"output_text","text":"   "}]}`
	recs, err := NewReader(strings.NewReader(line + "\n")).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Text != "可见结论" || recs[0].Block != 1 {
		t.Fatalf("只收文本块且保留块序号，got %+v", recs)
	}
}
