package distill

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractRulesFromSyntheticObs(t *testing.T) {
	obs := []Observation{
		{ID: "a", Summary: "stop status=completed"},
		{ID: "b", Summary: "决策：记忆库放在用户目录，不放代码目录"},
		{ID: "c", Summary: "报错：fts 把整句汉字当成一个词"},
		{ID: "d", Summary: "偏好：默认走 MCP，CLI 只做降级"},
		{ID: "e", Summary: "待办：还剩 accept 没做"},
	}
	facts := ExtractRules(obs)
	if len(facts) < 4 {
		t.Fatalf("规则抽取应至少 4 条，got %+v", facts)
	}
	got := map[string]string{}
	for _, f := range facts {
		got[f.Kind] = f.Content
		if f.Salience != 0.6 || len(f.ObsIDs) != 1 {
			t.Errorf("事实缺 salience/obs_ids: %+v", f)
		}
	}
	for _, kind := range []string{"decision", "pitfall", "preference", "task"} {
		if got[kind] == "" {
			t.Errorf("缺 kind %s，got %+v", kind, got)
		}
	}
	if strings.Contains(got["decision"], "stop status") {
		t.Fatal("套话不应成为候选")
	}
}

// TestMatchKindSkipsNegatedMatch 覆盖 C（否定语境前置判断）。
// 用例取自本机实测的 18 条噪音候选原句：它们都是「一切正常」的表述，
// 只因含「失败 / FAIL」被 kindRes 判成踩坑。
func TestMatchKindSkipsNegatedMatch(t *testing.T) {
	cases := []struct {
		name    string
		summary string
		want    string
	}{
		{"日志无FAIL", "日志无 `FAIL`/`ERROR`，进程正常退出", ""},
		{"一次也没有失败", "完整性校验：一次也没有**失败**", ""},
		{"失败计数零", "校验结果 失败：0 个", ""},
		{"管道表零", "FAIL | 0（一次也没有失败）", ""},
		{"均为零", "润色产物 与 原始 差异 FAIL 均为 0", ""},
		{"长枚举里否定向后传递", "无 timeout/error/panic/exception 失败", ""},
		{"真踩坑", "报错：fts 把整句汉字当成一个词", "pitfall"},
		{"否定后回落下一个kind", "日志无 FAIL，决策是继续跑下一批", "decision"},
		{"日期不是计数零", "2026-11-10 报错：内存溢出", "pitfall"},
		{"十六进制错误码不是计数零", "报错 0x8007 权限不足", "pitfall"},
		{"十次不是零次", "失败了 10 次才定位到", "pitfall"},
		{"无实义词不否定", "报错无法复现，先加日志", "pitfall"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchKind(c.summary); got != c.want {
				t.Errorf("matchKind(%q) = %q, want %q", c.summary, got, c.want)
			}
		})
	}
}

