package recall

import (
	"errors"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/commit"
	"github.com/v2camp/v2mem/internal/store"
)

// ---------- gate ----------

func TestQueryGate(t *testing.T) {
	cases := []struct {
		q      string
		run    bool
		reason string
	}{
		{"", false, GateSkipEmpty},
		{"   ？！  ", false, GateSkipEmpty},
		{"你好", false, GateSkipPleasantry},
		{"谢谢！", false, GateSkipPleasantry},
		{"ok thanks", false, GateSkipPleasantry},
		{"你好呀", false, GateSkipPleasantry},
		{"继续", false, GateSkipPleasantry},
		{"好", false, GateSkipPleasantry},
		{"天", false, GateSkipShortNoTech},
		{"天气", false, GateSkipShortNoTech},
		{"改下", false, GateSkipShortNoTech},
		{"报错", true, GateRun},
		{"db", true, GateRun},
		{"依赖怎么管", true, GateRun},
		{"iCloud 同步目录能不能放数据库", true, GateRun},
	}
	for _, c := range cases {
		got := QueryGate(c.q)
		if got.Run != c.run || got.Reason != c.reason {
			t.Errorf("QueryGate(%q) = %+v, want run=%v reason=%q", c.q, got, c.run, c.reason)
		}
	}
}

func TestWantsDeep(t *testing.T) {
	if !WantsDeep("把所有相关记忆都查一下") {
		t.Fatal("应识别显式深查")
	}
	if WantsDeep("这个配置怎么改") {
		t.Fatal("普通问题不应算深查")
	}
}

// ---------- 质量筛选 ----------

func TestAnalyzeQueryIdentifiers(t *testing.T) {
	p := AnalyzeQuery("FTS5 的 read_traces 和 /api/v1 怎么建索引")
	want := []string{"fts5", "read_traces", "api/v1"}
	if len(p.Identifiers) != len(want) {
		t.Fatalf("标识符 = %v, want %v", p.Identifiers, want)
	}
	for i := range want {
		if p.Identifiers[i] != want[i] {
			t.Fatalf("标识符 = %v, want %v", p.Identifiers, want)
		}
	}
	if got := AnalyzeQuery("how to use the thing"); len(got.Identifiers) != 0 {
		t.Fatalf("停用词不应成为标识符，got %v", got.Identifiers)
	}
}

func TestEvaluateSupportClassesAndScore(t *testing.T) {
	ctx := SupportContext{}
	// 弱 precise：单路命中。
	weak := Evaluate(store.Hit{
		Content: "完全无关的正文", Signals: &store.HitSignals{Precise: 0.9},
	}, QueryProfile{}, ctx)
	if weak.Score != 1.0 || weak.MatchClass() != "precise" {
		t.Fatalf("弱 precise 应为 1.0/precise，got %+v", weak)
	}
	// 强多路：两路共同支持，必须压过弱 precise。
	multi := Evaluate(store.Hit{
		Content: "完全无关的正文",
		Signals: &store.HitSignals{Precise: 0.3, Broad: 0.3},
	}, QueryProfile{}, ctx)
	if multi.Score <= weak.Score || !strings.Contains(multi.MatchClass(), "multi") {
		t.Fatalf("多路支持应压过弱 precise，got %+v vs %+v", multi, weak)
	}
	// 精确技术标识符必须整体出现。
	p := QueryProfile{Identifiers: []string{"fts5", "read_traces"}}
	hit := Evaluate(store.Hit{Content: "FTS5 索引与 read_traces 表", Signals: &store.HitSignals{Broad: 0.2}}, p, ctx)
	if !strings.Contains(hit.MatchClass(), "identifier") || hit.Score < 3.0 {
		t.Fatalf("标识符支持未生效: %+v", hit)
	}
	partial := Evaluate(store.Hit{Content: "FTS5 索引"}, p, ctx)
	if strings.Contains(partial.MatchClass(), "identifier") {
		t.Fatalf("只出现一半标识符不应算精确支持: %+v", partial)
	}
	// 显式深查与同 subject 正反馈。
	deep := Evaluate(store.Hit{Content: "无关"}, QueryProfile{}, SupportContext{Deep: true})
	if deep.Score < 3.0 || deep.MatchClass() != "deep" {
		t.Fatalf("显式深查支持未生效: %+v", deep)
	}
	subject := Evaluate(store.Hit{Kind: "decision", Project: "demo", Content: "无关"},
		QueryProfile{}, SupportContext{PositiveSubjects: map[string]bool{"decision|demo": true}})
	if subject.Score < 1.5 || subject.MatchClass() != "subject" {
		t.Fatalf("同 subject 正反馈未生效: %+v", subject)
	}
}

