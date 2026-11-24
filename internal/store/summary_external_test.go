package store

// 外置 summary/summary_idx 的读写、迁移与 FTS 契约测试。

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSummaryLineCodecAndHash(t *testing.T) {
	line := SummaryLine{V: 1, Seq: 7, ID: "ev-7", Summary: "中文摘要", SummaryIdx: "中 文 中文"}
	b, err := EncodeSummaryLine(line)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSummaryLine(b)
	if err != nil || got != line {
		t.Fatalf("往返不一致: %+v err=%v", got, err)
	}
	if h := SummaryLineHash(b); h != SummaryLineHash(b) || len(h) != 64 {
		t.Fatalf("hash 不可复现: %q", h)
	}
	// 字段顺序固定：同一结构两次编码必须逐字节一致。
	b2, _ := EncodeSummaryLine(line)
	if !bytes.Equal(b, b2) {
		t.Fatal("规范 JSON 字节不稳定")
	}
	if _, err := ParseSummaryLine([]byte("{")); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

func TestAppendAndReadSummaryLine(t *testing.T) {
	root := t.TempDir()
	ts := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local).Unix()
	line := SummaryLine{V: 1, Seq: 1, ID: "ev-1", Summary: "first"}
	ref, hash, n, err := AppendSummaryLine(root, ts, line)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ref, "#1") || n != len(`{"v":1,"seq":1,"id":"ev-1","summary":"first","summary_idx":""}`) {
		t.Fatalf("ref/payload 不对: %s %d", ref, n)
	}
	got, err := ReadSummaryLine(root, ref, hash)
	if err != nil || got.Summary != "first" {
		t.Fatalf("读回失败: %+v %v", got, err)
	}
	// 第二个 member 追加后，第一行的行级 hash 仍稳定。
	line2 := SummaryLine{V: 1, Seq: 2, ID: "ev-2", Summary: "second"}
	ref2, hash2, _, err := AppendSummaryLine(root, ts, line2)
	if err != nil {
		t.Fatal(err)
	}
	if ref2 == ref || hash2 == hash {
		t.Fatal("两行应有不同 ref/hash")
	}
	if _, err := ReadSummaryLine(root, ref, hash); err != nil {
		t.Fatalf("append 后旧行应仍可读: %v", err)
	}
	if _, err := ReadSummaryLine(root, ref2, hash2); err != nil {
		t.Fatalf("新行应可读: %v", err)
	}

	cases := []struct{ name, ref, hash string }{
		{"缺 #seq", "content/2026/11/24/observations.jsonl.gz", hash},
		{"seq 非法", strings.Replace(ref, "#1", "#x", 1), hash},
		{"seq 为 0", strings.Replace(ref, "#1", "#0", 1), hash},
		{"空 locator", "   ", hash},
		{"缺 hash", ref, ""},
		{"hash 不匹配", ref, strings.Repeat("0", 64)},
		{"seq 不存在", strings.Replace(ref, "#1", "#99", 1), hash},
		{"文件不存在", "content/1999/01/01/observations.jsonl.gz#1", hash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadSummaryLine(root, tc.ref, tc.hash); err == nil {
				t.Fatal("坏引用应报错")
			}
		})
	}
	if _, _, _, err := AppendSummaryLine(root, ts, SummaryLine{Seq: 0}); err == nil {
		t.Fatal("seq=0 应报错")
	}
	blocked := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := AppendSummaryLine(blocked, ts, SummaryLine{V: 1, Seq: 1, ID: "x"}); err == nil {
		t.Fatal("根目录是文件时应报错")
	}
}

