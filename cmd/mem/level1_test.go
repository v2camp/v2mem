package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanghui/v2mem/internal/store"
)

// ---------- M6: Level 1 文件运维（ingest / budget） ----------
//
// 背景：会话级文件（AGENTS.md / MEMORY.md）每轮注入且有容量上限，
// 灌满即被截断，于是要反复人工「压缩」。根治办法是把细节搬到 Level 2，
// 而「搬」必须能机械化 —— 否则迁移本身就是又一次人工压缩。

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

const ingestSample = `# 2026-09-17 工作日志

- 记忆库数据固定放 ~/.v2mem，代码与数据分离
- 活库不能放进 iCloud 同步目录，会损坏 SQLite

## 排查记录

短

这一行是一段自由文本，也应当被当作一条候选事实收进来

- 见 [某文档](https://example.com/x)
- 又来一条有效记录，内容够长
- 一条被硬换行的事实，前半句在这里，
  后半句缩进续行，必须与前半句合并成同一条

` + "```\n- 代码围栏里的这一行不能被当成事实\n```" + `

#### 另一个小节

- 这一条重复：记忆库数据固定放 ~/.v2mem，代码与数据分离
`

func TestCmdIngestExtractsListItemsAndParagraphs(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "log.md", ingestSample)

	out, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo", "--json", "--no-audit", src})
	})
	if err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}
	if !strings.Contains(out, `"inserted"`) {
		t.Fatalf("应报告 JSON 统计，got: %s", out)
	}

	// 必须枚举**全库**，不能只看检索结果 ——
	// 标题行本来就不会被任何查询命中，用检索结果做负向断言等于没检查（实测漏过）。
	all := allMemories(t, db)
	if len(all) == 0 {
		t.Fatal("应至少收进一条")
	}
	for _, content := range all {
		switch {
		case strings.HasPrefix(content, "#"):
			t.Errorf("标题不应入库: %q", content)
		case strings.Contains(content, "代码围栏里的这一行"):
			t.Errorf("代码围栏内容不应入库: %q", content)
		case strings.Contains(content, "example.com"):
			t.Errorf("纯链接行不应入库: %q", content)
		case strings.TrimSpace(content) == "短":
			t.Errorf("过短的行不应入库: %q", content)
		case strings.HasPrefix(content, "后半句缩进续行"):
			t.Errorf("缩进续行不应单独成条（须与前半句合并）: %q", content)
		}
	}

	// 自由段落应被收进
	if !containsSubstring(all, "自由文本") {
		t.Error("非列表的自由段落也应作为候选收进")
	}
	// 缩进续行必须与上一条合并
	if !containsSubstring(all, "前半句在这里") || !containsSubstring(all, "后半句缩进续行") {
		t.Errorf("缩进续行未合并进上一条，全库：%q", all)
	}
	for _, content := range all {
		if strings.Contains(content, "前半句在这里") && !strings.Contains(content, "后半句缩进续行") {
			t.Errorf("合并结果应同时含前后半句，got: %q", content)
		}
	}
}

// allMemories 枚举库内全部可见记忆的正文。
func allMemories(t *testing.T, db string) []string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	hits, err := st.List(store.ListQuery{Scope: "all", Limit: 1000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Content)
	}
	return out
}

func containsSubstring(xs []string, sub string) bool {
	for _, x := range xs {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

// 幂等：同一份文件 ingest 两次，第二次应全部命中去重而不是新增。
func TestCmdIngestIsIdempotent(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "log.md", ingestSample)

	first, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo", "--json", "--no-audit", src})
	})
	if err != nil {
		t.Fatalf("cmdIngest #1: %v", err)
	}
	before := countMemories(t, db)

	second, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo", "--json", "--no-audit", src})
	})
	if err != nil {
		t.Fatalf("cmdIngest #2: %v", err)
	}
	if after := countMemories(t, db); after != before {
		t.Errorf("重复 ingest 不应增加条数：%d → %d\nfirst=%s\nsecond=%s", before, after, first, second)
	}
	if !strings.Contains(second, `"inserted":0`) {
		t.Errorf("第二次应报告 inserted=0，got: %s", second)
	}
}

