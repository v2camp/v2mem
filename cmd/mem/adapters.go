package main

import (
	"encoding/json"
	"strings"

	"github.com/v2camp/v2mem/internal/harness"
)

// 本文件是「独立适配器层」的轻量形态：一张按宿主名分派的字段映射表。
//
// 它只描述「去哪里取字段」，不描述 Fidelity 判定 —— observationText 的主体
// 分支（有真实正文→full / 只有摘要→summary / 无→none）对任何宿主一致。
// 新增宿主 = 在 adapterTable 加一行 + 一份 fixture 测试，核心函数无需改动。
// 不新建 CaptureEvent、不改 hookInput、不引入第四层契约：仍复用
// hookInput → observationText → evidence.RawEvent 现有链路。

// textSelector 从 hookInput 取一个文本型字段（Stop / SessionEnd 的最终回复候选）。
type textSelector func(hookInput) string

// rawSelector 从 hookInput 取一个原始 JSON 字段（PostToolUse 的工具正文）。
type rawSelector func(hookInput) json.RawMessage

// adapterConfig 是单个宿主的字段映射。
type adapterConfig struct {
	// stopReply 是 Stop / SessionEnd 最终回复字段的优先级列表：
	// 命中第一个非空值即作为真实正文。空列表表示该宿主不在 stdin 带
	// 最终回复（如 Claude 走 transcript），此时最多只能得到 meta 摘要或 none。
	stopReply []textSelector
	// toolInput / toolOutput 是 PostToolUse 的工具输入 / 输出来源。
	// 留空（nil）表示沿用默认的 in.ToolInput / in.ToolOutput。
	toolInput  rawSelector
	toolOutput rawSelector
	// allowMetaSummary 表示无真实正文时，是否允许把 status / reason
	// 拼成结构化摘要（绝不回退成事件名字面量）。
	allowMetaSummary bool
}

// 字段选择器：把「字段名」收敛成函数，新增宿主只需组合它们，不动核心分支。
func selectLastAssistant(in hookInput) string       { return in.LastAssistantMessage }
func selectTextContent(in hookInput) string         { return in.TextContent }
func selectToolInput(in hookInput) json.RawMessage  { return in.ToolInput }
func selectToolOutput(in hookInput) json.RawMessage { return in.ToolOutput }

// defaultAdapter 是未知 / 未指定 harness 的保守默认：保持本波之前的既有行为
// —— last_assistant_message 优先、text_content 回退，两者皆无才用 status / reason 摘要。
var defaultAdapter = adapterConfig{
	stopReply:        []textSelector{selectLastAssistant, selectTextContent},
	toolInput:        selectToolInput,
	toolOutput:       selectToolOutput,
	allowMetaSummary: true,
}

// claudeAdapter：Claude 系的最终回复走 transcript 增量补写，stdin 不带
// Stop 正文字段，故 stopReply 为空；无正文时仍允许 status / reason 作摘要。
var claudeAdapter = adapterConfig{
	stopReply:        nil,
	toolInput:        selectToolInput,
	toolOutput:       selectToolOutput,
	allowMetaSummary: true,
}

// traeAdapter：Trae（TraeCode / TraeWork）把最终回复放在 last_assistant_message，
// 退化时给 text_content；两者皆无才回退 status / reason 摘要。
var traeAdapter = adapterConfig{
	stopReply:        []textSelector{selectLastAssistant, selectTextContent},
	toolInput:        selectToolInput,
	toolOutput:       selectToolOutput,
	allowMetaSummary: true,
}

// adapterTable 是「按宿主名分派」的字段映射表。key 用 harness.Get 归一后的
// 规范名（claude / traecode / traework / …）。新增宿主只在此加一行。
var adapterTable = map[string]adapterConfig{
	"claude":   claudeAdapter,
	"traecode": traeAdapter,
	"traework": traeAdapter,
}

// adapterFor 按宿主名返回字段映射配置；未知 / 空 harness 走保守默认。
// 纯函数：只读适配器表与 harness 注册表，不依赖进程级可变状态。
func adapterFor(hname string) adapterConfig {
	key := strings.ToLower(strings.TrimSpace(hname))
	// 别名（trae / trae-cn / claude-code / …）归一到规范名，表只需登记规范名。
	if h, err := harness.Get(key); err == nil {
		key = h.Name
	}
	if cfg, ok := adapterTable[key]; ok {
		return cfg
	}
	return defaultAdapter
}

// adapterRawField 取宿主配置的工具正文原始字段；选择器为空时回退默认字段。
func adapterRawField(sel rawSelector, fallback json.RawMessage, in hookInput) json.RawMessage {
	if sel == nil {
		return fallback
	}
	return sel(in)
}
