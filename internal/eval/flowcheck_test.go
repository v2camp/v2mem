package eval

import (
	"strings"
	"testing"
	"time"
)

// G 类逐跳检查的口径来源：docs/design-eval-flows.md §3（逐跳内容流转）。
// 这里用合成快照把每条检查的「通过 / 违例 / 样本缺失」三个分支都钉住，
// 注入类用例另见 test/flow：真实库上单点破坏只允许对应 G 项变红。

const testFlowWindow = 15 * 24 * time.Hour

// cleanSnapshot 是一条完整、健康的数据动线：
// 捕获 → 归档 → Bundle（ok/noop/fail 重试）→ 候选 → 裁决 → 记忆。
func cleanSnapshot(now int64) FlowSnapshot {
	mk := func(id, src string, gen, seq int, hash string) FlowObservation {
		return FlowObservation{ID: id, SourceID: src, Generation: gen, Seq: seq,
			EventUID: "uid-" + id, ArchiveHash: hash, CreatedAt: now - 3600}
	}
	arch := func(hash, src string, gen, seq int) FlowArchive {
		return FlowArchive{Hash: hash, SourceID: src, Generation: gen,
			FromSeq: seq, ToSeq: seq, RawCount: 1}
	}
	cand := func(id, hash, status, mem, bundle string) FlowCandidate {
		return FlowCandidate{ID: id, SessionID: "s-" + id, ContentHash: hash,
			Status: status, MemoryID: mem, BundleID: bundle, Revision: 0, CreatedAt: now - 1800}
	}
	return FlowSnapshot{
		Now: now,
		Observations: []FlowObservation{
			mk("o1", "srcA", 1, 1, "ah1"), mk("o2", "srcA", 1, 2, "ah2"),
			mk("o3", "srcA", 1, 3, "ah3"), mk("o4", "srcN", 1, 1, "ah4"),
			mk("o5", "srcF", 1, 1, "ah5"),
		},
		Archives: []FlowArchive{
			arch("ah1", "srcA", 1, 1), arch("ah2", "srcA", 1, 2), arch("ah3", "srcA", 1, 3),
			arch("ah4", "srcN", 1, 1), arch("ah5", "srcF", 1, 1),
		},
		Bundles: []FlowBundle{
			{ID: "bA", SourceID: "srcA", Generation: 1, FromSeqExclusive: 0, ToSeqInclusive: 3,
				Status: "ok", Attempt: 1, LastRunID: "runA"},
			{ID: "bN", SourceID: "srcN", Generation: 1, FromSeqExclusive: 0, ToSeqInclusive: 1,
				Status: "noop", Attempt: 1, LastRunID: "runN"},
			{ID: "bF", SourceID: "srcF", Generation: 1, FromSeqExclusive: 0, ToSeqInclusive: 1,
				Status: "ok", Attempt: 2, LastRunID: "runF2"},
		},
		Sources: []FlowSource{
			{ID: "srcA", Generation: 1, LastSeq: 3, CursorSeq: 3},
			{ID: "srcN", Generation: 1, LastSeq: 1, CursorSeq: 1},
			{ID: "srcF", Generation: 1, LastSeq: 1, CursorSeq: 1},
		},
		Runs: []FlowRun{
			{ID: "runA", BundleID: "bA", Status: "ok", Attempt: 1},
			{ID: "runN", BundleID: "bN", Status: "noop", Attempt: 1},
			{ID: "runF1", BundleID: "bF", Status: "invalid", Attempt: 1},
			{ID: "runF2", BundleID: "bF", Status: "ok", Attempt: 2},
		},
		Candidates: []FlowCandidate{
			cand("c1", "h1", "accepted", "m1", "bA"),
			cand("c2", "h2", "accepted", "m2", "bA"),
			cand("c3", "h3", "rejected", "", "bA"),
			cand("c4", "h4", "accepted", "m3", "bF"),
		},
		Decisions: []FlowDecision{
			{CandidateID: "c1", Revision: 0, Action: "human_accept", Actor: "human", MemoryID: "m1", IdempotencyKey: "k1"},
			{CandidateID: "c2", Revision: 0, Action: "human_accept", Actor: "human", MemoryID: "m2", IdempotencyKey: "k2"},
			{CandidateID: "c3", Revision: 0, Action: "reject", Actor: "human", IdempotencyKey: "k3"},
			{CandidateID: "c4", Revision: 0, Action: "auto_commit", Actor: "auto", MemoryID: "m3", IdempotencyKey: "k4"},
		},
		CandidateEvidence: []FlowCandidateEvidence{
			{CandidateID: "c1", EvidenceID: "e1", Role: "direct"},
			{CandidateID: "c2", EvidenceID: "e2", Role: "direct"},
			{CandidateID: "c3", EvidenceID: "e3", Role: "direct"},
			{CandidateID: "c4", EvidenceID: "e4", Role: "direct"},
		},
		MemoryEvidence: []FlowMemoryEvidence{
			{MemoryID: "m1", EvidenceID: "e1"},
			{MemoryID: "m2", EvidenceID: "e2"},
			{MemoryID: "m3", EvidenceID: "e4"},
		},
		Memories: []FlowMemory{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}},
	}
}

