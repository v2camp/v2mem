package evidence

// 方案 B P2 契约测试：批量写账、幂等重放、批内重复、失败 spool 回放。
// 设计依据见 docs/design-write-batch.md；灰度开关在 cmd/mem/observe.go。

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/v2camp/v2mem/internal/store"
)

func p2Engine(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	e, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, db
}

func p2Query(t *testing.T, db, q string) [][]any {
	t.Helper()
	c, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rows, err := c.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]any, 0)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]any, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				row[i] = string(b)
			} else {
				row[i] = v
			}
		}
		out = append(out, row)
	}
	return out
}

func p2Count(t *testing.T, db, table string) int64 {
	t.Helper()
	rows := p2Query(t, db, "select count(*) from "+table)
	return rows[0][0].(int64)
}

func p2Events(n int, prefix string) []RawEvent {
	out := make([]RawEvent, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, RawEvent{
			EventUID:         fmt.Sprintf("%s-%d", prefix, i),
			SourceID:         "src-p2",
			SessionID:        "s-p2",
			Project:          "p-p2",
			Harness:          "workbuddy",
			EventType:        "stop",
			OccurredAtNS:     int64(1000 + i),
			ProposalEligible: i%2 == 0,
			Raw:              []byte(fmt.Sprintf("raw-%s-%d", prefix, i)),
		})
	}
	return out
}

