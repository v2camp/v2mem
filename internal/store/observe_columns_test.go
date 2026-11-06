package store

import (
	"path/filepath"
	"testing"
)

// observations 表被两条写路径共用：legacy 的 AppendObservation 与
// 新证据路径 CaptureEvidence。计划 §2.2 要求读侧覆盖全部新列，
// 否则「有证据身份」的行经 ListObservations / ObservationsForDistill
// 读出来仍是空壳，形成事实上的双写分叉。
func TestObservationColumnsCoverBothWritePaths(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mem.db")
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 1) legacy 路径：显式写空身份，旧的内容去重仍生效。
	first, err := st.AppendObservation(ObsInput{
		SessionID: "s", Event: "stop", Summary: "legacy 观测", Channel: "agent",
	})
	if err != nil || !first.Created {
		t.Fatalf("首条 legacy 观测应创建: %+v err=%v", first, err)
	}
	dup, err := st.AppendObservation(ObsInput{
		SessionID: "s", Event: "stop", Summary: "legacy 观测", Channel: "agent",
	})
	if err != nil || !dup.Duplicate {
		t.Fatalf("同会话同摘要仍应去重: %+v err=%v", dup, err)
	}

	// 2) 模拟 CaptureEvidence 产出的带身份行（store 包不 import evidence，
	//    直接写表即可覆盖「读侧能不能看见新列」这件事）。
	const evidenceID = "obs-evidence-1"
	_, err = st.db.Exec(`INSERT INTO observations(
		id, session_id, project, harness, event, tool_name, summary, summary_idx,
		raw_ref, content_hash, created_at, meta_json, channel, event_id,
		event_uid, source_id, source_generation, source_seq, occurred_at_ns,
		archive_hash, raw_record_hash, parser_version
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		evidenceID, "s", "demo", "claude", "user-prompt", "", "带身份的观测", "", "",
		"hash-evidence", 2, "{}", "agent", "",
		"tx:u1", "src-1", 3, 7, 1793923200000000000,
		"arch-abc", "raw-def", "v1")
	if err != nil {
		t.Fatal(err)
	}

	assertColumns := func(name string, obs []Observation) {
		t.Helper()
		var legacy, identified *Observation
		for i := range obs {
			switch obs[i].ID {
			case first.ID:
				legacy = &obs[i]
			case evidenceID:
				identified = &obs[i]
			}
		}
		if legacy == nil || identified == nil {
			t.Fatalf("%s 应同时读到 legacy 与带身份的行，got %+v", name, obs)
		}
		if legacy.EventUID != "" || legacy.SourceID != "" || legacy.SourceSeq != 0 ||
			legacy.ArchiveHash != "" || legacy.ParserVersion != "" {
			t.Fatalf("%s: legacy 行身份列应全为空，got %+v", name, legacy)
		}
		if identified.EventUID != "tx:u1" || identified.SourceID != "src-1" ||
			identified.SourceGeneration != 3 || identified.SourceSeq != 7 ||
			identified.OccurredAtNS != 1793923200000000000 ||
			identified.ArchiveHash != "arch-abc" || identified.RawRecordHash != "raw-def" ||
			identified.ParserVersion != "v1" {
			t.Fatalf("%s: 带身份行的新列没读出来，got %+v", name, identified)
		}
	}

	list, err := st.ListObservations("s", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertColumns("ListObservations", list)

	distill, err := st.ObservationsForDistill("s", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertColumns("ObservationsForDistill", distill)
}