// TestExtractRulesAssistantSourceGate 覆盖 A（分源准入）：
// assistant 的进度汇报默认不产候选，可复用结论仍放行，user 来源不受过程性过滤影响。
func TestExtractRulesAssistantSourceGate(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		summary string
		want    int
	}{
		{
			name: "助手进度汇报挡掉", event: "stop",
			summary: "## Qwen3-ASR 全量重跑进度 **已完成：52 / 65（80%）**",
			want:    0,
		},
		{
			name: "助手完成汇报挡掉", event: "stop",
			summary: "全量重跑已完成。**进度**：已完成 65/65 场，日志末尾出现 `DONE`",
			want:    0,
		},
		{
			name: "助手巡检报告自身不再自我喂养", event: "stop",
			summary: "巡检已完成，全程只读。健康度：无 P0，采集链路正常",
			want:    0,
		},
		{
			name: "助手可复用结论保留", event: "stop",
			summary: "报错根因是 fts5 把整句汉字当一个词，必须显式建 unicode61 分词器",
			want:    1,
		},
		{
			name: "结论与过程混在一起也挡掉", event: "stop",
			summary: "根因是分词器没建，已修复并合并",
			want:    0,
		},
		{
			name: "结构化声明保留", event: "stop",
			summary: "决策：记忆库放在用户目录，不放代码目录",
			want:    1,
		},
		{
			name: "口语化的决定不算声明", event: "stop",
			summary: "决定先跑一遍测试，再合入",
			want:    0,
		},
		{
			name: "用户原话不看过程性标记", event: "user-prompt",
			summary: "进度 52/65，报错了 3 次，先别再重跑",
			want:    1,
		},
		{
			name: "工具载荷不产事实", event: "post-tool-use",
			summary: "Grep {\"pattern\":\"失败\"}",
			want:    0,
		},
		{
			// 真实漏网（obs ev_2b5a1 → cand 23ef4adc，2026-11-17 13:08 蒸馏：
			// 该轮唯一一条 assistant 候选，是「讨论判据的句子」本身被当成判据的产物。
			name: "助手复述判据原文挡掉", event: "stop",
			summary: "这个失败用例暴露了一处真实取舍：`决策：…` 这类结构化声明本身就可复用，" +
				"不该依赖「必须/不要」等词。我加了一条结构化声明判据。",
			want: 0,
		},
		{
			// 判据的判定对象是引号外的叙述；引号内的词是「被引用的字面」，
			// 不该反过来把结论判成过程叙述。
			name: "引号内的过程词不阻断结论", event: "stop",
			summary: "决策：候选必须带 `已完成` 字样才算数",
			want:    1,
		},
		{
			// 真实漏网（obs ev_7be27 → cand 1ec0a682，2026-11-18 17:41 蒸馏）：
			// 「根因是既有的测试范围问题」在 300 字处，落库时被截到 240 rune，
			// 存下来的整段只剩「测试已执行 / 通过 / 失败」，裁决时无从判断可复用性。
			// 判定必须看将要存储的那一段，否则门槛形同虚设。
			name: "结论在截断窗口外不算数", event: "stop",
			summary: "全量 Java 测试已执行（聚合入口，`-fae` 跑完所有模块）： ```bash mvn -f projects/pom.xml test -fae ``` " +
				"结果： - **通过**：`agent-shared-core`、5-8（含 basic）、9-12（含 basic）、13-16 basic、17-18 basic、17-20 " +
				"- **17-20 全量**：483 条测试，0 失败、0 错误、1 跳过，`BUILD SUCCESS` " +
				"- **失败**：`lesson13-16-shop` 的 2 个护栏测试，共 5 条 - `MetricFormulaGuardTest`：3 条 " +
				"- `WhitelistSchemaDriftTest`：2 条 根因是既有的测试范围问题，必须按模块重划边界。",
			want: 0,
		},
		{
			name: "结论在窗口内、进度在窗口外仍保留", event: "stop",
			summary: "决策：图表题采用两层契约，LLM 只产出 ChartIntent，代码查库回填 ChartData，渲染器只吃 ChartData。" +
				"这样模型不接触数字，渲染库可以整体替换，也不需要在 prompt 里塞任何表结构。" +
				"落地时先把 intent 的字段命名收敛到五个维度，再让查询层按维度拼 SQL，最后统一在渲染层做单位换算。" +
				"字段命名沿用数据字典里已有的中文标签，避免前端再维护一套映射表；单位换算只用整数，避免浮点误差。" +
				"渲染库的替换成本因此压到只改一个适配器文件，测试也只需覆盖适配器边界。" +
				"本轮进度：已完成 52/65 场，日志末尾 DONE。",
			want: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			facts := ExtractRules([]Observation{{ID: "x", Event: c.event, Summary: c.summary}})
			if len(facts) != c.want {
				t.Fatalf("len(facts) = %d, want %d (%+v)", len(facts), c.want, facts)
			}
		})
	}
}