func checkByID(rep FlowCheckReport, id string) (FlowCheck, bool) {
	for _, c := range rep.Checks {
		if c.ID == id {
			return c, true
		}
	}
	return FlowCheck{}, false
}

func TestCheckFlowCleanSnapshotAllPass(t *testing.T) {
	rep := CheckFlow(cleanSnapshot(1_700_000_000), testFlowWindow)
	if len(rep.Checks) != 13 {
		t.Fatalf("检查条数 = %d, want 13", len(rep.Checks))
	}
	if rep.Passed != 13 || rep.Failed != 0 || !rep.OK() {
		for _, c := range rep.Checks {
			t.Logf("%s pass=%v %s", c.ID, c.Pass, c.Detail)
		}
		t.Fatalf("Passed/Failed = %d/%d, want 13/0", rep.Passed, rep.Failed)
	}
	// 人工介入率 = 人工动作 / 自动处置 = 3 / 1（design-data-lifecycle §11.9）。
	if rep.ManualRate != 3 {
		t.Fatalf("ManualRate = %v, want 3", rep.ManualRate)
	}
}

// 空库不是「没发现问题」，而是「这条动线根本没被验证」：13 项必须全红且是 fail，不是 error。
func TestCheckFlowEmptySnapshotAllFail(t *testing.T) {
	rep := CheckFlow(FlowSnapshot{Now: 1_700_000_000}, testFlowWindow)
	if len(rep.Checks) != 13 || rep.Failed != 13 || rep.Passed != 0 {
		t.Fatalf("空库: checks=%d passed=%d failed=%d, want 13/0/13",
			len(rep.Checks), rep.Passed, rep.Failed)
	}
	if rep.OK() {
		t.Fatal("空库 OK() 必须为 false")
	}
	for _, c := range rep.Checks {
		if c.Pass {
			t.Errorf("%s 在空库上不应通过", c.ID)
		}
	}
}

