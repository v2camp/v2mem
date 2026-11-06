package proposal

import (
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
)

// runner 与 BuildPrompt / executor 必须共用同一个提案准入判定。
// Fidelity 接入后若 runner 仍只看 ProposalEligible，pointer/none 会被
// 误算成可提案证据，形成「runner 算有、执行器算无」的双轨语义。
func TestRunnerUsesSingleProposalEligibility(t *testing.T) {
	ref := func(session string, f evidence.Fidelity) evidence.Ref {
		return evidence.Ref{SessionID: session, ProposalEligible: true, Fidelity: f}
	}

	blocked := []evidence.Ref{
		ref("s-pointer", evidence.FidelityPointer),
		ref("s-none", evidence.FidelityNone),
	}
	if got := countEligible(blocked); got != 0 {
		t.Fatalf("pointer/none 不应计入 eligible，got %d", got)
	}
	// 第二个 fallback 循环仍要给出非空 session，供读侧轨迹记录。
	if got := bundleSession(blocked); got != "s-pointer" {
		t.Fatalf("fallback 应返回首个非空 session，got %q", got)
	}

	mixed := []evidence.Ref{
		ref("s-pointer", evidence.FidelityPointer),
		ref("s-full", evidence.FidelityFull),
		ref("s-summary", evidence.FidelitySummary),
		ref("s-none", evidence.FidelityNone),
	}
	if got := countEligible(mixed); got != 2 {
		t.Fatalf("只有 full/summary 应计数，got %d", got)
	}
	if got := bundleSession(mixed); got != "s-full" {
		t.Fatalf("应优先返回首个可提案证据的 session，got %q", got)
	}

	off := []evidence.Ref{{SessionID: "s-off", ProposalEligible: false, Fidelity: evidence.FidelityFull}}
	if got := countEligible(off); got != 0 {
		t.Fatalf("ProposalEligible 总开关关闭时不得计数，got %d", got)
	}

	legacy := []evidence.Ref{{SessionID: "s-legacy", ProposalEligible: true}}
	if got := countEligible(legacy); got != 1 {
		t.Fatalf("空 Fidelity 的旧行应按 full 兼容，got %d", got)
	}
}
