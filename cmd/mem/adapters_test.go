package main

import (
	"encoding/json"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/harness"
)

// 本文件锁住 T1 的「按宿主分派字段映射」：表决定「去哪里取字段」，
// observationText 的主体 Fidelity 判定分支（full / summary / none）对所有宿主一致。
// 全部用例只调纯函数与构造层，不碰 ~/.v2mem、不跑库。

// TestAdapterForDispatch 锁住查表纯函数：已知宿主命中专表、别名归一、
// 未知 / 空宿主走保守默认（保持既有行为）。
func TestAdapterForDispatch(t *testing.T) {
	cases := []struct {
		hname         string
		wantStopReply int
		wantMeta      bool
	}{
		{"claude", 0, true},
		{"claude-code", 0, true}, // 别名归一到 claude
		{"trae", 2, true},        // 别名归一到 traecode
		{"traecode", 2, true},
		{"traework", 2, true},
		{"cursor", 2, true}, // 已知 harness 但未登记 → 保守默认
		{"unknown-host", 2, true},
		{"", 2, true}, // 无宿主上下文 → 保守默认
	}
	for _, tc := range cases {
		t.Run("adapterFor/"+tc.hname, func(t *testing.T) {
			cfg := adapterFor(tc.hname)
			if len(cfg.stopReply) != tc.wantStopReply {
				t.Errorf("stopReply 字段数 = %d, want %d", len(cfg.stopReply), tc.wantStopReply)
			}
			if cfg.allowMetaSummary != tc.wantMeta {
				t.Errorf("allowMetaSummary = %v, want %v", cfg.allowMetaSummary, tc.wantMeta)
			}
			if cfg.toolInput == nil || cfg.toolOutput == nil {
				t.Errorf("工具正文选择器不应为空")
			}
		})
	}
}

// TestAdapterFieldMappingByHarness 表驱动：各宿主取最终回复 / 工具正文由表决定，
// 而 Fidelity 分级规则不随宿主变化。覆盖 Claude（走 transcript）、Trae 优先级、
// 以及未知宿主的保守默认。
func TestAdapterFieldMappingByHarness(t *testing.T) {
	cases := []struct {
		name     string
		hname    string
		event    harness.Event
		in       hookInput
		wantText string
		wantFid  evidence.Fidelity
	}{
		// ---- Claude：走 transcript，stdin 不带 Stop 最终回复正文字段 ----
		{
			name:     "claude/Stop 带 last_assistant_message 也忽略 → none",
			hname:    "claude",
			event:    harness.EventStop,
			in:       hookInput{LastAssistantMessage: "不该被当作正文"},
			wantText: "",
			wantFid:  evidence.FidelityNone,
		},
		{
			name:     "claude/Stop 只有 status → 结构化 summary",
			hname:    "claude",
			event:    harness.EventStop,
			in:       hookInput{Status: "completed"},
			wantText: "status=completed",
			wantFid:  evidence.FidelitySummary,
		},
		{
			name:     "claude/Stop 空载荷 → none + 空正文",
			hname:    "claude",
			event:    harness.EventStop,
			in:       hookInput{},
			wantText: "",
			wantFid:  evidence.FidelityNone,
		},
		{
			name:     "claude/SessionEnd 空载荷 → none",
			hname:    "claude",
			event:    harness.EventSessionEnd,
			in:       hookInput{},
			wantText: "",
			wantFid:  evidence.FidelityNone,
		},
		{
			name:  "claude/PostToolUse 仍取默认工具字段 → full",
			hname: "claude",
			event: harness.EventPostToolUse,
			in: hookInput{
				ToolName: "Bash", ToolInput: json.RawMessage(`"echo claude"`),
			},
			wantText: "tool:Bash\necho claude",
			wantFid:  evidence.FidelityFull,
		},
		{
			name:     "claude/PostToolUse 只有工具名 → summary",
			hname:    "claude",
			event:    harness.EventPostToolUse,
			in:       hookInput{ToolName: "Bash"},
			wantText: "tool:Bash",
			wantFid:  evidence.FidelitySummary,
		},
		// ---- Trae：last_assistant_message 优先、text_content 回退 ----
		{
			name:  "trae/last_assistant_message 优先于 text_content",
			hname: "trae",
			event: harness.EventStop,
			in: hookInput{
				LastAssistantMessage: "结论甲", TextContent: "备用乙",
			},
			wantText: "结论甲",
			wantFid:  evidence.FidelityFull,
		},
		{
			name:     "trae/last_assistant_message 缺失回退 text_content",
			hname:    "trae",
			event:    harness.EventStop,
			in:       hookInput{TextContent: "备用乙"},
			wantText: "备用乙",
			wantFid:  evidence.FidelityFull,
		},
		{
			name:     "trae/缺载荷 → none + 空正文",
			hname:    "trae",
			event:    harness.EventStop,
			in:       hookInput{},
			wantText: "",
			wantFid:  evidence.FidelityNone,
		},
		{
			name:     "trae/只有 status → summary",
			hname:    "trae",
			event:    harness.EventStop,
			in:       hookInput{Status: "completed"},
			wantText: "status=completed",
			wantFid:  evidence.FidelitySummary,
		},
		{
			name:     "traework/SessionEnd 带 last_assistant_message → full",
			hname:    "traework",
			event:    harness.EventSessionEnd,
			in:       hookInput{LastAssistantMessage: "会话总结"},
			wantText: "会话总结",
			wantFid:  evidence.FidelityFull,
		},
		// ---- 未知宿主：保守默认，保持既有行为 ----
		{
			name:     "unknown/仍取 last_assistant_message → full",
			hname:    "some-new-host",
			event:    harness.EventStop,
			in:       hookInput{LastAssistantMessage: "默认正文"},
			wantText: "默认正文",
			wantFid:  evidence.FidelityFull,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, fid := observationText(tc.event, tc.in, false, tc.hname)
			if fid != tc.wantFid {
				t.Errorf("Fidelity=%q want %q", fid, tc.wantFid)
			}
			if text != tc.wantText {
				t.Errorf("text=%q want %q", text, tc.wantText)
			}
			if tc.wantFid == evidence.FidelityNone && text != "" {
				t.Errorf("none 时不得伪造正文: %q", text)
			}
		})
	}
}