// TestUnquotedStripsQuotedSpans 覆盖引号剥除本身：只剥成对定界符，半个引号不吞句。
func TestUnquotedStripsQuotedSpans(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"行内代码", "必须带 `已完成` 字样", "必须带   字样"},
		{"围栏代码块", "看这段：\n```\nrm -rf /\n```\n以后别这么干", "看这段：\n \n以后别这么干"},
		{"中文直角引号", "不该依赖「必须/不要」等词", "不该依赖 等词"},
		{"英文双引号", `日志里说 "no error" 但进程挂了`, "日志里说   但进程挂了"},
		{"半个反引号不吞句", "这个 ` 后面还有结论要说", "这个 ` 后面还有结论要说"},
		{"没有引号原样返回", "根因是分词器没建", "根因是分词器没建"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := unquoted(c.in); got != c.want {
				t.Fatalf("unquoted(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNegatedHelpers(t *testing.T) {
	if negatedAfter("：0 个失败") != true {
		t.Error("「：0」应判为否定")
	}
	if negatedAfter("：2 次") {
		t.Error("「：2 次」不是否定")
	}
	if negatedBefore("日志无 ") != true {
		t.Error("「无」应判为否定")
	}
	if negatedBefore("2026-11-10 ") {
		t.Error("日期末尾的 0 不是计数零")
	}
}

func TestRunProviderNone(t *testing.T) {
	out := Run([]Observation{{ID: "1", Summary: "决定：钩子不写 memories"}}, Options{Provider: "none"})
	if out.Status != "ok" || out.Provider != "none" || len(out.Facts) != 1 {
		t.Fatalf("none 应成功抽出 1 条，got %+v", out)
	}
}

func TestRunAutoFallsOpenToNone(t *testing.T) {
	out := Run([]Observation{{ID: "1", Summary: "决定：零常驻"}}, Options{
		Provider: "auto",
		LookPath: func(string) (string, error) { return "", os.ErrNotExist },
		Getenv:   func(string) string { return "" },
	})
	if out.Provider != "none" || out.Status != "ok" || len(out.Facts) != 1 {
		t.Fatalf("auto 无本地模型时应规则抽取，got %+v", out)
	}
}

func TestRunLLMTimeoutFailOpen(t *testing.T) {
	out := Run([]Observation{{ID: "1", Summary: "决定：超时也不能炸"}}, Options{
		Provider: "openai",
		Timeout:  30 * time.Millisecond,
		Getenv: func(k string) string {
			if k == "OPENAI_API_KEY" {
				return "test-key"
			}
			return ""
		},
		Complete: func(ctx context.Context, spec ModelSpec, prompt string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	if out.Status != "fail" || len(out.Facts) != 0 || out.Error == "" {
		t.Fatalf("超时应 fail 且不产候选，got %+v", out)
	}
}

func TestRunBadJSONIsFail(t *testing.T) {
	out := Run([]Observation{{ID: "1", Summary: "x"}}, Options{
		Provider: "openai",
		Getenv: func(k string) string {
			if k == "OPENAI_API_KEY" {
				return "k"
			}
			return ""
		},
		Complete: func(context.Context, ModelSpec, string) (string, error) {
			return "这不是 JSON", nil
		},
	})
	if out.Status != "fail" || len(out.Facts) != 0 {
		t.Fatalf("坏 JSON 应 fail，got %+v", out)
	}
}

func TestParseFactsJSONL(t *testing.T) {
	raw := "说明文字\n" +
		`{"kind":"decision","content":"用 WAL","salience":0.7,"obs_ids":["a"],"rationale":"观测里写了"}` + "\n" +
		`{"kind":"fact","content":"不该要"}` + "\n" +
		`{"kind":"pitfall","content":"","salience":0.2}` + "\n"
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Content != "用 WAL" || facts[0].Kind != "decision" {
		t.Fatalf("只应留下合规行，got %+v", facts)
	}
}

func TestMissingAPIKey(t *testing.T) {
	out := Run(nil, Options{
		Provider: "openai",
		Getenv:   func(string) string { return "" },
	})
	if out.Status != "fail" || !strings.Contains(out.Error, "OPENAI_API_KEY") {
		t.Fatalf("缺 key 应失败，got %+v", out)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("# c\n[other]\nmodel = \"nope\"\n\n[distill]\nmodel = \"qwen2.5:3b\"\ntimeout_sec = 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model, timeout := LoadConfig(path)
	if model != "qwen2.5:3b" || timeout != 9*time.Second {
		t.Fatalf("got %s %s", model, timeout)
	}
	model, timeout = LoadConfig(filepath.Join(dir, "missing.toml"))
	if model != DefaultModel || timeout != DefaultTimeout {
		t.Fatalf("缺文件应回默认，got %s %s", model, timeout)
	}
}

func TestWindowKeyStable(t *testing.T) {
	if WindowKey([]string{"b", "a"}) != WindowKey([]string{"a", "b"}) {
		t.Fatal("窗口键应与 id 顺序无关")
	}
	if WindowKey([]string{"a"}) == WindowKey([]string{"a", "b"}) {
		t.Fatal("观测集合变了，窗口键应变")
	}
}

func TestCompleteOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "no", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"kind\":\"decision\",\"content\":\"用 WAL\"}"}}]}`))
	}))
	defer srv.Close()
	got, err := completeOpenAI(context.Background(), ModelSpec{
		Name: "m", BaseURL: srv.URL, APIKey: "k", Kind: "openai",
	}, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "用 WAL") {
		t.Fatalf("got %s", got)
	}
}

func TestResolveOllamaMissingAndUnknown(t *testing.T) {
	out := Run(nil, Options{
		Provider: "ollama",
		LookPath: func(string) (string, error) { return "", os.ErrNotExist },
	})
	if out.Status != "fail" || !strings.Contains(out.Error, "ollama") {
		t.Fatalf("got %+v", out)
	}
	out = Run(nil, Options{Provider: "nope"})
	if out.Status != "fail" || !strings.Contains(out.Error, "未知") {
		t.Fatalf("got %+v", out)
	}
}

func TestAutoProbesOllamaThenFallsThrough(t *testing.T) {
	out := Run([]Observation{{ID: "1", Summary: "决定：探测失败就用规则"}}, Options{
		Provider: "auto",
		Timeout:  200 * time.Millisecond,
		LookPath: func(string) (string, error) { return "/usr/bin/ollama", nil },
		Getenv:   func(string) string { return "" },
	})
	if out.Provider != "none" || out.Status != "ok" || len(out.Facts) != 1 {
		t.Fatalf("ollama 不在时应落到 none，got %+v", out)
	}
}

func TestParseFactsArrayAndTruncate(t *testing.T) {
	facts, err := ParseFacts(`[{"kind":"task","content":"还剩一步","salience":2}]`)
	if err != nil || len(facts) != 1 || facts[0].Salience != 0.6 {
		t.Fatalf("got %+v %v", facts, err)
	}
	long := strings.Repeat("决", 300)
	got := ExtractRules([]Observation{{ID: "1", Summary: "决定：" + long}})
	if len(got) != 1 || len([]rune(got[0].Content)) > 240 {
		t.Fatalf("应截断，got %d", len([]rune(got[0].Content)))
	}
}

func TestCompleteLLMSwitch(t *testing.T) {
	if _, err := completeLLM(context.Background(), ModelSpec{Kind: "other"}, "x"); err == nil {
		t.Fatal("未知种类应失败")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := completeLLM(ctx, ModelSpec{Kind: "ollama", Name: "missing-model-for-test"}, "hi"); err == nil {
		t.Fatal("ollama 失败应冒泡")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 500)
	}))
	defer srv.Close()
	if _, err := completeLLM(context.Background(), ModelSpec{
		Kind: "openai", Name: "m", BaseURL: srv.URL, APIKey: "k",
	}, "hi"); err == nil {
		t.Fatal("http 500 应失败")
	}
	if ConfigPath() == "" {
		t.Fatal("配置路径不应为空")
	}
}

func TestCompleteOllamaFailOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := completeOllama(ctx, "missing-model-for-test", "hi")
	if err == nil {
		t.Fatal("不存在的本地模型应失败")
	}
}

// ---------- 三态与类型化错误 ----------

// llmOpt 构造一个「一定走 LLM 分支」的选项，Complete 由用例给定。
func llmOpt(complete func(context.Context, ModelSpec, string) (string, error)) Options {
	return Options{
		Provider: "openai",
		Getenv: func(k string) string {
			if k == "OPENAI_API_KEY" {
				return "k"
			}
			return ""
		},
		Complete: complete,
	}
}

// 0 条不是失败。模型明确「没有可抽取的事实」时必须是 empty，
// 否则调用方只能把合法空结果也报成故障。
func TestRunZeroFactsIsEmptyNotFail(t *testing.T) {
	cases := map[string]string{
		"空输出":        "",
		"空数组":        "[]",
		"全是不合规 kind": `[{"kind":"fact","content":"不该要"}]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			out := Run([]Observation{{ID: "1", Summary: "x"}}, llmOpt(
				func(context.Context, ModelSpec, string) (string, error) { return raw, nil }))
			if out.Status != StatusEmpty {
				t.Fatalf("应判 empty，got %+v", out)
			}
			if out.Failed() || out.Error != "" || out.Err != nil || len(out.Facts) != 0 {
				t.Fatalf("empty 不应带错误，got %+v", out)
			}
		})
	}
}

