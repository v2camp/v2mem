package eval

import (
	"math"
	"testing"
)

// 口径与 design-eval-flows.md §1 一致：准出率 = 终态 / (准入 - 豁免)；
// 重复、悬空、严密度违例任一非 0 即不过。

func TestReconcileBalancedLedgerPasses(t *testing.T) {
	got := Reconcile([]FlowLedger{
		{WindowID: "b1", Admitted: 3, Terminal: 3},
		{WindowID: "b2", Admitted: 2, Terminal: 2},
	})
	if got.ExitRate != 1.0 {
		t.Fatalf("ExitRate = %v, want 1.0", got.ExitRate)
	}
	if got.Dangling != 0 || got.Duplicates != 0 || got.Forced != 0 {
		t.Fatalf("Dangling/Duplicates/Forced = %d/%d/%d, want 0/0/0", got.Dangling, got.Duplicates, got.Forced)
	}
	if !got.Pass() {
		t.Fatalf("Pass() = false, want true")
	}
}

func TestReconcileMissingTerminalReportsDangling(t *testing.T) {
	// 逐窗口「终态数 = 准入数」：b2 差 1 即报悬空并点名窗口。
	got := Reconcile([]FlowLedger{
		{WindowID: "b1", Admitted: 3, Terminal: 3},
		{WindowID: "b2", Admitted: 2, Terminal: 1},
	})
	if got.Dangling != 1 {
		t.Fatalf("Dangling = %d, want 1", got.Dangling)
	}
	if len(got.DanglingWindows) != 1 || got.DanglingWindows[0] != "b2" {
		t.Fatalf("DanglingWindows = %v, want [b2]", got.DanglingWindows)
	}
	if got.ExitRate >= 1.0 {
		t.Fatalf("ExitRate = %v, want < 1", got.ExitRate)
	}
	if got.Pass() {
		t.Fatalf("Pass() = true, want false")
	}
}

func TestReconcileExemptRemovedFromDenominator(t *testing.T) {
	// 豁免（休眠/关机/总闸/灾备）显式标记后从分母剔除，剩余数据全部终态即 100%。
	got := Reconcile([]FlowLedger{{WindowID: "b1", Admitted: 5, Terminal: 3, Exempt: 2}})
	if got.Denominator != 3 {
		t.Fatalf("Denominator = %d, want 3", got.Denominator)
	}
	if got.ExitRate != 1.0 || !got.Pass() {
		t.Fatalf("ExitRate = %v Pass = %v, want 1.0 true", got.ExitRate, got.Pass())
	}
}

func TestReconcileCountsDuplicatesAndForced(t *testing.T) {
	// 重复提交与严密度违例（强行写入）都必须让对账失败，哪怕准出率是 100%。
	got := Reconcile([]FlowLedger{{WindowID: "b1", Admitted: 2, Terminal: 2, Duplicates: 1}})
	if got.Duplicates != 1 || got.Pass() {
		t.Fatalf("Duplicates = %d Pass = %v, want 1 false", got.Duplicates, got.Pass())
	}
	got = Reconcile([]FlowLedger{{WindowID: "b1", Admitted: 2, Terminal: 2, Forced: 3}})
	if got.Forced != 3 || got.Pass() {
		t.Fatalf("Forced = %d Pass = %v, want 3 false", got.Forced, got.Pass())
	}
}

func TestReconcileEmptyIsVacuousPass(t *testing.T) {
	// 无窗口 = 无未决数据。分母 0 时准出率记 1，不虚报失败。
	got := Reconcile(nil)
	if got.ExitRate != 1.0 || !got.Pass() {
		t.Fatalf("ExitRate = %v Pass = %v, want 1.0 true", got.ExitRate, got.Pass())
	}
}

func TestReconcileExemptClampsDenominator(t *testing.T) {
	// 坏账（豁免 > 准入）不得让分母为负；多余终态是账目矛盾，必须显式上报而非静默通过。
	got := Reconcile([]FlowLedger{{WindowID: "b1", Admitted: 1, Terminal: 0, Exempt: 5}})
	if got.Denominator != 0 {
		t.Fatalf("Denominator = %d, want 0", got.Denominator)
	}
	got = Reconcile([]FlowLedger{{WindowID: "b1", Admitted: 1, Terminal: 4}})
	if len(got.InconsistentWindows) != 1 || got.InconsistentWindows[0] != "b1" {
		t.Fatalf("InconsistentWindows = %v, want [b1]", got.InconsistentWindows)
	}
	if got.Pass() {
		t.Fatalf("Pass() = true on inconsistent ledger, want false")
	}
}

func TestReconcileAccumulatesAcrossWindows(t *testing.T) {
	got := Reconcile([]FlowLedger{
		{WindowID: "b1", Admitted: 4, Terminal: 3, Exempt: 0, Duplicates: 2},
		{WindowID: "b2", Admitted: 6, Terminal: 4, Exempt: 1, Duplicates: 1},
	})
	if got.Admitted != 10 || got.Exempt != 1 || got.Denominator != 9 || got.Terminal != 7 {
		t.Fatalf("totals = %d/%d/%d/%d, want 10/1/9/7", got.Admitted, got.Exempt, got.Denominator, got.Terminal)
	}
	if got.Dangling != 2 || got.Duplicates != 3 {
		t.Fatalf("Dangling/Duplicates = %d/%d, want 2/3", got.Dangling, got.Duplicates)
	}
	if math.Abs(got.ExitRate-7.0/9.0) > 1e-9 {
		t.Fatalf("ExitRate = %v, want %v", got.ExitRate, 7.0/9.0)
	}
}

func TestManualRate(t *testing.T) {
	cases := []struct {
		name         string
		manual, auto int
		want         float64
	}{
		{"零动作记 0", 0, 0, 0},
		{"正常比例", 1, 100, 0.01},
		{"无人工记 0", 0, 10, 0},
		{"全自动为 0 且有人工记 1", 5, 0, 1},
		{"负数钳 0", -5, -3, 0},
		{"人工负数钳 0", -5, 0, 0},
	}
	for _, c := range cases {
		if got := ManualRate(c.manual, c.auto); got != c.want {
			t.Fatalf("%s: ManualRate(%d,%d) = %v, want %v", c.name, c.manual, c.auto, got, c.want)
		}
	}
}
