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

func writeJSON(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "model.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// 老文件没有 mode 字段：必须归一化成 api，保持既有行为不变。
func TestLoadModelConfigLegacyDefaultsToAPI(t *testing.T) {
	p := writeJSON(t, `{"provider":"deepseek","model":"m","api_key":"k"}`)
	c, err := LoadModelConfig(p)
	if err != nil || c.Mode != "api" {
		t.Fatalf("老文件应归一化为 api，got %+v err=%v", c, err)
	}
}

// 未知 mode 不得静默退化成 api；cli 模式缺字段等于配置不可执行，同样报错。
func TestLoadModelConfigRejectsBadMode(t *testing.T) {
	for _, body := range []string{
		`{"mode":"bogus"}`,
		`{"mode":"cli","cli":"claude"}`,
		`{"mode":"cli","model_tag":"claude@1+tpl1"}`,
	} {
		if _, err := LoadModelConfig(writeJSON(t, body)); err == nil {
			t.Errorf("应报错: %s", body)
		}
	}
}

// 新写入必须是 0600（含 key），并保留一份写前备份。
func TestSaveModelConfig0600AndBackup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "model.json")
	if err := os.WriteFile(p, []byte(`{"provider":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveModelConfig(p, ModelConfig{Mode: "cli", CLI: "claude", ModelTag: "claude@1+tpl1"}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("权限应为 0600，got %v", st.Mode().Perm())
	}
	bak, err := os.ReadFile(p + ".v2mem.bak")
	if err != nil || !strings.Contains(string(bak), `"old"`) {
		t.Errorf("备份应保留原内容，got %q err=%v", bak, err)
	}
	c, _ := LoadModelConfig(p)
	if c.Mode != "cli" || c.CLI != "claude" {
		t.Errorf("写后读不一致: %+v", c)
	}
}

// 优先级只在 ResolveOptions 里实现一次：flag > cli > off > api 字段。
func TestResolveOptionsPrecedence(t *testing.T) {
	cli := ModelConfig{Mode: "cli", CLI: "claude", ModelTag: "claude@1+tpl1"}
	if opt := ResolveOptions(cli, "deepseek", ""); opt.CLI != "" || opt.Provider != "deepseek" {
		t.Errorf("显式 flag 应压过 cli，got %+v", opt)
	}
	if opt := ResolveOptions(cli, "", ""); opt.CLI != "claude" || opt.ModelTag != "claude@1+tpl1" {
		t.Errorf("无 flag 应用 cli，got %+v", opt)
	}
	if opt := ResolveOptions(ModelConfig{Mode: "off"}, "", ""); opt.Provider != "none" {
		t.Errorf("off 应映射 none，got %+v", opt)
	}
	if opt := ResolveOptions(ModelConfig{Mode: "api", Provider: "deepseek", Model: "m"}, "", ""); opt.Provider != "deepseek" || opt.CLI != "" {
		t.Errorf("api 应保持旧行为，got %+v", opt)
	}
}

// 只给 --model 不给 --provider 是常见的临时换模型用法，任何分支都不能吞掉它。
func TestResolveOptionsFlagModelOnly(t *testing.T) {
	cliCfg := ModelConfig{Mode: "cli", CLI: "claude", ModelTag: "claude@1+tpl1"}
	if opt := ResolveOptions(cliCfg, "", "foo"); opt.ModelTag != "foo" || opt.CLI != "claude" {
		t.Errorf("cli 下单给 --model 应映射到 ModelTag，got %+v", opt)
	}
	apiCfg := ModelConfig{Mode: "api", Provider: "deepseek", Model: "m"}
	if opt := ResolveOptions(apiCfg, "", "foo"); opt.Model != "foo" || opt.Provider != "deepseek" {
		t.Errorf("api 下单给 --model 应覆盖 Model，got %+v", opt)
	}
}

// 零值配置等于「没有配置文件」，必须按 api 处理而不是落进 cli 分支。
func TestResolveOptionsEmptyConfigIsAPI(t *testing.T) {
	if opt := ResolveOptions(ModelConfig{}, "", ""); opt.CLI != "" || opt.Provider != "" {
		t.Errorf("零值配置应按 api 处理，got %+v", opt)
	}
}

// auto_spawn 是三态：缺省（nil）与显式 false 必须可区分，否则「老配置升级后默认开」
// 与「用户显式关掉」会塌成同一个值。老配置没有这个字段时必须读回 nil。
func TestLoadModelConfigAutoSpawnTriState(t *testing.T) {
	on := writeJSON(t, `{"mode":"cli","cli":"claude","model_tag":"claude@1+tpl1","auto_spawn":true}`)
	c, err := LoadModelConfig(on)
	if err != nil || c.AutoSpawn == nil || !*c.AutoSpawn {
		t.Fatalf("auto_spawn 应读回 true 指针，got %+v err=%v", c, err)
	}
	off := writeJSON(t, `{"mode":"cli","cli":"claude","model_tag":"claude@1+tpl1","auto_spawn":false}`)
	offCfg, err := LoadModelConfig(off)
	if err != nil || offCfg.AutoSpawn == nil || *offCfg.AutoSpawn {
		t.Fatalf("显式 false 应读回 false 指针，got %+v err=%v", offCfg, err)
	}
	legacy := writeJSON(t, `{"provider":"deepseek","api_key":"k"}`)
	old, err := LoadModelConfig(legacy)
	if err != nil || old.AutoSpawn != nil {
		t.Fatalf("老配置缺省 auto_spawn 应为 nil，got %+v err=%v", old, err)
	}
}

// AutoSpawnEnabled 是文件层面的授权判定：缺省时只要有可用模型就开。
// 这是「升级后默认启用」的唯一实现点，别在调用点各判一次。
func TestAutoSpawnEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  ModelConfig
		want bool
	}{
		{"cli 缺省开", ModelConfig{Mode: "cli", CLI: "claude", ModelTag: "claude@1+tpl1"}, true},
		{"api 缺省开", ModelConfig{Mode: "api", Provider: "deepseek", Model: "deepseek-flash"}, true},
		{"老文件 api 缺省开", ModelConfig{Provider: "deepseek", APIKey: "k"}, true},
		{"空配置关", ModelConfig{}, false},
		{"显式 false 关", ModelConfig{Mode: "cli", CLI: "claude", ModelTag: "claude@1+tpl1", AutoSpawn: BoolPtr(false)}, false},
		{"显式 true 开", ModelConfig{Mode: "cli", CLI: "claude", ModelTag: "claude@1+tpl1", AutoSpawn: BoolPtr(true)}, true},
		{"mode off 压过显式 true", ModelConfig{Mode: "off", AutoSpawn: BoolPtr(true)}, false},
	}
	for _, c := range cases {
		if got := c.cfg.AutoSpawnEnabled(); got != c.want {
			t.Errorf("%s: AutoSpawnEnabled() = %v, want %v", c.name, got, c.want)
		}
	}
}

// 原文件存在但读不出来时不能继续写：否则用户配置被覆盖且没有备份可回退。
func TestSaveModelConfigFailsWhenExistingUnreadable(t *testing.T) {
	// 路径是目录：读失败，rename 也失败，原路径必须保持原样。
	p := filepath.Join(t.TempDir(), "model.json")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveModelConfig(p, ModelConfig{Mode: "off"}); err == nil {
		t.Error("原文件不可读应报错，而不是覆盖")
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		t.Errorf("原路径应保持原样，got %v err=%v", st, err)
	}

	// 权限不可读的文件：目录可写，rename 会成功，只能靠读原文件时 fail-closed 兜住。
	if os.Geteuid() == 0 {
		return // root 无视权限位，这一半验不出东西
	}
	locked := filepath.Join(t.TempDir(), "model.json")
	if err := os.WriteFile(locked, []byte(`{"provider":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o200); err != nil {
		t.Fatal(err)
	}
	if err := SaveModelConfig(locked, ModelConfig{Mode: "off"}); err == nil {
		t.Error("读不到原文件时应报错，而不是无备份覆盖")
	}
	if err := os.Chmod(locked, 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(locked); err != nil || !strings.Contains(string(b), `"old"`) {
		t.Errorf("原文件内容应保持不变，got %q err=%v", b, err)
	}
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

// ResolveOptions 要把 deepseek 的默认模型补进 Options.Model：judgeModelTag 只看得到
// Options.Model，留空会让 judge_model 丢掉模型分量、历史判定无法按模型分组。
func TestResolveOptionsDeepSeekDefaultModel(t *testing.T) {
	if opt := ResolveOptions(ModelConfig{Mode: "api", Provider: "deepseek"}, "", ""); opt.Model != "deepseek-flash" {
		t.Errorf("api 分支 deepseek 应补默认模型，got %q", opt.Model)
	}
	if opt := ResolveOptions(ModelConfig{Provider: "openai"}, "deepseek", ""); opt.Model != "deepseek-flash" {
		t.Errorf("显式 --provider deepseek 应补默认模型，got %q", opt.Model)
	}
	// 显式模型不能被默认值覆盖。
	if opt := ResolveOptions(ModelConfig{Mode: "api", Provider: "deepseek", Model: "m1"}, "", ""); opt.Model != "m1" {
		t.Errorf("显式模型应保留，got %q", opt.Model)
	}
	if opt := ResolveOptions(ModelConfig{Mode: "api", Provider: "ollama"}, "", ""); opt.Model != "" {
		t.Errorf("非 deepseek 不该被补模型，got %q", opt.Model)
	}
}

// --undo 的语义是「回到最近一次写盘前」，所以备份必须跟着每次写盘前进；
// 只在首次写盘时留备份会让 cli→api→cli 之后的回退落到很久以前的状态。
func TestSaveModelConfigBackupTracksLatest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "model.json")
	a := ModelConfig{Mode: "api", Provider: "deepseek", Model: "a", APIKey: "sk-a"}
	b := ModelConfig{Mode: "api", Provider: "deepseek", Model: "b", APIKey: "sk-b"}
	c := ModelConfig{Mode: "off"}
	for _, step := range []ModelConfig{a, b, c} {
		if err := SaveModelConfig(p, step); err != nil {
			t.Fatalf("SaveModelConfig(%+v): %v", step, err)
		}
	}
	bak, err := os.ReadFile(p + ".v2mem.bak")
	if err != nil {
		t.Fatalf("读备份失败: %v", err)
	}
	if !strings.Contains(string(bak), `"model": "b"`) {
		t.Errorf("备份应是最新一轮写盘前的 B，got %q", bak)
	}
	if strings.Contains(string(bak), `"model": "a"`) {
		t.Errorf("备份不应停留在最早写入的 A，got %q", bak)
	}
	got, err := LoadModelConfig(p)
	if err != nil || got.Mode != "off" {
		t.Errorf("当前配置应是 C（mode=off），got %+v err=%v", got, err)
	}
}