// TestAdapterNewHarnessOnlyNeedsTableEntry 是「新增宿主只加表项即可生效」的示范：
// 在测试里注册一个临时宿主配置，observationText 的核心判定分支一行未改，
// 仅凭表项就改变了「去哪里取字段」与「是否允许摘要」。
func TestAdapterNewHarnessOnlyNeedsTableEntry(t *testing.T) {
	const name = "fixture-host"
	old, existed := adapterTable[name]
	adapterTable[name] = adapterConfig{
		// 该宿主把最终回复放在 text_content、忽略 last_assistant_message，
		// 且不允许 status / reason 摘要；工具正文选择器留空以验证「省略即回退默认」。
		stopReply:        []textSelector{selectTextContent},
		allowMetaSummary: false,
	}
	t.Cleanup(func() {
		if existed {
			adapterTable[name] = old
		} else {
			delete(adapterTable, name)
		}
	})

	// 1) 按新表项取 text_content，忽略 last_assistant_message。
	text, fid := observationText(harness.EventStop, hookInput{
		LastAssistantMessage: "不该取这个", TextContent: "新宿主正文",
	}, false, name)
	if fid != evidence.FidelityFull || text != "新宿主正文" {
		t.Fatalf("新表项应生效：fid=%q text=%q", fid, text)
	}

	// 2) 无正文 + 不允许 meta 摘要 → none（而不是 status=... 摘要）。
	text, fid = observationText(harness.EventStop, hookInput{Status: "completed"}, false, name)
	if fid != evidence.FidelityNone || text != "" {
		t.Fatalf("allowMetaSummary=false 时应为 none+空正文：fid=%q text=%q", fid, text)
	}

	// 3) 工具正文选择器留空时，PostToolUse 仍回退默认字段。
	text, fid = observationText(harness.EventPostToolUse, hookInput{
		ToolName: "Bash", ToolInput: json.RawMessage(`"echo fixture"`),
	}, false, name)
	if fid != evidence.FidelityFull || text != "tool:Bash\necho fixture" {
		t.Fatalf("工具字段应回退默认：fid=%q text=%q", fid, text)
	}
}
