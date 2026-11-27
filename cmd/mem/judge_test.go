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
	}, "ollama@judge-v1", false, false)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Reject != 1 {
		t.Fatalf("影子期应记下判定，got %+v", r)
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
	}, "ollama@judge-v1", false, false)
	if err != nil {
		t.Fatalf("applyJudgments: %v", err)
	}
	if r.Accept != 0 || r.Downgraded != 1 || r.Unsure != 1 {
		t.Fatalf("应降级为 unsure，got %+v", r)
	}
}

// 落库入口必须是真的不存在：--apply 若被静默接受，等于绕开了回滚与标定门槛。
func TestJudgeApplyFlagRemoved(t *testing.T) {
	if err := cmdJudge([]string{"--apply"}); err == nil {
		t.Fatal("--apply 已撤，不应被接受")
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
	out := judgeCandidatesJSON(list, nil, nil)
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

// 一输入一输出：未知 id、缺失、重复都算坏输出。
func TestValidateJudgeVerdicts(t *testing.T) {
	list := []store.Candidate{{ID: "aaaaaaaa1111"}, {ID: "bbbbbbbb2222"}}
	cases := []struct {
		name string
		vs   []judgeVerdict
		ok   bool
	}{
		{"完整", []judgeVerdict{{ID: "aaaaaaaa", Verdict: "reject"}, {ID: "bbbbbbbb", Verdict: "unsure"}}, true},
		{"缺失", []judgeVerdict{{ID: "aaaaaaaa", Verdict: "reject"}}, false},
		{"重复", []judgeVerdict{{ID: "aaaaaaaa", Verdict: "reject"}, {ID: "aaaaaaaa", Verdict: "unsure"}, {ID: "bbbbbbbb", Verdict: "unsure"}}, false},
		{"未知", []judgeVerdict{{ID: "cccccccc", Verdict: "reject"}, {ID: "aaaaaaaa", Verdict: "reject"}, {ID: "bbbbbbbb", Verdict: "unsure"}}, false},
	}
	for _, c := range cases {
		err := validateJudgeVerdicts(list, c.vs)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

// 上下文补齐：revision / task_state / evidence 摘要进入送模型 JSON。
func TestJudgeCandidatesJSONIncludesContext(t *testing.T) {
	list := []store.Candidate{{
		ID: "abcdef123456", Kind: "task", Project: "p", Content: "待办",
		Revision: 3, TaskState: "in_progress",
	}}
	out := judgeCandidatesJSON(list, func(string) []string { return []string{"原始证据摘要"} }, nil)
	var parsed []struct {
		Revision  int      `json:"revision"`
		TaskState string   `json:"task_state"`
		Evidence  []string `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed[0].Revision != 3 || parsed[0].TaskState != "in_progress" ||
		len(parsed[0].Evidence) != 1 || parsed[0].Evidence[0] != "原始证据摘要" {
		t.Fatalf("上下文未进入 JSON: %+v", parsed[0])
	}
}

// ---------- 已有记忆上下文（judge-v4） ----------
//
// 判定「重复/更新」需要看到库里相似的既有条目；没有它，模型只能凭单条正文猜。

// 种子记忆必须出现在送模型的 JSON 里。
func TestJudgeCandidatesJSONIncludesNeighborMemories(t *testing.T) {
	st := newJudgeStore(t)
	if _, err := st.Add(store.AddInput{
		Content: "v2mem 的 MCP 通道与 hook 注入相互独立", Project: "v2mem", Source: "human",
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	list := []store.Candidate{{
		ID: "abcdef123456", Kind: "fact", Project: "v2mem",
		Content: "v2mem 的 MCP 通道与 hook 注入相互独立",
	}}
	out := judgeCandidatesJSON(list, nil, judgeNeighborLookup(st))
	var parsed []struct {
		Neighbors []string `json:"neighbor_memories"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("送模型的 JSON 必须可解析: %v（%s）", err, out)
	}
	if len(parsed) != 1 || len(parsed[0].Neighbors) == 0 {
		t.Fatalf("应带出已有记忆摘要，got %s", out)
	}
	if !strings.Contains(parsed[0].Neighbors[0], "MCP 通道") {
		t.Errorf("邻居摘要应是已有记忆正文，got %+v", parsed[0].Neighbors)
	}
}

// 邻居检索不能跨工程串味：Project 非空时只看本工程（全局记忆另行按 Scope 规则处理）。
func TestJudgeNeighborsDoNotLeakAcrossProjects(t *testing.T) {
	st := newJudgeStore(t)
	if _, err := st.Add(store.AddInput{
		Content: "SSH 端口转发的默认配置记录", Project: "other", Source: "human",
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	res := judgeNeighborLookup(st)(store.Candidate{
		ID: "abcdef123456", Kind: "fact", Project: "v2mem", Content: "SSH 端口转发的默认配置记录",
	})
	for _, s := range res {
		if strings.Contains(s, "SSH 端口转发") {
			t.Fatalf("其他工程的记忆不应进入邻居，got %+v", res)
		}
	}
}

// 候选没有工程归属时按全库检索，否则全局知识永远看不到自己的重复。
func TestJudgeNeighborsUseAllScopeWhenProjectEmpty(t *testing.T) {
	st := newJudgeStore(t)
	if _, err := st.Add(store.AddInput{
		Content: "提交前跑 make cover-gate 是硬约束", Project: "", Source: "human",
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	res := judgeNeighborLookup(st)(store.Candidate{
		ID: "abcdef123456", Kind: "fact", Content: "提交前跑 make cover-gate 是硬约束",
	})
	if len(res) == 0 {
		t.Fatal("无工程候选应能在全库找到已有记忆")
	}
}

// 邻居正文只取摘要：过长会挤掉候选本身，也把无关细节塞进提示词。
func TestJudgeNeighborsAreTruncated(t *testing.T) {
	st := newJudgeStore(t)
	long := strings.Repeat("邻居记忆的正文", 100)
	if _, err := st.Add(store.AddInput{Content: long, Project: "v2mem", Source: "human"}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	res := judgeNeighborLookup(st)(store.Candidate{
		ID: "abcdef123456", Kind: "fact", Project: "v2mem", Content: long,
	})
	if len(res) != 1 {
		t.Fatalf("应命中 1 条邻居，got %d", len(res))
	}
	if n := len([]rune(res[0])); n > 201 {
		t.Errorf("邻居摘要应截断到 200 rune（含省略号 201），got %d", n)
	}
	if !strings.HasSuffix(res[0], "…") {
		t.Error("截断后应带省略号")
	}
}

// 空上下文省略字段：没有邻居时不要给模型一个空数组去「回显」。
func TestJudgeCandidatesJSONOmitsEmptyNeighbors(t *testing.T) {
	list := []store.Candidate{{ID: "abcdef123456", Kind: "fact", Content: "无邻近记忆"}}
	out := judgeCandidatesJSON(list, nil, func(store.Candidate) []string { return nil })
	if strings.Contains(out, "neighbor_memories") {
		t.Errorf("空上下文应省略字段，got %s", out)
	}
}

// 检索失败不阻断判定：库坏了是上下文的损失，不是判定失败的理由。
func TestJudgeNeighborLookupFailureIsEmpty(t *testing.T) {
	st := newJudgeStore(t)
	st.Close()
	res := judgeNeighborLookup(st)(store.Candidate{ID: "abcdef123456", Kind: "fact", Content: "任意"})
	if len(res) != 0 {
		t.Fatalf("检索失败应退化为空上下文，got %+v", res)
	}
}

// 提示词必须解释 neighbor_memories 的用途且禁止回显；版本随之升级。
func TestJudgePromptMentionsNeighborMemories(t *testing.T) {
	if judgePromptVersion != "judge-v4" {
		t.Errorf("prompt 版本应升到 judge-v4，got %q", judgePromptVersion)
	}
	if !strings.Contains(judgePrompt, "neighbor_memories") || !strings.Contains(judgePrompt, "不要回显") {
		t.Errorf("提示词应说明 neighbor_memories 是已有记忆且不要回显")
	}
}

// 判定器自己落库的裁决（历史 --apply 数据）不能算人工样本：否则 once 转正后
// 一致率恒 100%，「≥95% 才放行」这条门槛会被自己的产出喂饱。
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
}

// judge_model 的分段决定历史判定能不能按模型分组：flag 显式给了就写 flag，
// 否则写 ResolveOptions 解析出的实际模型（model.json 或 deepseek 默认）。
// cli 的版本分量已经在 provider 里，Model 为空，不能再拼一段。
func TestJudgeModelTag(t *testing.T) {
	cases := []struct {
		provider, flagModel, resolved, want string
	}{
		{"cli:claude@1+tpl1", "", "", "cli:claude@1+tpl1@judge-v4"},
		{"cli:m1@1+tpl1", "m1", "", "cli:m1@1+tpl1@judge-v4"}, // 模型已在 cli: 前缀里，不能再拼一次
		{"deepseek", "", "deepseek-flash", "deepseek:deepseek-flash@judge-v4"},
		{"deepseek", "m1", "", "deepseek:m1@judge-v4"},
		{"ollama", "", "", "ollama@judge-v4"}, // ollama 的默认模型是运行期兜底，不写进归因
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

// S12 退役 --stats（§12.1）：统计面并入 mem review stats，这里只验引导文案。
func TestCmdJudgeStatsRetired(t *testing.T) {
	err := cmdJudge([]string{"--db", testDB(t), "--config", filepath.Join(t.TempDir(), "absent.json"), "--stats", "--json"})
	if err == nil || !strings.Contains(err.Error(), "mem review stats") {
		t.Fatalf("--stats 应退役并指向 mem review stats，got %v", err)
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
