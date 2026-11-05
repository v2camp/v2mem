package commit

import (
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/proposal"
)

func link(uid string, grade Grade) Link {
	return Link{EventUID: uid, ArchiveHash: "arch-" + uid, Grade: grade, Seq: 1}
}

func item(role Role, uid string, grade Grade) Item {
	return Item{EvidenceID: "ev_" + uid, Link: link(uid, grade), Role: role}
}

func cand(kind, scope, project, taskState, review string) proposal.Candidate {
	return proposal.Candidate{
		ID: "cand-1", Kind: kind, Scope: scope, Project: project,
		TaskState: taskState, ReviewClass: review, Revision: 3,
	}
}

func TestMatrixFactNeedsGradeADirectEvidence(t *testing.T) {
	m := Matrix{}
	cases := []struct {
		name string
		c    proposal.Candidate
		ev   []Item
		want Action
		code string
	}{
		{"项目 fact + A 直接", cand("fact", "project", "p", "", ""), []Item{item(RoleDirect, "u1", GradeA)}, ActionAutoCommit, ReasonAutoEligible},
		{"无工程 fact 人工", cand("fact", "", "", "", ""), []Item{item(RoleDirect, "u1", GradeA)}, ActionHumanAccept, ReasonProjectMissing},
		{"无证据 defer", cand("fact", "project", "p", "", ""), nil, ActionDefer, ReasonMissingEvidence},
		{"只有 context defer", cand("fact", "project", "p", "", ""), []Item{item(RoleContext, "u1", GradeA)}, ActionDefer, ReasonMissingDirectEvidence},
		{"B 级 defer", cand("fact", "project", "p", "", ""), []Item{item(RoleDirect, "u1", GradeB)}, ActionDefer, ReasonGradeBelowA},
		{"C 级 defer", cand("fact", "project", "p", "", ""), []Item{item(RoleDirect, "u1", GradeC)}, ActionDefer, ReasonGradeBelowA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := m.DecideItems(tc.c, tc.ev)
			if err != nil {
				t.Fatal(err)
			}
			if d.Action != tc.want {
				t.Fatalf("action=%s want %s reasons=%v", d.Action, tc.want, d.ReasonCodes)
			}
			if !contains(d.ReasonCodes, tc.code) {
				t.Fatalf("reason codes %v 缺 %s", d.ReasonCodes, tc.code)
			}
			if d.IdempotencyKey == "" || d.EvidenceHash == "" {
				t.Fatalf("决策必须带幂等键与证据哈希: %+v", d)
			}
			if d.CandidateID != tc.c.ID || d.Revision != tc.c.Revision {
				t.Fatalf("决策身份应回填候选，got %+v", d)
			}
		})
	}
}

func TestMatrixPitfallNeedsErrorAndResolution(t *testing.T) {
	m := Matrix{}
	c := cand("pitfall", "project", "p", "", "")
	cases := []struct {
		name string
		ev   []Item
		want Action
		code string
	}{
		{"error+resolution", []Item{item(RoleError, "e1", GradeA), item(RoleResolution, "r1", GradeB)}, ActionAutoCommit, ReasonAutoEligible},
		{"仅 error", []Item{item(RoleError, "e1", GradeA)}, ActionDefer, ReasonPitfallNoResolution},
		{"仅 resolution", []Item{item(RoleResolution, "r1", GradeA)}, ActionDefer, ReasonPitfallNoError},
		{"都没有", nil, ActionDefer, ReasonPitfallNoError},
		{"error C 级太弱", []Item{item(RoleError, "e1", GradeC), item(RoleResolution, "r1", GradeA)}, ActionDefer, ReasonPitfallWeakEvidence},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := m.DecideItems(c, tc.ev)
			if err != nil {
				t.Fatal(err)
			}
			if d.Action != tc.want || !contains(d.ReasonCodes, tc.code) {
				t.Fatalf("action=%s reasons=%v want %s/%s", d.Action, d.ReasonCodes, tc.want, tc.code)
			}
		})
	}
}

