package distill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 没配模型是常态：文件缺失必须返回零值而不是错误，否则所有离线命令都会被它拖垮。
func TestLoadModelConfigMissingIsZero(t *testing.T) {
	c, err := LoadModelConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || c.Provider != "" || c.APIKey != "" {
		t.Fatalf("缺失文件应返回零值，got %+v err=%v", c, err)
	}
}

func TestLoadModelConfigReadsDeepSeek(t *testing.T) {
	p := filepath.Join(t.TempDir(), "model.json")
	body := `{"provider":"deepseek","model":"deepseek-flash","api_key":"sk-test","base_url":"https://api.deepseek.com/v1"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, err := LoadModelConfig(p)
	if err != nil {
		t.Fatalf("LoadModelConfig: %v", err)
	}
	if c.Provider != "deepseek" || c.Model != "deepseek-flash" || c.APIKey != "sk-test" {
		t.Fatalf("配置未按预期解析，got %+v", c)
	}
	if _, err := LoadModelConfig(writeBad(t)); err == nil {
		t.Error("非法 JSON 应当报错")
	}
}

func writeBad(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// DeepSeek 走 OpenAI 兼容协议，只有 base 与 model 名不同；默认模型名必须自己兜，
// 不能落到 ollama 的 qwen2.5:7b 上。
func TestResolveDeepSeek(t *testing.T) {
	name, spec, err := resolve(Options{
		Provider: "deepseek",
		APIKey:   "sk-from-config",
		Getenv:   func(string) string { return "" },
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if name != "deepseek" || spec.Kind != "openai" {
		t.Fatalf("应解析成 openai 兼容，got %s/%s", name, spec.Kind)
	}
	if spec.Name != "deepseek-flash" {
		t.Errorf("默认模型应为 deepseek-flash，got %q", spec.Name)
	}
	if !strings.HasPrefix(spec.BaseURL, "https://api.deepseek.com") {
		t.Errorf("默认 base 应为 api.deepseek.com，got %q", spec.BaseURL)
	}
	if spec.APIKey != "sk-from-config" {
		t.Errorf("应优先用显式 APIKey，got %q", spec.APIKey)
	}
}

// 环境变量是次选：文件没配时还能临时 export 一个。
func TestResolveDeepSeekFromEnv(t *testing.T) {
	env := map[string]string{"DEEPSEEK_API_KEY": "sk-env"}
	_, spec, err := resolve(Options{
		Provider: "deepseek",
		Getenv:   func(k string) string { return env[k] },
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec.APIKey != "sk-env" {
		t.Errorf("应从环境变量取 key，got %q", spec.APIKey)
	}
	if _, _, err := resolve(Options{Provider: "deepseek", Getenv: func(string) string { return "" }}); err == nil {
		t.Error("无 key 应当报错，而不是静默用空 key 发请求")
	}
}

// 显式 BaseURL 覆盖（自建网关、代理）必须生效。
func TestResolveDeepSeekBaseOverride(t *testing.T) {
	_, spec, err := resolve(Options{
		Provider: "deepseek", APIKey: "k", BaseURL: "https://gw.internal/v1",
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec.BaseURL != "https://gw.internal/v1" {
		t.Errorf("显式 BaseURL 未生效，got %q", spec.BaseURL)
	}
	if spec.Name == "" {
		t.Error("模型名不应为空")
	}
}
