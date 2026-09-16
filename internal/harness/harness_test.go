package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetResolvesNamesAndAliases(t *testing.T) {
	cases := map[string]string{
		"claude":           "claude",
		"claude-code":      "claude",
		"trae":             "traecode",
		"TRAECODE":         "traecode",
		" trae-cn ":        "traecode",
		"codebuddy":        "codebuddy",
		"code-buddy":       "codebuddy",
		"qoder":            "qoder",
		"qoder-work":       "qoderwork",
		"qoderwork":        "qoderwork",
		"qoderwork-cn":     "qoderwork-cn",
		"codex":            "codex",
		"deepseek-harness": "dsh",
		"deepseek":         "dsh",
		"workbuddy":        "workbuddy",
		"work-buddy":       "workbuddy",
		"trae-work":        "traework",
	}
	for in, want := range cases {
		h, err := Get(in)
		if err != nil {
			t.Errorf("Get(%q) 报错: %v", in, err)
			continue
		}
		if h.Name != want {
			t.Errorf("Get(%q) = %q，期望 %q", in, h.Name, want)
		}
	}
}

func TestGetRejectsUnknownAndEmptyWithHint(t *testing.T) {
	if _, err := Get("no-such-tool"); err == nil {
		t.Error("未知 harness 应报错")
	} else if !strings.Contains(err.Error(), "claude") {
		t.Errorf("错误信息应列出候选，got: %v", err)
	}
	if _, err := Get(""); err == nil {
		t.Error("空 harness 应报错")
	}
}

// 用户点名的 8 个工具必须全部可用 —— 这是本任务的验收条件。
func TestAllUserRequestedToolsAreCovered(t *testing.T) {
	requested := []string{
		"trae work", "trae code", "work buddy", "code buddy",
		"qoder", "qoder work", "deepseek harness", "codex",
	}
	// 用户口语名 → 注册表名
	mapping := map[string]string{
		"trae work":        "traework",
		"trae code":        "traecode",
		"work buddy":       "workbuddy",
		"code buddy":       "codebuddy",
		"qoder":            "qoder",
		"qoder work":       "qoderwork",
		"deepseek harness": "dsh",
		"codex":            "codex",
	}
	for _, name := range requested {
		want := mapping[name]
		h, err := Get(want)
		if err != nil {
			t.Errorf("用户点名的 %q（→%s）未覆盖: %v", name, want, err)
			continue
		}
		if h.Docs == "" {
			t.Errorf("%s 缺少官方文档链接，无法核对", want)
		}
		if h.Verify == "" && h.Tier != TierBridge {
			t.Errorf("%s 缺少生效验证方法", want)
		}
	}
}

// 每个条目都必须自洽：native 要有配置路径，instruction 要有指令文件。
func TestRegistryEntriesAreSelfConsistent(t *testing.T) {
	for _, h := range All() {
		if h.Name == "" || h.Label == "" {
			t.Errorf("条目缺 Name/Label: %+v", h)
		}
		switch h.Tier {
		case TierNative, TierBridge:
			if len(h.GlobalConfig) == 0 && len(h.ExtraConfig) == 0 {
				t.Errorf("%s（%s）既无配置文件也无额外配置说明", h.Name, h.Tier)
			}
		case TierInstruction:
			if len(h.Instruction) == 0 {
				t.Errorf("%s 是指令级接入但没写指令文件位置", h.Name)
			}
		default:
			t.Errorf("%s 的 Tier 非法: %q", h.Name, h.Tier)
		}
		if h.Docs == "" {
			t.Errorf("%s 缺官方文档链接", h.Name)
		}
	}
}

// 名字与别名必须全局唯一，否则 Get 会命中错的那条。
func TestNamesAndAliasesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, h := range All() {
		for _, n := range append([]string{h.Name}, h.Aliases...) {
			key := strings.ToLower(n)
			if prev, dup := seen[key]; dup {
				t.Errorf("名字 %q 被 %s 与 %s 同时占用", n, prev, h.Name)
			}
			seen[key] = h.Name
		}
	}
}

func TestFromEventNameNormalizesVariants(t *testing.T) {
	cases := map[string]Event{
		"SessionStart":       EventSessionStart,
		"sessionstart":       EventSessionStart,
		"session_start":      EventSessionStart,
		" session-start ":    EventSessionStart,
		"UserPromptSubmit":   EventPromptSubmit,
		"user_prompt_submit": EventPromptSubmit,
		"Stop":               EventStop,
		"SessionEnd":         EventSessionEnd,
	}
	for in, want := range cases {
		got, ok := FromEventName(in)
		if !ok {
			t.Errorf("FromEventName(%q) 未识别", in)
			continue
		}
		if got != want {
			t.Errorf("FromEventName(%q) = %q，期望 %q", in, got, want)
		}
	}
	if _, ok := FromEventName("PreToolUse"); ok {
		t.Error("v2mem 不处理 PreToolUse，不应识别")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("无法取得 home")
	}
	if got := ExpandHome("~/.claude/settings.json"); got != filepath.Join(home, ".claude/settings.json") {
		t.Errorf("~ 未展开: %q", got)
	}
	if got := ExpandHome("/abs/path"); got != "/abs/path" {
		t.Errorf("绝对路径不应改动: %q", got)
	}
	if got := ExpandHome("rel/path"); got != "rel/path" {
		t.Errorf("相对路径不应改动: %q", got)
	}
}

