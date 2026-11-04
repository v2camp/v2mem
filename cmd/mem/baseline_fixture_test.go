package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

type evidenceFixtureEvent struct {
	SourceID         string `json:"source_id"`
	SourceGeneration int    `json:"source_generation"`
	SourceSeq        int    `json:"source_seq"`
	EventUID         string `json:"event_uid"`
	OccurredAt       string `json:"occurred_at"`
	EventType        string `json:"event_type"`
	Summary          string `json:"summary"`
	ToolName         string `json:"tool_name"`
	ToolOutput       string `json:"tool_output"`
}

type evidenceFixtureManifest struct {
	SchemaVersion int `json:"schema_version"`
	Expected      struct {
		V1Events            int   `json:"v1_events"`
		V2Events            int   `json:"v2_events"`
		SameSecondEvents    int   `json:"same_second_events"`
		DuplicateSummarySeq []int `json:"duplicate_summary_seqs"`
		LongOutputSeq       int   `json:"long_output_seq"`
		LongOutputMinChars  int   `json:"long_output_min_chars"`
		FinalAssistantSeq   int   `json:"final_assistant_seq"`
		SpoolSegments       int   `json:"spool_segments"`
		ConcurrentWriters   int   `json:"concurrent_writers"`
		DeletedSource       bool  `json:"deleted_source"`
	} `json:"expected"`
}

func readEvidenceFixtureJSONL(t *testing.T, name string) []evidenceFixtureEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "evidence", name))
	if err != nil {
		t.Fatalf("读取夹具 %s: %v", name, err)
	}
	var out []evidenceFixtureEvent
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev evidenceFixtureEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("解析夹具 %s: %v", name, err)
		}
		out = append(out, ev)
	}
	return out
}

func TestEvidenceFixtureCoversKnownLossModes(t *testing.T) {
	manifestRaw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "evidence", "manifest.json"))
	if err != nil {
		t.Fatalf("读取 manifest: %v", err)
	}
	var manifest evidenceFixtureManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("解析 manifest: %v", err)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("fixture schema_version = %d, want 1", manifest.SchemaVersion)
	}

	v1 := readEvidenceFixtureJSONL(t, "events-v1.jsonl")
	v2 := readEvidenceFixtureJSONL(t, "events-v2.jsonl")
	if len(v1) != manifest.Expected.V1Events || len(v1) <= 200 {
		t.Fatalf("v1 事件数 = %d, want %d 且必须超过 200", len(v1), manifest.Expected.V1Events)
	}
	if len(v2) != manifest.Expected.V2Events {
		t.Fatalf("v2 事件数 = %d, want %d", len(v2), manifest.Expected.V2Events)
	}

	sameSecond := 0
	uids := map[string]bool{}
	bySeq := map[int]evidenceFixtureEvent{}
	for _, ev := range v1 {
		if ev.OccurredAt == "2026-11-04T00:00:00Z" {
			sameSecond++
		}
		if uids[ev.EventUID] {
			t.Fatalf("event_uid 重复: %s", ev.EventUID)
		}
		uids[ev.EventUID] = true
		bySeq[ev.SourceSeq] = ev
	}
	if sameSecond != manifest.Expected.SameSecondEvents {
		t.Fatalf("同秒事件 = %d, want %d", sameSecond, manifest.Expected.SameSecondEvents)
	}
	if len(manifest.Expected.DuplicateSummarySeq) != 2 {
		t.Fatalf("重复摘要序列应有 2 项, got %v", manifest.Expected.DuplicateSummarySeq)
	}
	a, b := bySeq[manifest.Expected.DuplicateSummarySeq[0]], bySeq[manifest.Expected.DuplicateSummarySeq[1]]
	if a.Summary != "stop" || b.Summary != "stop" || a.EventUID == b.EventUID {
		t.Fatalf("重复摘要夹具未形成「同内容、不同事件身份」: %+v / %+v", a, b)
	}
	long := bySeq[manifest.Expected.LongOutputSeq]
	if len(long.ToolOutput) < manifest.Expected.LongOutputMinChars {
		t.Fatalf("长工具输出 = %d 字符, want >= %d", len(long.ToolOutput), manifest.Expected.LongOutputMinChars)
	}
	last := bySeq[manifest.Expected.FinalAssistantSeq]
	if last.EventType != "assistant_text" || !strings.Contains(last.Summary, "final assistant conclusion") {
		t.Fatalf("最后助手结论未进入夹具: %+v", last)
	}
	if v2[0].SourceGeneration != 2 || v2[0].SourceSeq != 1 || !strings.HasPrefix(v2[0].EventUID, "fixture-session:2:") {
		t.Fatalf("源轮转夹具未建立新 generation 与序号: %+v", v2[0])
	}

	spoolRaw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "evidence", "spool-segments.jsonl"))
	if err != nil {
		t.Fatalf("读取 spool 夹具: %v", err)
	}
	spoolLines := 0
	writers := map[string]bool{}
	for _, line := range strings.Split(string(spoolRaw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var seg struct {
			Writer string `json:"writer"`
		}
		if err := json.Unmarshal([]byte(line), &seg); err != nil {
			t.Fatalf("解析 spool 段: %v", err)
		}
		spoolLines++
		writers[seg.Writer] = true
	}
	if spoolLines != manifest.Expected.SpoolSegments || len(writers) < manifest.Expected.ConcurrentWriters {
		t.Fatalf("spool 夹具段数=%d writers=%d, want %d/%d", spoolLines, len(writers), manifest.Expected.SpoolSegments, manifest.Expected.ConcurrentWriters)
	}

	deletedRaw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "evidence", "deleted-source.json"))
	if err != nil {
		t.Fatalf("读取 deleted-source: %v", err)
	}
	var deleted struct {
		Deleted         bool `json:"deleted"`
		ArchiveRequired bool `json:"archive_required"`
	}
	if err := json.Unmarshal(deletedRaw, &deleted); err != nil {
		t.Fatalf("解析 deleted-source: %v", err)
	}
	if manifest.Expected.DeletedSource && (!deleted.Deleted || !deleted.ArchiveRequired) {
		t.Fatalf("源删除夹具必须同时要求 deleted 与 archive_required: %+v", deleted)
	}
}

