package store

import (
	"errors"
	"testing"
)

func evidenceInput(uid string, seq int) EvidenceCaptureInput {
	return EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-a", SourceKind: "hook", Generation: 1, Seq: seq,
		OccurredAtNS: 1700000000000000000, SessionID: "sess-1", Project: "proj",
		Harness: "workbuddy", EventType: "post-tool-use", ParserVersion: "evidence/v1",
		ProposalEligible: true, Summary: "summary-" + uid,
		ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
		RawRecordHash: "rh-" + uid, RawHashConcat: "rh-" + uid, ByteSize: 32, RawCount: 1,
	}
}

func mustCaptureEvidence(t *testing.T, s *Store, in EvidenceCaptureInput) EvidenceRecord {
	t.Helper()
	rec, created, err := s.CaptureEvidence(in)
	if err != nil {
		t.Fatalf("CaptureEvidence(%s): %v", in.EventUID, err)
	}
	if !created {
		t.Fatalf("CaptureEvidence(%s) 首次应写入", in.EventUID)
	}
	return rec
}

func TestCaptureEvidenceWritesLedgerAndCanonicalRow(t *testing.T) {
	s := newTestStore(t)
	first := mustCaptureEvidence(t, s, evidenceInput("uid-1", 0))
	if first.Seq != 1 || first.Generation != 1 || first.SourceID != "src-a" {
		t.Fatalf("首次 capture 身份不对: %+v", first)
	}
	if first.Summary != "summary-uid-1" || first.RawLocator != "/tmp/uid-1.gz#1" {
		t.Fatalf("摘要或 locator 不对: %+v", first)
	}
	if !first.ProposalEligible || first.ParserVersion != "evidence/v1" {
		t.Fatalf("meta 未持久化: %+v", first)
	}

	var (
		eventUID, sourceID, event, channel, archiveHash, rawRef string
		gen, seq                                                int
	)
	if err := s.db.QueryRow(`SELECT event_uid, source_id, source_generation, source_seq,
			event, channel, archive_hash, raw_ref FROM observations WHERE id = ?`, first.ID).
		Scan(&eventUID, &sourceID, &gen, &seq, &event, &channel, &archiveHash, &rawRef); err != nil {
		t.Fatal(err)
	}
	if eventUID != "uid-1" || sourceID != "src-a" || gen != 1 || seq != 1 ||
		event != "post-tool-use" || channel != ChannelTool || archiveHash != "ah-uid-1" {
		t.Fatalf("canonical 行不对: uid=%s src=%s gen=%d seq=%d event=%s ch=%s ah=%s ref=%s",
			eventUID, sourceID, gen, seq, event, channel, archiveHash, rawRef)
	}

	var lastSeq, cursorSeq int
	var tip string
	if err := s.db.QueryRow(`SELECT last_seq, cursor_seq, source_tip FROM evidence_sources WHERE id='src-a'`).
		Scan(&lastSeq, &cursorSeq, &tip); err != nil {
		t.Fatal(err)
	}
	if lastSeq != 1 || cursorSeq != 0 || tip == "" {
		t.Fatalf("capture 只推进 last_seq/tip: last=%d cursor=%d tip=%q", lastSeq, cursorSeq, tip)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM evidence_archives WHERE archive_hash='ah-uid-1'
		AND from_seq=1 AND to_seq=1 AND raw_count=1 AND byte_size=32`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("archive 账应有 1 行，got %d", count)
	}

	second := mustCaptureEvidence(t, s, evidenceInput("uid-2", 0))
	if second.Seq != 2 {
		t.Fatalf("seq 应自增到 2，got %d", second.Seq)
	}
}

func TestCaptureEvidenceReplaysByIdentity(t *testing.T) {
	s := newTestStore(t)
	first := mustCaptureEvidence(t, s, evidenceInput("uid-1", 0))

	// event_uid 重放：即使 seq 不同也返回原行。
	replay := evidenceInput("uid-1", 9)
	replay.Summary = "换个摘要"
	rec, created, err := s.CaptureEvidence(replay)
	if err != nil {
		t.Fatal(err)
	}
	if created || rec.ID != first.ID || rec.Summary != first.Summary {
		t.Fatalf("uid 重放应先写赢: created=%v %+v", created, rec)
	}

	// (source, gen, seq) 重放：另一个 event_uid 占同一位置，也被幂等挡回。
	posReplay := evidenceInput("uid-other", 1)
	posReplay.ArchiveHash = "ah-other"
	rec, created, err = s.CaptureEvidence(posReplay)
	if err != nil {
		t.Fatal(err)
	}
	if created || rec.EventUID != "uid-1" || rec.ID != first.ID {
		t.Fatalf("位置重放应返回既有行: created=%v %+v", created, rec)
	}

	// 同摘要不同 event_uid、不同 seq 必须共存（不走 content_hash 去重）。
	sameSummary := evidenceInput("uid-2", 0)
	sameSummary.Summary = first.Summary
	second := mustCaptureEvidence(t, s, sameSummary)
	if second.Seq != 2 || second.ID == first.ID {
		t.Fatalf("同摘要不同 uid 应共存: %+v", second)
	}
	if n := migrationCount(t, s, "observations"); n != 2 {
		t.Fatalf("应有两行证据，got %d", n)
	}

	// 全量重放之后再读，仍是原行。
	rec, created, err = s.CaptureEvidence(evidenceInput("uid-1", 1))
	if err != nil || created || rec.ID != first.ID {
		t.Fatalf("二次重放: created=%v err=%v %+v", created, err, rec)
	}
}

func TestCaptureEvidenceRejectsBadIdentity(t *testing.T) {
	s := newTestStore(t)
	mustCaptureEvidence(t, s, evidenceInput("uid-1", 0))

	cases := []struct {
		name string
		in   EvidenceCaptureInput
		want error
	}{
		{"跳号", evidenceInput("uid-gap", 5), ErrEvidenceSeqGap},
		{"补历史", evidenceInput("uid-old", 1), nil}, // 位置已存在 → 幂等返回
		{"空 uid", EvidenceCaptureInput{SourceID: "s", ArchiveHash: "a"}, nil},
		{"空 source", EvidenceCaptureInput{EventUID: "u", ArchiveHash: "a"}, nil},
		{"空 archive", EvidenceCaptureInput{EventUID: "u", SourceID: "s"}, nil},
	}
	for _, tc := range cases {
		_, _, err := s.CaptureEvidence(tc.in)
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v want %v", tc.name, err, tc.want)
			}
			continue
		}
		if tc.name == "补历史" {
			if err != nil {
				t.Errorf("补历史应走位置幂等，got %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 应报错", tc.name)
		}
	}

	// generation 不是当前代：拒绝写入新证据。
	badGen := evidenceInput("uid-gen", 2)
	badGen.Generation = 2
	if _, _, err := s.CaptureEvidence(badGen); !errors.Is(err, ErrEvidenceGenerationMismatch) {
		t.Fatalf("generation 不匹配应拒绝: %v", err)
	}
}

func TestCaptureEvidenceRollbackLeavesNoRows(t *testing.T) {
	s := newTestStore(t)
	// archive_hash 为空在校验阶段就被挡住，先写入的行不应存在。
	in := evidenceInput("uid-bad", 1)
	in.ArchiveHash = ""
	if _, _, err := s.CaptureEvidence(in); err == nil {
		t.Fatal("空 archive_hash 应报错")
	}
	if n := migrationCount(t, s, "evidence_sources"); n != 0 {
		t.Fatalf("失败 capture 不应留 source: %d", n)
	}
	if n := migrationCount(t, s, "observations"); n != 0 {
		t.Fatalf("失败 capture 不应留 observation: %d", n)
	}
}

func TestCaptureEvidenceObservesDistillWatermark(t *testing.T) {
	s := newTestStore(t)
	for i := 1; i <= 3; i++ {
		mustCaptureEvidence(t, s, evidenceInput("uid-wm-"+itoa(i), 0))
	}
	// 旧蒸馏窗口水位不能影响证据游标：插入一条“已完成”的蒸馏记录后照常出窗。
	if _, err := s.db.Exec(`INSERT INTO distill_runs(
		id, session_id, provider, status, cand_count, started_at, finished_at, window_key
	) VALUES ('run-1','sess-1','local','ok',0,1,9999999999,'完成到未来')`); err != nil {
		t.Fatal(err)
	}
	b, err := s.NextEvidenceBundle(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || len(b.EvidenceIDs) != 3 || b.ToSeqInclusive != 3 {
		t.Fatalf("应拿到全部 3 条证据，got %+v", b)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestNextEvidenceBundleWindowsAndLimits(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 55; i++ {
		mustCaptureEvidence(t, s, evidenceInput("uid-"+itoa(i), 0))
	}
	b, err := s.NextEvidenceBundle(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || len(b.EvidenceIDs) != 50 || b.FromSeqExclusive != 0 || b.ToSeqInclusive != 50 {
		t.Fatalf("默认窗口应 50 条 [0,50]: %+v", b)
	}
	if b.Status != "pending" || b.EvidenceHash == "" || b.SourceTip == "" {
		t.Fatalf("bundle 元数据缺失: %+v", b)
	}
	for i, id := range b.EvidenceIDs {
		if id == "" {
			t.Fatalf("第 %d 条证据 id 为空", i)
		}
	}

	refs, err := s.ListEvidenceRecords(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 50 || refs[0].Seq != 1 || refs[49].Seq != 50 {
		t.Fatalf("ListEvidence 顺序/数量不对: n=%d", len(refs))
	}
	if refs[0].ID != b.EvidenceIDs[0] {
		t.Fatalf("item 顺序与 bundle 不一致")
	}

	b2, err := s.NextEvidenceBundle(100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b2 == nil || len(b2.EvidenceIDs) != 5 || b2.FromSeqExclusive != 50 || b2.ToSeqInclusive != 55 {
		t.Fatalf("第二窗应为 5 条 [50,55]: %+v", b2)
	}
	b3, err := s.NextEvidenceBundle(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b3 != nil {
		t.Fatalf("没有剩余证据应返回 nil: %+v", b3)
	}
	if _, err := s.ListEvidenceRecords("bnd-missing"); !errors.Is(err, ErrEvidenceBundleNotFound) {
		t.Fatalf("未知 bundle 应报 NotFound: %v", err)
	}
}

func TestNextEvidenceBundleByteCap(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 20; i++ {
		in := evidenceInput("uid-b-"+itoa(i), 0)
		in.ByteSize = 10 << 10
		mustCaptureEvidence(t, s, in)
	}
	b, err := s.NextEvidenceBundle(50, 30<<10)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || len(b.EvidenceIDs) != 3 || b.ToSeqInclusive != 3 {
		t.Fatalf("30KB 应只装 3 条: %+v", b)
	}

	// 单条就超上限也必须出 1 条，否则永远卡在同一个位置。
	s2 := newTestStore(t)
	in := evidenceInput("uid-huge", 0)
	in.ByteSize = 1 << 20
	mustCaptureEvidence(t, s2, in)
	b, err = s2.NextEvidenceBundle(50, 64<<10)
	if err != nil || b == nil || len(b.EvidenceIDs) != 1 {
		t.Fatalf("超大单条应至少出 1 条: %+v err=%v", b, err)
	}
}

func TestNextEvidenceBundleStopsAtGapAndSkipsToOtherSource(t *testing.T) {
	s := newTestStore(t)
	mustCaptureEvidence(t, s, evidenceInput("uid-a1", 0))
	mustCaptureEvidence(t, s, evidenceInput("uid-a2", 0))
	if _, err := s.db.Exec(`DELETE FROM observations WHERE event_uid='uid-a2'`); err != nil {
		t.Fatal(err)
	}
	// A 源 cursor=0，seq 1 之后缺 2：不能跳过空洞把 3.. 发出去。
	b, err := s.NextEvidenceBundle(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.ToSeqInclusive != 1 || b.SourceID != "src-a" {
		t.Fatalf("A 源应只发到空洞前: %+v", b)
	}
	next, err := s.NextEvidenceBundle(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("空洞未补齐前不应出窗: %+v", next)
	}

	// 另一个源没有空洞，应能继续出窗（遍历多个源，但绝不跨源跳号）。
	inB := evidenceInput("uid-b1", 0)
	inB.SourceID = "src-b"
	mustCaptureEvidence(t, s, inB)
	b, err = s.NextEvidenceBundle(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.SourceID != "src-b" || b.ToSeqInclusive != 1 {
		t.Fatalf("应轮到 B 源: %+v", b)
	}
}

func TestRotateEvidenceSource(t *testing.T) {
	s := newTestStore(t)
	mustCaptureEvidence(t, s, evidenceInput("uid-1", 0))
	if _, err := s.RotateEvidenceSource("src-a"); !errors.Is(err, ErrEvidenceSourceBacklog) {
		t.Fatalf("有未消费证据应拒绝轮转: %v", err)
	}
	if _, err := s.RotateEvidenceSource("nope"); !errors.Is(err, ErrEvidenceSourceNotFound) {
		t.Fatalf("未知源应报 NotFound: %v", err)
	}

	if _, err := s.NextEvidenceBundle(10, 1<<20); err != nil {
		t.Fatal(err)
	}
	gen, err := s.RotateEvidenceSource("src-a")
	if err != nil || gen != 2 {
		t.Fatalf("轮转应到第 2 代: gen=%d err=%v", gen, err)
	}
	in := evidenceInput("uid-g2", 0)
	in.Generation = 0
	rec := mustCaptureEvidence(t, s, in)
	if rec.Generation != 2 || rec.Seq != 1 {
		t.Fatalf("新一代序号应归零: %+v", rec)
	}
	b, err := s.NextEvidenceBundle(10, 1<<20)
	if err != nil || b == nil || b.Generation != 2 || b.FromSeqExclusive != 0 || b.ToSeqInclusive != 1 {
		t.Fatalf("新一代应从 0 出窗: %+v err=%v", b, err)
	}
}

func TestEvidenceSourceHeadMissing(t *testing.T) {
	s := newTestStore(t)
	st, ok, err := s.EvidenceSourceHead("absent")
	if err != nil || ok || st.Generation != 1 || st.LastSeq != 0 {
		t.Fatalf("缺省头应为 gen=1/last=0: %+v ok=%v err=%v", st, ok, err)
	}
	mustCaptureEvidence(t, s, evidenceInput("uid-1", 0))
	st, ok, err = s.EvidenceSourceHead("src-a")
	if err != nil || !ok || st.Generation != 1 || st.LastSeq != 1 || st.SourceTip == "" {
		t.Fatalf("capture 后头不对: %+v ok=%v err=%v", st, ok, err)
	}
}

func TestLookupEvidenceByIdentity(t *testing.T) {
	s := newTestStore(t)
	rec := mustCaptureEvidence(t, s, evidenceInput("uid-1", 0))

	got, ok, err := s.LookupEvidence("uid-1", "src-a", 0, 0)
	if err != nil || !ok || got.ID != rec.ID {
		t.Fatalf("按 uid 查: %+v ok=%v err=%v", got, ok, err)
	}
	got, ok, err = s.LookupEvidence("", "src-a", 1, 1)
	if err != nil || !ok || got.ID != rec.ID {
		t.Fatalf("按位置查: %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, err := s.LookupEvidence("missing", "", 0, 0); err != nil || ok {
		t.Fatalf("未命中应 ok=false: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.LookupEvidence("", "src-a", 1, 9); err != nil || ok {
		t.Fatalf("位置未命中应 ok=false: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.LookupEvidence("", "", 0, 0); err != nil || ok {
		t.Fatalf("空身份应 ok=false: ok=%v err=%v", ok, err)
	}
}

func TestNextEvidenceBundleReusesExistingWindow(t *testing.T) {
	s := newTestStore(t)
	mustCaptureEvidence(t, s, evidenceInput("uid-1", 0))
	mustCaptureEvidence(t, s, evidenceInput("uid-2", 0))
	first, err := s.NextEvidenceBundle(10, 1<<20)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	// 模拟「Bundle 已写、cursor 还没推进」的恢复：重放同一窗口不能建第二份。
	if _, err := s.db.Exec(`UPDATE evidence_sources SET cursor_seq = 0 WHERE id='src-a'`); err != nil {
		t.Fatal(err)
	}
	again, err := s.NextEvidenceBundle(10, 1<<20)
	if err != nil || again == nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || len(again.EvidenceIDs) != 2 {
		t.Fatalf("应复用既有窗口: %+v", again)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM evidence_bundles`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("不应重复建 Bundle，got %d", n)
	}
	var cursor int
	if err := s.db.QueryRow(`SELECT cursor_seq FROM evidence_sources WHERE id='src-a'`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != 2 {
		t.Fatalf("复用时 cursor 应推进到窗口末尾，got %d", cursor)
	}
}

func TestEvidencePathsReportClosedStore(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CaptureEvidence(evidenceInput("uid-x", 1)); err == nil {
		t.Error("CaptureEvidence 应报错")
	}
	if _, _, err := s.EvidenceSourceHead("src-a"); err == nil {
		t.Error("EvidenceSourceHead 应报错")
	}
	if _, _, err := s.LookupEvidence("", "src-a", 1, 1); err == nil {
		t.Error("LookupEvidence 应按位置查时报错")
	}
	if _, err := s.NextEvidenceBundle(10, 1<<20); err == nil {
		t.Error("NextEvidenceBundle 应报错")
	}
	if _, err := s.ListEvidenceRecords("bnd-x"); err == nil {
		t.Error("ListEvidenceRecords 应报错")
	}
	if _, err := s.RotateEvidenceSource("src-a"); err == nil {
		t.Error("RotateEvidenceSource 应报错")
	}
}

func TestCaptureEvidenceRejectsBadChannel(t *testing.T) {
	s := newTestStore(t)
	in := evidenceInput("uid-ch", 1)
	in.Channel = "shared"
	if _, _, err := s.CaptureEvidence(in); err == nil {
		t.Fatal("非法 channel 应报错")
	}
}
