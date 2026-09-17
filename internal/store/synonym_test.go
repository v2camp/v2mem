package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 解析 ----------

func TestParseSynonymFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "synonyms.txt")
	content := "" +
		"# 注释行\n" +
		"ingest = 搬进, 导入, 灌库\n" +
		"删除 = remove, 移除\n" +
		"!停用词 = 不该进来\n" +
		"[v2mem]\n" +
		"harness = 原生记忆, 宿主\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	d, err := ParseSynonymFile(p)
	if err != nil {
		t.Fatalf("ParseSynonymFile: %v", err)
	}
	// 3 个别名 + 2 个别名 + 1 项停用不计 + 2 个别名
	if n := d.Count(); n != 3+2+2 {
		t.Errorf("活跃别名数 = %d，want 7", n)
	}
	if d.global["搬进"] != "ingest" || d.byProject["v2mem"]["宿主"] != "harness" {
		t.Errorf("解析映射不符: %+v", d)
	}
	if _, ok := d.global["停用词"]; ok {
		t.Error("停用行不应进词典")
	}
}

// ---------- 展开（作用域） ----------

func newTestDict(t *testing.T) *SynonymDict {
	t.Helper()
	p := filepath.Join(t.TempDir(), "synonyms.txt")
	os.WriteFile(p, []byte("ingest = 搬进, 导入\n[v2mem]\nharness = 宿主\n"), 0o644)
	d, err := ParseSynonymFile(p)
	if err != nil {
		t.Fatalf("ParseSynonymFile: %v", err)
	}
	return d
}

func TestSynonymExpandScoping(t *testing.T) {
	d := newTestDict(t)

	// 全局别名命中：追加标准词，保留原词
	ex := d.Expand("怎么搬进记忆库", "any")
	if !strings.Contains(ex, "ingest") || !strings.Contains(ex, "搬进") {
		t.Errorf("全局别名应追加标准词并保留原词，got %q", ex)
	}

	// 工程别名只在对应工程命中
	if d.Expand("宿主怎么注入", "v2mem") == d.Expand("宿主怎么注入", "other") {
		t.Error("工程段别名应对 project 收敛（不同 project 展开应不同）")
	}
	if got := d.Expand("宿主怎么注入", "other"); got != "宿主怎么注入" {
		t.Errorf("无关工程不应展开工程别名，got %q", got)
	}

	// 无命中原样返回
	if got := d.Expand("完全无关的词", "any"); got != "完全无关的词" {
		t.Errorf("无命中应原样，got %q", got)
	}
}

func TestSynonymExpandNilDict(t *testing.T) {
	if got := (*SynonymDict)(nil).Expand("搬进", "x"); got != "搬进" {
		t.Errorf("nil 词典应原样返回，got %q", got)
	}
}

func TestSynonymDisabledByEnv(t *testing.T) {
	t.Setenv("MEM_SYN_DICT", "0")
	d, err := LoadSynonym(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("LoadSynonym: %v", err)
	}
	if d != nil {
		t.Error("MEM_SYN_DICT=0 应禁用词库")
	}
}

// ---------- 检索集成：词库让「别名」查到「标准词」记忆 ----------

func TestSearchSynonymExpansion(t *testing.T) {
	s := newTestStore(t)
	mustAdd(t, s, "ingest 机械化搬运 未打 source 标记")

	// 无词库时「搬进」查不到
	h, err := s.Search(SearchQuery{Query: "怎么搬进记忆库"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(h) != 0 {
		t.Fatalf("无词库时 cangentine 查不到（want 0 got %d）", len(h))
	}

	// 载入词库后能查到
	s.syn = newTestDict(t)
	h, err = s.Search(SearchQuery{Query: "怎么搬进记忆库"})
	if err != nil {
		t.Fatalf("search with dict: %v", err)
	}
	found := false
	for _, hit := range h {
		if strings.Contains(hit.Content, "ingest") {
			found = true
		}
	}
	if !found {
		t.Errorf("有词库时「搬进」应能命中 ingest 记忆，got %d 条: %+v", len(h), h)
	}
}