// 规则抽取抽不到同样是 empty：观测里真的没有可记的事实。
func TestRunRulesZeroFactsIsEmpty(t *testing.T) {
	out := Run([]Observation{{ID: "1", Summary: "stop status=completed"}}, Options{Provider: "none"})
	if out.Status != StatusEmpty || out.Failed() {
		t.Fatalf("规则抽不到应为 empty，got %+v", out)
	}
}

// 坏响应与模型不可用必须是两个可区分的原因，否则调用方无法决定
// 「换个 prompt」还是「重试/降级」。
func TestRunTypedErrorsAreDistinguishable(t *testing.T) {
	bad := Run([]Observation{{ID: "1", Summary: "x"}}, llmOpt(
		func(context.Context, ModelSpec, string) (string, error) { return "这不是 JSON", nil }))
	if bad.Status != StatusFail || !errors.Is(bad.Err, ErrBadResponse) {
		t.Fatalf("坏响应应 ErrBadResponse，got %+v", bad)
	}
	if errors.Is(bad.Err, ErrLLMUnavailable) || errors.Is(bad.Err, ErrNoFacts) {
		t.Fatalf("坏响应不应同时命中其它原因: %v", bad.Err)
	}

	down := Run([]Observation{{ID: "1", Summary: "x"}}, llmOpt(
		func(context.Context, ModelSpec, string) (string, error) {
			return "", errors.New("connection refused")
		}))
	if down.Status != StatusFail || !errors.Is(down.Err, ErrLLMUnavailable) {
		t.Fatalf("连接失败应 ErrLLMUnavailable，got %+v", down)
	}
	if !strings.Contains(down.Error, "connection refused") {
		t.Fatalf("Error 应保留底层原因，got %q", down.Error)
	}

	// 缺 key / 未知 provider 也属于「模型不可用」。
	noKey := Run(nil, Options{Provider: "openai", Getenv: func(string) string { return "" }})
	if !errors.Is(noKey.Err, ErrLLMUnavailable) {
		t.Fatalf("缺 key 应 ErrLLMUnavailable，got %v", noKey.Err)
	}
	unknown := Run(nil, Options{Provider: "nope"})
	if !errors.Is(unknown.Err, ErrLLMUnavailable) {
		t.Fatalf("未知 provider 应 ErrLLMUnavailable，got %v", unknown.Err)
	}
}