func TestContainsIdentBoundary(t *testing.T) {
	if !containsIdent("fts5 索引", "fts5") {
		t.Fatal("fts5 应命中")
	}
	if containsIdent("fts50 索引", "fts5") {
		t.Fatal("fts5 不应命中 fts50")
	}
	if !containsIdent("path/to/file.go", "file.go") {
		t.Fatal("带扩展名的标识符应命中")
	}
}

// ---------- 供给（假 store） ----------

type fakeStore struct {
	hits       []store.Hit
	searchErr  error
	searchN    int
	links      map[string][]commit.Link
	linkErr    error
	linkCalls  int
	hashes     map[string]string
	hashErr    error
	positives  map[string]bool
	writeErr   error
	traces     []store.ReadTrace
	traceItems [][]store.ReadTraceItem
}

func (f *fakeStore) Search(store.SearchQuery) ([]store.Hit, error) {
	f.searchN++
	return f.hits, f.searchErr
}

func (f *fakeStore) AdoptedSubjectKeys() (map[string]bool, error) { return f.positives, nil }

func (f *fakeStore) EvidenceLinksBatch([]string) (map[string][]commit.Link, error) {
	f.linkCalls++
	return f.links, f.linkErr
}

func (f *fakeStore) ContentHashes([]string) (map[string]string, error) {
	return f.hashes, f.hashErr
}

