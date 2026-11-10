package store

import (
	"database/sql"
	"fmt"
	"testing"
)

// addAsset 写入一条带 provenance 的记忆，返回 id。
func addAsset(t *testing.T, s *Store, content, project, file string, line int) string {
	t.Helper()
	res, err := s.Add(AddInput{
		Content: content, Project: project, Device: "test",
		Provenance: &Provenance{File: file, Line: line, Source: "asset", Preview: "预览:" + file},
	})
	if err != nil {
		t.Fatalf("Add(%q): %v", content, err)
	}
	return res.ID
}

// 命中、不命中、同一文件内按行号升序——这就是「改文件前按行走读」的形态。
func TestAssetsByFileHitsByFileAndOrdersByLine(t *testing.T) {
	s := newTestStore(t)
	l30 := addAsset(t, s, "hook 只在 session-start 注入", "proj", "cmd/mem/hook.go", 30)
	l10 := addAsset(t, s, "hook 注入预算默认 1200 字符", "proj", "cmd/mem/hook.go", 10)
	addAsset(t, s, "main.go 的清单只有一处", "proj", "cmd/mem/main.go", 1)

	hits, err := s.AssetsByFile([]string{"cmd/mem/hook.go"}, "proj", 0)
	if err != nil {
		t.Fatalf("AssetsByFile: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("应命中 2 条，got %d: %+v", len(hits), hits)
	}
	if hits[0].ID != l10 || hits[0].Line != 10 {
		t.Errorf("第 1 条应是 L10（%s），got %+v", l10, hits[0])
	}
	if hits[1].ID != l30 || hits[1].Line != 30 {
		t.Errorf("第 2 条应是 L30（%s），got %+v", l30, hits[1])
	}
	if hits[0].Kind != "fact" || hits[0].Project != "proj" {
		t.Errorf("展示字段应透传 kind/project，got %+v", hits[0])
	}
	if hits[0].Preview != "预览:cmd/mem/hook.go" {
		t.Errorf("preview 应取自 provenance，got %q", hits[0].Preview)
	}
	if hits[0].CreatedAt <= 0 || hits[0].Salience <= 0 {
		t.Errorf("salience/created_at 应被填充，got %+v", hits[0])
	}

	// 不命中：不存在的文件名返回空且不报错。
	miss, err := s.AssetsByFile([]string{"cmd/mem/nope.go"}, "proj", 0)
	if err != nil {
		t.Fatalf("AssetsByFile(miss): %v", err)
	}
	if len(miss) != 0 {
		t.Errorf("不应命中，got %+v", miss)
	}
}

// 一条截断的 provenance 不能让整次查询失败，也不能连累同一文件其它行的返回。
// SQLite 的 json_extract 对非法 JSON 是**抛错**而不是返回 NULL，所以读
// provenance 的每个表达式都必须先过 json_valid。
func TestAssetsByFileSurvivesTruncatedProvenanceJSON(t *testing.T) {
	s := newTestStore(t)
	good := addAsset(t, s, "正常的一条", "proj", "cmd/mem/hook.go", 12)
	// 两种截断：整体只有 `{"file":`，以及缺右括号。
	for i, raw := range []string{`{"file":`, `{"file": "cmd/mem/hook.go"`} {
		id := mustAdd(t, s, fmt.Sprintf("脏数据第 %d 条", i+1))
		setColumn(t, s, id, "provenance", raw)
	}

	hits, err := s.AssetsByFile([]string{"cmd/mem/hook.go"}, "proj", 0)
	if err != nil {
		t.Fatalf("脏 provenance 不应让查询失败: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != good {
		t.Fatalf("只应返回正常的一条（%s），got %+v", good, hits)
	}
	// preview 也走同一守卫：正常行的 preview 仍要读出来。
	if hits[0].Preview != "预览:cmd/mem/hook.go" {
		t.Errorf("正常行的 preview 应可读，got %q", hits[0].Preview)
	}
}

// 三个读 provenance 的表达式本身必须能容忍脏行：直接 SELECT（不经 WHERE 过滤）
// 非法 JSON 也必须返回 NULL/空串，而不是抛 malformed JSON。
//
// 这条用例是守卫的**唯一有效证明**：走 AssetsByFile 时脏行已先被 assetFileExpr
// 过滤掉，SELECT 里的表达式根本不会在它上面求值，守卫与否都看不出差别。
func TestAssetProvenanceExprsTolerateMalformedJSON(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "脏数据：半截 JSON")
	setColumn(t, s, id, "provenance", `{"file":`)

	var file, preview sql.NullString
	var line sql.NullInt64
	err := s.db.QueryRow(
		`SELECT `+assetFileExpr+`, `+assetLineExpr+`, `+assetPreviewExpr+` FROM memories m`,
	).Scan(&file, &line, &preview)
	if err != nil {
		t.Fatalf("非法 JSON 下三个表达式都不该抛错: %v", err)
	}
	if file.Valid || line.Valid {
		t.Errorf("file/line 应为 NULL，got %+v %+v", file, line)
	}
	if !preview.Valid || preview.String != "" {
		t.Errorf("preview 应被 COALESCE 成空串，got %+v", preview)
	}
}

// provenance 为 NULL / 空串 / 非法 JSON 都只是「不命中」，不能让整次查询失败。
func TestAssetsByFileToleratesNullOrInvalidProvenance(t *testing.T) {
	s := newTestStore(t)
	noProv := mustAdd(t, s, "这条没有溯源")
	broken := addAsset(t, s, "这条的溯源 JSON 是坏的", "proj", "cmd/mem/hook.go", 1)
	setColumn(t, s, broken, "provenance", `{"file": "cmd/mem/hook.go"`) // 截断，非法 JSON
	empty := addAsset(t, s, "这条的溯源是空串", "proj", "cmd/mem/hook.go", 2)
	setColumn(t, s, empty, "provenance", "")

	hits, err := s.AssetsByFile([]string{"cmd/mem/hook.go", ""}, "proj", 0)
	if err != nil {
		t.Fatalf("非法 provenance 不应让查询失败: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("NULL/空串/非法 JSON 都不该命中，got %+v", hits)
	}
	// 前置条件：三条记忆确实都在库里，上面的「不命中」不是因为没数据。
	if n := countAll(t, s); n != 3 {
		t.Fatalf("前置条件不成立，库里有 %d 条", n)
	}
	_ = noProv
}

// 没有行号的溯源（line 省略）仍要命中，且 Line 报 0（NULL 不该变成扫描错误）。
func TestAssetsByFileWithoutLineStillMatches(t *testing.T) {
	s := newTestStore(t)
	id := addAsset(t, s, "整份文件级结论，没有锚点行", "proj", "AGENTS.md", 0)

	hits, err := s.AssetsByFile([]string{"AGENTS.md"}, "proj", 0)
	if err != nil {
		t.Fatalf("AssetsByFile: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != id {
		t.Fatalf("无行号也应命中，got %+v", hits)
	}
	if hits[0].Line != 0 {
		t.Errorf("无行号时 Line 应为 0，got %d", hits[0].Line)
	}
}

// project 为空表示不限工程；非空按 project 列过滤。
func TestAssetsByFileFiltersByProject(t *testing.T) {
	s := newTestStore(t)
	a := addAsset(t, s, "alpha 的 AGENTS.md 第 3 行", "alpha", "AGENTS.md", 3)
	b := addAsset(t, s, "beta 的 AGENTS.md 第 3 行", "beta", "AGENTS.md", 3)

	one, err := s.AssetsByFile([]string{"AGENTS.md"}, "alpha", 0)
	if err != nil {
		t.Fatalf("AssetsByFile: %v", err)
	}
	if len(one) != 1 || one[0].ID != a {
		t.Fatalf("按 alpha 过滤应只剩 %s，got %+v", a, one)
	}

	all, err := s.AssetsByFile([]string{"AGENTS.md"}, "", 0)
	if err != nil {
		t.Fatalf("AssetsByFile(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("不限工程应命中 2 条，got %+v", all)
	}
	// 排序第一键是 project，alpha 在 beta 之前。
	if all[0].ID != a || all[1].ID != b {
		t.Errorf("应按 project 升序，got %+v", all)
	}
}

// 多候选字面量：相对与绝对两种存法都要命中，重复候选不影响结果。
func TestAssetsByFileMatchesAnyCandidateLiteral(t *testing.T) {
	s := newTestStore(t)
	rel := addAsset(t, s, "相对路径存法", "proj", "cmd/mem/hook.go", 5)
	abs := addAsset(t, s, "绝对路径存法", "proj", "/abs/proj/cmd/mem/hook.go", 7)

	hits, err := s.AssetsByFile(
		[]string{"cmd/mem/hook.go", "/abs/proj/cmd/mem/hook.go", "cmd/mem/hook.go"}, "proj", 0)
	if err != nil {
		t.Fatalf("AssetsByFile: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("两种存法都应命中，got %+v", hits)
	}
	got := map[string]bool{hits[0].ID: true, hits[1].ID: true}
	if !got[rel] || !got[abs] {
		t.Errorf("应同时命中 %s 与 %s，got %+v", rel, abs, hits)
	}
}

// 空候选直接短路：既不拼 SQL 也不报错。
func TestAssetsByFileEmptyFilesReturnsNothing(t *testing.T) {
	s := newTestStore(t)
	addAsset(t, s, "有溯源的记忆", "proj", "cmd/mem/hook.go", 1)

	for _, files := range [][]string{nil, {}, {"", ""}} {
		hits, err := s.AssetsByFile(files, "proj", 0)
		if err != nil {
			t.Fatalf("空候选不应报错: %v", err)
		}
		if len(hits) != 0 {
			t.Errorf("空候选应返回空，got %+v", hits)
		}
	}
}

// limit：<=0 走默认，超过上限被截到 200。
func TestAssetsByFileLimitDefaultAndCap(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		addAsset(t, s, fmt.Sprintf("第 %d 条", i+1), "proj", "f.go", i+1)
	}

	got, err := s.AssetsByFile([]string{"f.go"}, "proj", 2)
	if err != nil {
		t.Fatalf("AssetsByFile: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("limit=2 应返回 2 条，got %d", len(got))
	}
	// 默认 50 大于本用例数据量，应全部返回。
	def, err := s.AssetsByFile([]string{"f.go"}, "proj", 0)
	if err != nil {
		t.Fatalf("AssetsByFile(default): %v", err)
	}
	if len(def) != 5 {
		t.Errorf("limit=0 应走默认并把 5 条都返回，got %d", len(def))
	}
	if neg, err := s.AssetsByFile([]string{"f.go"}, "proj", -3); err != nil || len(neg) != 5 {
		t.Errorf("limit<0 应等同默认，got %d %v", len(neg), err)
	}

	// 上限：写 205 条，limit 给一个超大值也必须停在 200。
	for i := 0; i < 200; i++ {
		addAsset(t, s, fmt.Sprintf("批量第 %d 条", i+1), "proj", "big.go", i+1)
	}
	capped, err := s.AssetsByFile([]string{"big.go"}, "proj", 10000)
	if err != nil {
		t.Fatalf("AssetsByFile(capped): %v", err)
	}
	if len(capped) != assetMaxLimit {
		t.Errorf("应被截到 %d 条，got %d", assetMaxLimit, len(capped))
	}
}

// 同一行号的多条记忆按 created_at 降序（新写的在前）。
func TestAssetsByFileSameLineOrdersByCreatedAtDesc(t *testing.T) {
	s := newTestStore(t)
	oldID := addAsset(t, s, "旧结论", "proj", "f.go", 10)
	newID := addAsset(t, s, "新结论", "proj", "f.go", 10)
	setColumn(t, s, oldID, "created_at", int64(100))
	setColumn(t, s, newID, "created_at", int64(200))

	hits, err := s.AssetsByFile([]string{"f.go"}, "proj", 0)
	if err != nil {
		t.Fatalf("AssetsByFile: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("应命中 2 条，got %+v", hits)
	}
	if hits[0].ID != newID || hits[1].ID != oldID {
		t.Errorf("同按行号时应新的在前，got %+v", hits)
	}
}

// 可见性与 mem search 同一口径：被归并取代的、已过期的都不出现。
func TestAssetsByFileHonoursVisibility(t *testing.T) {
	s := newTestStore(t)
	okID := addAsset(t, s, "仍然有效", "proj", "f.go", 1)
	super := addAsset(t, s, "已被取代", "proj", "f.go", 2)
	setColumn(t, s, super, "superseded_by", "other")
	expired := addAsset(t, s, "已过期", "proj", "f.go", 3)
	setColumn(t, s, expired, "expires_at", int64(1))

	hits, err := s.AssetsByFile([]string{"f.go"}, "proj", 0)
	if err != nil {
		t.Fatalf("AssetsByFile: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != okID {
		t.Errorf("只应返回未取代未过期的 %s，got %+v", okID, hits)
	}
}
