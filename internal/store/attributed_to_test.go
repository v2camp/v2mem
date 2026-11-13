package store

import (
	"path/filepath"
	"testing"
)

// attributed_to 是「事实的原始来源」，不是写候选的人。两条写入路径
// （规则蒸馏 InsertCandidate / Proposal InsertProposalCandidate）都必须带上。
func TestCandidateAttributedToRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	res, err := st.InsertCandidate(CandidateInput{
		SessionID: "sess", Project: "demo", Kind: "preference",
		Content: "用户要求候选标来源", Source: "distill", AttributedTo: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Candidate.AttributedTo != "user" {
		t.Fatalf("规则候选标注错: %+v", res.Candidate)
	}

	pres, err := st.InsertProposalCandidate(ProposalCandidateInput{
		SessionID: "sess", Project: "demo", BundleID: "b1", RunID: "r1",
		ProposalIdx: 1, Kind: "pitfall", Content: "根因在 transcript 解析器",
		AttributedTo: "assistant", ObsIDs: []string{"e1"},
		Evidence: []CandidateEvidence{{EvidenceID: "e1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !pres.Created {
		t.Fatal("Proposal 候选应新建")
	}

	list, err := st.ListCandidates("pending", "sess", 10)
	if err != nil {
		t.Fatal(err)
	}
	byContent := map[string]string{}
	for _, c := range list {
		byContent[c.Kind] = c.AttributedTo
	}
	if byContent["preference"] != "user" || byContent["pitfall"] != "assistant" {
		t.Fatalf("ListCandidates 应读出 attributed_to，got %+v", byContent)
	}

	// 第三类值不进库；旧行为（空串）保持兼容。
	bad, err := st.InsertCandidate(CandidateInput{
		SessionID: "sess", Kind: "decision", Content: "来源自造要被清空",
		Source: "distill", AttributedTo: "system",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Candidate.AttributedTo != "" {
		t.Fatalf("非法 attributed_to 应收敛为空，got %q", bad.Candidate.AttributedTo)
	}
}

// 旧库没有 attributed_to 列：Open 必须幂等补列，且既有行默认空串。
func TestMigrationAddsAttributedToToLegacyCandidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDB(t, path)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !migrationHasColumn(t, st, "candidates", "attributed_to") {
		t.Fatal("旧库应补出 candidates.attributed_to")
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("二次打开失败: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("关闭二次句柄: %v", err)
	}
	list, err := st.ListCandidates("", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("legacy 里没有候选，got %d", len(list))
	}
}
