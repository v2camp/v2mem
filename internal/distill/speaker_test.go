package distill

import (
	"strings"
	"testing"
)

func TestSpeakerForEvent(t *testing.T) {
	cases := map[string]string{
		"":                "user", // 旧夹具兼容
		"user-prompt":     "user",
		"user.prompt":     "user",
		"assistant-text":  "assistant",
		"assistant.final": "assistant",
		"stop":            "assistant",
		"session-end":     "assistant",
		"post-tool-use":   "",
		"tool-use":        "",
		"unparsed":        "",
	}
	for in, want := range cases {
		if got := SpeakerForEvent(in); got != want {
			t.Errorf("SpeakerForEvent(%q)=%q want %q", in, got, want)
		}
	}
}

// 规则蒸馏的分源约束：工具载荷不产事实、助手不产 preference、
// 每条事实带 attributed_to；空事件夹具仍兼容旧行为。
func TestExtractRulesSpeakerSplit(t *testing.T) {
	obs := []Observation{
		{ID: "1", Event: "post-tool-use", Summary: "决策：工具载荷不该变成事实"},
		{ID: "2", Event: "unparsed", Summary: "决策：原始 JSON 也不该变成事实"},
		{ID: "3", Event: "user-prompt", Summary: "偏好：候选要标出是谁说的"},
		{ID: "4", Event: "stop", Summary: "偏好：助手不该替用户定义偏好"},
		{ID: "5", Event: "assistant-text", Summary: "踩坑：根因在 transcript 解析器"},
		{ID: "6", Summary: "决策：旧夹具仍要能收敛"},
	}
	facts := ExtractRules(obs)
	if len(facts) != 3 {
		t.Fatalf("应产出 3 条，got %d: %+v", len(facts), facts)
	}
	byID := map[string]Fact{}
	for _, f := range facts {
		byID[f.ObsIDs[0]] = f
	}
	for _, id := range []string{"1", "2", "4"} {
		if _, ok := byID[id]; ok {
			t.Errorf("观测 %s 不该产事实", id)
		}
	}
	if byID["3"].AttributedTo != "user" || byID["3"].Kind != "preference" {
		t.Errorf("用户 preference 标注错: %+v", byID["3"])
	}
	if byID["5"].AttributedTo != "assistant" || byID["5"].Kind != "pitfall" {
		t.Errorf("助手 pitfall 标注错: %+v", byID["5"])
	}
	if byID["6"].AttributedTo != "user" {
		t.Errorf("空事件夹具应兼容为 user，got %+v", byID["6"])
	}
}

func TestAttributeFactsPrefersEvidenceOverModel(t *testing.T) {
	obs := []Observation{
		{ID: "u", Event: "user-prompt", Summary: "用户原话"},
		{ID: "a", Event: "stop", Summary: "助手结论"},
	}
	facts := []Fact{
		{ObsIDs: []string{"a", "u"}, AttributedTo: "assistant"},
		{ObsIDs: []string{"a"}, AttributedTo: "user"},
		{ObsIDs: []string{"unknown"}, AttributedTo: "system"},
	}
	got := attributeFacts(facts, obs)
	for i, want := range []string{"user", "assistant", ""} {
		if got[i].AttributedTo != want {
			t.Errorf("第 %d 条=%q want %q", i+1, got[i].AttributedTo, want)
		}
	}
}

func TestPromptForStatesSourceRules(t *testing.T) {
	p := promptFor([]Observation{{ID: "u", Event: "user-prompt", Summary: "按 B 来推进"}})
	for _, want := range []string{"(user/user-prompt)", "No Echo Extraction", "attributed_to", "工具载荷不产事实"} {
		if !strings.Contains(p, want) {
			t.Errorf("提示词缺少 %q:\n%s", want, p)
		}
	}
}