func addReadPoolBaselineMemories(t *testing.T, db string, longFirst bool) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	for i := 0; i < 3; i++ {
		content := "ALPHA RULE " + string(rune('A'+i)) + " baseline hard rule"
		kind := "rule"
		salience := 1.0
		if longFirst {
			content = "ALPHA LONG " + string(rune('A'+i)) + " " + strings.Repeat("LONG", 60)
			kind = "fact"
		}
		if _, err := st.Add(store.AddInput{Content: content, Kind: kind, Source: "test", Salience: salience}); err != nil {
			t.Fatalf("Add priority %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		content := "ALPHA FACT " + string(rune('D'+i)) + " baseline short fact"
		if longFirst {
			content = "ALPHA SHORT " + string(rune('D'+i))
		}
		if _, err := st.Add(store.AddInput{Content: content, Kind: "fact", Source: "test", Salience: 0.01}); err != nil {
			t.Fatalf("Add fallback %d: %v", i, err)
		}
	}
}

func TestReadPoolBaselineNoBackfillAfterRuleDedupOrBudget(t *testing.T) {
	t.Run("rule dedup", func(t *testing.T) {
		t.Setenv("MEM_NO_NOTES", "")
		db := testDB(t)
		addReadPoolBaselineMemories(t, db, false)
		st, err := store.Open(db)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer st.Close()

		full, err := st.Search(store.SearchQuery{Query: "ALPHA", Project: "demo", Scope: "current", Limit: 10, NoFuzzy: true})
		if err != nil {
			t.Fatalf("Search full: %v", err)
		}
		top, err := st.Search(store.SearchQuery{Query: "ALPHA", Project: "demo", Scope: "current", Limit: 3, NoFuzzy: true})
		if err != nil {
			t.Fatalf("Search top: %v", err)
		}
		if len(full) < 6 || len(top) != 3 {
			t.Fatalf("基线前置条件不成立: full=%d top=%d", len(full), len(top))
		}
		filtered := filterRulesCoveredByNotes(st, top)
		if len(filtered) >= len(top) {
			t.Fatalf("基线夹具要求 top3 至少含一条被 notes 覆盖的 rule, got %d/%d", len(filtered), len(top))
		}

		text, injected := renderHookContext(common{db: db}, harness.EventPromptSubmit, "demo", "ALPHA", 3, 1200)
		if len(injected) >= 3 || len(injected) >= len(full) {
			t.Fatalf("当前基线应在 top-k 后过滤且不回补, injected=%d full=%d text=%q", len(injected), len(full), text)
		}
	})

	t.Run("budget truncation", func(t *testing.T) {
		t.Setenv("MEM_NO_NOTES", "")
		db := testDB(t)
		addReadPoolBaselineMemories(t, db, true)
		st, err := store.Open(db)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer st.Close()

		full, err := st.Search(store.SearchQuery{Query: "ALPHA LONG", Project: "demo", Scope: "current", Limit: 10, NoFuzzy: true})
		if err != nil {
			t.Fatalf("Search full: %v", err)
		}
		top, err := st.Search(store.SearchQuery{Query: "ALPHA LONG", Project: "demo", Scope: "current", Limit: 3, NoFuzzy: true})
		if err != nil {
			t.Fatalf("Search top: %v", err)
		}
		if len(full) < 6 || len(top) != 3 || !strings.Contains(top[0].Content, "LONG") {
			t.Fatalf("基线前置条件不成立: full=%d top=%d first=%q", len(full), len(top), top[0].Content)
		}

		text, injected := renderHookContext(common{db: db}, harness.EventPromptSubmit, "demo", "ALPHA LONG", 3, 100)
		if len(injected) != 0 || strings.TrimSpace(text) != "" {
			t.Fatalf("当前基线应在预算截断后不回补短候选, injected=%d text=%q", len(injected), text)
		}
	})
}