// 每条检查各注入一个单点破坏：只允许对应 ID 变红。
func TestCheckFlowSingleViolationPerCheck(t *testing.T) {
	now := int64(1_700_000_000)
	cases := []struct {
		id     string
		mutate func(*FlowSnapshot)
		also   []string // 聚合检查（G12 是逐窗口账本，悬空会同时反映）
	}{
		{id: "G1", mutate: func(s *FlowSnapshot) {
			s.Observations = append(s.Observations, FlowObservation{ID: "dup", SourceID: "srcA",
				Generation: 1, Seq: 1, EventUID: "uid-dup", Legacy: 1})
		}},
		{id: "G2", mutate: func(s *FlowSnapshot) {
			s.Observations = append(s.Observations, FlowObservation{ID: "no-arch", SourceID: "srcA",
				Generation: 1, Seq: 99, EventUID: "uid-no-arch", CreatedAt: s.Now})
		}},
		{id: "G3", mutate: func(s *FlowSnapshot) { s.Archives[0].RawCount = 2 }},
		{id: "G4", mutate: func(s *FlowSnapshot) { s.Archives[1].FromSeq = 9 }},
		{id: "G5", mutate: func(s *FlowSnapshot) { s.Sources[0].CursorSeq = 9 }},
		{id: "G6", mutate: func(s *FlowSnapshot) { s.Runs[1].Status = "ok" }},
		{id: "G7", mutate: func(s *FlowSnapshot) {
			s.Bundles[2].Status = "fail"
			s.Bundles[2].Attempt = 0
			s.Bundles[2].LastRunID = ""
		}},
		{id: "G8", mutate: func(s *FlowSnapshot) {
			s.Candidates = append(s.Candidates, FlowCandidate{ID: "cP", SessionID: "sP",
				ContentHash: "hP", Status: "pending", BundleID: "bA", CreatedAt: now - 40*24*3600})
		}, also: []string{"G12"}},
		{id: "G9", mutate: func(s *FlowSnapshot) {
			s.Candidates = append(s.Candidates, FlowCandidate{ID: "cE", SessionID: "sE",
				ContentHash: "hE", Status: "expired", BundleID: "bA", CreatedAt: now})
		}},
		{id: "G10", mutate: func(s *FlowSnapshot) { s.Decisions = append(s.Decisions, s.Decisions[0]) }},
		{id: "G11", mutate: func(s *FlowSnapshot) { s.Candidates[0].MemoryID = "missing" }},
		{id: "G12", mutate: func(s *FlowSnapshot) {
			s.Candidates = append(s.Candidates, FlowCandidate{ID: "cD", SessionID: "sD",
				ContentHash: "hD", Status: "pending", BundleID: "bA", CreatedAt: now})
		}},
		{id: "G13", mutate: func(s *FlowSnapshot) {
			s.Candidates = append(s.Candidates, FlowCandidate{ID: "cDup", SessionID: "s-c1",
				ContentHash: "h1", Status: "accepted", MemoryID: "m1", BundleID: "bA", CreatedAt: now})
			s.Decisions = append(s.Decisions, FlowDecision{CandidateID: "cDup", Revision: 0,
				Action: "human_accept", Actor: "human", MemoryID: "m1", IdempotencyKey: "kDup"})
			s.CandidateEvidence = append(s.CandidateEvidence,
				FlowCandidateEvidence{CandidateID: "cDup", EvidenceID: "e1", Role: "direct"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			s := cleanSnapshot(now)
			tc.mutate(&s)
			rep := CheckFlow(s, testFlowWindow)
			extra := map[string]bool{}
			for _, id := range tc.also {
				extra[id] = true
			}
			for _, c := range rep.Checks {
				want := c.ID != tc.id && !extra[c.ID]
				if c.Pass != want {
					t.Errorf("%s pass=%v, want %v（detail: %s）", c.ID, c.Pass, want, c.Detail)
				}
			}
			if rep.Failed != 1+len(tc.also) {
				t.Fatalf("Failed = %d, want %d", rep.Failed, 1+len(tc.also))
			}
			if c, ok := checkByID(rep, tc.id); !ok || c.Pass {
				t.Fatalf("%s 必须为唯一失败项", tc.id)
			}
		})
	}
}

// 归档账本：同一归档覆盖多跳时应校验 from/to 与观测序号一致，而不是只数条数。
func TestCheckFlowG3MultiObservationArchive(t *testing.T) {
	s := cleanSnapshot(1_700_000_000)
	s.Archives = []FlowArchive{
		{Hash: "ahA", SourceID: "srcA", Generation: 1, FromSeq: 1, ToSeq: 3, RawCount: 3},
		{Hash: "ah5", SourceID: "srcF", Generation: 1, FromSeq: 1, ToSeq: 1, RawCount: 1},
	}
	for i := range s.Observations {
		if s.Observations[i].SourceID == "srcA" {
			s.Observations[i].ArchiveHash = "ahA"
		}
	}
	rep := CheckFlow(s, testFlowWindow)
	if c, _ := checkByID(rep, "G3"); !c.Pass {
		t.Fatalf("G3 应通过: %s", c.Detail)
	}
	// raw_count 与观测条数不符时必须报出。
	s.Archives[0].RawCount = 2
	rep = CheckFlow(s, testFlowWindow)
	if c, _ := checkByID(rep, "G3"); c.Pass {
		t.Fatal("G3 在 raw_count 与观测条数不符时应失败")
	}
}

// 失败窗口的判定只认账：fail/partial/backlog 要么重试过（attempt 递增），
// 要么已被隔离到 MaxAttempts；任何「attempt=0 就挂 fail」都不算重试。
func TestCheckFlowG7QuarantineRequiresMaxAttempts(t *testing.T) {
	s := cleanSnapshot(1_700_000_000)
	s.Bundles[2].Status = "quarantined"
	s.Bundles[2].Attempt = 1
	rep := CheckFlow(s, testFlowWindow)
	if c, _ := checkByID(rep, "G7"); c.Pass {
		t.Fatal("G7 在 quarantined 未达 MaxAttempts 时应失败")
	}
	s.Bundles[2].Attempt = 5
	rep = CheckFlow(s, testFlowWindow)
	if c, _ := checkByID(rep, "G7"); !c.Pass {
		t.Fatalf("G7 在 quarantined 达 MaxAttempts 后应通过: %s", c.Detail)
	}
}

// noop 窗口若落了候选，就不再是「0 提案」：G6 必须失败（口径在窗口自身，不靠别的检查兜底）。
func TestCheckFlowG6NoopMustStayEmpty(t *testing.T) {
	s := cleanSnapshot(1_700_000_000)
	s.Candidates = append(s.Candidates, FlowCandidate{ID: "cN", SessionID: "sN",
		ContentHash: "hN", Status: "rejected", BundleID: "bN", CreatedAt: s.Now})
	s.Decisions = append(s.Decisions, FlowDecision{CandidateID: "cN", Revision: 0,
		Action: "reject", Actor: "human", IdempotencyKey: "kN"})
	rep := CheckFlow(s, testFlowWindow)
	if c, _ := checkByID(rep, "G6"); c.Pass {
		t.Fatal("noop 窗口落候选时 G6 必须失败")
	}
}

func TestCheckFlowG9ExpiredNeedsDecision(t *testing.T) {
	s := cleanSnapshot(1_700_000_000)
	s.Candidates = append(s.Candidates, FlowCandidate{ID: "cE", SessionID: "sE",
		ContentHash: "hE", Status: "expired", BundleID: "bA", CreatedAt: s.Now})
	rep := CheckFlow(s, testFlowWindow)
	if c, _ := checkByID(rep, "G9"); c.Pass {
		t.Fatal("expired 无裁决时 G9 必须失败（过期不能顶替消费）")
	}
	s.Decisions = append(s.Decisions, FlowDecision{CandidateID: "cE", Revision: 0,
		Action: "defer", Actor: "auto", IdempotencyKey: "kE"})
	rep = CheckFlow(s, testFlowWindow)
	if c, _ := checkByID(rep, "G9"); !c.Pass {
		t.Fatalf("expired 有显式 defer 裁决后 G9 应通过: %s", c.Detail)
	}
}

func TestCheckFlowWindowDefaultsAndDetail(t *testing.T) {
	// window<=0 取 design-data-lifecycle §5 的 15 天处理 SLA。
	s := cleanSnapshot(1_700_000_000)
	s.Candidates = append(s.Candidates, FlowCandidate{ID: "old", SessionID: "s-old",
		ContentHash: "h-old", Status: "pending", BundleID: "bA",
		CreatedAt: s.Now - int64((16*24*time.Hour)/time.Second)})
	rep := CheckFlow(s, 0)
	c, _ := checkByID(rep, "G8")
	if c.Pass {
		t.Fatal("超过默认 15 天 SLA 的 pending 应判悬空")
	}
	if !strings.Contains(c.Detail, "old") {
		t.Fatalf("G8 detail 应点名悬空候选，got %q", c.Detail)
	}
}

func TestFlowCheckReportJSONShape(t *testing.T) {
	rep := CheckFlow(cleanSnapshot(1_700_000_000), testFlowWindow)
	if rep.Checks[0].ID == "" || rep.Checks[0].Name == "" || rep.Checks[0].Detail == "" {
		t.Fatalf("检查项必须带 id/name/detail: %+v", rep.Checks[0])
	}
}

// 边界分支守卫：这些不是主路径，但口径一旦分叉就会让门禁漏判。
func TestFlowCheckEdgeBranches(t *testing.T) {
	now := int64(1_700_000_000)

	// now<=0：纯函数仍要给出确定结论，不能因缺时钟而 panic。
	rep := CheckFlow(FlowSnapshot{Now: 0}, time.Hour)
	if len(rep.Checks) != 13 {
		t.Fatalf("checks = %d", len(rep.Checks))
	}

	if got := flowIDs(nil); got != "—" {
		t.Fatalf("flowIDs(nil) = %q", got)
	}
	if got := flowIDs([]string{"e", "d", "c", "b", "a"}); !strings.Contains(got, "等 5 条") {
		t.Fatalf("flowIDs 截断 = %q", got)
	}

	// G1：seq<=0 的观测不参与幂等键（legacy 无位置）。
	s := cleanSnapshot(now)
	s.Observations = append(s.Observations, FlowObservation{ID: "z", SourceID: "x", Seq: 0, Legacy: 1})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G1"); !c.Pass {
		t.Fatalf("G1 应忽略无序号观测: %s", c.Detail)
	}

	// G2：全部 legacy → 不能证明归档跳，必须失败。
	s = cleanSnapshot(now)
	s.Observations = []FlowObservation{{ID: "l", Legacy: 1}}
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G2"); c.Pass {
		t.Fatal("全部 legacy 时 G2 必须失败")
	}

	// G4：归档没有任何观测时跳过边界校验；乱序 obs 要能取到真实 min/max。
	s = cleanSnapshot(now)
	s.Archives = []FlowArchive{
		{Hash: "ah-empty", SourceID: "srcA", Generation: 1, FromSeq: 9, ToSeq: 9, RawCount: 0},
		{Hash: "ah-multi", SourceID: "srcA", Generation: 1, FromSeq: 10, ToSeq: 12, RawCount: 3},
	}
	s.Observations = []FlowObservation{
		{ID: "m2", SourceID: "srcA", Generation: 1, Seq: 11, ArchiveHash: "ah-multi"},
		{ID: "m1", SourceID: "srcA", Generation: 1, Seq: 10, ArchiveHash: "ah-multi"},
		{ID: "m3", SourceID: "srcA", Generation: 1, Seq: 12, ArchiveHash: "ah-multi"},
	}
	rep = CheckFlow(s, testFlowWindow)
	if c, _ := checkByID(rep, "G4"); !c.Pass {
		t.Fatalf("G4 乱序观测应按 min/max 通过: %s", c.Detail)
	}

	// G5：多窗口排序 / 首窗非 0 / 窗口断档。
	s = cleanSnapshot(now)
	s.Bundles = append(s.Bundles, FlowBundle{ID: "bA2", SourceID: "srcA", Generation: 1,
		FromSeqExclusive: 3, ToSeqInclusive: 4, Status: "ok", Attempt: 1, LastRunID: "runA2"})
	s.Sources[0].CursorSeq = 4
	s.Sources[0].LastSeq = 4
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G5"); !c.Pass {
		t.Fatalf("G5 多窗口应连续通过: %s", c.Detail)
	}
	s.Bundles = append(s.Bundles, FlowBundle{ID: "bX", SourceID: "srcX", Generation: 1,
		FromSeqExclusive: 5, ToSeqInclusive: 6, Status: "ok"})
	s.Sources = append(s.Sources, FlowSource{ID: "srcX", Generation: 1, LastSeq: 6, CursorSeq: 6})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G5"); c.Pass {
		t.Fatal("G5 首窗不从 0 开始必须失败")
	}
	s.Bundles[len(s.Bundles)-1] = FlowBundle{ID: "bX", SourceID: "srcX", Generation: 1,
		FromSeqExclusive: 0, ToSeqInclusive: 1, Status: "ok"}
	s.Bundles = append(s.Bundles, FlowBundle{ID: "bX2", SourceID: "srcX", Generation: 1,
		FromSeqExclusive: 3, ToSeqInclusive: 3, Status: "ok"})
	s.Sources[len(s.Sources)-1].CursorSeq = 3
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G5"); c.Pass {
		t.Fatal("G5 窗口断档必须失败")
	}

	// G6：noop attempt=0。
	s = cleanSnapshot(now)
	s.Bundles[1].Attempt = 0
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G6"); c.Pass {
		t.Fatal("G6 noop attempt=0 必须失败")
	}

	// G7：失败 run 找不到 Bundle；run.attempt=0。
	s = cleanSnapshot(now)
	s.Runs = append(s.Runs,
		FlowRun{ID: "rX", BundleID: "missing", Status: "fail", Attempt: 1},
		FlowRun{ID: "r0", BundleID: "bF", Status: "fail", Attempt: 0})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G7"); c.Pass {
		t.Fatal("G7 失联/零尝试的失败运行必须失败")
	}

	// G9：accepted/rejected 缺裁决、pending 已有终态裁决、非法状态。
	s = cleanSnapshot(now)
	s.Candidates = append(s.Candidates,
		FlowCandidate{ID: "cX", SessionID: "sX", ContentHash: "hX", Status: "accepted", MemoryID: "m1", BundleID: "bA", CreatedAt: now},
		FlowCandidate{ID: "cY", SessionID: "sY", ContentHash: "hY", Status: "rejected", BundleID: "bA", CreatedAt: now},
		FlowCandidate{ID: "cZ", SessionID: "sZ", ContentHash: "hZ", Status: "pending", BundleID: "bA", CreatedAt: now},
		FlowCandidate{ID: "cW", SessionID: "sW", ContentHash: "hW", Status: "weird", BundleID: "bA", CreatedAt: now})
	s.Decisions = append(s.Decisions, FlowDecision{CandidateID: "cZ", Revision: 0,
		Action: "human_accept", Actor: "human", IdempotencyKey: "kZ"})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G9"); c.Pass {
		t.Fatal("G9 必须覆盖四类不闭环")
	}

	// G10：决策 revision 超前于候选。
	s = cleanSnapshot(now)
	s.Decisions = append(s.Decisions, FlowDecision{CandidateID: "c1", Revision: 9,
		Action: "defer", Actor: "auto", IdempotencyKey: "kR"})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G10"); c.Pass {
		t.Fatal("G10 revision 超前必须失败")
	}

	// G11：裁决 memory_id 不一致 / 证据未绑定 / 无证据样本。
	s = cleanSnapshot(now)
	s.Decisions[0].MemoryID = "m2"
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G11"); c.Pass {
		t.Fatal("G11 memory_id 不一致必须失败")
	}
	s = cleanSnapshot(now)
	s.CandidateEvidence[0].EvidenceID = "e9"
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G11"); c.Pass {
		t.Fatal("G11 证据未绑定必须失败")
	}
	s = cleanSnapshot(now)
	s.CandidateEvidence = nil
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G11"); c.Pass {
		t.Fatal("G11 无证据样本必须失败")
	}

	// G12：无 Bundle 归属的候选单独成窗，不能被漏掉。
	s = cleanSnapshot(now)
	s.Candidates = append(s.Candidates, FlowCandidate{ID: "cU", SessionID: "sU",
		ContentHash: "hU", Status: "expired", CreatedAt: now})
	s.Decisions = append(s.Decisions, FlowDecision{CandidateID: "cU", Revision: 0,
		Action: "defer", Actor: "auto", IdempotencyKey: "kU"})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G12"); !c.Pass {
		t.Fatalf("G12 unbundled 终态应通过: %s", c.Detail)
	}

	// G13：采纳但无候选证据 = 严密度违例。
	s = cleanSnapshot(now)
	s.Candidates = append(s.Candidates, FlowCandidate{ID: "cF", SessionID: "sF",
		ContentHash: "hF", Status: "accepted", MemoryID: "m1", BundleID: "bA", CreatedAt: now})
	s.Decisions = append(s.Decisions, FlowDecision{CandidateID: "cF", Revision: 0,
		Action: "human_accept", Actor: "human", MemoryID: "m1", IdempotencyKey: "kF"})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G13"); c.Pass {
		t.Fatal("G13 无证据强写必须失败")
	}
}

