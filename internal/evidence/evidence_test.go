package evidence

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/v2camp/v2mem/internal/store"
)

func newTestEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	e, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, db
}

func rawEvent(uid string) RawEvent {
	return RawEvent{
		EventUID: uid, SourceID: "src-a", OccurredAtNS: 1700000000000000000,
		SessionID: "sess-1", Project: "proj", Harness: "workbuddy",
		EventType: "post-tool-use", ParserVersion: ParserVersion,
		ProposalEligible: true, Raw: []byte("原始字节 " + uid + "\n第二行"),
	}
}

func archiveFileCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl.gz") {
			n++
		}
	}
	return n
}

func TestCaptureArchivesRawBytesAndLedger(t *testing.T) {
	e, _ := newTestEngine(t)
	ref, err := e.Capture(rawEvent("uid-1"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Seq != 1 || ref.Generation != 1 || ref.SourceID != "src-a" {
		t.Fatalf("身份不对: %+v", ref)
	}
	if ref.ArchiveHash == "" || ref.RawRecordHash == "" || ref.RawLocator == "" {
		t.Fatalf("归档引用缺失: %+v", ref)
	}
	if !strings.Contains(ref.Summary, "原始字节") || !ref.ProposalEligible {
		t.Fatalf("摘要/meta 不对: %+v", ref)
	}
	path := strings.TrimSuffix(ref.RawLocator, "#1")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("archive 文件应存在: %v", err)
	}
	if !strings.HasPrefix(path, e.ArchiveDir()) {
		t.Fatalf("archive 应在归档目录内: %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != ref.ArchiveHash {
		t.Fatalf("文件名应等于压缩字节 hash: file=%s ref=%s", hex.EncodeToString(sum[:]), ref.ArchiveHash)
	}
	rec, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.Raw) != string(rawEvent("uid-1").Raw) || rec.EventUID != "uid-1" || rec.Seq != 1 {
		t.Fatalf("归档内容不能丢原始字节: %+v", rec)
	}
	if rec.ParserVersion != ParserVersion {
		t.Fatalf("parser version 未落盘: %q", rec.ParserVersion)
	}
}

func TestCaptureAutoSeqAndReplay(t *testing.T) {
	e, _ := newTestEngine(t)
	first, err := e.Capture(rawEvent("uid-1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.Capture(rawEvent("uid-2"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("自动序号应为 1,2: %d %d", first.Seq, second.Seq)
	}
	again, err := e.Capture(rawEvent("uid-1"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Seq != 1 || again.ArchiveHash != first.ArchiveHash {
		t.Fatalf("uid 重放应返回首条: %+v", again)
	}
	posReplay := rawEvent("uid-other")
	posReplay.Seq = 1
	posReplay.Generation = 1
	again, err = e.Capture(posReplay)
	if err != nil {
		t.Fatal(err)
	}
	if again.EventUID != "uid-1" {
		t.Fatalf("位置重放应返回首条: %+v", again)
	}

	sameSummary := rawEvent("uid-3")
	sameSummary.Raw = rawEvent("uid-1").Raw
	third, err := e.Capture(sameSummary)
	if err != nil {
		t.Fatal(err)
	}
	if third.Seq != 3 {
		t.Fatalf("同摘要不同 uid 应共存且递增: %+v", third)
	}
	b, err := e.NextBundle(10, 1<<20)
	if err != nil || b == nil || len(b.EvidenceIDs) != 3 {
		t.Fatalf("应有三条证据: %+v err=%v", b, err)
	}
}

func TestCaptureValidation(t *testing.T) {
	e, _ := newTestEngine(t)
	if _, err := e.Capture(RawEvent{SourceID: "s"}); err == nil {
		t.Fatal("空 event_uid 应拒绝")
	}
	ev := rawEvent("uid")
	ev.SourceID = ""
	if _, err := e.Capture(ev); err == nil {
		t.Fatal("空 source_id 应拒绝")
	}
}

func TestArchiveFailureDoesNotAdvanceCursor(t *testing.T) {
	e, _ := newTestEngine(t)
	// 让归档目录的父路径被一个普通文件占住 → MkdirAll 失败。
	if err := os.MkdirAll(filepath.Dir(e.ArchiveDir()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.ArchiveDir(), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Capture(rawEvent("uid-1")); err == nil {
		t.Fatal("归档失败应报错")
	}
	b, err := e.NextBundle(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b != nil {
		t.Fatalf("归档失败不能推进游标: %+v", b)
	}
	if _, err := os.Stat(e.SpoolRoot()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive 失败不应落 spool: %v", err)
	}

	// 修好目录后同一条事件应从 seq 1 重新开始（水位没被推进）。
	if err := os.Remove(e.ArchiveDir()); err != nil {
		t.Fatal(err)
	}
	ref, err := e.Capture(rawEvent("uid-1"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Seq != 1 {
		t.Fatalf("水位应仍是 0，got seq=%d", ref.Seq)
	}
}

func TestDBFailureKeepsArchiveAndSpools(t *testing.T) {
	e, db := newTestEngine(t)
	if _, err := e.Capture(rawEvent("uid-1")); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	failed := rawEvent("uid-2")
	failed.Generation = 1
	failed.Seq = 2
	if _, err := e.Capture(failed); err == nil {
		t.Fatal("DB 关闭时 capture 应报错")
	}
	if n := archiveFileCount(t, e.ArchiveDir()); n != 2 {
		t.Fatalf("DB 失败后 archive 必须保留，got %d", n)
	}
	spoolFiles := spoolSegments(t, e.SpoolRoot())
	if len(spoolFiles) != 1 {
		t.Fatalf("应落一个独立 spool segment: %v", spoolFiles)
	}
	if strings.HasSuffix(spoolFiles[0], ".processing") || strings.HasSuffix(spoolFiles[0], ".tmp") {
		t.Fatalf("spool segment 必须是最终名: %s", spoolFiles[0])
	}
	body, err := os.ReadFile(spoolFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	var rec segmentRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("spool 应是 JSONL: %v", err)
	}
	if rec.EventUID != "uid-2" || rec.Seq != 2 || string(rec.Raw) != string(failed.Raw) {
		t.Fatalf("spool 记录不完整: %+v", rec)
	}

	e2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	res, err := e2.DrainSpool(10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Drained != 1 || res.Files != 1 || res.Restored != 0 {
		t.Fatalf("drain 计数不对: %+v", res)
	}
	if n := len(spoolSegments(t, e2.SpoolRoot())); n != 0 {
		t.Fatalf("drain 成功后 spool 应清空: %d", n)
	}
	b, err := e2.NextBundle(10, 1<<20)
	if err != nil || b == nil || len(b.EvidenceIDs) != 2 || b.ToSeqInclusive != 2 {
		t.Fatalf("补写后应连续到 seq 2: %+v err=%v", b, err)
	}
}

func TestCorruptArchiveIsRewrittenFromRaw(t *testing.T) {
	e, _ := newTestEngine(t)
	ref, err := e.Capture(rawEvent("uid-1"))
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSuffix(ref.RawLocator, "#1")
	if err := os.WriteFile(path, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Capture(rawEvent("uid-1")); err != nil {
		t.Fatalf("重放应修复坏 archive: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != ref.ArchiveHash {
		t.Fatalf("archive 应被原始字节重建")
	}
	rec, err := readArchive(path)
	if err != nil || string(rec.Raw) != string(rawEvent("uid-1").Raw) {
		t.Fatalf("重建后内容不对: %+v err=%v", rec, err)
	}
}

func TestBundleContinuityAndRotation(t *testing.T) {
	e, _ := newTestEngine(t)
	for _, uid := range []string{"g1-1", "g1-2"} {
		if _, err := e.Capture(rawEvent(uid)); err != nil {
			t.Fatal(err)
		}
	}
	b, err := e.NextBundle(10, 1<<20)
	if err != nil || b == nil || b.Generation != 1 || b.FromSeqExclusive != 0 || b.ToSeqInclusive != 2 {
		t.Fatalf("第一代窗口不对: %+v err=%v", b, err)
	}
	if _, err := e.RotateSource("src-a"); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"g2-1", "g2-2"} {
		if _, err := e.Capture(rawEvent(uid)); err != nil {
			t.Fatal(err)
		}
	}
	next, err := e.NextBundle(10, 1<<20)
	if err != nil || next == nil || next.Generation != 2 || next.FromSeqExclusive != 0 || next.ToSeqInclusive != 2 {
		t.Fatalf("轮转后应从新一代 0 出窗: %+v err=%v", next, err)
	}
	refs, err := e.ListEvidence(b.ID)
	if err != nil || len(refs) != 2 || refs[0].Generation != 1 {
		t.Fatalf("旧 bundle 仍可读: %+v err=%v", refs, err)
	}
	if refs[0].Seq != 1 || refs[1].Seq != 2 {
		t.Fatalf("ListEvidence 顺序不对: %+v", refs)
	}
}

func TestRotateRejectsBacklog(t *testing.T) {
	e, _ := newTestEngine(t)
	if _, err := e.Capture(rawEvent("uid-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RotateSource("src-a"); err == nil {
		t.Fatal("有未消费证据应拒绝轮转")
	}
	if _, err := e.RotateSource("missing"); err == nil {
		t.Fatal("未知源应报错")
	}
}

func TestListEvidenceUnknownBundle(t *testing.T) {
	e, _ := newTestEngine(t)
	if _, err := e.ListEvidence("bnd-nope"); err == nil {
		t.Fatal("未知 bundle 应报错")
	}
}

func TestBundleLimitFifty(t *testing.T) {
	e, _ := newTestEngine(t)
	for i := 0; i < 60; i++ {
		if _, err := e.Capture(rawEvent("uid-" + itoaTest(i))); err != nil {
			t.Fatal(err)
		}
	}
	b, err := e.NextBundle(0, 0)
	if err != nil || b == nil || len(b.EvidenceIDs) != DefaultBundleLimit {
		t.Fatalf("默认上限应 50: n=%d err=%v", len(b.EvidenceIDs), err)
	}
	if b.ToSeqInclusive != 50 {
		t.Fatalf("第一窗应到 50: %+v", b)
	}
	b2, err := e.NextBundle(0, 0)
	if err != nil || b2 == nil || len(b2.EvidenceIDs) != 10 || b2.ToSeqInclusive != 60 {
		t.Fatalf("第二窗应 10 条: n=%d err=%v", len(b2.EvidenceIDs), err)
	}
}

func TestBundleByteCap(t *testing.T) {
	e, _ := newTestEngine(t)
	for i := 0; i < 20; i++ {
		ev := rawEvent("big-" + itoaTest(i))
		ev.Raw = randomBytes(t, 8<<10)
		if _, err := e.Capture(ev); err != nil {
			t.Fatal(err)
		}
	}
	b, err := e.NextBundle(0, 0)
	if err != nil || b == nil {
		t.Fatalf("应有窗口: %+v err=%v", b, err)
	}
	if len(b.EvidenceIDs) >= 20 {
		t.Fatalf("64KB 上限应截断 8KB×20 的窗口, got %d", len(b.EvidenceIDs))
	}
	refs, err := e.ListEvidence(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, r := range refs {
		fi, err := os.Stat(strings.Split(r.RawLocator, "#")[0])
		if err != nil {
			t.Fatal(err)
		}
		total += fi.Size()
	}
	if total > DefaultBundleBytes {
		t.Fatalf("窗口字节应 ≤ %d，got %d", DefaultBundleBytes, total)
	}
	if len(refs) < 1 {
		t.Fatal("至少出一条")
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func itoaTest(n int) string {
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

func spoolSegments(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	dirs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			out = append(out, filepath.Join(root, d.Name(), f.Name()))
		}
	}
	return out
}

func TestSpoolDrainClaimsAndDeletes(t *testing.T) {
	e, _ := newTestEngine(t)
	ev := rawEvent("uid-spool")
	ev.Generation = 1
	ev.Seq = 1
	path, err := e.writeSpool(ev)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.DrainSpool(10)
	if err != nil || res.Drained != 1 || res.Files != 1 {
		t.Fatalf("drain: %+v err=%v", res, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("成功 drain 后 segment 应删除: %v", err)
	}
	b, err := e.NextBundle(10, 1<<20)
	if err != nil || b == nil || len(b.EvidenceIDs) != 1 {
		t.Fatalf("drain 后应可出窗: %+v err=%v", b, err)
	}
}

func TestSpoolRestoreOnFailure(t *testing.T) {
	e, db := newTestEngine(t)
	ev := rawEvent("uid-spool")
	ev.Generation = 1
	ev.Seq = 1
	if _, err := e.writeSpool(ev); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DrainSpool(10); err == nil {
		t.Fatal("DB 不可用时 drain 应报错")
	}
	files := spoolSegments(t, e.SpoolRoot())
	if len(files) != 1 || strings.HasSuffix(files[0], ".processing") {
		t.Fatalf("失败后应恢复原文件名: %v", files)
	}

	e2, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	res, err := e2.DrainSpool(10)
	if err != nil || res.Drained != 1 {
		t.Fatalf("恢复后应能重放: %+v err=%v", res, err)
	}
}

func TestSpoolBudgetRestoresRemainingLines(t *testing.T) {
	e, _ := newTestEngine(t)
	ev1 := rawEvent("uid-1")
	ev1.Generation, ev1.Seq = 1, 1
	ev2 := rawEvent("uid-2")
	ev2.Generation, ev2.Seq = 1, 2
	dir := filepath.Join(e.SpoolRoot(), spoolSession(ev1.SessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(recordOf(ev1))
	b2, _ := json.Marshal(recordOf(ev2))
	path := filepath.Join(dir, "seg-manual.jsonl")
	if err := os.WriteFile(path, append(append(b1, '\n'), append(b2, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.DrainSpool(1)
	if err != nil {
		t.Fatalf("预算耗尽不应报错: %v", err)
	}
	if res.Drained != 1 || res.Restored != 1 {
		t.Fatalf("预算用尽后应恢复剩余: %+v", res)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("未处理的行必须保留: %v", err)
	}
	res, err = e.DrainSpool(10)
	if err != nil || res.Drained != 2 || res.Files != 1 {
		t.Fatalf("续跑应完成: %+v err=%v", res, err)
	}
}

func TestSpoolStaleProcessingAndCorruptLine(t *testing.T) {
	e, _ := newTestEngine(t)
	dir := filepath.Join(e.SpoolRoot(), "sess-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ev := rawEvent("uid-proc")
	ev.Generation, ev.Seq = 1, 1
	b, _ := json.Marshal(recordOf(ev))
	body := append([]byte("not-json\n"), append(b, '\n')...)
	path := filepath.Join(dir, "seg-stale.jsonl.processing")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.DrainSpool(10)
	if err != nil || res.Drained != 1 || res.Dropped != 1 || res.Files != 1 {
		t.Fatalf("坏行应计数丢弃，残留 .processing 应被认领: %+v err=%v", res, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("成功后应删除 stale processing: %v", err)
	}
}

func TestSummarizeRawBounded(t *testing.T) {
	long := strings.Repeat("汉", 600)
	got := summarizeRaw([]byte(long), "post-tool-use")
	if len(got) > 512 || !utf8.ValidString(got) {
		t.Fatalf("摘要应截断到 512 内且 UTF-8 合法: %d", len(got))
	}
	if got := summarizeRaw([]byte("a\nb\tc\x00d"), "e"); got != "a b cd" {
		t.Fatalf("应折叠空白并去控制字符: %q", got)
	}
	if got := summarizeRaw(nil, "post-tool-use"); got != "post-tool-use" {
		t.Fatalf("空字节应回落事件类型: %q", got)
	}
	if got := summarizeRaw(nil, ""); got != "evidence" {
		t.Fatalf("全空应回落 evidence: %q", got)
	}
}

func TestSpoolSessionSanitized(t *testing.T) {
	cases := map[string]string{
		"":                       "unknown",
		"../escape":              "_escape",
		"a/b\\c:d":               "a_b_c_d",
		"..":                     "unknown",
		strings.Repeat("x", 120): strings.Repeat("x", 100),
	}
	for in, want := range cases {
		if got := spoolSession(in); got != want {
			t.Errorf("spoolSession(%q)=%q want %q", in, got, want)
		}
	}
}

func TestOpenRejectsBadPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err == nil {
		t.Fatal("对目录开库应失败")
	}
}

func TestReadArchiveErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.jsonl.gz")
	if err := os.WriteFile(bad, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readArchive(bad); err == nil {
		t.Fatal("非 gzip 应报错")
	}
	if _, err := readArchive(filepath.Join(dir, "missing.gz")); err == nil {
		t.Fatal("缺文件应报错")
	}

	notJSON := filepath.Join(dir, "notjson.jsonl.gz")
	f, err := os.Create(notJSON)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte("not json")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readArchive(notJSON); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

func TestSpoolRootBlocked(t *testing.T) {
	e, _ := newTestEngine(t)
	if err := os.WriteFile(e.SpoolRoot(), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.writeSpool(rawEvent("uid-x")); err == nil {
		t.Fatal("spool 根目录被文件占住应报错")
	}
	if _, err := e.DrainSpool(10); err == nil {
		t.Fatal("spool 根目录不可读应报错")
	}
}

func TestCaptureLookupErrorWithAutoIdentity(t *testing.T) {
	e, _ := newTestEngine(t)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Capture(rawEvent("uid-auto")); err == nil {
		t.Fatal("库不可用且身份未给全应报错")
	}
}

func TestClosedStoreErrors(t *testing.T) {
	e, _ := newTestEngine(t)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.NextBundle(10, 1<<20); err == nil {
		t.Fatal("库关闭后 NextBundle 应报错")
	}
	if _, err := e.ListEvidence("bnd-x"); err == nil {
		t.Fatal("库关闭后 ListEvidence 应报错")
	}
}

func TestRestoreSpoolKeepsClaimWhenTargetExists(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "seg-a.jsonl")
	claimed := base + ".processing"
	for _, p := range []string{base, claimed} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restoreSpool(claimed)
	if _, err := os.Stat(claimed); err != nil {
		t.Fatalf("目标名被占用时应保留 .processing: %v", err)
	}
	restoreSpool(base) // 没有 .processing 后缀：no-op
	if _, err := os.Stat(base); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureReplayWithDifferentRawKeepsFirstBytes(t *testing.T) {
	e, _ := newTestEngine(t)
	first, err := e.Capture(rawEvent("uid-1"))
	if err != nil {
		t.Fatal(err)
	}
	changed := rawEvent("uid-1")
	changed.Raw = []byte("换了字节")
	again, err := e.Capture(changed)
	if err != nil {
		t.Fatal(err)
	}
	if again.EventUID != first.EventUID || again.ArchiveHash != first.ArchiveHash || again.RawRecordHash != first.RawRecordHash {
		t.Fatalf("先写赢，不应被不同字节改写: first=%+v again=%+v", first, again)
	}
}

func TestWriteArchiveReportsBrokenTarget(t *testing.T) {
	e, _ := newTestEngine(t)
	ev := rawEvent("uid-1")
	ev.Generation, ev.Seq = 1, 1
	seg, err := e.writeArchive(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(seg.path); err != nil {
		t.Fatal(err)
	}
	// 目标路径变成目录：校验既有文件时读失败，必须报错而不是继续引用。
	if err := os.Mkdir(seg.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.writeArchive(ev); err == nil {
		t.Fatal("目标不可读时应报错")
	}
}

// ---------- T2：Fidelity 捕获层透传与归档幂等回归 ----------

func TestResolveFidelity(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want Fidelity
	}{
		{"有内容", []byte("原始字节"), FidelityFull},
		{"带前后空白", []byte("  x  "), FidelityFull},
		{"nil", nil, FidelityNone},
		{"空切片", []byte{}, FidelityNone},
		{"纯空白", []byte("  \n\t\r "), FidelityNone},
	}
	for _, tc := range cases {
		if got := resolveFidelity(tc.raw); got != tc.want {
			t.Errorf("resolveFidelity(%s)=%q want %q", tc.name, got, tc.want)
		}
	}
}

func TestToRefCarriesFidelity(t *testing.T) {
	cases := []struct {
		in   string
		want Fidelity
	}{
		{"full", FidelityFull},
		{"summary", FidelitySummary},
		{"pointer", FidelityPointer},
		{"none", FidelityNone},
		{"", ""},
	}
	for _, tc := range cases {
		rec := store.EvidenceRecord{Fidelity: tc.in}
		if got := toRef(rec).Fidelity; got != tc.want {
			t.Errorf("toRef(Fidelity=%q).Fidelity=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestCaptureFidelityRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		fidelity Fidelity
		want     Fidelity // 显式传入或空值兜底后的期望
		nilRaw   bool
	}{
		{"full", FidelityFull, FidelityFull, false},
		{"summary", FidelitySummary, FidelitySummary, false},
		{"pointer", FidelityPointer, FidelityPointer, false},
		{"none", FidelityNone, FidelityNone, false},
		{"empty-default-full", "", FidelityFull, false}, // 空值 + 有 Raw → full
		{"empty-nil-raw-none", "", FidelityNone, true},  // 空值 + 无 Raw → none
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newTestEngine(t)
			ev := rawEvent("uid-" + tc.name)
			ev.Fidelity = tc.fidelity
			if tc.nilRaw {
				ev.Raw = nil
			}
			ref, err := e.Capture(ev)
			if err != nil {
				t.Fatal(err)
			}
			b, err := e.NextBundle(10, 1<<20)
			if err != nil || b == nil {
				t.Fatalf("出窗失败: b=%+v err=%v", b, err)
			}
			refs, err := e.ListEvidence(b.ID)
			if err != nil || len(refs) != 1 {
				t.Fatalf("读回失败: refs=%+v err=%v", refs, err)
			}
			// 核心不变量：Capture 返回的 Ref 与 ListEvidence 读回一致。
			if ref.Fidelity != refs[0].Fidelity {
				t.Fatalf("Ref.Fidelity 与 ListEvidence 不一致: ref=%q listed=%q",
					ref.Fidelity, refs[0].Fidelity)
			}
			// store 层回填 fidelity 后（T1 合入 M1），值应等于传入 / 兜底后的期望；
			// 纯 W0 基线 store 不回填时两者均为空，上面的一致性仍成立。
			if ref.Fidelity != "" && ref.Fidelity != tc.want {
				t.Fatalf("Ref.Fidelity=%q want %q", ref.Fidelity, tc.want)
			}
		})
	}
}

func TestCaptureReplayFidelityKeepsArchiveHash(t *testing.T) {
	e, _ := newTestEngine(t)
	firstEv := rawEvent("uid-fid-replay")
	firstEv.Fidelity = FidelityFull
	first, err := e.Capture(firstEv)
	if err != nil {
		t.Fatal(err)
	}
	// 同一 event_uid 用不同 Fidelity 重放：Fidelity 不进归档 segment，
	// archive_hash 必须逐字节不变，且不得报「归档无法按库内身份重建」。
	// 若 recordOf/segmentRecord 误加了 Fidelity，这里会算出的 seg.hash 与库内
	// ArchiveHash 失配而报错，从而把归档幂等回归钉死。
	for _, fid := range []Fidelity{FidelityNone, FidelityPointer, FidelitySummary, ""} {
		ev := rawEvent("uid-fid-replay")
		ev.Fidelity = fid
		again, err := e.Capture(ev)
		if err != nil {
			t.Fatalf("重放 Fidelity=%q 失败（归档哈希是否失配？）: %v", fid, err)
		}
		if again.ArchiveHash != first.ArchiveHash {
			t.Fatalf("Fidelity 不应影响归档哈希: first=%s again=%s (fid=%q)",
				first.ArchiveHash, again.ArchiveHash, fid)
		}
		if again.Fidelity != first.Fidelity {
			t.Fatalf("重放应返回首条身份的 Fidelity: first=%q again=%q",
				first.Fidelity, again.Fidelity)
		}
	}
}
