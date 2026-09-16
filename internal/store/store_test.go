package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustAdd(t *testing.T, s *Store, content string) string {
	t.Helper()
	res, err := s.Add(AddInput{Content: content, Device: "test"})
	if err != nil {
		t.Fatalf("Add(%q): %v", content, err)
	}
	return res.ID
}

func row(t *testing.T, s *Store, id string) (lastSeen int64, accessCount int, salience float64) {
	t.Helper()
	err := s.db.QueryRow(
		`SELECT last_seen_at, access_count, salience FROM memories WHERE id = ?`, id,
	).Scan(&lastSeen, &accessCount, &salience)
	if err != nil {
		t.Fatalf("读取记忆 %s: %v", id, err)
	}
	return
}

func countAll(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// setColumn 直接改列值，用于构造「很久没命中」「已过期」等时间相关场景。
func setColumn(t *testing.T, s *Store, id, col string, val any) {
	t.Helper()
	if _, err := s.db.Exec("UPDATE memories SET "+col+" = ? WHERE id = ?", val, id); err != nil {
		t.Fatalf("设置 %s: %v", col, err)
	}
}

// ---------- M1 回归（特性已存在，这里固化行为防退化） ----------

func TestAddSameContentOverridesInsteadOfDuplicating(t *testing.T) {
	s := newTestStore(t)
	a := mustAdd(t, s, "记忆库数据必须放在 ~/.v2mem")
	b, err := s.Add(AddInput{Content: "记忆库数据必须放在 ~/.V2MEM。", Device: "test"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if b.Created {
		t.Errorf("归一化后相同的内容应被识别为覆盖，Created 应为 false")
	}
	if b.ID != a {
		t.Errorf("覆盖应复用同一条 ID，got %s want %s", b.ID, a)
	}
	if n := countAll(t, s); n != 1 {
		t.Errorf("库内应只有 1 条，got %d", n)
	}
}

func TestSearchMatchesChineseTwoCharacterWord(t *testing.T) {
	s := newTestStore(t)
	mustAdd(t, s, "配置目录在 ~/.config")
	hits, err := s.Search(SearchQuery{Query: "目录"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("二字词「目录」应命中 1 条，got %d", len(hits))
	}
}

// ---------- M2: touch ----------

func TestTouchRefreshesLastSeenAndIncrementsAccessCount(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "记忆库数据必须放在 ~/.v2mem")

	past := time.Now().Add(-72 * time.Hour).Unix()
	setColumn(t, s, id, "last_seen_at", past)
	setColumn(t, s, id, "access_count", 0)

	if _, err := s.Touch(id); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	lastSeen, accessCount, _ := row(t, s, id)
	if lastSeen <= past {
		t.Errorf("Touch 应刷新 last_seen_at：%d 应大于 %d", lastSeen, past)
	}
	if accessCount != 1 {
		t.Errorf("access_count 应为 1，got %d", accessCount)
	}
}

func TestTouchAcceptsIDPrefix(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "绝不能把活的 sqlite 文件放进 iCloud")

	full, err := s.Touch(id[:8])
	if err != nil {
		t.Fatalf("按前缀 Touch: %v", err)
	}
	if full != id {
		t.Errorf("应解析出完整 ID，got %s want %s", full, id)
	}
	if _, accessCount, _ := row(t, s, id); accessCount != 1 {
		t.Errorf("prefix touch 后 access_count 应为 1，got %d", accessCount)
	}
}

func TestTouchRejectsAmbiguousPrefix(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"aaaa1111", "aaaa2222"} {
		if _, err := s.db.Exec(
			`INSERT INTO memories (id, content, content_idx, kind, content_hash, project,
			   salience, created_at, updated_at, last_seen_at, origin_device)
			 VALUES (?,?,?,'fact',?,?,0.5,0,0,0,'test')`,
			id, "内容"+id, "内容", "h"+id, id,
		); err != nil {
			t.Fatalf("插入: %v", err)
		}
	}

	_, err := s.Touch("aaaa")
	if err == nil {
		t.Fatal("歧义前缀应返回错误")
	}
	if !strings.Contains(err.Error(), "歧义") {
		t.Errorf("错误信息应说明歧义，got: %v", err)
	}
}

// ---------- M2: forget ----------

func TestForgetRemovesMemoryAndItsTags(t *testing.T) {
	s := newTestStore(t)
	res, err := s.Add(AddInput{
		Content: "临时事实",
		Device:  "test",
		Tags:    map[string]string{"machine": "mini"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	var tagCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tags WHERE memory_id = ?`, res.ID).Scan(&tagCount); err != nil {
		t.Fatalf("查 tags: %v", err)
	}
	if tagCount != 1 {
		t.Fatalf("前置条件：应有 1 个 tag，got %d", tagCount)
	}

	if _, err := s.Forget(res.ID); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if n := countAll(t, s); n != 0 {
		t.Errorf("Forget 后应无剩余记忆，got %d", n)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tags WHERE memory_id = ?`, res.ID).Scan(&tagCount); err != nil {
		t.Fatalf("查 tags: %v", err)
	}
	if tagCount != 0 {
		t.Errorf("Forget 应级联删除 tags，got %d", tagCount)
	}
}

// ---------- M2: gc ----------

func TestGCDeletesExpiredMemories(t *testing.T) {
	s := newTestStore(t)
	keep := mustAdd(t, s, "不过期的记忆")
	gone := mustAdd(t, s, "已过期的记忆")
	setColumn(t, s, gone, "expires_at", time.Now().Add(-time.Hour).Unix())

	res, err := s.GC(720*time.Hour, 0.2)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Expired != 1 {
		t.Errorf("应回收 1 条过期记忆，got %d", res.Expired)
	}
	if n := countAll(t, s); n != 1 {
		t.Errorf("应保留 1 条，got %d", n)
	}
	var remaining string
	if err := s.db.QueryRow(`SELECT id FROM memories`).Scan(&remaining); err != nil {
		t.Fatalf("查剩余: %v", err)
	}
	if remaining != keep {
		t.Errorf("被删除的应是过期那一条")
	}
}

func TestGCEvictsStaleLowSalienceMemory(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "很久没用且不重要的记忆")
	setColumn(t, s, id, "last_seen_at", time.Now().Add(-2000*time.Hour).Unix())
	setColumn(t, s, id, "salience", 0.1)

	res, err := s.GC(720*time.Hour, 0.2)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Decayed != 1 {
		t.Errorf("应淘汰 1 条衰减记忆，got %d", res.Decayed)
	}
	if n := countAll(t, s); n != 0 {
		t.Errorf("应全部淘汰，got %d", n)
	}
}

func TestGCKeepsStaleButSalientMemory(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "很久没用但很重要的记忆")
	setColumn(t, s, id, "last_seen_at", time.Now().Add(-2000*time.Hour).Unix())
	setColumn(t, s, id, "salience", 0.9)

	res, err := s.GC(720*time.Hour, 0.2)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Decayed != 0 {
		t.Errorf("高 salience 不应被淘汰，Decayed=%d", res.Decayed)
	}
	if n := countAll(t, s); n != 1 {
		t.Errorf("应保留 1 条，got %d", n)
	}
}

func TestGCKeepsRecentlySeenLowSalienceMemory(t *testing.T) {
	s := newTestStore(t)
	id := mustAdd(t, s, "最近命中过的低重要性记忆")
	setColumn(t, s, id, "last_seen_at", time.Now().Add(-time.Hour).Unix())
	setColumn(t, s, id, "salience", 0.1)

	if _, err := s.GC(720*time.Hour, 0.2); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n := countAll(t, s); n != 1 {
		t.Errorf("刚命中过的不应被淘汰，剩余 %d", n)
	}
}

// ---------- M3: 标记与工程维度 ----------

func TestSearchFiltersBySingleTag(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Add(AddInput{
		Content: "来自 mini 的机器记忆", Device: "test", Project: "p",
		Tags: map[string]string{"machine": "mini"},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	hit, err := s.Search(SearchQuery{Query: "记忆", Tags: map[string]string{"machine": "mini"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hit) != 1 {
		t.Errorf("按 machine=mini 过滤应命中 1 条，got %d", len(hit))
	}

	miss, err := s.Search(SearchQuery{Query: "记忆", Tags: map[string]string{"machine": "other"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(miss) != 0 {
		t.Errorf("machine=other 不应命中，got %d", len(miss))
	}
}

func TestSearchFiltersByMultipleTagsWithANDSemantics(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Add(AddInput{
		Content: "王辉的偏好设置", Device: "test", Project: "p",
		Tags: map[string]string{"machine": "mini", "tool": "workbuddy"},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	both, err := s.Search(SearchQuery{
		Query: "偏好",
		Tags:  map[string]string{"machine": "mini", "tool": "workbuddy"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(both) != 1 {
		t.Errorf("两个 tag 都满足应命中 1 条，got %d", len(both))
	}

	partial, err := s.Search(SearchQuery{
		Query: "偏好",
		Tags:  map[string]string{"machine": "mini", "tool": "cursor"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(partial) != 0 {
		t.Errorf("tag 过滤应为 AND 语义：只满足一个不应命中，got %d", len(partial))
	}
}

func TestAddMergesTagsWhenOverridingSameContent(t *testing.T) {
	s := newTestStore(t)
	in := AddInput{Content: "跨工具复用的知识", Device: "test", Project: "p"}
	in.Tags = map[string]string{"tool": "workbuddy"}
	if _, err := s.Add(in); err != nil {
		t.Fatalf("Add: %v", err)
	}
	in.Tags = map[string]string{"tool": "claude-code"}
	if _, err := s.Add(in); err != nil {
		t.Fatalf("Add: %v", err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tags WHERE key = 'tool'`).Scan(&n); err != nil {
		t.Fatalf("查 tags: %v", err)
	}
	if n != 2 {
		t.Errorf("覆盖时应合并标记而非丢弃，应有 2 个 tool 标记，got %d", n)
	}
}

func TestSearchScopeCurrentIncludesProjectAndGlobal(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []struct {
		content string
		project string
	}{
		{"甲工程的记忆", "projA"},
		{"乙工程的记忆", "projB"},
		{"全局通用的记忆", ""},
	} {
		if _, err := s.Add(AddInput{Content: c.content, Device: "test", Project: c.project}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	hits, err := s.Search(SearchQuery{Query: "记忆", Project: "projA", Scope: "current"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Content] = true
	}
	if !got["甲工程的记忆"] {
		t.Error("scope=current 应包含当前工程的记忆")
	}
	if !got["全局通用的记忆"] {
		t.Error("scope=current 应包含全局记忆（project 为空）")
	}
	if got["乙工程的记忆"] {
		t.Error("scope=current 不应包含其它工程的记忆")
	}
}

func TestSearchScopeAllReturnsEveryProjectByDefault(t *testing.T) {
	s := newTestStore(t)
	for _, p := range []string{"projA", "projB", ""} {
		if _, err := s.Add(AddInput{Content: "记忆" + p, Device: "test", Project: p}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	hits, err := s.Search(SearchQuery{Query: "记忆"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Errorf("默认应跨工程检索全部 3 条，got %d", len(hits))
	}
}

// scope=global 是 scope=current 的补集：只要 project 为空的全局记忆。
// 存在意义：Level 1 的「跨工程硬规则」需能被单独列出，不被当前工程噪声淹没。
func TestSearchScopeGlobalReturnsOnlyProjectlessMemories(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []struct{ content, project string }{
		{"作用域全局验证甲的记录", "projA"},
		{"作用域全局验证乙的记录", "projB"},
		{"作用域全局验证全局的记录", ""},
	} {
		if _, err := s.Add(AddInput{Content: c.content, Device: "test", Project: c.project}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	hits, err := s.Search(SearchQuery{Query: "作用域全局验证", Scope: "global", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("scope=global 应只返回 1 条全局记忆，got %d", len(hits))
	}
	if hits[0].Content != "作用域全局验证全局的记录" || hits[0].Project != "" {
		t.Errorf("scope=global 返回了非全局记忆: %+v", hits[0])
	}
}

// ---------- M6: 无查询词的列举（会话起始摘要用） ----------

// List 与 Search 的分工：Search 要匹配词，List 只要「最重要 / 最近的」。
// 会话起始注入需要的是后者 —— 那时还没有用户提问，没有关键词可用。
func TestListReturnsMostSalientFirst(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: "低重要性条目甲", Project: "p1", Salience: 0.2})
	addWith(t, s, AddInput{Content: "高重要性条目乙", Project: "p1", Salience: 0.95})
	addWith(t, s, AddInput{Content: "中重要性条目丙", Project: "p1", Salience: 0.5})

	hits, err := s.List(ListQuery{Scope: "all", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("应返回 3 条，got %d", len(hits))
	}
	want := []string{"高重要性条目乙", "中重要性条目丙", "低重要性条目甲"}
	for i, c := range want {
		if hits[i].Content != c {
			t.Errorf("第 %d 条应为 %q，got %q", i, c, hits[i].Content)
		}
	}
}

func TestListHonoursLimit(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []string{"列举条目甲", "列举条目乙", "列举条目丙", "列举条目丁"} {
		addWith(t, s, AddInput{Content: c, Project: "p1", Salience: 0.5})
	}
	hits, err := s.List(ListQuery{Scope: "all", Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("应受 Limit 限制，got %d", len(hits))
	}
}

func TestListScopeCurrentAndGlobal(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: "甲工程条目内容", Project: "projA", Salience: 0.9})
	addWith(t, s, AddInput{Content: "乙工程条目内容", Project: "projB", Salience: 0.9})
	addWith(t, s, AddInput{Content: "全局条目内容", Project: "", Salience: 0.9})

	cur, err := s.List(ListQuery{Scope: "current", Project: "projA", Limit: 10})
	if err != nil {
		t.Fatalf("List current: %v", err)
	}
	if len(cur) != 2 {
		t.Fatalf("current 应含本工程与全局共 2 条，got %d", len(cur))
	}
	glb, err := s.List(ListQuery{Scope: "global", Limit: 10})
	if err != nil {
		t.Fatalf("List global: %v", err)
	}
	if len(glb) != 1 || glb[0].Content != "全局条目内容" {
		t.Errorf("global 应只含全局 1 条，got %+v", glb)
	}
	all, err := s.List(ListQuery{Scope: "all", Limit: 10})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all 应含 3 条，got %d", len(all))
	}
}

// 与 Search 共用同一套可见性规则：被取代的、已过期的都不出现。
func TestListSharesVisibilityRulesWithSearch(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: "可见条目内容甲", Project: "p1", Salience: 0.9})
	sup := addWith(t, s, AddInput{Content: "已被取代的条目内容", Project: "p1", Salience: 0.9})
	exp := addWith(t, s, AddInput{Content: "已过期的条目内容", Project: "p1", Salience: 0.9})
	setColumn(t, s, sup, "superseded_by", "some-other-id")
	setColumn(t, s, exp, "expires_at", 1)

	hits, err := s.List(ListQuery{Scope: "all", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(hits) != 1 || hits[0].Content != "可见条目内容甲" {
		t.Errorf("应只返回 1 条可见条目，got %+v", hits)
	}
}

func TestListOnEmptyStoreReturnsNothing(t *testing.T) {
	s := newTestStore(t)
	hits, err := s.List(ListQuery{Scope: "all", Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("空库应返回 0 条，got %d", len(hits))
	}
}

// 显式 all 必须跨工程：若写成布尔串判断，「all + 非空 Project」会落进
// Project 分支使 all 静默失效（实测被变异测试暴露过）。
func TestSearchScopeAllIgnoresProjectArgument(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: "甲工程条目内容", Project: "projA"})
	addWith(t, s, AddInput{Content: "乙工程条目内容", Project: "projB"})

	hits, err := s.Search(SearchQuery{Query: "工程条目", Scope: "all", Project: "projA", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("scope=all 应跨工程返回 2 条（忽略 Project），got %d", len(hits))
	}
}

// ---------- M7: 自然语言长问句的召回退化 ----------
//
// 实测缺陷（由 mem eval 的审计日志暴露）：用户用自然语言提问时命中 0 条，
// 因为长问句没有空格 → 被当成一个词组 → 词组内 AND 要求全部 bigram 都出现。

func TestSearchFindsLongNaturalLanguageQuestion(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: "日志不再人工蒸馏进 MEMORY.md：改用 mem ingest 搬进库", Project: "p1"})
	addWith(t, s, AddInput{Content: "记忆库数据固定放 ~/.v2mem 目录，代码与数据分离", Project: "p1"})

	const q = "日志怎么搬进记忆库"
	// 先固化「精确表达式确实空手」这一前提，否则下面的断言可能因别的原因通过
	exact, err := s.Search(SearchQuery{Query: q, Scope: "all", Limit: 5, NoBroadFallback: true})
	if err != nil {
		t.Fatalf("Search exact: %v", err)
	}
	if len(exact) != 0 {
		t.Fatalf("前置条件不成立：精确表达式本应 0 命中（AND 过严），got %d 条", len(exact))
	}

	hits, err := s.Search(SearchQuery{Query: q, Scope: "all", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("长问句应通过退化召回拿到结果，got 0（这正是修复前的问题）")
	}
	if !strings.Contains(hits[0].Content, "mem ingest") {
		t.Errorf("最相关的那条应排在前，got %q", hits[0].Content)
	}
}

// 退化只在空手时发生：精确有结果时不得放宽（否则精度会悄悄下降）。
//
// 构造要点：AND 只作用在**同一段连续汉字**内（词组内），词组之间是 OR。
// 所以要让「精确能中、放宽会多中」，必须用**单段无空格的长查询**，
// 让那些 bigram 的 AND 恰好只被一条记忆满足。
func TestSearchDoesNotBroadenWhenExactMatches(t *testing.T) {
	s := newTestStore(t)
	// A 含完整 bigram 链（记忆/忆库/库不/不能/能放/放同/同步/步目/目录）
	addWith(t, s, AddInput{Content: "记忆库不能放同步目录", Project: "p1"})
	// B 是 A 的后缀，缺 记忆/忆库/库不 三个 bigram —— 放宽后会命中，精确不会
	addWith(t, s, AddInput{Content: "不能放同步目录", Project: "p1"})

	hits, err := s.Search(SearchQuery{Query: "记忆库不能放同步目录", Scope: "all", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("精确表达式本应命中 A")
	}
	for _, h := range hits {
		if h.Content == "不能放同步目录" {
			t.Errorf("精确有命中时不得放宽，否则会带出只共享部分 bigram 的 B：%q", h.Content)
		}
	}
}

// 退化不得改变可见性规则：被取代/过期的记忆仍不得出现。
func TestBroadFallbackKeepsVisibilityRules(t *testing.T) {
	s := newTestStore(t)
	id := addWith(t, s, AddInput{Content: "一条会被取代的长记忆内容用于测试", Project: "p1"})
	setColumn(t, s, id, "superseded_by", "someone-else")
	hits, err := s.Search(SearchQuery{Query: "这条长记忆怎么找", Scope: "all", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("退化路径也必须遵守 superseded 过滤，got %d 条", len(hits))
	}
}

// 单字查询走通（退化表达式的边界）。
func TestSearchSingleRuneQueryStillWorks(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: "库不放同步目录", Project: "p1"})
	hits, err := s.Search(SearchQuery{Query: "库", Scope: "all", Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("单字应能命中 1 条，got %d", len(hits))
	}
}