// S10a：G9 承认三类「非裁决驱动」状态。
// failed 是重试队列（G12 仍不许它当终态），quarantined/withdrawn 是明确终态，
// 它们本来就不该有 accept/reject 裁决——此前一律判「状态非法」，会逼实现为了过闸去伪造裁决。
func TestCheckFlowG9AdmitsFailedQuarantinedWithdrawn(t *testing.T) {
	s := cleanSnapshot(1_700_000_000)
	for _, st := range []string{"failed", "quarantined", "withdrawn"} {
		s.Candidates = append(s.Candidates, FlowCandidate{ID: "c-" + st, SessionID: "s-" + st,
			ContentHash: "h-" + st, Status: st, BundleID: "bA", CreatedAt: s.Now})
	}
	c, ok := checkByID(CheckFlow(s, testFlowWindow), "G9")
	if !ok || !c.Pass {
		t.Fatalf("failed/quarantined/withdrawn 应被 G9 承认: %+v", c)
	}
	// 但真正非法的状态仍然要红：白名单不是「什么都放行」。
	s.Candidates = append(s.Candidates, FlowCandidate{ID: "c-bad", SessionID: "s-bad",
		ContentHash: "h-bad", Status: "weird", BundleID: "bA", CreatedAt: s.Now})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G9"); c.Pass {
		t.Fatal("未知状态仍必须判非法")
	}
}

