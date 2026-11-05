package transcript

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleTranscript = `{"type":"user","uuid":"u1","sessionId":"s1","timestamp":"2026-11-06T00:00:00Z","cwd":"/work/demo","message":{"role":"user","content":"需求：写全证据"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-11-06T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"先看代码"},{"type":"thinking","thinking":"内部推理"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"a.txt"}]}]}}
{"type":"summary","uuid":"sum1","summary":"这轮定了 T1.2"}
{"type":"system","uuid":"sys1","message":{"role":"system","content":"系统提示"}}
{"type":"queue-operation","content":"忽略"}
not json
`

func TestReadAllPreservesOrderBlocksAndIdentity(t *testing.T) {
	rd := NewReader(strings.NewReader(sampleTranscript))
	recs, err := rd.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var kinds []string
	for _, r := range recs {
		kinds = append(kinds, r.EventType)
	}
	want := []string{
		EventUserPrompt, EventAssistantText, EventToolUse, EventToolResult,
		EventSummary, "system",
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("事件顺序/类型不对:\n got %v\nwant %v", kinds, want)
	}

	if recs[0].HostUID != "u1" || recs[0].SessionID != "s1" || recs[0].Project != "demo" {
		t.Fatalf("首条身份/元数据不对: %+v", recs[0])
	}
	if recs[0].At != 1793923200 { // 2026-11-06T00:00:00Z
		t.Fatalf("时间戳解析不对: %d", recs[0].At)
	}
	if recs[1].HostUID != "a1#0" || recs[1].Block != 0 || recs[1].Text != "先看代码" {
		t.Fatalf("文本块身份不对: %+v", recs[1])
	}
	if recs[2].HostUID != "tool_use:toolu_1" || recs[2].ToolID != "toolu_1" ||
		recs[2].ToolName != "Bash" || !strings.Contains(recs[2].Text, `"command":"ls"`) {
		t.Fatalf("工具调用块不对: %+v", recs[2])
	}
	if recs[3].HostUID != "tool_result:toolu_1" || recs[3].ToolID != "toolu_1" ||
		!strings.Contains(recs[3].Text, "a.txt") {
		t.Fatalf("工具结果块不对: %+v", recs[3])
	}
	if recs[4].Role != "summary" || !strings.Contains(recs[4].Text, "T1.2") {
		t.Fatalf("summary 行不对: %+v", recs[4])
	}

	// 原生行序：Start/End 严格递增且不重叠。
	for i := 1; i < len(recs); i++ {
		if recs[i].Start < recs[i-1].Start {
			t.Fatalf("行序被破坏: %+v 在 %+v 之前", recs[i], recs[i-1])
		}
	}
	if recs[1].Start != recs[2].Start || recs[1].End != recs[2].End {
		t.Fatalf("同一行的两块应共享行偏移: %+v / %+v", recs[1], recs[2])
	}
	if rd.Offset() != int64(len(sampleTranscript)) {
		t.Fatalf("消费偏移应等于输入长度: %d vs %d", rd.Offset(), len(sampleTranscript))
	}
}

func TestRecordsWithoutUUIDAndEmptyBlocks(t *testing.T) {
	raw := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"没有 uuid 的提问"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"  "},{"type":"tool_use","name":"Read","input":null}]}}`,
		`{"type":"user","message":{"role":"user","content":[]}}`,
	}, "\n")
	recs, err := NewReader(strings.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("空正文块应被跳过，got %+v", recs)
	}
	if recs[0].HostUID != "" || recs[0].EventType != EventUserPrompt {
		t.Fatalf("无 uuid 时 HostUID 应为空，交给调用方合成: %+v", recs[0])
	}
	if recs[1].EventType != EventToolUse || recs[1].HostUID != "" {
		t.Fatalf("无 id 的 tool_use 也应归档: %+v", recs[1])
	}
	if strings.Contains(recs[1].Text, "null") || recs[1].Text != "tool:Read" {
		t.Fatalf("空 input 不应写进正文: %q", recs[1].Text)
	}
}

// 只消费完整行：宿主正在写的半行留给下一次，否则 Stop 撞上写入会吞掉半条 JSON。
func TestReaderKeepsTrailingPartialLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	full := `{"type":"user","uuid":"u1","message":{"role":"user","content":"第一条"}}` + "\n"
	partial := `{"type":"user","uuid":"u2","message":{"role":"user","cont`
	if err := os.WriteFile(path, []byte(full+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd := NewReader(f)
	recs, err := rd.Read()
	if err != nil || len(recs) != 1 {
		t.Fatalf("首行应解析出 1 条，got %+v err=%v", recs, err)
	}
	if _, err := rd.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("半行应停在 EOF，got %v", err)
	}
	if rd.Offset() != int64(len(full)) {
		t.Fatalf("偏移必须停在完整行末尾: %d want %d", rd.Offset(), len(full))
	}
	// 补齐后半行：能从上次的偏移继续。
	if err := os.WriteFile(path, []byte(full+`{"type":"user","uuid":"u2","message":{"role":"user","content":"第二条"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if _, err := f2.Seek(rd.Offset(), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rd2 := NewReaderAt(f2, rd.Offset())
	recs, err = rd2.Read()
	if err != nil || len(recs) != 1 || recs[0].Text != "第二条" {
		t.Fatalf("补齐后应读出新行，got %+v err=%v", recs, err)
	}
	if recs[0].Start != int64(len(full)) {
		t.Fatalf("偏移基准应是流内绝对偏移: %d", recs[0].Start)
	}
}

// 没有尾换行但 JSON 完整（导出夹具常见）也要消费；坏 JSON 才留待下次。
func TestReaderTrailingCompleteLineWithoutNewline(t *testing.T) {
	line := `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"结论"}]}}`
	recs, err := NewReader(strings.NewReader(line)).ReadAll()
	if err != nil {
		t.Fatalf("完整行无换行也应消费: %v", err)
	}
	if len(recs) != 1 || recs[0].Text != "结论" {
		t.Fatalf("got %+v", recs)
	}
	if recs[0].End != int64(len(line)) {
		t.Fatalf("End 应等于行长度: %d vs %d", recs[0].End, len(line))
	}

	rd := NewReader(strings.NewReader(`{"type":"user"`))
	if _, err := rd.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("坏尾行应停在 EOF，got %v", err)
	}
	if rd.Offset() != 0 {
		t.Fatalf("坏尾行不应推进偏移，got %d", rd.Offset())
	}
}

func TestRecordOccurredAtNS(t *testing.T) {
	if got := (Record{}).OccurredAtNS(); got != 0 {
		t.Fatalf("无时间应为 0，got %d", got)
	}
	if got := (Record{At: 1700000000}).OccurredAtNS(); got != 1700000000000000000 {
		t.Fatalf("应换算成纳秒，got %d", got)
	}
}
