package store

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanghui/v2mem/internal/similarity"
)

// ---------- M5b-0 签名存储 ----------

func sigRow(t *testing.T, s *Store, id string) []byte {
	t.Helper()
	var raw []byte
	if err := s.db.QueryRow(`SELECT sig FROM mem_sigs WHERE memory_id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("读取签名 %s: %v", id, err)
	}
	return raw
}

func TestAddWritesSignature(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "日志怎么搬进记忆库里")
	got := sigRow(t, s, id)
	want := encodeSig(similarity.Sign("日志怎么搬进记忆库里"))
	if !bytes.Equal(got, want) {
		t.Errorf("Add 应写入内容的 MinHash 签名（%d bytes），got %d bytes", len(want), len(got))
	}
}

func TestOverwriteUpdatesSignature(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "日志怎么搬进记忆库里")
	if _, err := s.Add(AddInput{Content: "日志怎么搬进记忆库里。", Device: "test"}); err != nil {
		t.Fatalf("Add(overwrite): %v", err)
	}
	got := sigRow(t, s, id)
	want := encodeSig(similarity.Sign("日志怎么搬进记忆库里。"))
	if !bytes.Equal(got, want) {
		t.Errorf("覆盖后签名应随新内容更新")
	}
}

func TestForgetCascadesSignature(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "日志怎么搬进记忆库里")
	if _, err := s.Forget(id); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mem_sigs WHERE memory_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("Forget 应级联删除签名，剩余 %d", n)
	}
}

func TestOpenBackfillsMissingSignatures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// 模拟 import 直插路径：不经 Add，签名缺失
	if _, err := s1.db.Exec(
		`INSERT INTO memories (id, content, content_idx, kind, content_hash, project, salience,
		                       created_at, updated_at, last_seen_at, origin_device, origin_tool, access_count)
		 VALUES ('raw-1', '日志怎么搬进记忆库里',
		         '日志 志怎 怎么 么搬 搬进 进记 记忆 忆库 库里',
		         'fact', 'hash-1', '', 0.5, 1, 1, 1, 'dev', '', 0)`); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}
	defer s2.Close()
	got := sigRow(t, s2, "raw-1")
	want := encodeSig(similarity.Sign("日志怎么搬进记忆库里"))
	if !bytes.Equal(got, want) {
		t.Errorf("Open 应补齐缺失的签名")
	}
}

// ---------- M5b-0 模糊检索行为 ----------

func hitIDs(hits []Hit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	return ids
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// 精确命中 B 时，措辞不同的 A 也应经模糊检索进入结果，且 B 仍排最前。
// 此前「精确有结果就直接返回」，A 永远不会出现 —— 这是 M5b-0 的核心行为。
func TestSearchFuzzyBringsParaphrase(t *testing.T) {
	s := newTestStore(t)
	idA := mustAdd(t, s, "日志怎么放进记忆库里")
	idB := mustAdd(t, s, "日志怎么搬进记忆库里")

	hits, err := s.Search(SearchQuery{Query: "日志怎么搬进记忆库里", Scope: "all", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := hitIDs(hits)
	if len(ids) < 2 {
		t.Fatalf("应召回精确命中的 B 与措辞不同的 A，got %v", ids)
	}
	if ids[0] != idB {
		t.Errorf("精确命中的 B 应排最前，got %v", ids)
	}
	if !containsID(ids, idA) {
		t.Errorf("措辞不同的 A 应被模糊检索召回，got %v", ids)
	}

	hits2, err := s.Search(SearchQuery{Query: "日志怎么搬进记忆库里", Scope: "all", Limit: 10, NoFuzzy: true})
	if err != nil {
		t.Fatalf("Search(NoFuzzy): %v", err)
	}
	ids2 := hitIDs(hits2)
	if len(ids2) != 1 || ids2[0] != idB {
		t.Errorf("NoFuzzy 时应只有精确命中的 B，got %v", ids2)
	}
}

// 查询太短（shingle 数不足）时不走模糊检索，结果与 NoFuzzy 一致。
func TestSearchShortQuerySkipsFuzzy(t *testing.T) {
	s := newTestStore(t)
	mustAdd(t, s, "日志怎么放进记忆库里")
	idB := mustAdd(t, s, "日志怎么搬进记忆库里")

	if similarity.EnoughShingles("搬进记忆库") {
		t.Fatalf("前置条件：5 字查询不应触发模糊检索")
	}

	hits, err := s.Search(SearchQuery{Query: "搬进记忆库", Scope: "all", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := hitIDs(hits)
	if len(ids) != 1 || ids[0] != idB {
		t.Errorf("短查询应只返回精确命中，got %v", ids)
	}
}

// 模糊候选必须与精确检索共用同一套过滤：scope/kind 都要约束候选集。
func TestSearchFuzzyRespectsScopeAndKind(t *testing.T) {
	s := newTestStore(t)
	mustAdd(t, s, "日志怎么搬进记忆库里") // 目标（global）
	mustAdd(t, s, "日志怎么放进记忆库里") // 措辞不同的近亲（global）
	if _, err := s.Add(AddInput{Content: "日志怎么放进记忆库里啦", Project: "other", Device: "test"}); err != nil {
		t.Fatalf("Add other-project: %v", err)
	}
	if _, err := s.Add(AddInput{Content: "日志怎么放进记忆库里呀", Kind: "decision", Device: "test"}); err != nil {
		t.Fatalf("Add decision: %v", err)
	}

	hits, err := s.Search(SearchQuery{Query: "日志怎么搬进记忆库里", Scope: "current", Kind: "fact", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var contents []string
	for _, h := range hits {
		contents = append(contents, h.Content)
	}
	if len(contents) != 2 ||
		!containsStr(contents, "日志怎么搬进记忆库里") ||
		!containsStr(contents, "日志怎么放进记忆库里") {
		t.Errorf("其他工程 / 其他 kind 的近亲不得泄漏进模糊候选，got %v", contents)
	}
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// 退化路径回归：精确 0 命中时 broad 仍能把记忆带回；其中 trigram 也接近的
// 记忆同时是模糊候选，融合后仍应排前。
func TestSearchFuzzyBroadFallbackStillWorks(t *testing.T) {
	s := newTestStore(t)
	idC := mustAdd(t, s, "日志搬进记忆库里")     // broad + fuzzy 都命中
	idC2 := mustAdd(t, s, "日志归档到别的库") // 只 broad 命中（trigram 无交集）

	hits, err := s.Search(SearchQuery{Query: "日志怎么搬进记忆库里", Scope: "all", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := hitIDs(hits)
	if len(ids) != 2 {
		t.Fatalf("两条记忆都应被带回，got %v", ids)
	}
	if ids[0] != idC {
		t.Errorf("broad 与 fuzzy 双路命中的 C 应排最前，got %v", ids)
	}
	if !containsID(ids, idC2) {
		t.Errorf("仅 broad 命中的 C2 也应被带回，got %v", ids)
	}
}

// ---------- RRF 融合（纯函数） ----------

func TestRRFFuseDeterministicOrdering(t *testing.T) {
	got := rrfFuse(
		[]Hit{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		[]Hit{{ID: "b"}, {ID: "d"}},
	)
	ids := hitIDs(got)
	// b 两路都出现 → 分最高；a/d/c 按各自名次与先见顺序
	want := []string{"b", "a", "d", "c"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("rrfFuse 排序不符：got %v want %v", ids, want)
	}
}

func TestRRFFuseTieBreaksByFirstSeenOrder(t *testing.T) {
	got := rrfFuse(
		[]Hit{{ID: "a"}},
		[]Hit{{ID: "b"}},
	)
	ids := hitIDs(got)
	// a 与 b 同分（各自单路第 1），平局按列表先见顺序
	if strings.Join(ids, ",") != "a,b" {
		t.Errorf("同分应稳定地按先见顺序，got %v", ids)
	}
}

func TestRRFFuseEmptyAndSingleList(t *testing.T) {
	if got := rrfFuse(); len(got) != 0 {
		t.Errorf("空输入应返回空，got %v", got)
	}
	got := rrfFuse([]Hit{{ID: "a"}, {ID: "b"}})
	if ids := hitIDs(got); strings.Join(ids, ",") != "a,b" {
		t.Errorf("单路应保持原序，got %v", ids)
	}
}
