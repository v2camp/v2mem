package main

// S11 触达面的纯逻辑用例：逐字格式、预算整项截断、尾部审计判定、天数取整。
// 集成路径（真实库 + 真实钩子）在 touch_test.go。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touchItemsAll() []touchItem {
	return []touchItem{
		{kind: "isolated", label: "隔离 3"},
		{kind: "unreviewed", label: "未审核最老 4 天"},
		{kind: "model", label: "模型不可用"},
		{kind: "maintain", label: "维护失败"},
	}
}

func TestRenderTouchLineFormats(t *testing.T) {
	cases := []struct {
		name  string
		items []touchItem
		note  string
		want  string
	}{
		{"无项零输出", nil, "", ""},
		{"只有隔离", []touchItem{{kind: "isolated", label: "隔离 3"}}, "", "待人工 1 项（隔离 3）\n"},
		{"只有未审核", []touchItem{{kind: "unreviewed", label: "未审核最老 4 天"}}, "", "待人工 1 项（未审核最老 4 天）\n"},
		{"只有模型", []touchItem{{kind: "model", label: "模型不可用"}}, "", "待人工 1 项（模型不可用）\n"},
		{"只有维护", []touchItem{{kind: "maintain", label: "维护失败"}}, "", "待人工 1 项（维护失败）\n"},
		{
			"四项固定顺序", touchItemsAll(), "",
			"待人工 4 项（隔离 3 ｜ 未审核最老 4 天 ｜ 模型不可用 ｜ 维护失败）\n",
		},
		{
			"合并待裁决指引", []touchItem{{kind: "unreviewed", label: "未审核最老 4 天"}}, "[待裁决] 本工程 2 条",
			"待人工 1 项（未审核最老 4 天）｜[待裁决] 本工程 2 条\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderTouchLine(c.items, c.note, 1200); got != c.want {
				t.Fatalf("renderTouchLine = %q, want %q", got, c.want)
			}
		})
	}
}

// 超预算只丢整项，且按 隔离 → 未审核 → 模型 → 维护 的固定顺序从尾部丢。
func TestRenderTouchLineBudgetDropsWholeItems(t *testing.T) {
	items := touchItemsAll()
	two := "待人工 2 项（隔离 3 ｜ 未审核最老 4 天）\n"
	if got := renderTouchLine(items, "", runeLen(two)); got != two {
		t.Fatalf("预算只够两项时应保留前两项，got %q", got)
	}
	one := "待人工 1 项（隔离 3）\n"
	if got := renderTouchLine(items, "", runeLen(one)); got != one {
		t.Fatalf("预算只够一项时应只留隔离，got %q", got)
	}
	full := renderTouchLine(items, "", 1200)
	if got := renderTouchLine(items, "", runeLen(full)); got != full {
		t.Fatalf("预算恰好等于整行时应完整输出，got %q", got)
	}
	if got := renderTouchLine(items, "", runeLen(one)-1); got != "" {
		t.Fatalf("一项都放不下时应零输出，got %q", got)
	}

	// 扫全部预算：任何非空结果都必须是完整格式（不出现半个词）。
	for b := 0; b <= runeLen(full)+5; b++ {
		got := renderTouchLine(items, "[待裁决] 指引", b)
		if got == "" {
			continue
		}
		if n := runeLen(got); n > b {
			t.Fatalf("budget=%d 时输出 %d rune：%q", b, n, got)
		}
		if !strings.HasPrefix(got, "待人工 ") || !strings.HasSuffix(got, "）\n") {
			t.Fatalf("budget=%d 时出现半截行：%q", b, got)
		}
	}
}

// 指引整段可丢：预算装不下「核心行 + 指引」时先丢指引，再动项。
func TestRenderTouchLineNoteYieldsBeforeItems(t *testing.T) {
	items := []touchItem{{kind: "unreviewed", label: "未审核最老 4 天"}}
	core := "待人工 1 项（未审核最老 4 天）\n"
	got := renderTouchLine(items, "[待裁决] 很长很长的操作指引", runeLen(core))
	if got != core {
		t.Fatalf("指引放不下时应只保留核心行，got %q", got)
	}
}

func TestCeilDays(t *testing.T) {
	cases := []struct{ sec, want int64 }{
		{-5, 0}, {0, 0}, {1, 1}, {86399, 1}, {86400, 1}, {86401, 2}, {3 * 86400, 3},
	}
	for _, c := range cases {
		if got := ceilDays(c.sec); got != c.want {
			t.Errorf("ceilDays(%d) = %d, want %d", c.sec, got, c.want)
		}
	}
}

func writeAuditLine(t *testing.T, f *os.File, line string) {
	t.Helper()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// 尾部判定：只认最近一条 proposal distill；坏行跳过；读不到/关闭/空路径一律不显示。
func TestLastProposalDistill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// 先灌超过 64KB 的填充，强制走「尾部起点落在半行」的分支。
	for i := 0; i < 2000; i++ {
		writeAuditLine(t, f, fmt.Sprintf(`{"ts":%d,"event":"prompt-submit","empty":false,"ms":1}`, i))
	}
	writeAuditLine(t, f, `{"ts":9,"event":"distill","source":"proposal","mode":"backlog"}`)
	writeAuditLine(t, f, `{"ts":10,"event":"distill","source":"proposal","mode":"ok","provider":"rules"}`)
	writeAuditLine(t, f, `{not json`)
	f.Close()

	rec, ok := lastProposalDistill(path)
	if !ok || rec.Mode != "ok" || rec.Provider != "rules" {
		t.Fatalf("应取最近一条 distill=ok/rules，got %+v ok=%v", rec, ok)
	}

	for _, p := range []string{"", "-", filepath.Join(dir, "missing.jsonl")} {
		if _, ok := lastProposalDistill(p); ok {
			t.Fatalf("%q 不应给出信号", p)
		}
	}
	if modelUnavailable(path) {
		t.Fatal("最近一条已成功，不应报模型不可用")
	}

	// 再补一条 backlog（无 provider）→ 信号点亮。
	f2, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	writeAuditLine(t, f2, `{"ts":11,"event":"distill","source":"proposal","mode":"backlog"}`)
	f2.Close()
	if !modelUnavailable(path) {
		t.Fatal("最近一条是 backlog 且无 provider，应报模型不可用")
	}
}