func TestWriteSummaryDayAndSeq(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	ts := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local).Unix()
	if refs, hashes, payloads, err := WriteSummaryDay(root, ts, nil); err != nil || len(refs) != 0 || len(hashes) != 0 || len(payloads) != 0 {
		t.Fatalf("空批次应无副作用: %v %v %v %v", refs, hashes, payloads, err)
	}
	lines := []SummaryLine{
		{V: 1, Seq: 1, ID: "a", Summary: "甲"},
		{V: 1, Seq: 2, ID: "b", Summary: "乙"},
	}
	refs, hashes, payloads, err := WriteSummaryDay(root, ts, lines)
	if err != nil || len(refs) != 2 || payloads[0] <= 0 {
		t.Fatalf("写日分片失败: %v %v %v %v", refs, hashes, payloads, err)
	}
	for i, ref := range refs {
		if _, err := ReadSummaryLine(root, ref, hashes[i]); err != nil {
			t.Fatalf("第 %d 行读回失败: %v", i, err)
		}
	}
	// shortcut: WriteSummaryDay 用 os.Create，调用方（迁移）必须先检查分片不存在。
	for i := 0; i < 3; i++ {
		seq, err := NextSummarySeq(s.db, "2026/11/24")
		if err != nil || seq != i+1 {
			t.Fatalf("seq %d: got %d err=%v", i, seq, err)
		}
	}
	if err := SetSummarySeq(s.db, "2026/11/24", 9); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := s.db.QueryRow(`SELECT seq FROM summary_seq WHERE day='2026/11/24'`).Scan(&got); err != nil || got != 9 {
		t.Fatalf("SetSummarySeq 失败: %d %v", got, err)
	}
	if _, err := dayStartUnix("bad-day"); err == nil {
		t.Fatal("非法日期应报错")
	}
	if dbPages(s.db) <= 0 || fileSize(filepath.Join(root, "gone")) != 0 {
		t.Fatal("体积辅助函数行为不对")
	}
	if len(sortedDays(map[string][]SummaryLine{"b": nil, "a": nil})) != 2 {
		t.Fatal("sortedDays 应返回全部日期")
	}
}

func TestAppendObservationExternalAndLegacy(t *testing.T) {
	legacy := newTestStore(t)
	res, err := legacy.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "旧路径"})
	if err != nil {
		t.Fatal(err)
	}
	var sum, ref string
	if err := legacy.db.QueryRow(`SELECT summary, summary_ref FROM observations WHERE id=?`, res.ID).Scan(&sum, &ref); err != nil {
		t.Fatal(err)
	}
	if sum != "旧路径" || ref != "" {
		t.Fatalf("legacy 应保留库内摘要: %q %q", sum, ref)
	}

	s := newTestStore(t)
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS fts_obs`); err != nil {
		t.Fatal(err)
	}
	if err := ensureStandaloneObsFTS(s.db); err != nil {
		t.Fatal(err)
	}
	s.summaryExternal = true
	res, err = s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "新路径"})
	if err != nil {
		t.Fatal(err)
	}
	var sumIdx, extRef, extHash string
	var payload int
	if err := s.db.QueryRow(`SELECT summary, summary_idx, summary_ref, summary_hash, payload_bytes
		FROM observations WHERE id=?`, res.ID).Scan(&sumIdx, &sumIdx, &extRef, &extHash, &payload); err != nil {
		t.Fatal(err)
	}
	if sumIdx != "" || extRef == "" || extHash == "" || payload == 0 {
		t.Fatalf("外置写入留库字段不对: %q %q %q %d", sumIdx, extRef, extHash, payload)
	}
	line, err := ReadSummaryLine(SummaryContentRoot(s.path), extRef, extHash)
	if err != nil || line.Summary != "新路径" {
		t.Fatalf("外置摘要读回失败: %+v %v", line, err)
	}
	var ftsN int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM fts_obs WHERE obs_id=?`, res.ID).Scan(&ftsN); err != nil || ftsN != 1 {
		t.Fatalf("独立 FTS 未写入: %d %v", ftsN, err)
	}
	hits, err := s.Search(SearchQuery{Query: "新路径", Layer: "observation"})
	if err != nil || len(hits) == 0 || hits[0].SummaryRef == "" {
		t.Fatalf("检索应带出 ref 供 hydrate: %+v %v", hits, err)
	}
}

