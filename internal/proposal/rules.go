package proposal

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/v2camp/v2mem/internal/distill"
)

// rulesExecutorName 是规则兜底在链路上的名字（诊断与 executors 名单口径）。
const rulesExecutorName = "rules"

// rulesGenerator 把旧的「规则蒸馏」内核接进 proposal 链，作为模型不可用时的兜底。
//
// 它不读 store、不调用模型，也不写任何状态：输入只有 GenerateRequest 里的证据
// 切片，输出与模型 executor 完全相同的 JSONL 契约，于是解析 / 校验 / 落库 /
// 消费窗口整条路径一条也不分叉 —— 兜底不是第二套流水线，只是另一个正文来源。
//
// 与模型路径的取舍：
//   - 只用 Eligible 的证据（与 BuildPrompt / countEligible 同一判定），
//     否则会出现「runner 算有可提案证据、规则却拿噪音凑候选」的双轨语义；
//   - ExtractRules 自带 3 条上限与说话人分源约束（工具载荷不产事实、
//     助手只贡献可复用结论），兜底不额外放宽或收紧；
//   - 抽不出事实返回空串，即 ParseDrafts 的合法 0 条，runner 落 noop 并消费窗口，
//     绝不把它伪装成 fail/backlog，否则坏掉的模型会把窗口永久卡住。
type rulesGenerator struct{}

// NewRulesGenerator 构造规则兜底 executor。
func NewRulesGenerator() Generator { return rulesGenerator{} }

func (rulesGenerator) Name() string { return rulesExecutorName }

func (rulesGenerator) Generate(_ context.Context, req GenerateRequest) (string, error) {
	obs := make([]distill.Observation, 0, len(req.Evidence))
	// ordinals 是 event_uid → req.Evidence 的 1 起下标。
	// 必须对齐全量切片而不是可提案观测的紧凑序号：evidence_idx 最终由
	// evidenceIDsFor 按 b.EvidenceIDs[n-1] 落库，错位会把事实挂到别的证据上。
	ordinals := make(map[string]int, len(req.Evidence))
	for i, ref := range req.Evidence {
		if !Eligible(ref) {
			continue
		}
		obs = append(obs, distill.Observation{
			ID: ref.EventUID, SessionID: ref.SessionID, Project: ref.Project,
			Summary: ref.Summary, Event: ref.EventType,
		})
		if _, seen := ordinals[ref.EventUID]; !seen {
			ordinals[ref.EventUID] = i + 1
		}
	}
	facts := distill.ExtractRules(obs)
	if len(facts) == 0 {
		return "", nil
	}

	var b strings.Builder
	for _, f := range facts {
		idx := make([]int, 0, len(f.ObsIDs))
		for _, id := range f.ObsIDs {
			// f.ObsIDs 只会来自上面喂进去的观测，映射必然命中；万一没命中
			// （契约被破坏）就跳过该序号，validateDraft 会以「没有证据引用」
			// 整条拒收，不会把无来源的候选放进库。
			if n, ok := ordinals[id]; ok {
				idx = append(idx, n)
			}
		}
		// scope 固定 project：规则从单会话摘要抽出的是可复用结论，不是会话内
		// 临时态；与 validateDraft 的默认 scope 同值，落库层不必再猜。
		// wireDraft 只有 string/[]int，Marshal 不会失败，故不分支。
		line, _ := json.Marshal(wireDraft{
			Kind: f.Kind, Scope: "project", Content: f.Content,
			Rationale: f.Rationale, AttributedTo: f.AttributedTo,
			EvidenceIdx: idx,
		})
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String(), nil
}
