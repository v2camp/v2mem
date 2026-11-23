package main

// 准入判定只做两件可验证的事：影子期不动状态、模型不得越过矩阵的权限边界。
// 这两条靠读代码不可信，只能断言。

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

func newJudgeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedCandidate(t *testing.T, st *store.Store, kind, content string) store.Candidate {
	t.Helper()
	res, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s-judge", Project: "v2mem", Kind: kind, Content: content, Source: "distill",
	})
	if err != nil {
		t.Fatalf("InsertCandidate: %v", err)
	}
	return res.Candidate
}

// 影子期的全部意义：判定痕迹可读，状态与库都不动。这是 C 方案能成立的前提。
func TestJudgeShadowKeepsStateUntouched(t *testing.T) {
	st := newJudgeStore(t)
	c := seedCandidate(t, st, "fact", "v2mem 的 MCP 通道与 hook 注入相互独立")

	r, err := applyJudgments(st, []store.Candidate{c}, []judgeVerdict{
		{ID: c.ID[:8], Verdict: "reject", Reason: "process_narrative"},
	}, "ollama@judge-v1", false)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Reject != 1 || r.Applied != 0 {
		t.Fatalf("影子期应只记判定不改状态，got %+v", r)
	}
	got, err := st.ResolveCandidate(c.ID)
	if err != nil {
		t.Fatalf("ResolveCandidate: %v", err)
	}
	if got.Status != "pending" {
		t.Errorf("影子期状态应保持 pending，got %q", got.Status)
	}
}

// 模型的 accept 不得授予 pitfall/preference/decision：矩阵对这些类是 human_accept。
func TestJudgeDowngradesKindsMatrixKeepsForHumans(t *testing.T) {
	st := newJudgeStore(t)
	c := seedCandidate(t, st, "pitfall", "iPad 三栏布局的触控坐标错位，根因在 UIView 手势层")

	r, err := applyJudgments(st, []store.Candidate{c}, []judgeVerdict{
		{ID: c.ID[:8], Verdict: "accept", Reason: "durable"},
	}, "ollama@judge-v1", false)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Accept != 0 || r.Downgraded != 1 || r.Unsure != 1 {
		t.Fatalf("应降级为 unsure，got %+v", r)
	}
}

// apply 才改状态，且理由码进 rationale —— 没有理由码，规则沉淀永远没有语料。
func TestJudgeApplyRejectRecordsReason(t *testing.T) {
	st := newJudgeStore(t)
	c := seedCandidate(t, st, "fact", "巡检已完成，报告已输出并追加到自动化记忆")

	r, err := applyJudgments(st, []store.Candidate{c}, []judgeVerdict{
		{ID: c.ID[:8], Verdict: "reject", Reason: "process_narrative"},
	}, "ollama@judge-v1", true)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Applied != 1 {
		t.Fatalf("apply 应改一条状态，got %+v", r)
	}
	got, err := st.ResolveCandidate(c.ID)
	if err != nil {
		t.Fatalf("ResolveCandidate: %v", err)
	}
	if got.Status != "rejected" {
		t.Errorf("apply 后状态应为 rejected，got %q", got.Status)
	}
}

// 模型常包代码块或加解释，解析必须容忍；未知 verdict 必须报错而不是当 unsure 吞掉。
func TestParseJudgeVerdicts(t *testing.T) {
	raw := "```json\n[{\"id\":\"abcd1234\",\"verdict\":\"reject\",\"reason\":\"process_narrative\"}]\n```"
	vs, err := parseJudgeVerdicts(raw)
	if err != nil || len(vs) != 1 || vs[0].Verdict != "reject" {
		t.Fatalf("应解析出 1 条 reject，got %v %+v", err, vs)
	}
	if _, err := parseJudgeVerdicts(`[{"id":"abcd1234","verdict":"maybe"}]`); err == nil {
		t.Error("未知 verdict 应当报错")
	}
	if _, err := parseJudgeVerdicts("模型说都不错"); err == nil {
		t.Error("无 JSON 数组应当报错")
	}
}

