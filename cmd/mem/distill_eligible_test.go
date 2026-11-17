package main

import (
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// meta_json 的准入映射必须与 internal/store/evidence.go 的 evidenceMeta 契约同步。
// 重点：旧行没有 proposal_eligible 键时按事件类型兜底 —— 工具载荷 / 未解析行
// 不再一律放行（R9 的 JSON 噪音根因），其余事件仍兼容以让历史窗口收敛。
func TestDistillObservationEligibleMetaMatrix(t *testing.T) {
	cases := []struct {
		name  string
		meta  string
		event string
		want  bool
	}{
		{"legacy 空 meta", "", "", true},
		{"legacy 空对象", `{}`, "", true},
		{"legacy 只有 source", `{"source":"hook-observe"}`, "", true},
		{"full 准入", `{"proposal_eligible":true,"fidelity":"full"}`, "stop", true},
		{"summary 准入", `{"proposal_eligible":true,"fidelity":"summary"}`, "stop", true},
		{"legacy 兼容空 fidelity", `{"proposal_eligible":true}`, "user-prompt", true},
		{"pointer 原始载荷", `{"proposal_eligible":false,"fidelity":"pointer","adapter":"workbuddy"}`, "unparsed", false},
		{"总开关关闭", `{"proposal_eligible":false,"fidelity":"full"}`, "stop", false},
		{"none 无正文", `{"proposal_eligible":true,"fidelity":"none"}`, "stop", false},
		{"坏 JSON 按 legacy 放行", `not-json`, "", true},
		{"legacy 工具载荷挡住", `{"source":"hook-observe"}`, "post-tool-use", false},
		{"legacy unparsed 挡住", "", "unparsed", false},
		{"legacy 助手回复放行", "", "stop", true},
		{"legacy 用户提问放行", "", "user-prompt", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := distillObservationEligible(c.meta, c.event); got != c.want {
				t.Fatalf("distillObservationEligible(%q,%q)=%v want %v", c.meta, c.event, got, c.want)
			}
		})
	}
}

func appendObs(t *testing.T, db string, rows []store.ObsInput) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, r := range rows {
		if _, err := st.AppendObservation(r); err != nil {
			t.Fatalf("第 %d 条观测落库失败: %v", i, err)
		}
	}
}

func pendingCandidatesJSON(t *testing.T, db string) string {
	t.Helper()
	out, err := captureStdout(t, func() error {
		return cmdCandidates([]string{"ls", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// 回归护栏：pointer 观测的 transcript 原始 JSON、以及无 meta 键的 legacy
// 工具载荷都不得变成 240 字符的候选；同一窗口里 full 的可读正文与 legacy
// 用户提问仍须照常蒸馏，并带上 attributed_to。
func TestDistillSkipsPointerPayloadKeepsReadableAndLegacy(t *testing.T) {
	db := testDB(t)
	ap := t.TempDir() + "/audit.jsonl"
	appendObs(t, db, []store.ObsInput{
		{
			SessionID: "sess", Project: "demo", Event: "unparsed",
			Summary:  `{"id":"01a1","parentId":"01a0","type":"function_call","callId":"c1"}`,
			MetaJSON: `{"proposal_eligible":false,"fidelity":"pointer","adapter":"workbuddy"}`,
		},
		{
			SessionID: "sess", Project: "demo", Event: "stop",
			Summary:  "决策：记忆库放在用户目录，不放代码目录",
			MetaJSON: `{"proposal_eligible":true,"fidelity":"full","adapter":"workbuddy"}`,
		},
		{
			SessionID: "sess", Project: "demo", Event: "user-prompt",
			Summary: "偏好：旧行也要能蒸馏",
		},
		{
			SessionID: "sess", Project: "demo", Event: "post-tool-use",
			Summary: "偏好：工具载荷不该变成事实",
		},
	})

	if err := cmdDistill([]string{"--db", db, "--session", "sess", "--provider", "none", "--audit-file", ap}); err != nil {
		t.Fatal(err)
	}
	out := pendingCandidatesJSON(t, db)
	if strings.Contains(out, "function_call") || strings.Contains(out, "parentId") {
		t.Fatalf("pointer 载荷不得进候选，got %s", out)
	}
	if !strings.Contains(out, "用户目录") {
		t.Fatalf("full 可读正文应生成候选，got %s", out)
	}
	if !strings.Contains(out, "旧行也要能蒸馏") {
		t.Fatalf("legacy 用户提问应照常蒸馏，got %s", out)
	}
	if strings.Contains(out, "工具载荷不该变成事实") {
		t.Fatalf("legacy 工具载荷不得再进候选，got %s", out)
	}
	// 分源标注：用户提问产 preference=user，助手回复产 decision=assistant。
	if !strings.Contains(out, `"attributed_to":"user"`) ||
		!strings.Contains(out, `"attributed_to":"assistant"`) {
		t.Fatalf("候选应带 attributed_to，got %s", out)
	}
}

// 全部观测都不准入时，应安静收敛（无候选、不调 distill.Run、不占新窗口）。
func TestDistillAllPointerProducesNoCandidate(t *testing.T) {
	db := testDB(t)
	ap := t.TempDir() + "/audit.jsonl"
	appendObs(t, db, []store.ObsInput{
		{
			SessionID: "sess", Project: "demo", Event: "unparsed",
			Summary:  `{"id":"01a1","type":"function_call"}`,
			MetaJSON: `{"proposal_eligible":false,"fidelity":"pointer"}`,
		},
		{
			SessionID: "sess", Project: "demo", Event: "unparsed",
			Summary:  `{"id":"01a2","type":"function_call_result"}`,
			MetaJSON: `{"proposal_eligible":false,"fidelity":"pointer"}`,
		},
	})

	if err := cmdDistill([]string{"--db", db, "--session", "sess", "--provider", "none", "--audit-file", ap}); err != nil {
		t.Fatal(err)
	}
	out := pendingCandidatesJSON(t, db)
	if strings.Contains(out, `"status":"pending"`) || strings.Contains(out, "function_call") {
		t.Fatalf("全 pointer 窗口不应产出候选，got %s", out)
	}
}