func p2ArchiveFiles(t *testing.T, e *Engine) int {
	t.Helper()
	entries, err := os.ReadDir(e.ArchiveDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range entries {
		if !x.IsDir() && filepath.Ext(x.Name()) == ".gz" {
			n++
		}
	}
	return n
}

func TestP2CaptureBatchSingleLedgerAndLocateBySeq(t *testing.T) {
	e, db := p2Engine(t)
	evs := p2Events(5, "a")
	refs, err := e.CaptureBatch(evs)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 5 {
		t.Fatalf("应返回 5 个 Ref, got %d", len(refs))
	}
	if got := p2Count(t, db, "observations"); got != 5 {
		t.Fatalf("observations 应为 5, got %d", got)
	}
	if got := p2Count(t, db, "evidence_archives"); got != 1 {
		t.Fatalf("一批只应写 1 行 archive 账, got %d", got)
	}
	if got := p2ArchiveFiles(t, e); got != 1 {
		t.Fatalf("一批只应产生 1 个归档文件, got %d", got)
	}
	ledger := p2Query(t, db, "select from_seq,to_seq,raw_count from evidence_archives")
	if len(ledger) != 1 || fmt.Sprint(ledger[0][0]) != "1" || fmt.Sprint(ledger[0][1]) != "5" || fmt.Sprint(ledger[0][2]) != "5" {
		t.Fatalf("archive 账应覆盖 [1,5] 且 raw_count=5, got %v", ledger)
	}
	// 关键契约：多行归档里非首行必须能按 #seq 取回原始字节。
	for _, ref := range refs {
		got, err := ReadArchiveRecord(ref.RawLocator)
		if err != nil {
			t.Fatalf("按 %s 读取失败: %v", ref.RawLocator, err)
		}
		if got.EventUID != ref.EventUID || got.Seq != ref.Seq {
			t.Fatalf("定位错位: want %s/%d got %s/%d", ref.EventUID, ref.Seq, got.EventUID, got.Seq)
		}
	}
}

func TestP2CaptureBatchReplayIsIdempotent(t *testing.T) {
	e, db := p2Engine(t)
	evs := p2Events(4, "b")
	if _, err := e.CaptureBatch(evs); err != nil {
		t.Fatal(err)
	}
	refs, err := e.CaptureBatch(evs)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 4 {
		t.Fatalf("重放应返回全部已有 Ref, got %d", len(refs))
	}
	if got := p2Count(t, db, "observations"); got != 4 {
		t.Fatalf("重放不应新增观测行, got %d", got)
	}
	if got := p2Count(t, db, "evidence_archives"); got != 1 {
		t.Fatalf("重放不应新增 archive 账, got %d", got)
	}
	if got := p2ArchiveFiles(t, e); got != 1 {
		t.Fatalf("重放不应新增归档文件, got %d", got)
	}
}

func TestP2CaptureBatchPartialDuplicate(t *testing.T) {
	e, db := p2Engine(t)
	existing := p2Events(3, "c")
	if _, err := e.Capture(existing[1]); err != nil {
		t.Fatal(err)
	}
	refs, err := e.CaptureBatch(existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("含重复项也应返回 3 个 Ref, got %d", len(refs))
	}
	if got := p2Count(t, db, "observations"); got != 3 {
		t.Fatalf("应只补写 2 条，总计 3 条, got %d", got)
	}
	// 批内重复 uid 只保留首条，避免 to_seq / raw_count 与实际写入行数不符。
	dup := append([]RawEvent{}, p2Events(2, "d")...)
	dup = append(dup, dup[0])
	_, err = e.CaptureBatch(dup)
	if err != nil {
		t.Fatal(err)
	}
	seqs := p2Query(t, db, "select source_seq from observations where event_uid like 'd-%' order by source_seq")
	if len(seqs) != 2 {
		t.Fatalf("批内重复 uid 应只写 2 行, got %d", len(seqs))
	}
}

func TestP2CaptureBatchValidation(t *testing.T) {
	e, _ := p2Engine(t)
	if refs, err := e.CaptureBatch(nil); err != nil || len(refs) != 0 {
		t.Fatalf("空批应返回空, got %v err=%v", refs, err)
	}
	if _, err := e.CaptureBatch([]RawEvent{{SourceID: "src-p2", Raw: []byte("x")}}); err == nil {
		t.Fatal("缺 event_uid 应报错")
	}
	mixed := p2Events(2, "e")
	mixed[1].SourceID = "src-other"
	if _, err := e.CaptureBatch(mixed); err == nil {
		t.Fatal("批内证据源不一致应报错")
	}
	if recs, created, err := e.st.CaptureEvidenceBatch(nil); err != nil || len(recs) != 0 || len(created) != 0 {
		t.Fatalf("store 空批应返回空, got %v %v err=%v", recs, created, err)
	}
}

func TestP2StoreBatchValidation(t *testing.T) {
	e, _ := p2Engine(t)
	for _, in := range []store.EvidenceCaptureInput{
		{},
		{EventUID: "u"},
		{EventUID: "u", SourceID: "s"},
	} {
		if _, _, err := e.st.CaptureEvidenceBatch([]store.EvidenceCaptureInput{in}); err == nil {
			t.Fatalf("非法入参应被拒绝: %+v", in)
		}
	}
}

func TestP2WriteSpoolBatchAndDrain(t *testing.T) {
	e, db := p2Engine(t)
	evs := p2Events(3, "spool")
	path, err := e.writeSpoolBatch(evs)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, b := range body {
		if b == '\n' {
			lines++
		}
	}
	if lines != 3 {
		t.Fatalf("一个 spool 文件应含 3 行, got %d", lines)
	}
	res, err := e.DrainSpool(200)
	if err != nil {
		t.Fatal(err)
	}
	if res.Drained != 3 {
		t.Fatalf("drain 应回放 3 条, got %+v", res)
	}
	if got := p2Count(t, db, "observations"); got != 3 {
		t.Fatalf("drain 后应有 3 条观测, got %d", got)
	}
	// 再 drain 一次不应产生新的回放（文件已删除）。
	res2, err := e.DrainSpool(200)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Drained != 0 {
		t.Fatalf("二次 drain 应为 0, got %+v", res2)
	}
	if _, err := e.writeSpoolBatch(nil); err == nil {
		t.Fatal("空 spool 批应报错")
	}
}