// S10a：`mem batch --rollback` 把候选退回 pending 后，历史终态裁决仍在。
// pending 的「已有终态裁决」只能看**最近一次 reopen 之后**的部分，
// 否则回滚过的候选会被永久判红。
func TestCheckFlowG9PendingIgnoresDecisionsBeforeReopen(t *testing.T) {
	now := int64(1_700_000_000)
	newPending := func() FlowCandidate {
		return FlowCandidate{ID: "cR", SessionID: "sR", ContentHash: "hR", Status: "pending",
			BundleID: "bA", Revision: 2, CreatedAt: now}
	}

	// 回滚后 pending：accept(rev0) → reopen(rev1)，此后没有新裁决 ⇒ 合规。
	s := cleanSnapshot(now)
	s.Candidates = append(s.Candidates, newPending())
	s.Decisions = append(s.Decisions,
		FlowDecision{CandidateID: "cR", Revision: 0, Action: "human_accept", Actor: "human", MemoryID: "m1", IdempotencyKey: "kR0"},
		FlowDecision{CandidateID: "cR", Revision: 1, Action: "reopen", Actor: "human-reviewed-llm", IdempotencyKey: "kR1"})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G9"); !c.Pass {
		t.Fatalf("reopen 之前的终态裁决不应判红: %s", c.Detail)
	}

	// reopen 之后又落了一次终态裁决 ⇒ 说明状态没跟着走，必须红。
	s.Decisions = append(s.Decisions,
		FlowDecision{CandidateID: "cR", Revision: 2, Action: "reject", Actor: "human", IdempotencyKey: "kR2"})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G9"); c.Pass {
		t.Fatal("reopen 之后的终态裁决必须判红")
	}
}

