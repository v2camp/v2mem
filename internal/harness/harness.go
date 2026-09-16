// Package harness 记录各 AI 编码工具 / 工作台的原生钩子接入方式。
//
// 全部条目来自 2026-09-17 核对官方文档，来源与核实结论见 DESIGN.md §12。
// 未从文档确认的一律标进 Notes，不做推测性填写 —— 配置写进不生效的路径
// 是「静默失败」，比不写更糟。
//
// 核心设计结论：六家原生 shell hook 的协议高度同构，且
// SessionStart / UserPromptSubmit 两个事件在 Claude Code、TraeCode、CodeBuddy、
// Qoder、QoderWork、Codex 上**都支持 stdout 纯文本被注入上下文**。
// 因此不需要按 harness 分支渲染 —— 一条 `mem hook` 命令即可通吃，
// harness 参数只用于决定「配置文件写到哪」与「从哪个环境变量取项目目录」。
package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Tier 表示接入的确定性等级。这是本设计最重要的区分：
// 只有 TierNative 是「代码强制」，其余都依赖模型遵守，可靠性差一个量级。
type Tier string

const (
	// TierNative 有原生 shell hook：事件触发即执行，不受模型理解偏差影响。
	TierNative Tier = "native"
	// TierBridge 通过官方桥接包复用他家的 hook 协议。
	TierBridge Tier = "bridge"
	// TierInstruction 无原生 hook，只能把指令写进会话级文件靠模型遵守（指令级钩子）。
	TierInstruction Tier = "instruction"
)

// Event 是 v2mem 关心的归一化事件。
type Event string

const (
	EventSessionStart Event = "session-start"
	EventPromptSubmit Event = "prompt-submit"
	EventStop         Event = "stop"
	EventSessionEnd   Event = "session-end"
)

// EventNames 把归一化事件映射到各 harness 的实际事件名。
//
// 六家原生 hook 全部使用同一套 PascalCase 名称，故共用一张表（已逐家核对）。
var EventNames = map[Event]string{
	EventSessionStart: "SessionStart",
	EventPromptSubmit: "UserPromptSubmit",
	EventStop:         "Stop",
	EventSessionEnd:   "SessionEnd",
}

// EventAliases 覆盖各家在 stdin 里可能给出的写法差异。
var EventAliases = map[string]Event{
	"sessionstart":       EventSessionStart,
	"session-start":      EventSessionStart,
	"userpromptsubmit":   EventPromptSubmit,
	"user-prompt-submit": EventPromptSubmit,
	"promptsubmit":       EventPromptSubmit,
	"stop":               EventStop,
	"sessionend":         EventSessionEnd,
	"session-end":        EventSessionEnd,
}

// FromEventName 把 harness 提交的 hook_event_name 归一化。
// 大小写与连接符差异都容忍，避免因写法不同而静默不注入。
func FromEventName(name string) (Event, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	key = strings.ReplaceAll(key, "_", "-")
	if ev, ok := EventAliases[key]; ok {
		return ev, true
	}
	if ev, ok := EventAliases[strings.ReplaceAll(key, "-", "")]; ok {
		return ev, true
	}
	return "", false
}

// Harness 描述一个工具的接入方式。
type Harness struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
	Label   string   `json:"label"`
	Tier    Tier     `json:"tier"`

	// GlobalConfig 是用户级配置候选路径（可含 ~）。多候选时按序取第一个
	// 「父目录已存在」的写入 —— 目录不存在意味着该工具很可能没装。
	GlobalConfig []string `json:"global_config,omitempty"`
	// ProjectConfig 是项目级配置路径（相对项目根）。
	ProjectConfig []string `json:"project_config,omitempty"`
	// ProjectEnv 是用于推断项目目录的环境变量，按优先级排列。
	// 为空表示该工具不注入环境变量（只能从 stdin JSON 的 cwd 取）。
	ProjectEnv []string `json:"project_env,omitempty"`
	// Instruction 是 Tier=instruction 时写入的指令文件（相对项目根；~ 开头为用户级）。
	Instruction []string `json:"instruction,omitempty"`
	// ExtraConfig 是需要用户自行确保的额外配置（如 Codex 的 [features] hooks = true）。
	ExtraConfig []string `json:"extra_config,omitempty"`
	// Verify 是让用户确认配置真的生效的方法。
	Verify string   `json:"verify,omitempty"`
	Docs   string   `json:"docs"`
	Notes  []string `json:"notes,omitempty"`
}