func TestCaptureEvidenceExternal(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS fts_obs`); err != nil {
		t.Fatal(err)
	}
	if err := ensureStandaloneObsFTS(s.db); err != nil {
		t.Fatal(err)
	}
	s.summaryExternal = true
	rec := mustCaptureEvidence(t, s, evidenceInput("uid-ext", 0))
	if rec.SummaryRef == "" || rec.SummaryHash == "" || rec.Summary != "summary-uid-ext" {
		t.Fatalf("证据外置字段不对: %+v", rec)
	}
	line, err := ReadSummaryLine(SummaryContentRoot(s.path), rec.SummaryRef, rec.SummaryHash)
	if err != nil || line.Summary != rec.Summary {
		t.Fatalf("证据摘要读回失败: %+v %v", line, err)
	}
}

func TestMigrateSummaryExternalLifecycle(t *testing.T) {
	s := newTestStore(t)
	for _, text := range []string{"迁移一", "迁移二", "迁移三"} {
		if _, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: text}); err != nil {
			t.Fatal(err)
		}
	}
	dry, err := s.MigrateSummaryExternal(true)
	if err != nil || dry.Migrated != 3 || dry.Shards < 1 {
		t.Fatalf("dry-run 统计不对: %+v %v", dry, err)
	}
	if _, err := os.Stat(filepath.Join(SummaryContentRoot(s.path), "content")); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不应创建 content/: %v", err)
	}
	stats, err := s.MigrateSummaryExternal(false)
	if err != nil || stats.Migrated != 3 {
		t.Fatalf("迁移失败: %+v %v", stats, err)
	}
	if !s.summaryExternal {
		t.Fatal("迁移后 Store 应切到外置模式")
	}
	var emptyRows, refRows, ftsN int
	for _, q := range []struct {
		sql string
		out *int
	}{
		{`SELECT COUNT(*) FROM observations WHERE summary='' AND summary_idx='' AND summary_ref<>''`, &emptyRows},
		{`SELECT COUNT(*) FROM observations WHERE summary_ref<>''`, &refRows},
		{`SELECT COUNT(*) FROM fts_obs`, &ftsN},
	} {
		if err := s.db.QueryRow(q.sql).Scan(q.out); err != nil {
			t.Fatal(err)
		}
	}
	if emptyRows != 3 || refRows != 3 || ftsN != 3 {
		t.Fatalf("迁移后行数不对: empty=%d ref=%d fts=%d", emptyRows, refRows, ftsN)
	}
	if !summaryExternalMigrated(s.db) {
		t.Fatal("迁移完成标记缺失")
	}
	var marker string
	if err := s.db.QueryRow(`SELECT v FROM db_meta WHERE k='summary_external_version'`).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("版本标记不对: %q %v", marker, err)
	}
	again, err := s.MigrateSummaryExternal(false)
	if err != nil || !again.AlreadyDone || again.Migrated != 0 {
		t.Fatalf("重复迁移应幂等: %+v %v", again, err)
	}
	if stats.PagesAfter > stats.PagesBefore {
		t.Fatalf("迁移后 DB 不应变大: %d -> %d", stats.PagesBefore, stats.PagesAfter)
	}

	// 迁移后 GC 必须同步删独立 FTS 行。
	if _, err := s.GCObservations(time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM fts_obs`).Scan(&ftsN); err != nil || ftsN != 1 {
		t.Fatalf("GC 后 FTS 行数应剩 1: %d %v", ftsN, err)
	}
}

func TestSummaryExternalErrorBranches(t *testing.T) {
	ts := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local).Unix()

	t.Run("closed db", func(t *testing.T) {
		s := newTestStore(t)
		_ = s.Close()
		if _, err := s.MigrateSummaryExternal(true); err == nil {
			t.Fatal("closed db 迁移应报错")
		}
		if _, err := NextSummarySeq(s.db, "2026/11/24"); err == nil {
			t.Fatal("NextSummarySeq 应报错")
		}
		if err := ensureSummaryExternalSchema(s.db); err == nil {
			t.Fatal("ensureSummaryExternalSchema 应报错")
		}
		if err := ensureStandaloneObsFTS(s.db); err == nil {
			t.Fatal("ensureStandaloneObsFTS 应报错")
		}
		if err := obsFTSDelete(s.db, "ev"); err == nil {
			t.Fatal("obsFTSDelete 应报错")
		}
		if err := SetSummarySeq(s.db, "2026/11/24", 1); err == nil {
			t.Fatal("SetSummarySeq 应报错")
		}
		if dbPages(s.db) != 0 {
			t.Fatal("closed db 的体积查询应返回 0")
		}
	})

	t.Run("bad paths", func(t *testing.T) {
		root := t.TempDir()
		blocked := filepath.Join(root, "file")
		if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		lines := []SummaryLine{{V: 1, Seq: 1, ID: "x"}}
		if _, _, _, err := WriteSummaryDay(blocked, ts, lines); err == nil {
			t.Fatal("WriteSummaryDay 根目录是文件时应报错")
		}
		if got := resolveSummaryPath("/ignored", "/tmp/abs.jsonl.gz#1"); got != "/tmp/abs.jsonl.gz#1" {
			t.Fatalf("绝对路径应直接使用: %q", got)
		}
		rel := filepath.ToSlash(dayRelPath(ts))
		corrupt := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(corrupt), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(corrupt, []byte("not gzip"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSummaryLine(root, rel+"#1", strings.Repeat("a", 64)); err == nil {
			t.Fatal("坏 gzip 应报错")
		}
	})

	t.Run("no pending and existing shard", func(t *testing.T) {
		empty := newTestStore(t)
		stats, err := empty.MigrateSummaryExternal(false)
		if err != nil || stats.Migrated != 0 {
			t.Fatalf("空库迁移应无操作: %+v %v", stats, err)
		}
		s := newTestStore(t)
		if _, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "待迁"}); err != nil {
			t.Fatal(err)
		}
		day := time.Unix(time.Now().Unix(), 0).Format("2006/01/02")
		path := filepath.Join(SummaryContentRoot(s.path), "content", day, "observations.jsonl.gz")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("残留"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MigrateSummaryExternal(false); err == nil {
			t.Fatal("分片已存在时必须拒绝，不能覆写")
		}
	})
}