func TestJudgeStatsCountsHumanAgreement(t *testing.T) {
	st := newJudgeStore(t)
	c := seedCandidate(t, st, "fact", "a")
	if err := st.MarkJudgeVerdict(c.ID, "reject", "ollama@judge-v1", "process_narrative"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	if _, err := st.CommitReject(c.ID, -1, "human", "process_narrative"); err != nil {
		t.Fatalf("CommitReject: %v", err)
	}
	stats, err := st.JudgeStats()
	if err != nil {
		t.Fatalf("JudgeStats: %v", err)
	}
	if len(stats) != 1 || stats[0].Verdict != "reject" || stats[0].Rejected != 1 {
		t.Fatalf("应统计到 1 条 reject 且人工也否决，got %+v", stats)
	}
	if !strings.Contains(stats[0].Verdict, "reject") {
		t.Errorf("verdict 分组异常: %+v", stats[0])
	}
}

// 权限边界是纯函数，用表把它钉住：改判据时这里先红。
func TestMayAutoAccept(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		scope   string
		project string
		want    bool
	}{
		{"项目事实", "fact", "project", "v2mem", true},
		{"项目任务", "task", "", "v2mem", true},
		{"踩坑必须人工", "pitfall", "project", "v2mem", false},
		{"偏好必须人工", "preference", "project", "v2mem", false},
		{"决策必须人工", "decision", "project", "v2mem", false},
		{"全局作用域", "fact", "global", "v2mem", false},
		{"跨工程作用域", "fact", "cross_project", "v2mem", false},
		{"无工程归属", "fact", "", "", false},
	}
	for _, c := range cases {
		got := mayAutoAccept(&store.Candidate{Kind: c.kind, Scope: c.scope, Project: c.project})
		if got != c.want {
			t.Errorf("%s: mayAutoAccept(%s/%s/%q) = %v, want %v", c.name, c.kind, c.scope, c.project, got, c.want)
		}
	}
}

// 送进模型的只有短 id、kind、scope、project 与截断后的正文；长正文必须截断。
func TestJudgeCandidatesJSONTruncates(t *testing.T) {
	long := strings.Repeat("很长的事实", 300)
	list := []store.Candidate{{ID: "abcdef123456", Kind: "fact", Project: "p", Content: long}}
	out := judgeCandidatesJSON(list)
	if !strings.Contains(out, "abcdef12") || strings.Contains(out, "abcdef123456") {
		t.Errorf("应只送 8 位前缀，got %s", out[:60])
	}
	var parsed []struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("送模型的 JSON 必须可解析: %v", err)
	}
	// truncateRunes 截到 n 后补一个省略号，故上限是 601。
	if n := len([]rune(parsed[0].Content)); n > 601 {
		t.Errorf("正文应截断到 600 rune（含省略号 601），got %d", n)
	}
	if !strings.HasSuffix(parsed[0].Content, "…") {
		t.Error("截断后应带省略号")
	}
}