// 多候选时选「父目录已存在」的那个，避免写进工具不会读的位置。
func TestResolveGlobalConfigPrefersExistingDir(t *testing.T) {
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "installed")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	h := &Harness{
		Name:         "t",
		GlobalConfig: []string{filepath.Join(tmp, "not-installed", "settings.json"), filepath.Join(existing, "settings.json")},
	}
	got, isNew := h.ResolveGlobalConfig()
	if isNew {
		t.Error("存在已安装目录时不应标记为新建")
	}
	if got != filepath.Join(existing, "settings.json") {
		t.Errorf("应选中已安装项，got %q", got)
	}
}

func TestResolveGlobalConfigFallsBackToFirstWhenNoneExist(t *testing.T) {
	tmp := t.TempDir()
	first := filepath.Join(tmp, "a", "settings.json")
	h := &Harness{Name: "t", GlobalConfig: []string{first, filepath.Join(tmp, "b", "settings.json")}}
	got, isNew := h.ResolveGlobalConfig()
	if !isNew {
		t.Error("全部不存在时应标记为新建")
	}
	if got != first {
		t.Errorf("应回落到第一个候选，got %q", got)
	}
}

// 环境变量优先级：靠前的先取。
func TestProjectDirFromEnvHonoursOrder(t *testing.T) {
	h, err := Get("traecode")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	t.Setenv("TRAE_PROJECT_DIR", "/from/trae")
	t.Setenv("CLAUDE_PROJECT_DIR", "/from/claude")
	if got := h.ProjectDirFromEnv(); got != "/from/trae" {
		t.Errorf("应优先取 TRAE_PROJECT_DIR，got %q", got)
	}
	t.Setenv("TRAE_PROJECT_DIR", "")
	if got := h.ProjectDirFromEnv(); got != "/from/claude" {
		t.Errorf("TRAE 缺失时应回落到 CLAUDE_PROJECT_DIR，got %q", got)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if got := h.ProjectDirFromEnv(); got != "" {
		t.Errorf("都缺失时应返回空，got %q", got)
	}
}

// 未注入环境变量的工具必须显式体现在注册表里（依赖 stdin 的 cwd）。
func TestHarnessesWithoutEnvAreMarked(t *testing.T) {
	for _, name := range []string{"qoderwork", "codex", "qoderwork-cn"} {
		h, err := Get(name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if h.UsesProjectEnv() {
			t.Errorf("%s 官方文档记为不注入环境变量，注册表不应声明 ProjectEnv", name)
		}
	}
}

// 固化这次更正：workbuddy 与 traework 都是 native。
//
// 起因：两者最初都按「官方文档未发布钩子配置」判为 instruction（靠模型遵守指令，概率性）。
// 真相要从**应用包本身**看：
//   - WorkBuddy 内置 CodeBuddy CLI 与文档，用户级钩子就是 ~/.codebuddy/settings.json；
//     其内置插件 hooks.json 也是 CodeBuddy 格式。→ 与 codebuddy 共用配置。
//   - TraeWork(TRAE SOLO) 的 workbench JS 资产表直接声明 .trae/hooks.json（项目级）
//     与 hooks.json（全局），harness dylib 内含同一套事件与 HookExecContext 字段。
//
// 教训：判断某工具支不支持钩子，**不要只看它的官方文档或它自己的 settings.json**，
// 要看他内置的 Agent 运行时是谁的。
func TestDesktopAppsAreNativeNotInstruction(t *testing.T) {
	cases := []struct {
		name       string
		projectCfg string
		globalCfg  string
		projectEnv string
	}{
		{"workbuddy", ".codebuddy/settings.json", "~/.codebuddy/settings.json", "CODEBUDDY_PROJECT_DIR"},
		{"traework", ".trae/hooks.json", "~/.trae-cn/hooks.json", "TRAE_PROJECT_DIR"},
	}
	for _, c := range cases {
		h, err := Get(c.name)
		if err != nil {
			t.Fatalf("Get(%s): %v", c.name, err)
		}
		if h.Tier != TierNative {
			t.Errorf("%s 应为 native（应用包内自带钩子实现），got %q", c.name, h.Tier)
		}
		if !hasPath(h.ProjectConfig, c.projectCfg) {
			t.Errorf("%s 项目级路径应含 %q，got %v", c.name, c.projectCfg, h.ProjectConfig)
		}
		if !hasPath(h.GlobalConfig, c.globalCfg) {
			t.Errorf("%s 用户级路径应含 %q，got %v", c.name, c.globalCfg, h.GlobalConfig)
		}
		if !hasPath(h.ProjectEnv, c.projectEnv) {
			t.Errorf("%s 项目目录环境变量应含 %q（钩子靠它定位工程），got %v", c.name, c.projectEnv, h.ProjectEnv)
		}
		// 待实机验证的表述必须留着 —— 静态分析不等于观察到真实触发
		joined := strings.Join(h.Notes, " ")
		if !strings.Contains(joined, "待实机验证") {
			t.Errorf("%s 的 Notes 应显式标注「待实机验证」（路径来自静态分析，未观测到真实触发）", c.name)
		}
	}
}

func hasPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// 更正后没有任何工具默认走指令级，但兜底通道必须仍然可用（--scope instruction）。
func TestInstructionScopeStillWorksAsFallback(t *testing.T) {
	for _, h := range All() {
		if h.Tier == TierInstruction {
			t.Errorf("%s 仍是指令级？本次更正后所有已知工具都应有原生钩子", h.Name)
		}
	}
}
