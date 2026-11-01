package store

import (
	"bytes"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/similarity"
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

// 精确命中 B 时，措辞不同的 A 也应进入结果，且 B 仍排最前。
// 此前「精确有结果就直接返回」，A 永远不会出现 —— 这是 M5b-0 的核心行为；
// P0 之后 A 由 fuzzy 与 broad 双路带回，两条路都必须有贡献。
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

	// 关掉模糊路后，A 仍会由 broad 常开带回 —— 但不得再有任何 fuzzy 贡献。
	hits2, err := s.Search(SearchQuery{Query: "日志怎么搬进记忆库里", Scope: "all", Limit: 10, NoFuzzy: true})
	if err != nil {
		t.Fatalf("Search(NoFuzzy): %v", err)
	}
	ids2 := hitIDs(hits2)
	if len(ids2) == 0 || ids2[0] != idB {
		t.Errorf("NoFuzzy 时精确命中的 B 仍应排最前，got %v", ids2)
	}
	for _, h := range hits2 {
		if h.Signals != nil && h.Signals.Fuzzy != 0 {
			t.Errorf("NoFuzzy 时不该有模糊路贡献，%s got %+v", h.ID, h.Signals)
		}
	}
}

// 查询太短（shingle 数不足）时不走模糊检索：任何一条都不该有 fuzzy 贡献。
// broad 仍然常开，所以措辞相近的 A 依旧可能出现，只是必须排在精确命中的 B 之后。
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
	if len(ids) == 0 || ids[0] != idB {
		t.Errorf("精确命中的 B 应排最前，got %v", ids)
	}
	for _, h := range hits {
		if h.Signals != nil && h.Signals.Fuzzy != 0 {
			t.Errorf("短查询不该走模糊路，%s got %+v", h.ID, h.Signals)
		}
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

// 三路常开的回归：精确空手时 broad 与 fuzzy 各自独立召回，不靠「谁来触发」。
// 其中 trigram 也接近的记忆同时是模糊候选，融合后仍应排前。
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

// ---------- 融合：加权 RRF + 自适应分母（纯函数） ----------

// hitsOf 构造只带 ID 的候选，供融合的纯函数测试使用（融合只看名次，与内容无关）。
func hitsOf(ids ...string) []Hit {
	out := make([]Hit, len(ids))
	for i, id := range ids {
		out[i] = Hit{ID: id}
	}
	return out
}

func TestRRFFuseDeterministicOrdering(t *testing.T) {
	// 用两条非精确路，避免「精确前置」这一排序主键掩盖分数比较
	got := fuseRecall([]recallStream{
		{name: streamBroad, weight: 1, hits: hitsOf("a", "b", "c")},
		{name: streamFuzzy, weight: 1, hits: hitsOf("b", "d")},
	})
	ids := hitIDs(got)
	// b 两路都出现 → 分最高；a/d/c 按各自名次与先见顺序
	want := []string{"b", "a", "d", "c"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("fuseRecall 排序不符：got %v want %v", ids, want)
	}
	for i, h := range got {
		if h.Rank != float64(i+1) {
			t.Errorf("融合后 Rank 应是 1 起的名次，第 %d 条 got %v", i, h.Rank)
		}
		if h.Score <= 0 {
			t.Errorf("融合后每条都应带正的总分，%s got %v", h.ID, h.Score)
		}
		if h.Signals == nil {
			t.Errorf("%s 缺少明细，无法解释排序依据", h.ID)
		}
	}
}

func TestRRFFuseTieBreaksByFirstSeenOrder(t *testing.T) {
	got := fuseRecall([]recallStream{
		{name: streamPrecise, weight: 1, hits: hitsOf("a")},
		{name: streamBroad, weight: 1, hits: hitsOf("b")},
	})
	ids := hitIDs(got)
	// a 与 b 同分（各自单路第 1），平局按列表先见顺序
	if strings.Join(ids, ",") != "a,b" {
		t.Errorf("同分应稳定地按先见顺序，got %v", ids)
	}
}

func TestRRFFuseEmptyAndSingleList(t *testing.T) {
	if got := fuseRecall(nil); len(got) != 0 {
		t.Errorf("空输入应返回空，got %v", got)
	}
	// 有流但全是空候选：分母为 0，不能出一条同样空的结果
	if got := fuseRecall([]recallStream{{name: streamBroad, weight: 1}}); len(got) != 0 {
		t.Errorf("全场空手应返回空，got %v", got)
	}
	got := fuseRecall([]recallStream{{name: streamPrecise, weight: weightPrecise, hits: hitsOf("a", "b")}})
	if ids := hitIDs(got); strings.Join(ids, ",") != "a,b" {
		t.Errorf("单路应保持原序，got %v", ids)
	}
}

// 自适应分母：只有真正产出结果的流才进分母。
//
// 若固定按三路权重之和（1.0+0.8+0.6）归一化，单信号查询的每条分数都会被压到
// 1/2.4，与「三路都有结果」时的量纲不一致，跨查询复用的阈值就失准了。
func TestRRFFuseAdaptiveDenominatorCountsOnlyActiveStreams(t *testing.T) {
	streams := []recallStream{
		{name: streamPrecise, weight: weightPrecise, hits: hitsOf("a")},
		{name: streamBroad, weight: weightBroad}, // 空手
		{name: streamFuzzy, weight: weightFuzzy}, // 空手
	}
	if got := fuseDenominator(streams); got != weightPrecise {
		t.Errorf("分母只应累计在场流的权重，got %v want %v", got, weightPrecise)
	}

	hits := fuseRecall(streams)
	if len(hits) != 1 {
		t.Fatalf("应只有 1 条结果，got %d", len(hits))
	}
	// 单信号时分数等于未融合的单路分数（1/(k+1)），没有任何稀释
	undiluted := 1 / float64(rrfK+1)
	if math.Abs(hits[0].Score-undiluted) > 1e-12 {
		t.Errorf("单信号分数不应被稀释：got %v want %v", hits[0].Score, undiluted)
	}
	if diluted := 1 / (weightPrecise + weightBroad + weightFuzzy) / float64(rrfK+1); hits[0].Score <= diluted {
		t.Errorf("分数被三路权重稀释了：got %v，固定分母会得到 %v", hits[0].Score, diluted)
	}
	// 与「根本不传空流」的结果必须逐位相同 —— 空流不该改变量纲
	bare := fuseRecall([]recallStream{{name: streamPrecise, weight: weightPrecise, hits: hitsOf("a")}})
	if hits[0].Score != bare[0].Score {
		t.Errorf("空流不该影响分数：got %v want %v", hits[0].Score, bare[0].Score)
	}
}

// 三路都在场时分母等于三者之和，且各自贡献之和等于总分。
func TestRRFFuseSignalsSumToScore(t *testing.T) {
	streams := []recallStream{
		{name: streamPrecise, weight: weightPrecise, hits: hitsOf("a")},
		{name: streamBroad, weight: weightBroad, hits: hitsOf("a")},
		{name: streamFuzzy, weight: weightFuzzy, hits: hitsOf("a")},
	}
	if got, want := fuseDenominator(streams), weightPrecise+weightBroad+weightFuzzy; math.Abs(got-want) > 1e-12 {
		t.Errorf("三路都在场时分母应为权重和，got %v want %v", got, want)
	}
	hits := fuseRecall(streams)
	if len(hits) != 1 {
		t.Fatalf("同一 ID 的三路贡献应合并成 1 条，got %d", len(hits))
	}
	sig := hits[0].Signals
	if sig == nil {
		t.Fatal("三路都命中的条目必须带明细")
	}
	if sig.Precise <= 0 || sig.Broad <= 0 || sig.Fuzzy <= 0 {
		t.Errorf("三路各应有正贡献，got %+v", *sig)
	}
	if sum := sig.Precise + sig.Broad + sig.Fuzzy; math.Abs(sum-hits[0].Score) > 1e-12 {
		t.Errorf("各路贡献之和应等于总分：sum=%v score=%v", sum, hits[0].Score)
	}
}

// 精确命中不越位：只被 precise 命中的条目，必须排在 broad+fuzzy 双路命中的条目前面，
// 哪怕后者分数更高。这是精度契约的硬边界 —— 加权 RRF 的分数不足以表达它，
// 所以 fuseRecall 把「是否精确命中」作为第一排序键。
func TestRRFFusePreciseHitsStayAboveWiderHits(t *testing.T) {
	got := fuseRecall([]recallStream{
		{name: streamPrecise, weight: weightPrecise, hits: hitsOf("exact")},
		{name: streamBroad, weight: weightBroad, hits: hitsOf("wide")},
		{name: streamFuzzy, weight: weightFuzzy, hits: hitsOf("wide")},
	})
	if len(got) != 2 {
		t.Fatalf("应召回 2 条，got %d", len(got))
	}
	if got[0].ID != "exact" {
		t.Errorf("精确命中必须排最前，got %v", hitIDs(got))
	}
	if got[1].Score <= got[0].Score {
		t.Fatalf("本用例的前提是「宽路双命中分数更高」：got %v vs %v", got[1].Score, got[0].Score)
	}
}
