package store

// 方案 B P2 的包内测试：批量写账必须自己负责覆盖率（AGENTS.md 包内视角口径），
// 不能依赖 internal/evidence 的跨包调用刷分。

import (
	"fmt"
	"testing"
)

func batchEvidenceInputs(n int) []EvidenceCaptureInput {
	ins := make([]EvidenceCaptureInput, 0, n)
	for i := 1; i <= n; i++ {
		in := evidenceInput(fmt.Sprintf("batch-%d", i), i)
		in.ArchiveHash = "ah-batch"
		in.ArchivePath = "/tmp/batch.jsonl.gz"
		in.RawRecordHash = fmt.Sprintf("rh-%d", i)
		if i == 1 {
			in.ToSeq = n
			in.RawCount = n
			in.RawHashConcat = "rh-1rh-2rh-3rh-4"
		} else {
			in.SkipArchiveLedger = true
			in.RawCount = 0
		}
		ins = append(ins, in)
	}
	return ins
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("select count(*) from " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCaptureEvidenceBatchWritesOneLedger(t *testing.T) {
	s := newTestStore(t)
	ins := batchEvidenceInputs(4)
	recs, created, err := s.CaptureEvidenceBatch(ins)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 || len(created) != 4 {
		t.Fatalf("应返回 4 条记录, got %d/%d", len(recs), len(created))
	}
	for i, ok := range created {
		if !ok {
			t.Fatalf("第 %d 条首次写入应为 created", i)
		}
	}
	if got := countRows(t, s, "observations"); got != 4 {
		t.Fatalf("observations 应为 4, got %d", got)
	}
	if got := countRows(t, s, "evidence_archives"); got != 1 {
		t.Fatalf("一批只应写 1 行 archive 账, got %d", got)
	}
	var fromSeq, toSeq, rawCount int
	var concat string
	if err := s.db.QueryRow(`select from_seq,to_seq,raw_count,raw_hash_concat
		from evidence_archives where archive_hash=?`, "ah-batch").
		Scan(&fromSeq, &toSeq, &rawCount, &concat); err != nil {
		t.Fatal(err)
	}
	if fromSeq != 1 || toSeq != 4 || rawCount != 4 || concat != "rh-1rh-2rh-3rh-4" {
		t.Fatalf("archive 账区间/计数不对: %d-%d count=%d concat=%q", fromSeq, toSeq, rawCount, concat)
	}
	var lastSeq int
	if err := s.db.QueryRow(`select last_seq from evidence_sources where id=?`, "src-a").Scan(&lastSeq); err != nil {
		t.Fatal(err)
	}
	if lastSeq != 4 {
		t.Fatalf("水位应推进到 4, got %d", lastSeq)
	}
	for i, rec := range recs {
		if rec.Seq != i+1 {
			t.Fatalf("seq 应连续递增, got %+v", recs)
		}
		if rec.RawLocator != fmt.Sprintf("/tmp/batch.jsonl.gz#%d", rec.Seq) {
			t.Fatalf("批内 locator 应指向同一归档的不同 seq: %s", rec.RawLocator)
		}
	}
}

func TestCaptureEvidenceBatchReplayAndSeqGap(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.CaptureEvidenceBatch(batchEvidenceInputs(3)); err != nil {
		t.Fatal(err)
	}
	// 重放：event_uid 命中已有行，不新增、不推进水位，也不重复插 archive 账。
	_, created, err := s.CaptureEvidenceBatch(batchEvidenceInputs(3))
	if err != nil {
		t.Fatal(err)
	}
	for i, ok := range created {
		if ok {
			t.Fatalf("重放第 %d 条不应 created", i)
		}
	}
	if got := countRows(t, s, "observations"); got != 3 {
		t.Fatalf("重放不应新增观测行, got %d", got)
	}
	if got := countRows(t, s, "evidence_archives"); got != 1 {
		t.Fatalf("重放不应新增 archive 账, got %d", got)
	}
	// 跳号必须被拒绝，连续游标不被破坏。
	gap := batchEvidenceInputs(1)
	gap[0].EventUID = "gap-uid"
	gap[0].Seq = 99
	if _, _, err := s.CaptureEvidenceBatch(gap); err == nil {
		t.Fatal("seq 跳号应返回 ErrEvidenceSeqGap")
	}
}

func TestCaptureEvidenceBatchEmptyAndValidation(t *testing.T) {
	s := newTestStore(t)
	if recs, created, err := s.CaptureEvidenceBatch(nil); err != nil || len(recs) != 0 || len(created) != 0 {
		t.Fatalf("空批应返回空: %v %v %v", recs, created, err)
	}
	for _, bad := range []EvidenceCaptureInput{
		{},
		{EventUID: "u"},
		{EventUID: "u", SourceID: "s"},
	} {
		if _, _, err := s.CaptureEvidenceBatch([]EvidenceCaptureInput{bad}); err == nil {
			t.Fatalf("非法入参应被拒绝: %+v", bad)
		}
	}
}

func TestCaptureEvidenceSinglePathUnchangedWithBatchFields(t *testing.T) {
	s := newTestStore(t)
	in := evidenceInput("solo", 0)
	rec := mustCaptureEvidence(t, s, in)
	if rec.Seq != 1 {
		t.Fatalf("单条 seq 应为 1, got %d", rec.Seq)
	}
	var fromSeq, toSeq, rawCount int
	if err := s.db.QueryRow(`select from_seq,to_seq,raw_count from evidence_archives
		where archive_hash=?`, in.ArchiveHash).Scan(&fromSeq, &toSeq, &rawCount); err != nil {
		t.Fatal(err)
	}
	if fromSeq != 1 || toSeq != 1 || rawCount != 1 {
		t.Fatalf("单条路径 ToSeq 零值时应退化为 [1,1] count=1, got %d-%d count=%d", fromSeq, toSeq, rawCount)
	}
}