// registry 是全部已知 harness。顺序即展示顺序。
var registry = []Harness{
	{
		Name:          "claude",
		Aliases:       []string{"claude-code", "cc"},
		Label:         "Claude Code",
		Tier:          TierNative,
		GlobalConfig:  []string{"~/.claude/settings.json"},
		ProjectConfig: []string{".claude/settings.json", ".claude/settings.local.json"},
		ProjectEnv:    []string{"CLAUDE_PROJECT_DIR"},
		Verify:        "启动 claude 后输入 /hooks 查看已注册钩子",
		Docs:          "https://docs.claude.com/en/docs/claude-code/hooks",
		Notes: []string{
			"TraeCode 会读取本文件并合并执行（见 traecode 条目）",
			"DeepSeek Harness 可用官方桥接包 dsh-hooks-claude-code 复用本文件",
		},
	},
	{
		Name:          "traecode",
		Aliases:       []string{"trae", "trae-code", "trae-cn", "solo"},
		Label:         "TraeCode（Trae IDE / SOLO）",
		Tier:          TierNative,
		GlobalConfig:  []string{"~/.trae-cn/hooks.json", "~/.trae/hooks.json"},
		ProjectConfig: []string{".trae/hooks.json"},
		ProjectEnv:    []string{"TRAE_PROJECT_DIR", "CLAUDE_PROJECT_DIR"},
		Verify:        "在 TraeCode 中创建一个新会话，观察是否出现记忆注入",
		Docs:          "https://docs.trae.cn/ide_hook-configuration-reference",
		Notes: []string{
			"官方文档记全局路径为 ~/.trae-cn/hooks.json（CN 版），项目级为 <项目>/.trae/hooks.json",
			"国际版全局路径未从文档确认，故候选表同时列 ~/.trae/hooks.json",
			"会额外读取 Claude Code 的 ~/.claude/settings.json 并合并执行 —— 故给 claude 配置等于顺带覆盖 TraeCode",
			"matcher 仅对 PreToolUse / PostToolUse / Notification 有效；SessionStart / UserPromptSubmit / Stop 不吃 matcher",
		},
	},
	{
		Name:          "codebuddy",
		Aliases:       []string{"code-buddy", "cb", "codebuddy-code"},
		Label:         "CodeBuddy Code",
		Tier:          TierNative,
		GlobalConfig:  []string{"~/.codebuddy/settings.json"},
		ProjectConfig: []string{".codebuddy/settings.json", ".codebuddy/settings.local.json"},
		ProjectEnv:    []string{"CODEBUDDY_PROJECT_DIR"},
		Verify:        "运行 /hooks 面板查看；面板外的手工改动需在面板内审核后才生效",
		Docs:          "https://www.codebuddy.cn/docs/cli/hooks",
		Notes: []string{
			"hook 脚本 60 秒超时后自动终止",
			"退出码 2 时消息来源优先级为 stdout（reason/stopReason 或纯文本）> stderr",
			"支持 type=prompt 的 LLM 评估型 hook，但仅限 Stop / UserPromptSubmit / PreToolUse",
		},
	},
	{
		Name:          "qoder",
		Aliases:       []string{"qoder-ide", "qoder-cli"},
		Label:         "Qoder（IDE / JetBrains 插件 / CLI）",
		Tier:          TierNative,
		GlobalConfig:  []string{"~/.qoder/settings.json"},
		ProjectConfig: []string{".qoder/settings.json", ".qoder/settings.local.json"},
		ProjectEnv:    []string{"QODER_PROJECT_DIR"},
		Verify:        "Qoder CLI 中用 /hooks 查看；IDE 侧直接发起新会话观察注入",
		Docs:          "https://docs.qoder.com/en/cli/hooks",
		Notes: []string{
			"IDE/插件支持 12 个事件（含 PostToolUseFailure、SubagentStart/Stop、PreCompact）",
			"🔴 json 输出若带 hookSpecificOutput 必须同时给 hookEventName，否则整份 JSON 被拒",
			"三处 settings（用户级 / 项目级 / 项目本地级）会合并执行，同事件的 hook 不互相覆盖",
		},
	},
	{
		Name:          "qoder-cn",
		Aliases:       []string{"qodercn", "lingma"},
		Label:         "Qoder CN（原通义灵码）",
		Tier:          TierNative,
		GlobalConfig:  []string{"~/.qoder-cn/settings.json"},
		ProjectConfig: []string{".qoder/settings.json", ".qoder/settings.local.json"},
		ProjectEnv:    []string{"QODERCN_PROJECT_DIR"},
		Verify:        "启动 Qoder CN CLI 后查看 hooks 面板",
		Docs:          "https://help.aliyun.com/zh/lingma/hook",
		Notes:         []string{"项目级配置文件名为 .qoder/，与 CN 的用户级路径 ~/.qoder-cn/ 不一致，属官方现状"},
	},
	{
		Name:         "qoderwork",
		Aliases:      []string{"qoder-work", "qw"},
		Label:        "QoderWork（桌面端）",
		Tier:         TierNative,
		GlobalConfig: []string{"~/.qoderwork/settings.json"},
		Verify:       "重启 QoderWork 后发起新会话观察是否注入（当前版本不支持热加载）",
		Docs:         "https://docs.qoder.com/zh/qoderwork/hooks",
		Notes: []string{
			"🔴 仅支持用户级配置，没有项目级",
			"🔴 不向 hook 脚本注入任何环境变量，项目目录只能从 stdin JSON 的 cwd 取",
			"🔴 不支持热加载，改配置必须重启应用",
		},
	},
	{
		Name:         "qoderwork-cn",
		Aliases:      []string{"qoderworkcn", "qoder-work-cn"},
		Label:        "QoderWork CN",
		Tier:         TierNative,
		GlobalConfig: []string{"~/.qoderworkcn/settings.json"},
		Verify:       "重启 QoderWork CN 后观察（当前版本不支持热加载）",
		Docs:         "https://www.alibabacloud.com/help/zh/lingma/hook",
		Notes:        []string{"同 QoderWork：仅用户级、不注入环境变量、不支持热加载"},
	},
	{
		Name:          "codex",
		Aliases:       []string{"codex-cli", "openai-codex"},
		Label:         "Codex CLI",
		Tier:          TierNative,
		GlobalConfig:  []string{"~/.codex/hooks.json"},
		ProjectConfig: []string{".codex/hooks.json"},
		ExtraConfig:   []string{"在 ~/.codex/config.toml 中确保 [features] hooks = true（默认开启）"},
		Verify:        "Codex 中用 /hooks 检查、trust 并启停；新增 hook 首次运行需显式信任",
		Docs:          "https://developers.openai.com/codex/hooks",
		Notes: []string{
			"🔴 不注入环境变量，项目目录取自 stdin JSON 的 cwd",
			"🔴 非托管 hook 首次执行前需用户审核，信任按 hook 哈希持久化；改动后需重新审核",
			"hook 也可内联写在 config.toml 的 [hooks] 下，本工具只写 hooks.json 侧车文件",
		},
	},
	{
		Name:    "dsh",
		Aliases: []string{"deepseek-harness", "deepseek"},
		Label:   "DeepSeek Harness (dsh)",
		Tier:    TierBridge,
		// dsh 没有 shell hook 定义文件；官方桥接包把它翻译成自己的扩展点，
		// 所以复用的仍是 Claude Code 的 settings.json。
		GlobalConfig: []string{"~/.claude/settings.json"},
		ProjectEnv:   []string{"CLAUDE_PROJECT_DIR"},
		ExtraConfig: []string{
			"安装官方桥接包：dsh plugin --profile add dsh-hooks-claude-code",
			"该包把 Claude Code 的 hooks.json 协议翻译到 dsh 的 agent/session-start、agent/pre-step、tools/pre-execute、session/event",
		},
		Verify: "dsh 启动后查看会话日志中是否出现 hook 调用记录",
		Docs:   "https://github.com/deepseek-ai/deepseek-harness",
		Notes: []string{
			"🔴 dsh 原生不是 shell hook：钩子是注册在 Cordis 类型化扩展点上的插件，进程内调用、无 JSON-over-stdin 协议",
			"另一条桥接包 dsh-hooks-codex 可复用 Codex 的 hooks.json，二选一即可",
			"本工具不产出 dsh 插件代码（需 TypeScript + Cordis），只复用桥接包",
		},
	},
	{
		Name:    "traework",
		Aliases: []string{"trae-work", "tw", "trae-solo"},
		Label:   "TraeWork（原 TRAE SOLO）",
		Tier:    TierNative,
		// 证据取自应用包本身，不再靠「有没有公开文档」推断：
		//   · workbench JS 的资产表：{assetType:"hook", projectRelPath:".trae/hooks.json", globalRelPath:"hooks.json"}
		//   · libharness.dylib 含 SessionStart / UserPromptSubmit / PreToolUse / PostToolUse /
		//     Stop / SessionEnd / Notification，以及 HooksConfiguration（global_hooks_enabled、
		//     import_claude_folders、global_import_claude_enabled）
		//   · HookExecContext 字段 additional_context / text_stdout / blocking_error / stop_reason
		//     —— 与 TraeCode 是同一套「stdout 文本即注入上下文」协议
		// 全局候选：dylib 记默认数据目录 ~/.trae-local（本机不存在），
		// 而 ~/.trae-cn 与 ~/.trae 存在且与 TraeCode CN 同族，故三者都列。
		GlobalConfig:  []string{"~/.trae-local/hooks.json", "~/.trae-cn/hooks.json", "~/.trae/hooks.json"},
		ProjectConfig: []string{".trae/hooks.json"},
		ProjectEnv:    []string{"TRAE_PROJECT_DIR", "CLAUDE_PROJECT_DIR"},
		Verify:        "新开一个 TraeWork 会话，再 `mem audit --tail 5` 看是否出现 prompt-submit 记录",
		Docs:          "https://docs.trae.cn/traework/",
		Notes: []string{
			"🔴 原判为指令级是错的：官方文档未发布，但应用包内自带完整钩子实现",
			"项目级 `.trae/hooks.json` 与 TraeCode 项目级路径相同；全局是数据目录下的 hooks.json",
			"支持 import_claude_folders / global_import_claude_enabled：可直接导入 Claude Code 的钩子",
			"⚠️ 待实机验证：路径与事件来自应用包静态分析，尚未观察到真实会话触发（验证方法见 Verify）",
		},
	},
	{
		Name:    "workbuddy",
		Aliases: []string{"work-buddy", "wb"},
		Label:   "WorkBuddy",
		Tier:    TierNative,
		// 证据取自应用包本身。决定权**不在**应用自己的 ~/.workbuddy/settings.json
		// （那份只有 IM 通道绑定与凭据、无 hooks 段），而在它内置的 CodeBuddy 运行时：
		//   · WorkBuddy.app 内自带 CodeBuddy CLI 及中文文档，hooks.md 明确列出
		//     用户级 ~/.codebuddy/settings.json、项目级 <项目根>/.codebuddy/settings.json、
		//     项目本地 <项目根>/.codebuddy/settings.local.json
		//   · 内置插件 sheetagent 的 hooks/hooks.json 就是 CodeBuddy 格式
		//     （type:command + ${CODEBUDDY_PLUGIN_ROOT}）
		//   · 环境变量 ${CODEBUDDY_PROJECT_DIR} / ${CODEBUDDY_PLUGIN_ROOT} / ${CODEBUDDY_SKILL_DIR}
		// 故与 codebuddy 共用同一份用户级配置：写一处，两个工具都生效。
		GlobalConfig:  []string{"~/.codebuddy/settings.json"},
		ProjectConfig: []string{".codebuddy/settings.json", ".codebuddy/settings.local.json"},
		ProjectEnv:    []string{"CODEBUDDY_PROJECT_DIR"},
		Verify:        "新开一个 WorkBuddy 会话，再 `mem audit --tail 5` 看是否出现 prompt-submit 记录",
		Docs:          "https://www.workbuddy.cn/docs/workbuddy/Overview",
		Notes: []string{
			"🔴 原判为指令级是错的：不该只看应用自己的 settings.json，要看他内置的 Agent 运行时",
			"与 codebuddy 共用 ~/.codebuddy/settings.json —— 无需单独安装",
			"⚠️ 两者共用同一运行时（WorkBuddy 内置 CodeBuddy），故钩子侧无法区分二者；配置里保留运行时本名 codebuddy 作为标签，审计中的 harness 字段也会是 codebuddy —— 这是事实而非缺陷，二者钩子行为完全一致",
			"兜底通道仍在：`mem init --scope instruction` 把指令写进 AGENTS.md，但那是概率性的",
			"⚠️ 待实机验证：路径取自应用包内文档，尚未观察到桌面端会话触发（验证方法见 Verify）",
		},
	},
}

