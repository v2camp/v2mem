package evidence

// 批量归档（方案 B / P1）的契约测试。
// 设计依据见 docs/design-write-batch.md；本阶段新 API 默认未接入 Engine.Capture。

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func batchTestEngine(t *testing.T, dir string) *Engine {
	t.Helper()
	e, err := Open(filepath.Join(dir, "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func batchEvents(n int) []RawEvent {
	out := make([]RawEvent, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, RawEvent{
			EventUID:         fmt.Sprintf("uid-%d", i),
			SourceID:         "src-batch",
			Generation:       1,
			Seq:              i,
			OccurredAtNS:     int64(1000 + i),
			SessionID:        "s1",
			Project:          "p1",
			Harness:          "workbuddy",
			EventType:        "stop",
			ParserVersion:    ParserVersion,
			ProposalEligible: i%2 == 0,
			Raw:              []byte(fmt.Sprintf("raw-%d", i)),
		})
	}
	return out
}

func countArchives(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".gz" {
			n++
		}
	}
	return n
}

func writeGzipLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	for _, l := range lines {
		if _, err := zw.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func validRecordLine(t *testing.T, seq int) string {
	t.Helper()
	b, err := json.Marshal(segmentRecord{
		Version: 1, EventUID: fmt.Sprintf("uid-%d", seq), SourceID: "src-batch",
		Generation: 1, Seq: seq, Raw: []byte(fmt.Sprintf("raw-%d", seq)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteArchiveBatchMultiLineAndLocateBySeq(t *testing.T) {
	dir := t.TempDir()
	e := batchTestEngine(t, dir)
	evs := batchEvents(5)

	seg, err := e.writeArchiveBatch(evs)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(seg.path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != seg.hash {
		t.Fatalf("文件名应等于压缩字节 hash: %s vs %s", hex.EncodeToString(sum[:]), seg.hash)
	}
	if got := countArchives(t, e.ArchiveDir()); got != 1 {
		t.Fatalf("5 条记录应只产生 1 个归档文件, got %d", got)
	}

	// 关键契约：非首行必须能按 #seq 定位，且原始字节可逆。
	for _, ev := range evs {
		got, err := ReadArchiveRecord(fmt.Sprintf("%s#%d", seg.path, ev.Seq))
		if err != nil {
			t.Fatalf("定位 seq=%d 失败: %v", ev.Seq, err)
		}
		if got.EventUID != ev.EventUID || got.Seq != ev.Seq || string(got.Raw) != string(ev.Raw) {
			t.Fatalf("seq=%d 取回内容不符: %+v", ev.Seq, got)
		}
		if got.ProposalEligible != ev.ProposalEligible {
			t.Fatalf("seq=%d 的 ProposalEligible 丢失", ev.Seq)
		}
	}
	// 不带 #seq 时兼容返回首行。
	first, err := ReadArchiveRecord(seg.path)
	if err != nil || first.EventUID != evs[0].EventUID {
		t.Fatalf("无后缀应返回首行, got %+v err=%v", first, err)
	}
	// 旧的单条解析无法处理多行，证明新读取 API 不是冗余。
	if _, err := readArchive(seg.path); err == nil {
		t.Fatal("readArchive 应无法把多行 JSONL 当成单个 JSON 对象")
	}
}

func TestWriteArchiveBatchReproducibleAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	e := batchTestEngine(t, dir)
	evs := batchEvents(4)

	first, err := e.writeArchiveBatch(evs)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.writeArchiveBatch(evs)
	if err != nil {
		t.Fatal(err)
	}
	if again.hash != first.hash || again.path != first.path {
		t.Fatalf("同批重复写应命中同 hash 文件: %+v vs %+v", first, again)
	}
	if got := countArchives(t, e.ArchiveDir()); got != 1 {
		t.Fatalf("重复写不应新增归档文件, got %d", got)
	}

	// 不同目录下同批同顺序，压缩字节仍须一致（gzip header 固定）。
	other := batchTestEngine(t, t.TempDir())
	cross, err := other.writeArchiveBatch(evs)
	if err != nil {
		t.Fatal(err)
	}
	if cross.hash != first.hash {
		t.Fatalf("跨目录同批 hash 应相同: %s vs %s", first.hash, cross.hash)
	}
	// 顺序变化必须产生不同 hash，否则批边界不确定会污染重放身份。
	reordered := append([]RawEvent{}, evs...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	changed, err := other.writeArchiveBatch(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if changed.hash == first.hash {
		t.Fatal("批内顺序变化应产生不同 archive_hash")
	}
}

func TestWriteArchiveSinglePathStillWorks(t *testing.T) {
	e := batchTestEngine(t, t.TempDir())
	ev := batchEvents(1)[0]
	seg, err := e.writeArchive(ev)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := readArchive(seg.path)
	if err != nil || rec.EventUID != ev.EventUID || rec.Seq != ev.Seq {
		t.Fatalf("单条路径回归失败: %+v err=%v", rec, err)
	}
	if _, err := ReadArchiveRecord(seg.path); err != nil {
		t.Fatalf("单条归档无后缀读取失败: %v", err)
	}
	if _, err := ReadArchiveRecord(seg.path + "#1"); err != nil {
		t.Fatalf("单条归档带 #1 读取失败: %v", err)
	}
}

func TestWriteArchiveBatchRejectsEmptyAndBadDir(t *testing.T) {
	e := batchTestEngine(t, t.TempDir())
	if _, err := e.writeArchiveBatch(nil); err == nil {
		t.Fatal("空批应报错")
	}
	if _, err := e.writeArchiveBatch([]RawEvent{}); err == nil {
		t.Fatal("零长度批应报错")
	}
	if _, err := e.writeArchiveRecords(nil); err == nil {
		t.Fatal("writeArchiveRecords 空输入应报错")
	}

	dir := t.TempDir()
	notDir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := &Engine{archiveDir: notDir}
	if _, err := bad.writeArchiveBatch(batchEvents(1)); err == nil {
		t.Fatal("归档目录是普通文件时应报错")
	}

	ro := filepath.Join(dir, "readonly")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	noWrite := &Engine{archiveDir: ro}
	if _, err := noWrite.writeArchiveBatch(batchEvents(1)); err == nil {
		t.Fatal("归档目录不可写时应报错")
	}
}

func TestReadArchiveRecordSkipsBlankLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blank.jsonl.gz")
	line := validRecordLine(t, 7)
	writeGzipLines(t, path, "", "  ", line, "")
	rec, err := ReadArchiveRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Seq != 7 || rec.EventUID != "uid-7" {
		t.Fatalf("空行应被跳过并取到 seq=7: %+v", rec)
	}
	bySeq, err := ReadArchiveRecord(path + "#7")
	if err != nil || bySeq.Seq != 7 {
		t.Fatalf("按 seq 定位失败: %+v err=%v", bySeq, err)
	}
	// "#" 后为空等价于不带后缀。
	if _, err := ReadArchiveRecord(path + "#"); err != nil {
		t.Fatalf("空 seq 后缀应退化为首行: %v", err)
	}
}

func TestReadArchiveRecordErrors(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jsonl.gz")
	writeGzipLines(t, good, validRecordLine(t, 1), validRecordLine(t, 2))
	notGzip := filepath.Join(dir, "plain.gz")
	if err := os.WriteFile(notGzip, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	badJSON := filepath.Join(dir, "bad.jsonl.gz")
	writeGzipLines(t, badJSON, "{not-json")
	empty := filepath.Join(dir, "empty.jsonl.gz")
	writeGzipLines(t, empty, "", "   ")

	cases := []struct {
		name    string
		locator string
		want    string
	}{
		{"空 locator", "", "缺少路径"},
		{"只有 seq", "#1", "缺少路径"},
		{"seq 非数字", good + "#abc", "seq 无效"},
		{"seq 不存在", good + "#999", "找不到 seq"},
		{"文件不存在", filepath.Join(dir, "missing.gz") + "#1", "打开归档失败"},
		{"非 gzip", notGzip, "gzip"},
		{"坏 JSON", badJSON, "解析 segment 记录失败"},
		{"空归档", empty, "归档为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReadArchiveRecord(c.locator)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望错误含 %q, got %v", c.want, err)
			}
		})
	}

	// 截断 gzip 尾部校验和，覆盖 scanner 读错误分支。
	// 必须用不存在的 seq 让扫描走到底，否则命中首行就直接返回，读不到损坏的尾部。
	trunc := filepath.Join(dir, "trunc.jsonl.gz")
	writeGzipLines(t, trunc, validRecordLine(t, 1))
	b, err := os.ReadFile(trunc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trunc, b[:len(b)-4], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchiveRecord(trunc + "#999"); err == nil {
		t.Fatal("截断的 gzip 在扫描到底时应报读取错误")
	}
}
