package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendReadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	for _, q := range []string{"第一个查询", "第二个查询"} {
		if err := Append(p, Record{TS: 1, Event: "prompt-submit", Query: q, Hashes: []string{"aaaa1111"}}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	rs, err := Read(p, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(rs) != 2 || rs[0].Query != "第一个查询" {
		t.Fatalf("应读到 2 条且保序，got %+v", rs)
	}
}

func TestReadTakesLastN(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	for _, q := range []string{"a1", "a2", "a3"} {
		if err := Append(p, Record{Event: "manual", Query: q}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	rs, err := Read(p, 2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(rs) != 2 || rs[0].Query != "a2" || rs[1].Query != "a3" {
		t.Errorf("应取最后 2 条，got %+v", rs)
	}
}

func TestReadMissingFileIsNotAnError(t *testing.T) {
	rs, err := Read(filepath.Join(t.TempDir(), "nope.jsonl"), 0)
	if err != nil {
		t.Errorf("文件不存在不应报错（首次使用场景）: %v", err)
	}
	if len(rs) != 0 {
		t.Errorf("应返回空，got %+v", rs)
	}
}

// 一行坏数据不该让整个审计不可读。
func TestReadSkipsMalformedLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	body := `{"event":"a","query":"好的"}` + "\n" + `{坏行` + "\n" + `{"event":"b","query":"也好"}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rs, err := Read(p, 0)
	if err != nil {
		t.Fatalf("坏行不应导致整体失败: %v", err)
	}
	if len(rs) != 2 {
		t.Errorf("应跳过坏行保留 2 条，got %d", len(rs))
	}
}

func TestSummarizeCountsEvaluableSamples(t *testing.T) {
	rs := []Record{
		{Event: "session-start", Harness: "claude", Empty: false, MS: 10},
		{Event: "prompt-submit", Harness: "claude", Query: "有查询", MS: 20},
		{Event: "prompt-submit", Harness: "codex", Empty: true, MS: 30}, // 无 query
	}
	s := Summarize(rs)
	if s.Total != 3 {
		t.Fatalf("总数应为 3，got %d", s.Total)
	}
	if s.WithQuery != 1 {
		t.Errorf("可用于评测的样本数应为 1（只有带 query 的才算），got %d", s.WithQuery)
	}
	if s.Empty != 1 {
		t.Errorf("空注入数应为 1，got %d", s.Empty)
	}
	if s.AvgMS != 20 {
		t.Errorf("平均耗时应为 20，got %v", s.AvgMS)
	}
	if s.ByEvent["prompt-submit"] != 2 || s.ByHarness["claude"] != 2 {
		t.Errorf("分布统计有误: %+v", s)
	}
}

func TestSummarizeOnEmptyInput(t *testing.T) {
	s := Summarize(nil)
	if s.Total != 0 || s.EmptyRate != 0 || s.AvgMS != 0 {
		t.Errorf("空输入应得零值，got %+v", s)
	}
}

// 读/写两侧必须分开计数：只记不查（写了一堆但从不检索）或只查不记（检索但从不沉淀）
// 都会体现在这两个数上，看总次数是看不出来的。
func TestSummarizeSeparatesReadAndWriteSides(t *testing.T) {
	rs := []Record{
		{Event: "session-start"}, {Event: "prompt-submit", Query: "q"},
		{Event: "manual-search", Query: "q2"},
		{Event: "manual-add", Mode: "create"},
		{Event: "manual-add", Mode: "overwrite"},
		{Event: "manual-add", Mode: "create"},
	}
	s := Summarize(rs)
	if s.Retrievals != 3 {
		t.Errorf("读侧应为 3，got %d", s.Retrievals)
	}
	if s.Writes != 3 {
		t.Errorf("写侧应为 3，got %d", s.Writes)
	}
	if s.Creates != 2 || s.Overwrites != 1 {
		t.Errorf("写侧构成应为 新增2/覆盖1，got %d/%d", s.Creates, s.Overwrites)
	}
	// 写侧没有 query，不应污染「可评测样本」（那是读侧口径）
	if s.WithQuery != 2 {
		t.Errorf("可评测样本应只算读侧带 query 的 2 条，got %d", s.WithQuery)
	}
}

// 写侧不应计入「空注入率」—— 它没有「注入」这个概念。
func TestWriteOnlyLogHasNoEmptyInjectionRate(t *testing.T) {
	s := Summarize([]Record{{Event: "manual-add", Mode: "create"}})
	if s.EmptyRate != 0 {
		t.Errorf("纯写日志的空注入率应为 0，got %v", s.EmptyRate)
	}
}

// Read 与 Append 必须对空路径保持一致：都取默认位置。
// 此前 Read 不兜底 ⇒ os.Open("") 的 ENOENT 被 isNotExist 分支吞掉，
// 调用方拿到「空日志」而非「路径错了」，进而得出错误结论。
func TestReadDefaultsEmptyPathLikeAppend(t *testing.T) {
	// 测试 HOME 已隔离到临时目录，默认路径落在其中
	def := Path()
	if err := Append("", Record{Event: "manual-search", Query: "写默认路径"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !Exists("") {
		t.Fatal("Append 空路径后，Exists 应看到默认位置的日志")
	}
	rs, err := Read("", 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// 默认路径在整个测试二进制里共享，`-count=2` 时记录会累积 ⇒
	// 断言「至少包含」而不是精确条数（卡条数会在重复运行时误报）。
	found := false
	for _, r := range rs {
		if r.Query == "写默认路径" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Read 空路径应读到默认位置的内容，got %+v（默认路径 %s）", rs, def)
	}
}

// Exists 要能区分「日志不存在」与「日志存在但为空」。
func TestExistsDistinguishesMissingFromEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	if Exists(p) {
		t.Error("文件尚未创建时 Exists 应为 false")
	}
	if err := Append(p, Record{Event: "manual-add"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !Exists(p) {
		t.Error("文件创建后 Exists 应为 true")
	}
}