// All 返回全部 harness 的副本，顺序稳定。
func All() []Harness {
	out := make([]Harness, len(registry))
	copy(out, registry)
	return out
}

// Names 返回全部可用的 harness 名（含别名），已排序，供错误提示使用。
func Names() []string {
	var out []string
	for _, h := range registry {
		out = append(out, h.Name)
		out = append(out, h.Aliases...)
	}
	sort.Strings(out)
	return out
}

// Get 按名字或别名查找。
func Get(name string) (*Harness, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return nil, fmt.Errorf("未指定 harness；可选：%s", strings.Join(Names(), ", "))
	}
	for i := range registry {
		h := &registry[i]
		if h.Name == key {
			return h, nil
		}
		for _, a := range h.Aliases {
			if a == key {
				return h, nil
			}
		}
	}
	return nil, fmt.Errorf("未知 harness %q；可选：%s", name, strings.Join(Names(), ", "))
}

// Label 返回展示名。
func (h *Harness) LabelOrName() string {
	if h.Label != "" {
		return h.Label
	}
	return h.Name
}

// UsesProjectEnv 表示该 harness 是否注入环境变量来传递项目目录。
func (h *Harness) UsesProjectEnv() bool { return len(h.ProjectEnv) > 0 }

// ProjectDirFromEnv 按优先级从进程环境中推断项目目录。取不到返回空串。
func (h *Harness) ProjectDirFromEnv() string {
	for _, k := range h.ProjectEnv {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// ResolveGlobalConfig 选出应写入的用户级配置路径。
//
// 多候选时取「父目录已存在」的第一个，避免写进该工具根本不会读的位置；
// 若都不存在，返回第一个候选（首次配置场景）并置 isNew=true。
func (h *Harness) ResolveGlobalConfig() (path string, isNew bool) {
	for _, cand := range h.GlobalConfig {
		p := ExpandHome(cand)
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			return p, false
		}
	}
	if len(h.GlobalConfig) == 0 {
		return "", false
	}
	return ExpandHome(h.GlobalConfig[0]), true
}

// ExistingGlobalConfigs 返回候选路径中父目录已存在的全部项，供检测展示。
func (h *Harness) ExistingGlobalConfigs() []string {
	var out []string
	for _, cand := range h.GlobalConfig {
		p := ExpandHome(cand)
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// ExpandHome 展开路径开头的 ~。
func ExpandHome(p string) string {
	if p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return home
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[2:])
	}
	return p
}