// S10a：G12 终态集合 = accepted/rejected/expired/quarantined/withdrawn。
// failed 不是终态：它还在重试队列里，算成终态会让准出率虚高。
func TestCheckFlowG12TerminalSet(t *testing.T) {
	now := int64(1_700_000_000)
	s := cleanSnapshot(now)
	s.Candidates = []FlowCandidate{
		{ID: "c1", SessionID: "s1", ContentHash: "h1", Status: "accepted", MemoryID: "m1", BundleID: "bA", CreatedAt: now},
		{ID: "c2", SessionID: "s2", ContentHash: "h2", Status: "quarantined", BundleID: "bA", CreatedAt: now},
		{ID: "c3", SessionID: "s3", ContentHash: "h3", Status: "withdrawn", BundleID: "bA", CreatedAt: now},
	}
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G12"); !c.Pass {
		t.Fatalf("quarantined/withdrawn 应计入终态: %s", c.Detail)
	}
	s.Candidates = append(s.Candidates, FlowCandidate{ID: "c4", SessionID: "s4",
		ContentHash: "h4", Status: "failed", BundleID: "bA", CreatedAt: now})
	if c, _ := checkByID(CheckFlow(s, testFlowWindow), "G12"); c.Pass {
		t.Fatal("failed 不算终态：未重试完就走完的窗口必须判悬空")
	}
}
