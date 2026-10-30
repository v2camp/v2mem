package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultChannelMatrix(t *testing.T) {
	cases := []struct {
		explicit, source, want string
	}{
		{"", "human", ChannelUser},
		{"", "", ChannelUser},
		{"", "bench", ChannelUser},
		{"", "llm", ChannelAgent},
		{"", "distill", ChannelAgent},
		{"", "distill-accept", ChannelAgent},
		{"", "harness-summary", ChannelAgent},
		{"", "ingest", ChannelImport},
		{"", "import-proposal", ChannelImport},
		{"tool", "human", ChannelTool},
		{"AGENT", "human", ChannelAgent},
	}
	for _, tc := range cases {
		got, err := DefaultChannel(tc.explicit, tc.source)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if got != tc.want {
			t.Errorf("DefaultChannel(%q,%q)=%q want %q", tc.explicit, tc.source, got, tc.want)
		}
	}
	if _, err := DefaultChannel("shared", "human"); err == nil {
		t.Fatal("非法 channel 应拒绝")
	}
	obs, err := DefaultObsChannel("post-tool-use", "")
	if err != nil || obs != ChannelTool {
		t.Fatalf("post-tool-use 默认 tool，got %s %v", obs, err)
	}
	obs, err = DefaultObsChannel("stop", "")
	if err != nil || obs != ChannelAgent {
		t.Fatalf("stop 默认 agent，got %s %v", obs, err)
	}
}

func TestAddChannelFirstWriterWins(t *testing.T) {
	s := newTestStore(t)
	first, err := s.Add(AddInput{Content: "通道先写赢", Source: "llm", Device: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var ch string
	if err := s.db.QueryRow(`SELECT channel FROM memories WHERE id=?`, first.ID).Scan(&ch); err != nil {
		t.Fatal(err)
	}
	if ch != ChannelAgent {
		t.Fatalf("llm 默认 channel=agent，got %s", ch)
	}
	if _, err := s.Add(AddInput{Content: "通道先写赢", Source: "human", Channel: ChannelUser, Device: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT channel FROM memories WHERE id=?`, first.ID).Scan(&ch); err != nil {
		t.Fatal(err)
	}
	if ch != ChannelAgent {
		t.Fatalf("覆盖不应改 channel，got %s", ch)
	}
	if _, err := s.Add(AddInput{Content: "坏通道", Channel: "shared"}); err == nil {
		t.Fatal("非法 channel 应拒绝")
	}
}

func TestEventIDReplayDoesNotRewrite(t *testing.T) {
	s := newTestStore(t)
	first, err := s.AppendObservation(ObsInput{
		SessionID: "s", Event: "post-tool-use", ToolName: "Shell",
		Summary: "第一次摘要", EventID: "evt-1",
	})
	if err != nil || !first.Created || first.Duplicate {
		t.Fatalf("首次应创建: %+v %v", first, err)
	}
	again, err := s.AppendObservation(ObsInput{
		SessionID: "s", Event: "post-tool-use", ToolName: "Shell",
		Summary: "重放换成别的摘要", EventID: "evt-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || !again.Duplicate || again.DupReason != DupEventID || again.ID != first.ID {
		t.Fatalf("重放应命中 event_id: %+v", again)
	}
	obs, err := s.ListObservations("s", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Summary != "第一次摘要" || obs[0].Channel != ChannelTool || obs[0].EventID != "evt-1" {
		t.Fatalf("先写赢且通道为 tool: %+v", obs)
	}
}

func TestContentHashDupKeepsSingleRow(t *testing.T) {
	s := newTestStore(t)
	a, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "同样的摘要"})
	if err != nil || !a.Created {
		t.Fatal(err)
	}
	b, err := s.AppendObservation(ObsInput{SessionID: "s", Event: "stop", Summary: "同样的摘要", EventID: "later"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Created || !b.Duplicate || b.DupReason != DupContentHash || b.ID != a.ID {
		t.Fatalf("相同摘要应走 content_hash: %+v", b)
	}
	obs, err := s.ListObservations("s", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Channel != ChannelAgent {
		t.Fatalf("stop 默认 agent: %+v", obs)
	}
}

func TestAcceptCopiesCandidateChannel(t *testing.T) {
	s := newTestStore(t)
	res, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "decision", Content: "采纳时沿用通道", Channel: ChannelTool,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidate.Channel != ChannelTool {
		t.Fatalf("候选 channel=%s", res.Candidate.Channel)
	}
	acc, err := s.AcceptCandidate(res.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ch, source string
	if err := s.db.QueryRow(`SELECT channel, source FROM memories WHERE id=?`, acc.MemoryID).Scan(&ch, &source); err != nil {
		t.Fatal(err)
	}
	if ch != ChannelTool || source != "distill-accept" {
		t.Fatalf("记忆 channel=%s source=%s", ch, source)
	}
}

func TestCaptureColumnsMigrateOnLegacyObservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE observations (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL, project TEXT NOT NULL DEFAULT '',
		harness TEXT NOT NULL DEFAULT '', event TEXT NOT NULL, tool_name TEXT NOT NULL DEFAULT '',
		summary TEXT NOT NULL DEFAULT '', summary_idx TEXT NOT NULL DEFAULT '',
		raw_ref TEXT NOT NULL DEFAULT '', content_hash TEXT NOT NULL,
		created_at INTEGER NOT NULL, meta_json TEXT NOT NULL DEFAULT '{}'
	)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.AppendObservation(ObsInput{
		SessionID: "s", Event: "post-tool-use", Summary: "迁移后的观测", EventID: "mig-1",
	})
	if err != nil || !res.Created {
		t.Fatalf("旧库补列后应能写入: %+v %v", res, err)
	}
	again, err := s.AppendObservation(ObsInput{
		SessionID: "s", Event: "post-tool-use", Summary: "另一条", EventID: "mig-1",
	})
	if err != nil || !again.Duplicate || again.DupReason != DupEventID {
		t.Fatalf("迁移后的 event_id 索引应生效: %+v %v", again, err)
	}
}

func TestImportFillsEmptyChannel(t *testing.T) {
	s := newTestStore(t)
	content := "导入前没有通道的事实"
	if _, err := s.Add(AddInput{Content: content, Device: "local", Source: "human"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE memories SET channel='' WHERE content=?`, content); err != nil {
		t.Fatal(err)
	}
	rec := ExportRecord{
		Content: content, ContentHash: HashOf(content), Kind: "fact",
		Source: "human", Channel: ChannelImport, OriginDevice: "remote",
		CreatedAt: 10, UpdatedAt: 10, LastSeenAt: 10, Salience: 0.5, AccessCount: 1,
	}
	if _, err := s.Import([]ExportRecord{rec}); err != nil {
		t.Fatal(err)
	}
	var ch string
	if err := s.db.QueryRow(`SELECT channel FROM memories WHERE content_hash=?`, HashOf(content)).Scan(&ch); err != nil {
		t.Fatal(err)
	}
	if ch != ChannelImport {
		t.Fatalf("空 channel 应被导入补上，got %s", ch)
	}
	out, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range out {
		if strings.Contains(r.Content, "导入前") {
			found = r.Channel == ChannelImport
		}
	}
	if !found {
		t.Fatalf("导出应带 channel: %+v", out)
	}
}