func TestRunTimeoutIsUnavailable(t *testing.T) {
	out := Run([]Observation{{ID: "1", Summary: "x"}}, Options{
		Provider: "openai",
		Timeout:  20 * time.Millisecond,
		Getenv: func(k string) string {
			if k == "OPENAI_API_KEY" {
				return "k"
			}
			return ""
		},
		Complete: func(ctx context.Context, _ ModelSpec, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	if !errors.Is(out.Err, ErrLLMUnavailable) {
		t.Fatalf("超时应 ErrLLMUnavailable，got %v", out.Err)
	}
}

func TestParseFactsErrorKinds(t *testing.T) {
	for _, raw := range []string{"", "   ", "[]", `[{"kind":"fact","content":"x"}]`} {
		if _, err := ParseFacts(raw); !errors.Is(err, ErrNoFacts) {
			t.Fatalf("%q 应 ErrNoFacts，got %v", raw, err)
		}
	}
	for _, raw := range []string{"这不是 JSON", `{"kind":"decision",`} {
		if _, err := ParseFacts(raw); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("%q 应 ErrBadResponse，got %v", raw, err)
		}
	}
	if _, err := ParseFacts("[1,2]"); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("数组元素类型不对应 ErrBadResponse，got %v", err)
	}
	// 坏行与好行混排：好行留下，不因为坏行判失败。
	facts, err := ParseFacts("前言\n" + `{"kind":"task","content":"还剩一步"}` + "\n{坏的}")
	if err != nil || len(facts) != 1 {
		t.Fatalf("混排应留下好行，got %+v %v", facts, err)
	}
}