func TestGCObservationsAgeBranch(t *testing.T) {
	s := newTestStore(t)
	old := time.Now().Add(-72 * time.Hour).Unix()
	if _, err := s.db.Exec(`INSERT INTO observations
		(id, session_id, event, summary, content_hash, created_at)
		VALUES ('old-1','s','stop','旧观测','hash-old',?)`, old); err != nil {
		t.Fatal(err)
	}
	res, err := s.GCObservations(24*time.Hour, 100)
	if err != nil || res.Deleted != 1 || res.Kept != 0 {
		t.Fatalf("按年龄回收失败: %+v %v", res, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("旧行应被删除: %d %v", n, err)
	}
}

func TestLegacyObservationSearchBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDB(t, path)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE observations SET summary_idx = ''`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var filled, ftsN int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE summary <> '' AND summary_idx <> ''`).Scan(&filled); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM fts_obs`).Scan(&ftsN); err != nil {
		t.Fatal(err)
	}
	if filled == 0 || ftsN != filled {
		t.Fatalf("legacy 回填失败: filled=%d fts=%d", filled, ftsN)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
}

func TestAppendObservationFallbackSummaries(t *testing.T) {
	s := newTestStore(t)
	first, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "post-tool-use"})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.db.QueryRow(`SELECT summary FROM observations WHERE id=?`, first.ID).Scan(&got); err != nil || got != "post-tool-use" {
		t.Fatalf("空摘要应回退事件名: %q %v", got, err)
	}
	second, err := s.AppendObservation(ObsInput{SessionID: "s", EventID: "evt-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT summary FROM observations WHERE id=?`, second.ID).Scan(&got); err != nil || got != "observe" {
		t.Fatalf("摘要与事件都空应回退 observe: %q %v", got, err)
	}
	replay, err := s.AppendObservation(ObsInput{SessionID: "s", EventID: "evt-1", Summary: "不该覆盖"})
	if err != nil || replay.ID != second.ID || !replay.Duplicate {
		t.Fatalf("event_id 重放应返回原行: %+v %v", replay, err)
	}
}

func TestAppendObservationRejectsBadChannel(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Channel: "not-a-channel"}); err == nil {
		t.Fatal("非法 channel 应报错")
	}
}

func TestObsFilterProjectBranch(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendObservation(ObsInput{SessionID: "s", Project: "proj-x", Event: "stop", Summary: "项目限定词"}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(SearchQuery{Query: "项目限定词", Project: "proj-x", Layer: "observation"})
	if err != nil || len(hits) == 0 {
		t.Fatalf("项目过滤分支应能命中: %+v %v", hits, err)
	}
}

func TestMigrateSummaryExternalRejectsBrokenContentRoot(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "待迁"}); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(SummaryContentRoot(s.path), "content")
	if err := os.WriteFile(blocked, []byte("not a dir"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigrateSummaryExternal(false); err == nil {
		t.Fatal("content 路径不是目录时应拒绝迁移")
	}
}

func TestAppendObservationEventIDLookupError(t *testing.T) {
	s := newTestStore(t)
	_ = s.Close()
	if _, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", EventID: "evt-err"}); err == nil {
		t.Fatal("event_id 查询失败时应报错")
	}
}
