package distill

import (
	"context"
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