func TestCmdIngestDryRunWritesNothing(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "log.md", ingestSample)

	out, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "demo", "--dry-run", src})
	})
	if err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}
	if !strings.Contains(out, "记忆库数据固定放") {
		t.Errorf("dry-run 应列出候选项，got:\n%s", out)
	}
	if n := countMemories(t, db); n != 0 {
		t.Errorf("dry-run 不应写库，got %d 条", n)
	}
}

func TestCmdIngestHonoursProjectKindAndTags(t *testing.T) {
	db := testDB(t)
	src := writeFile(t, t.TempDir(), "log.md",
		"- 这是一条用于验证工程与类型标记的记录内容\n")

	if _, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--db", db, "--project", "proj-x",
			"--kind", "pitfall", "--tag", "src=log", "--no-audit", src})
	}); err != nil {
		t.Fatalf("cmdIngest: %v", err)
	}
	hits := searchOnce(t, db, store.SearchQuery{Query: "验证工程 类型标记", Limit: 5})
	if len(hits) != 1 {
		t.Fatalf("应入库 1 条，got %d", len(hits))
	}
	if hits[0].Project != "proj-x" || hits[0].Kind != "pitfall" {
		t.Errorf("工程/类型未生效: project=%q kind=%q", hits[0].Project, hits[0].Kind)
	}
	byTag := searchOnce(t, db, store.SearchQuery{Query: "验证工程 类型标记", Tags: map[string]string{"src": "log"}, Limit: 5})
	if len(byTag) != 1 {
		t.Errorf("标记未生效，按标记检索 got %d 条", len(byTag))
	}
}

func TestCmdIngestMissingFileErrors(t *testing.T) {
	db := testDB(t)
	if err := cmdIngest([]string{"--db", db, filepath.Join(t.TempDir(), "nope.md")}); err == nil {
		t.Error("文件不存在应报错")
	}
	if err := cmdIngest([]string{"--db", db}); err == nil {
		t.Error("缺少文件参数应报错")
	}
}

// ---------- budget：Level 1 体量守卫 ----------

func TestCmdBudgetPassesWhenUnderLimit(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "MEMORY.md", "# 标题\n\n- 一条规则\n")
	out, err := captureStdout(t, func() error {
		return cmdBudget([]string{"--file", f, "--max-chars", "1000"})
	})
	if err != nil {
		t.Fatalf("未超预算不应报错: %v", err)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("应给出 OK 结论，got:\n%s", out)
	}
}

// 超预算必须报错退出 —— WorkBuddy 的自动化任务靠退出码判定是否告警。
func TestCmdBudgetFailsWhenOverLimit(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "MEMORY.md", strings.Repeat("内容", 500))
	_, err := captureStdout(t, func() error {
		return cmdBudget([]string{"--file", f, "--max-chars", "100"})
	})
	if err == nil {
		t.Fatal("超预算应报错（供自动化判定）")
	}
	if !strings.Contains(err.Error(), "超") {
		t.Errorf("错误信息应说明超出，got: %v", err)
	}
}

// 对照：报告「若把库里的记忆留在文件里会占多少」——这是迁移收益的量化。
func TestCmdBudgetReportsLibraryCounterfactual(t *testing.T) {
	db := testDB(t)
	for _, c := range []string{
		"记忆库数据固定放 ~/.v2mem，代码与数据分离",
		"活库不能放进 iCloud 同步目录，会损坏 SQLite",
	} {
		if err := cmdAdd([]string{"--db", db, "--project", "demo", c}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}
	dir := t.TempDir()
	f := writeFile(t, dir, "MEMORY.md", "# 空的 Level 1\n")

	out, err := captureStdout(t, func() error {
		return cmdBudget([]string{"--db", db, "--file", f, "--max-chars", "7800", "--project", "demo"})
	})
	if err != nil {
		t.Fatalf("cmdBudget: %v", err)
	}
	if !strings.Contains(out, "2") {
		t.Errorf("应报告库中该工程的记忆条数（2），got:\n%s", out)
	}
}

func TestCmdBudgetMissingFileErrors(t *testing.T) {
	if err := cmdBudget([]string{"--file", filepath.Join(t.TempDir(), "nope.md")}); err == nil {
		t.Error("文件不存在应报错")
	}
	if err := cmdBudget([]string{}); err == nil {
		t.Error("缺少 --file 应报错")
	}
}

// countMemories 数库里的可见条数。
func countMemories(t *testing.T, db string) int {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	hits, err := st.List(store.ListQuery{Scope: "all", Limit: 1000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return len(hits)
}