func (f *fakeStore) WriteReadTrace(t store.ReadTrace, items []store.ReadTraceItem) (string, error) {
	if f.writeErr != nil {
		return "", f.writeErr
	}
	f.traces = append(f.traces, t)
	f.traceItems = append(f.traceItems, items)
	return "trace-" + itoa(len(f.traces)), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func hit(id, content, project string, sig *store.HitSignals) store.Hit {
	return store.Hit{
		ID: id, Content: content, Kind: "fact", Project: project,
		Signals: sig, Score: 0.5, Rank: 1,
	}
}

func TestQuerySkipGateDoesNotSearchAndWritesTrace(t *testing.T) {
	f := &fakeStore{hits: []store.Hit{hit("aaaaaaaa", "不该被查", "demo", &store.HitSignals{Precise: 1})}}
	res, err := Query(f, QueryOptions{Query: "你好", Project: "demo"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Outcome != OutcomeSkippedGate || len(res.Injected) != 0 {
		t.Fatalf("客套 query 应跳过并弃答: %+v", res)
	}
	if f.searchN != 0 {
		t.Fatalf("gate 跳过时不得进入召回，search 调用 %d 次", f.searchN)
	}
	if len(f.traces) != 1 || f.traces[0].QueryGate != "skip" || f.traces[0].GateReason != GateSkipPleasantry {
		t.Fatalf("gate 跳过应写轨迹标记: %+v", f.traces)
	}
}

func TestQueryAbstainsWithoutStructuralSupport(t *testing.T) {
	f := &fakeStore{hits: []store.Hit{
		hit("aaaaaaaa", "无信号正文", "demo", nil),
		hit("bbbbbbbb", "另一条无信号", "demo", nil),
	}}
	res, err := Query(f, QueryOptions{Query: "alpha", Project: "demo"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Outcome != OutcomeLowQuality || len(res.Injected) != 0 || res.Qualified != 0 {
		t.Fatalf("无结构支持必须弃答: %+v", res)
	}
	if len(res.Items) != 2 || res.Items[0].DropReason != dropUnsupported {
		t.Fatalf("应记录不支持原因: %+v", res.Items)
	}
}

func TestQueryNoHitOutcome(t *testing.T) {
	f := &fakeStore{}
	res, err := Query(f, QueryOptions{Query: "alpha", Project: "demo"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Outcome != OutcomeNoHit || res.RawHits != 0 {
		t.Fatalf("空池应为 no_hit: %+v", res)
	}
	if len(f.traces) != 1 || f.traces[0].Outcome != OutcomeNoHit {
		t.Fatalf("空池也应落轨迹: %+v", f.traces)
	}
}

func TestQueryDedupsNotesAndContent(t *testing.T) {
	sig := &store.HitSignals{Precise: 0.9}
	f := &fakeStore{hits: []store.Hit{
		hit("aaaaaaaa", "commit 前必跑覆盖门禁", "demo", sig),
		hit("bbbbbbbb", "提交前统一跑 pnpm test", "demo", sig),
		hit("cccccccc", "提交前统一跑  pnpm   TEST。", "demo", sig),
	}}
	seen := 0
	res, err := Query(f, QueryOptions{
		Query: "pnpm", Project: "demo",
		NotesCovered: func(h store.Hit) bool {
			seen++
			return strings.Contains(h.Content, "覆盖门禁")
		},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if seen != 3 {
		t.Fatalf("notes 去重判定应覆盖全部候选, got %d", seen)
	}
	if len(res.Injected) != 1 || res.Injected[0].Hit.ID != "bbbbbbbb" {
		t.Fatalf("应只注入未被 notes 覆盖且内容唯一的一条: %+v", res.Injected)
	}
	reasons := map[string]int{}
	for _, it := range res.Items {
		reasons[it.DropReason]++
	}
	if reasons[dropNotes] != 1 || reasons[dropDuplicateContent] != 1 {
		t.Fatalf("丢弃原因不符: %+v", reasons)
	}
}

func TestQueryBudgetBackfillsFromQualifiedPool(t *testing.T) {
	sig := &store.HitSignals{Precise: 0.9}
	long := hit("aaaaaaaa", strings.Repeat("很长的内容", 60), "demo", sig)
	long.Score, long.Rank = 0.9, 1
	short := hit("bbbbbbbb", "短内容", "demo", sig)
	short.Score, short.Rank = 0.1, 2
	f := &fakeStore{hits: []store.Hit{long, short}}

	budget := len([]rune(RenderLine(short, "p", 120)))
	res, err := Query(f, QueryOptions{Query: "alpha", Project: "demo", Limit: 3, Budget: budget})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Injected) != 1 || res.Injected[0].Hit.ID != "bbbbbbbb" {
		t.Fatalf("预算不足时应从合格池补位到短条目: %+v", res.Injected)
	}
	if res.Items[0].DropReason != dropBudget {
		t.Fatalf("长条目应记 budget 丢弃: %+v", res.Items[0])
	}
	if res.Injected[0].Rank != 1 || res.Items[1].InjectionRank != 1 {
		t.Fatalf("注入名次应连续: %+v", res)
	}
}

func TestQueryScopeQuotaAndCrossProject(t *testing.T) {
	sig := &store.HitSignals{Precise: 0.9}
	f := &fakeStore{hits: []store.Hit{
		hit("ggggggg1", "全局一", "", sig),
		hit("ggggggg2", "全局二", "", sig),
		hit("ppppppp1", "本工程一", "demo", sig),
		hit("ppppppp2", "本工程二", "demo", sig),
		hit("xxxxxxxx", "别的工程", "other", sig),
	}}
	res, err := Query(f, QueryOptions{Query: "alpha", Project: "demo", Scope: "current", Limit: 3})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	global, project := 0, 0
	for _, in := range res.Injected {
		switch in.Hit.Project {
		case "":
			global++
		case "demo":
			project++
		}
	}
	if project != 2 || global != 1 {
		t.Fatalf("默认 limit=3 应本工程 2 位、global 1 位，got project=%d global=%d: %+v", project, global, res.Injected)
	}
	if res.Items[4].DropReason != dropScope {
		t.Fatalf("非显式 all 时跨工程应丢弃: %+v", res.Items[4])
	}

	f2 := &fakeStore{hits: []store.Hit{hit("xxxxxxxx", "别的工程", "other", sig)}}
	resAll, err := Query(f2, QueryOptions{Query: "alpha", Project: "demo", Scope: "all", Limit: 3})
	if err != nil {
		t.Fatalf("Query all: %v", err)
	}
	if len(resAll.Injected) != 1 || resAll.Injected[0].Hit.Project != "other" {
		t.Fatalf("显式 scope=all 才允许跨工程: %+v", resAll.Injected)
	}
}

func TestQueryGlobalQuotaIsHardCap(t *testing.T) {
	sig := &store.HitSignals{Precise: 0.9}
	f := &fakeStore{hits: []store.Hit{
		hit("ggggggg1", "全局一", "", sig), hit("ggggggg2", "全局二", "", sig),
		hit("ggggggg3", "全局三", "", sig),
	}}
	res, err := Query(f, QueryOptions{Query: "alpha", Project: "demo", Limit: 3})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Injected) != 1 {
		t.Fatalf("global 至多 1 位是硬上限，got %d", len(res.Injected))
	}
	for _, it := range res.Items[1:] {
		if it.DropReason != dropScopeQuota {
			t.Fatalf("其余全局候选应记配额丢弃: %+v", it)
		}
	}
}

func TestQueryDeepAndSubjectFeedback(t *testing.T) {
	f := &fakeStore{hits: []store.Hit{hit("aaaaaaaa", "无关正文", "demo", nil)}}
	res, err := Query(f, QueryOptions{Query: "这个问题的完整历史", Project: "demo", Deep: true})
	if err != nil {
		t.Fatalf("Query deep: %v", err)
	}
	if len(res.Injected) != 1 || !strings.Contains(res.Items[0].MatchClass, "deep") {
		t.Fatalf("显式深查应获得支持: %+v", res)
	}

	f2 := &fakeStore{
		hits:      []store.Hit{{ID: "bbbbbbbb", Content: "无关正文", Kind: "decision", Project: "demo"}},
		positives: map[string]bool{"decision|demo": true},
	}
	res2, err := Query(f2, QueryOptions{Query: "alpha", Project: "demo"})
	if err != nil {
		t.Fatalf("Query subject: %v", err)
	}
	if len(res2.Injected) != 1 || res2.Items[0].MatchClass != "subject" {
		t.Fatalf("同 subject 正反馈应获得支持: %+v", res2)
	}
}

func TestQueryRecordsTraceCountsAndProvenance(t *testing.T) {
	sig := &store.HitSignals{Precise: 0.5, Broad: 0.5}
	f := &fakeStore{
		hits:   []store.Hit{hit("aaaaaaaa", "命中正文", "demo", sig)},
		hashes: map[string]string{"aaaaaaaa": "hash-a"},
		links:  map[string][]commit.Link{"aaaaaaaa": {{EventUID: "evt_abcdef012345", Grade: commit.GradeA}}},
	}
	res, err := Query(f, QueryOptions{
		Query: "alpha", Project: "demo", Scope: "current", Limit: 3, Budget: 500,
		SessionID: "sess-1", Harness: "claude",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.TraceID == "" || len(f.traces) != 1 {
		t.Fatalf("应写一条轨迹: %+v", res)
	}
	tr := f.traces[0]
	if tr.SessionID != "sess-1" || tr.Harness != "claude" || tr.Overfetch != Overfetch {
		t.Fatalf("轨迹元数据不符: %+v", tr)
	}
	if tr.RawHits != 1 || tr.Qualified != 1 || tr.Injected != 1 || tr.Outcome != OutcomeInjected {
		t.Fatalf("轨迹计数不符: %+v", tr)
	}
	items := f.traceItems[0]
	if len(items) != 1 || items[0].ContentHash != "hash-a" || items[0].InjectionRank != 1 {
		t.Fatalf("轨迹条目不符: %+v", items)
	}
	if !strings.Contains(items[0].Provenance, "证据") || items[0].SignalsJSON == "" || items[0].SignalsJSON == "{}" {
		t.Fatalf("provenance/signals 未记录: %+v", items[0])
	}
	if res.Items[0].Provenance == "" || !strings.Contains(RenderLine(res.Items[0].Hit, res.Items[0].Provenance, 120), "mem:aaaaaaaa") {
		t.Fatalf("注入行应带短 id 与 provenance: %+v", res.Items[0])
	}
}

func TestQueryTraceWriteFailureDoesNotBlockInjection(t *testing.T) {
	sig := &store.HitSignals{Precise: 0.9}
	f := &fakeStore{hits: []store.Hit{hit("aaaaaaaa", "正文", "demo", sig)}, writeErr: errors.New("disk full")}
	res, err := Query(f, QueryOptions{Query: "alpha", Project: "demo"})
	if err != nil {
		t.Fatalf("Query 不应因轨迹写失败报错: %v", err)
	}
	if len(res.Injected) != 1 {
		t.Fatalf("轨迹失败不应影响注入: %+v", res)
	}
	if res.TraceError == "" {
		t.Fatal("应记录轨迹写失败")
	}
}

func TestQuerySearchErrorStillWritesTrace(t *testing.T) {
	f := &fakeStore{searchErr: errors.New("fts broken")}
	res, err := Query(f, QueryOptions{Query: "alpha", Project: "demo"})
	if err == nil {
		t.Fatal("检索错误应返回")
	}
	if res.Outcome != OutcomeError {
		t.Fatalf("结局应为 error: %+v", res)
	}
	if len(f.traces) != 1 || f.traces[0].Outcome != OutcomeError {
		t.Fatalf("检索错误也应落轨迹: %+v", f.traces)
	}
}

func TestQueryContentHashAndEvidenceFailOpen(t *testing.T) {
	sig := &store.HitSignals{Precise: 0.9}
	f := &fakeStore{
		hits:    []store.Hit{hit("aaaaaaaa", "正文", "demo", sig)},
		hashErr: errors.New("hash fail"),
		linkErr: errors.New("link fail"),
	}
	res, err := Query(f, QueryOptions{Query: "alpha", Project: "demo"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Injected) != 1 || f.traces[0].Injected != 1 {
		t.Fatalf("哈希/证据失败应失败开放: %+v", res)
	}
	if f.traceItems[0][0].ContentHash != "" || f.traceItems[0][0].Provenance != "p" {
		t.Fatalf("失败时应留空哈希并退回路径 provenance: %+v", f.traceItems[0][0])
	}
}

// ---------- provenance / 渲染 ----------

func TestProvenanceFor(t *testing.T) {
	f := &fakeStore{links: map[string][]commit.Link{
		"aaaaaaaa": {{EventUID: "evt_abc123456789", Grade: commit.GradeB}},
	}}
	hits := []store.Hit{
		{ID: "aaaaaaaa", Content: "有证据"},
		{ID: "bbbbbbbb", Content: "无证据", Project: "demo", Signals: &store.HitSignals{Precise: 1, Broad: 0.5}},
		{ID: "cccccccc", Content: "全局", Signals: nil},
	}
	got := ProvenanceFor(f, hits)
	if !strings.Contains(got["aaaaaaaa"], "证据") {
		t.Fatalf("有证据应压成紧凑证据链: %q", got["aaaaaaaa"])
	}
	if got["bbbbbbbb"] != "p+b" {
		t.Fatalf("无证据应退回路径 provenance, got %q", got["bbbbbbbb"])
	}
	if got["cccccccc"] != "global" {
		t.Fatalf("无信号全局条应标 global, got %q", got["cccccccc"])
	}
	if f.linkCalls != 1 {
		t.Fatalf("证据必须批量读一次，got %d", f.linkCalls)
	}
	if empty := ProvenanceFor(f, nil); len(empty) != 0 {
		t.Fatalf("空输入应返回空 map, got %+v", empty)
	}
}

func TestRenderLineTruncatesAndKeepsOneLine(t *testing.T) {
	h := store.Hit{ID: "0123456789abcdef", Kind: "decision", Content: "第一行\n第二行 " + strings.Repeat("字", 30)}
	line := RenderLine(h, "p+b", 10)
	if !strings.HasPrefix(line, "- [decision] mem:01234567 ") {
		t.Fatalf("注入行格式不符: %q", line)
	}
	if strings.Contains(line, "\n第") || strings.Count(line, "\n") != 1 {
		t.Fatalf("正文必须压成一行: %q", line)
	}
	if !strings.Contains(line, "…") || !strings.Contains(line, " · p+b") {
		t.Fatalf("应截断并带 provenance: %q", line)
	}
	if ShortID("short") != "short" || ShortID("0123456789") != "01234567" {
		t.Fatal("ShortID 截断不符")
	}
	if NormalizeContent("  A  B。") != "a b" {
		t.Fatalf("NormalizeContent = %q", NormalizeContent("  A  B。"))
	}
}

func TestSignalsJSON(t *testing.T) {
	if got := signalsJSON(nil); got != "{}" {
		t.Fatalf("nil signals 应为 {}，got %q", got)
	}
	got := signalsJSON(&store.HitSignals{Precise: 0.5, Fuzzy: 0.25})
	if !strings.Contains(got, `"precise":0.5`) || !strings.Contains(got, `"fuzzy":0.25`) || strings.Contains(got, "broad") {
		t.Fatalf("signals 序列化不符: %q", got)
	}
}