// 判定器自己 --apply 的裁决不能算人工样本：否则 once 转正后一致率恒 100%，
// 「≥95% 才放行」这条门槛会被自己的产出喂饱。
func TestJudgeStatsExcludesSelfRun(t *testing.T) {
	st := newJudgeStore(t)
	c := seedCandidate(t, st, "fact", "判定器自裁的样本")
	if err := st.MarkJudgeVerdict(c.ID, "reject", "deepseek@judge-v2", "process_narrative"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	if _, err := st.CommitReject(c.ID, -1, "judge", "judge:process_narrative"); err != nil {
		t.Fatalf("CommitReject: %v", err)
	}
	stats, err := st.JudgeStats()
	if err != nil {
		t.Fatalf("JudgeStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("应有 1 组，got %+v", stats)
	}
	if stats[0].Rejected != 0 || stats[0].SelfRun != 1 {
		t.Fatalf("自裁应计入 SelfRun 而非人工否决，got %+v", stats[0])
	}
	if !noHuman(stats) {
		t.Error("只有自裁样本时应判为「无人工样本」")
	}
}

// 文本输出是给人看的最后一步：分母口径改了，表头与「无人工样本」提示必须同步。
func TestPrintJudgeStatsText(t *testing.T) {
	st := newJudgeStore(t)
	if err := printJudgeStats(st, false); err != nil { // 空表
		t.Fatalf("printJudgeStats 空表: %v", err)
	}
	c := seedCandidate(t, st, "fact", "人工裁决样本")
	if err := st.MarkJudgeVerdict(c.ID, "accept", "deepseek@judge-v2", "durable"); err != nil {
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	if _, err := st.CommitAccept(c.ID, -1, "human"); err != nil {
		t.Fatalf("CommitAccept: %v", err)
	}
	if err := printJudgeStats(st, false); err != nil {
		t.Fatalf("printJudgeStats: %v", err)
	}
}

// judge_model 的分段决定历史判定能不能按模型分组：flag 显式给了就写 flag，
// 否则写 ResolveOptions 解析出的实际模型（model.json 或 deepseek 默认）。
// cli 的版本分量已经在 provider 里，Model 为空，不能再拼一段。
func TestJudgeModelTag(t *testing.T) {
	cases := []struct {
		provider, flagModel, resolved, want string
	}{
		{"cli:claude@1+tpl1", "", "", "cli:claude@1+tpl1@judge-v2"},
		{"cli:m1@1+tpl1", "m1", "", "cli:m1@1+tpl1@judge-v2"}, // 模型已在 cli: 前缀里，不能再拼一次
		{"deepseek", "", "deepseek-flash", "deepseek:deepseek-flash@judge-v2"},
		{"deepseek", "m1", "", "deepseek:m1@judge-v2"},
		{"ollama", "", "", "ollama@judge-v2"}, // ollama 的默认模型是运行期兜底，不写进归因
	}
	for _, tc := range cases {
		if got := judgeModelTag(tc.provider, tc.flagModel, tc.resolved); got != tc.want {
			t.Errorf("judgeModelTag(%q,%q,%q)=%q，要 %q",
				tc.provider, tc.flagModel, tc.resolved, got, tc.want)
		}
	}
}

// ---------- CLI 入口的三个早退分支 ----------
//
// 这三种「什么都没判」必须各自说话：没有候选 ≠ 模型不可用 ≠ 判定失败。
// 把「判定器没起来」说成「0 条判定」，读的人会以为候选都干净。

// 空库：直接提示没有待判候选，不去碰模型配置（否则离线环境会报配置错误）。
func TestCmdJudgeNoPendingSkipsModel(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return cmdJudge([]string{"--db", testDB(t)})
	})
	if err != nil {
		t.Fatalf("cmdJudge 空库应正常返回，got %v", err)
	}
	if !strings.Contains(out, "没有待判候选") {
		t.Fatalf("空库应提示没有待判候选，got %q", out)
	}
}

// -stats 只读库不调模型：它得在模型没配的机器上也能用。
func TestCmdJudgeStatsJSONNeedsNoModel(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	c := seedCandidate(t, st, "fact", "候选表量级在千条以内，不为它加索引")
	if err := st.MarkJudgeVerdict(c.ID, "reject", "ollama@judge-v2", "process_narrative"); err != nil {
		st.Close()
		t.Fatalf("MarkJudgeVerdict: %v", err)
	}
	st.Close()

	out, err := captureStdout(t, func() error {
		// 指向不存在的配置文件：-stats 不该走到读配置那一步。
		return cmdJudge([]string{"--db", db, "--config", filepath.Join(t.TempDir(), "absent.json"), "--stats", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdJudge -stats: %v", err)
	}
	if !strings.Contains(out, `"verdict":"reject"`) {
		t.Fatalf("stats 输出应含 reject 分组，got %q", out)
	}
}

// 有候选但模型起不来：必须报「判定器不可用」，且提示出路（mem setup / 加 API key）。
// 出路文案随模型来源配置改版：旧版指向手写 model.json，现由 mem setup 统一落配置。
func TestCmdJudgeUnavailableModelIsExplicit(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	seedCandidate(t, st, "fact", "MCP 与 hook 注入相互独立")
	st.Close()

	_, err = captureStdout(t, func() error {
		return cmdJudge([]string{"--db", db, "--config", filepath.Join(t.TempDir(), "absent.json"), "--provider", "none"})
	})
	if err == nil {
		t.Fatal("模型不可用时应返回错误，而不是静默判 0 条")
	}
	for _, want := range []string{"判定器不可用", "mem setup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，got %v", want, err)
		}
	}
}
