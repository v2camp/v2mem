package proposal

import (
	"fmt"
	"testing"
)

// 真实存量样本：命中的是「过程叙述/一次性回执」，即该送去模型提炼的形态。
func TestIsProcessNarrativeSamples(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"完成回执+日期", "course-skill v4.29.1 已修复 v2svg 计数漂移（2026-11-21）…", true},
		{"补丁+日期", "replay-export v0.2.0 已补…（2026-11-21）", true},
		{"待办", "待办子仓任务：① …", true},
		{"完成括号", "B 任务完成（2026-11-21）…", true},
		{"显式日期", "根 AGENTS.md 通用原则表 2026-11-20 起按四区分组…", true},
		{"第一人称过程", "这个失败用例暴露了…我加了一条结构化声明判据", true},
		{"会话指代报错", "这个报错不是我们应用的问题…", true},
		{"时点词", "本次巡检发现三个 WARN，无 FAIL", true},
		{"提交短 sha", "提交 3f2a1bc 后主线恢复", true},

		{"既定事实", "本机 v2mem 生效二进制是 ~/.local/bin/mem，重建后版本随 main 走…", false},
		{"钩子配置", "WorkBuddy 钩子在 ~/.workbuddy/settings.json…", false},
		{"Java 轨", "Java 轨 lesson13-14-shop-basic 不读 .env，只读 System.getenv…", false},
		{"TS 轨", "TS 轨 13-16 的 .env 由 apps/lesson13-16-shop/src/config/env.ts 按候选路径加载…", false},
		{"图表契约", "图表题采用两层契约：LLM 只产出 ChartIntent…", false},
		{"Text-to-SQL", "Text-to-SQL 不做全量 schema 注入…", false},

		{"空串", "", false},
		{"纯事实陈述", "记忆库默认落在 ~/.v2mem，不放代码目录", false},
		{"含年份非日期", "2026 年发布的版本才有该字段", false},
		{"日期格式不全", "记录于 2026-11 的版本", false},
		{"提交号不足七位", "提交 abcdef 前先跑测试", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProcessNarrative(tc.content); got != tc.want {
				t.Fatalf("IsProcessNarrative(%q)=%v want %v", tc.content, got, tc.want)
			}
		})
	}
}

// 导出选择器是内部判据的扩展集合：三个基础信号命中时必然也命中。
func TestIsProcessNarrativeCoversBaseSignals(t *testing.T) {
	for _, s := range []string{
		"我无法读取该页面",
		"这个报错说明缺少凭证",
		"目前还没有跑完迁移",
	} {
		if !isProcessNarrative(s) {
			t.Fatalf("基础判据应命中: %q", s)
		}
		if !IsProcessNarrative(s) {
			t.Fatalf("导出选择器必须包含基础判据: %q", s)
		}
	}
}

// 选择器只用于「挑出来送提炼」，不得反向改变 validateDraft 的拒收口径：
// 仅含日期/回执的正文仍应作为合法候选通过。
func TestIsProcessNarrativeNotWiredIntoValidateDraft(t *testing.T) {
	content := "B 任务完成（2026-11-21）"
	raw := fmt.Sprintf(`{"kind":"fact","content":%q,"evidence_idx":[1]}`, content)
	got, err := ParseDrafts(raw, 1)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(got.Drafts) != 1 {
		t.Fatalf("validateDraft 行为必须不变，got drafts=%d invalid=%d", len(got.Drafts), got.Invalid)
	}
}
