package distill

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPromptCompleterRejectsMissingProvider(t *testing.T) {
	cases := []struct {
		name string
		opt  Options
	}{
		{"none", Options{Provider: "none"}},
		{"openai 无 key", Options{Provider: "openai", Getenv: func(string) string { return "" }}},
		{"未知 provider", Options{Provider: "nope", Getenv: func(string) string { return "" }}},
		{"ollama 未安装", Options{Provider: "ollama", LookPath: func(string) (string, error) {
			return "", errors.New("not found")
		}}},
		{"auto 无任何 provider", Options{Provider: "auto",
			LookPath: func(string) (string, error) { return "", errors.New("not found") },
			Getenv:   func(string) string { return "" }}},
	}
	for _, tc := range cases {
		fn, _, err := PromptCompleter(tc.opt)
		if err == nil || !errors.Is(err, ErrLLMUnavailable) {
			t.Errorf("%s: 应报 ErrLLMUnavailable，got %v", tc.name, err)
		}
		if fn != nil {
			t.Errorf("%s: 失败时不应返回 completer", tc.name)
		}
	}
}

func TestPromptCompleterWiresModel(t *testing.T) {
	got := ""
	fn, name, err := PromptCompleter(Options{
		Provider: "openai", Model: "m1", Timeout: 0,
		Getenv: func(k string) string {
			if k == "OPENAI_API_KEY" {
				return "sk-test"
			}
			return ""
		},
		Complete: func(_ context.Context, spec ModelSpec, prompt string) (string, error) {
			if spec.Name != "m1" || spec.APIKey != "sk-test" || spec.Kind != "openai" {
				t.Errorf("spec 不对: %+v", spec)
			}
			got = prompt
			return "ok", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if name != "openai" {
		t.Fatalf("provider=%q", name)
	}
	out, err := fn(context.Background(), "hello")
	if err != nil || out != "ok" || got != "hello" {
		t.Fatalf("调用应透传: out=%q got=%q err=%v", out, got, err)
	}
}

func TestPromptCompleterLocalOllama(t *testing.T) {
	fn, name, err := PromptCompleter(Options{
		Provider: "ollama", Model: "qwen",
		LookPath: func(string) (string, error) { return "/bin/ollama", nil },
		Complete: func(_ context.Context, spec ModelSpec, prompt string) (string, error) {
			if spec.Kind != "ollama" || !strings.Contains(prompt, "hi") {
				t.Errorf("spec/prompt 不对: %+v %q", spec, prompt)
			}
			return `[]`, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if name != "ollama" {
		t.Fatalf("provider=%q", name)
	}
	if out, err := fn(context.Background(), "hi"); err != nil || out != "[]" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