func TestMatrixTaskNeedsExplicitState(t *testing.T) {
	m := Matrix{}
	for _, state := range []string{"done", "blocked", "next", "DONE"} {
		d, err := m.DecideItems(cand("task", "project", "p", state, ""), nil)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != ActionAutoCommit {
			t.Fatalf("state=%s 应自动提交，got %s %v", state, d.Action, d.ReasonCodes)
		}
	}
	for _, state := range []string{"", "in_progress", "大概完成"} {
		d, err := m.DecideItems(cand("task", "project", "p", state, ""), nil)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != ActionDefer || !contains(d.ReasonCodes, ReasonTaskStateInvalid) {
			t.Fatalf("state=%q 应 defer，got %s %v", state, d.Action, d.ReasonCodes)
		}
	}
}

func TestMatrixManualKindsScopesAndConflict(t *testing.T) {
	m := Matrix{}
	for _, kind := range []string{"decision", "preference", "rule", "DECISION"} {
		d, err := m.DecideItems(cand(kind, "project", "p", "", ""), []Item{item(RoleDirect, "u", GradeA)})
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != ActionHumanAccept || !contains(d.ReasonCodes, ReasonManualKind) {
			t.Fatalf("kind=%s 应人工，got %s %v", kind, d.Action, d.ReasonCodes)
		}
	}
	for _, scope := range []string{"global", "cross_project", "GLOBAL"} {
		d, err := m.DecideItems(cand("fact", scope, "p", "", ""), []Item{item(RoleDirect, "u", GradeA)})
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != ActionHumanAccept {
			t.Fatalf("scope=%s 应人工，got %s", scope, d.Action)
		}
	}
	for _, c := range []proposal.Candidate{
		cand("conflict", "project", "p", "", ""),
		cand("fact", "conflict", "p", "", ""),
		cand("fact", "project", "p", "", "conflict"),
	} {
		d, err := m.DecideItems(c, []Item{item(RoleDirect, "u", GradeA)})
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != ActionConflict || !contains(d.ReasonCodes, ReasonConflict) {
			t.Fatalf("冲突应人工裁决，got %s %v", d.Action, d.ReasonCodes)
		}
	}
	d, err := m.DecideItems(cand("mystery", "project", "p", "", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionHumanAccept || !contains(d.ReasonCodes, ReasonUnknownKind) {
		t.Fatalf("未知 kind 应人工，got %s %v", d.Action, d.ReasonCodes)
	}
}

func TestDecideWithoutRolesFallsBackToManualForPitfall(t *testing.T) {
	m := Matrix{}
	d, err := m.Decide(cand("fact", "project", "p", "", ""), []Link{link("u1", GradeA)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionAutoCommit {
		t.Fatalf("无角色时 fact 仍应按 direct 判定，got %s", d.Action)
	}
	d, err = m.Decide(cand("pitfall", "project", "p", "", ""), []Link{link("u1", GradeA), link("u2", GradeA)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != ActionDefer {
		t.Fatalf("无角色信息时 pitfall 不得自动提交，got %s", d.Action)
	}
}

func TestDecideRejectsEmptyCandidateID(t *testing.T) {
	if _, err := (Matrix{}).DecideItems(proposal.Candidate{}, nil); err == nil {
		t.Fatal("空候选 id 应报错")
	}
}

func TestHashEvidenceIsOrderIndependentButContentSensitive(t *testing.T) {
	a := []Item{item(RoleError, "e1", GradeA), item(RoleResolution, "r1", GradeB)}
	b := []Item{item(RoleResolution, "r1", GradeB), item(RoleError, "e1", GradeA)}
	if HashEvidence(a) != HashEvidence(b) {
		t.Fatal("同一批证据换序应得到同一哈希")
	}
	changed := []Item{item(RoleError, "e1", GradeA), item(RoleResolution, "r1", GradeA)}
	if HashEvidence(a) == HashEvidence(changed) {
		t.Fatal("等级变化必须改变证据哈希")
	}
	if HashEvidence(nil) == "" {
		t.Fatal("空证据也必须有确定的哈希")
	}
}

func TestIdempotencyKeyCoversRevisionAndAction(t *testing.T) {
	base := IdempotencyKey("c1", 0, ActionHumanAccept, "h")
	if base != IdempotencyKey("c1", 0, ActionHumanAccept, "h") {
		t.Fatal("同输入应得到同一幂等键")
	}
	for _, other := range []string{
		IdempotencyKey("c1", 1, ActionHumanAccept, "h"),
		IdempotencyKey("c1", 0, ActionReject, "h"),
		IdempotencyKey("c1", 0, ActionHumanAccept, "h2"),
		IdempotencyKey("c2", 0, ActionHumanAccept, "h"),
	} {
		if other == base {
			t.Fatalf("幂等键必须覆盖 candidate/revision/action/evidence 四个维度: %s", other)
		}
	}
	if NormalizeAction("") != ActionHumanAccept || NormalizeAction(ActionReject) != ActionReject {
		t.Fatal("空动作应兜底为 human_accept")
	}
}

func TestSortLinksIsStableBySequence(t *testing.T) {
	links := []Link{
		{EventUID: "b", OccurredAtNS: 20, Seq: 2},
		{EventUID: "a", OccurredAtNS: 10, Seq: 1},
		{EventUID: "c", OccurredAtNS: 20, Seq: 1},
	}
	SortLinks(links)
	if links[0].EventUID != "a" || links[1].EventUID != "c" || links[2].EventUID != "b" {
		t.Fatalf("按发生时间/序号排序错误: %+v", links)
	}
	if Role("weird").Valid() {
		t.Fatal("未知角色不应合法")
	}
	if CanonicalRole("ERROR") != RoleError || CanonicalRole("") != RoleContext {
		t.Fatal("角色归一化错误")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func runeLen(s string) int { return len([]rune(s)) }

func TestCompactProvenance(t *testing.T) {
	c := Compact{}
	if got := c.CompactProvenance(nil); got != "" {
		t.Fatalf("空证据应为空串，got %q", got)
	}
	one := c.CompactProvenance([]Link{{EventUID: "evt_abcdef012345", Grade: GradeA, Seq: 1}})
	if !strings.Contains(one, "1条") || !strings.Contains(one, "A级") || !strings.Contains(one, "abcdef01") {
		t.Fatalf("单条 provenance 形态不对: %q", one)
	}
	if runeLen(one) > MaxProvenanceRunes {
		t.Fatalf("超长: %d runes %q", runeLen(one), one)
	}

	// 同一 event_uid 补边多次只算一条。
	dup := c.CompactProvenance([]Link{
		{EventUID: "u1", Grade: GradeA},
		{EventUID: "u1", Grade: GradeA},
	})
	if !strings.Contains(dup, "1条") {
		t.Fatalf("事件身份应去重，got %q", dup)
	}

	multi := c.CompactProvenance([]Link{
		{EventUID: "u1", Grade: GradeB},
		{EventUID: "u2", Grade: GradeA},
		{EventUID: "u3", Grade: GradeC},
	})
	if !strings.Contains(multi, "3条") || !strings.Contains(multi, "A级") || !strings.Contains(multi, "+2") {
		t.Fatalf("多条 provenance 形态不对: %q", multi)
	}
	if runeLen(multi) > MaxProvenanceRunes {
		t.Fatalf("超长: %d runes %q", runeLen(multi), multi)
	}

	// 极端长 token 必须按 rune 截断，不能超过上限。
	longTok := strings.Repeat("汉字", 40)
	long := c.CompactProvenance([]Link{
		{EventUID: longTok, Grade: GradeA},
		{EventUID: longTok + "x", Grade: GradeA},
	})
	if runeLen(long) > MaxProvenanceRunes {
		t.Fatalf("必须截断到 %d runes，got %d: %q", MaxProvenanceRunes, runeLen(long), long)
	}
	if !strings.HasPrefix(long, "证据2条·A级") {
		t.Fatalf("截断应保留头部语义，got %q", long)
	}

	// 没有身份的证据也要给出条数与等级。
	for _, l := range []Link{{Grade: GradeA}, {Grade: GradeB}} {
		got := c.CompactProvenance([]Link{l})
		if !strings.Contains(got, "1条") {
			t.Fatalf("无身份证据也要计数，got %q", got)
		}
	}

	// 退化为归档哈希 / locator 的身份。
	if got := c.CompactProvenance([]Link{{ArchiveHash: "abcdefghijkl", Grade: GradeB}}); !strings.Contains(got, "abcdefgh") {
		t.Fatalf("应退回归档哈希标识，got %q", got)
	}
	if got := c.CompactProvenance([]Link{{RawLocator: "seg/xyz.gz#2", Grade: GradeC}}); !strings.Contains(got, "seg/xyz") {
		t.Fatalf("应退回 locator 标识，got %q", got)
	}
	// 只有前缀、没有后续字符时不能产出空 token。
	if got := c.CompactProvenance([]Link{{EventUID: "evt_", ArchiveHash: "keepme1234", Grade: GradeA}}); !strings.Contains(got, "keepme12") {
		t.Fatalf("前缀剥离后应继续尝试下一个身份，got %q", got)
	}
	// 未知等级按 C 处理。
	if got := c.CompactProvenance([]Link{{EventUID: "u", Grade: Grade("Z")}}); !strings.Contains(got, "C级") {
		t.Fatalf("未知等级应降为 C，got %q", got)
	}
	if gradeRank("Z") != 0 {
		t.Fatal("未知等级排名应为 0")
	}
	if truncateRunes("abc", 0) != "" {
		t.Fatal("n<=0 应返回空串")
	}
}

func TestHashEvidenceTieBreakers(t *testing.T) {
	// 每一对只差一个字段，必须得到不同哈希；同序重排必须相同。
	pairs := [][2]Item{
		{{Link: Link{EventUID: "u", ArchiveHash: "a1", Seq: 1}, Role: RoleDirect},
			{Link: Link{EventUID: "u", ArchiveHash: "a2", Seq: 1}, Role: RoleDirect}},
		{{Link: Link{EventUID: "u", ArchiveHash: "a", Seq: 1}, Role: RoleDirect},
			{Link: Link{EventUID: "u", ArchiveHash: "a", Seq: 2}, Role: RoleDirect}},
		{{EvidenceID: "e1", Link: Link{EventUID: "u", ArchiveHash: "a", Seq: 1}, Role: RoleDirect},
			{EvidenceID: "e2", Link: Link{EventUID: "u", ArchiveHash: "a", Seq: 1}, Role: RoleDirect}},
		{{EvidenceID: "e1", Link: Link{EventUID: "u", ArchiveHash: "a", Seq: 1}, Role: RoleDirect},
			{EvidenceID: "e1", Link: Link{EventUID: "u", ArchiveHash: "a", Seq: 1}, Role: RoleContext}},
	}
	for _, p := range pairs {
		if HashEvidence([]Item{p[0]}) == HashEvidence([]Item{p[1]}) {
			t.Fatalf("不同证据字段必须产生不同哈希: %+v vs %+v", p[0], p[1])
		}
	}
	// 同一 EventUID 但证据 id 不同，排序走到 role/证据 id 的兜底分支仍应稳定。
	a := []Item{
		{EvidenceID: "z", Link: Link{EventUID: "same", ArchiveHash: "a", Seq: 1}, Role: RoleError},
		{EvidenceID: "a", Link: Link{EventUID: "same", ArchiveHash: "a", Seq: 1}, Role: RoleError},
	}
	b := []Item{a[1], a[0]}
	if HashEvidence(a) != HashEvidence(b) {
		t.Fatal("同字段证据换序仍应稳定")
	}
}
