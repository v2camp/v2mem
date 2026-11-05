// Package recall 实现读侧供给：先判断「这一问值不值得查」，再把 store 的
// 完整融合池按结构支持、去重、作用域配额、预算挑成可注入的少数几条，
// 并把「查了什么、过取多少、为什么丢」落到 read_traces / read_trace_items。
//
// 设计边界（T4）：
//   - 不重造检索：候选池来自 store.Search 的融合结果（提高 Limit 过取），
//     本包不实现 FTS、BM25 或 RRF。
//   - 不猜：没有结构支持的命中一律丢弃；无合格项就弃答，不凑数。
//   - 只读：除轨迹表外不写库；注入/检索/列举都不更新 memories 的命中字段。
package recall

import (
	"fmt"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/commit"
	"github.com/v2camp/v2mem/internal/store"
)

// ---------- Query gate ----------

// Gate 是「是否进入召回」的判定。
type Gate struct {
	Run    bool   `json:"run"`
	Reason string `json:"reason"`
}

// Skipped 表示这次查询被 gate 主动跳过（不计入空注入分母）。
func (g Gate) Skipped() bool { return !g.Run }

const (
	GateRun             = "run"
	GateSkipEmpty       = "empty"
	GateSkipPleasantry  = "pleasantry"
	GateSkipShortNoTech = "short_no_tech"
)

// socialTokens 是纯客套/应答词。全部 token 都落在这里时视为没有检索意图。
var socialTokens = map[string]bool{
	"你好": true, "您好": true, "hi": true, "hello": true, "hey": true,
	"谢谢": true, "多谢": true, "thanks": true, "thank": true, "thx": true,
	"好的": true, "好": true, "ok": true, "okay": true, "嗯": true, "嗯嗯": true,
	"收到": true, "了解": true, "明白": true, "明白了": true, "继续": true,
	"在吗": true, "在么": true, "早": true, "早上好": true, "晚安": true,
	"辛苦": true, "辛苦了": true, "没事": true, "随便": true, "可以": true,
	"行": true, "行吧": true, "再见": true, "拜拜": true,
	"你好呀": true, "您好呀": true, "嗨": true, "哈喽": true, "谢谢你": true,
	"多谢啦": true, "好滴": true, "好嘞": true, "收到啦": true, "明白啦": true,
	"继续吧": true, "在不在": true, "没问题": true, "可以呀": true,
}

// techShortTokens 是「极短但确实有技术/历史语义」的两字词。
// 没有这份白名单，gate 会把「报错」「部署」这类真问题误判成寒暄。
var techShortTokens = map[string]bool{
	"报错": true, "异常": true, "超时": true, "崩溃": true, "卡死": true,
	"修复": true, "排查": true, "调试": true, "回滚": true, "上线": true,
	"发布": true, "提交": true, "合并": true, "分支": true, "冲突": true,
	"接口": true, "字段": true, "索引": true, "缓存": true, "日志": true,
	"依赖": true, "版本": true, "编译": true, "构建": true, "部署": true,
	"配置": true, "测试": true, "脚本": true, "路径": true, "目录": true,
	"权限": true, "环境": true, "数据": true, "查询": true, "检索": true,
	"记忆": true, "规则": true, "偏好": true, "决策": true, "任务": true,
	"问题": true, "内存": true, "线程": true, "进程": true, "网络": true,
	"端口": true, "证书": true, "密钥": true, "迁移": true, "备份": true,
	"恢复": true, "同步": true, "评审": true, "文档": true, "注释": true,
	"函数": true, "变量": true, "类型": true, "错误": true, "性能": true,
}

// QueryGate 判断一次用户提问是否值得进入召回。
//
// 空 query、纯客套、以及「极短且没有任何技术/历史语义」的 query 直接跳过；
// 跳过的原因单独记在轨迹的 query_gate/gate_reason 上，**不**计入空注入分母——
// 「今天天气」没查与「查了但没命中」是两件事，混在一起会让检索质量看起来更差。
func QueryGate(q string) Gate {
	content := gateContent(q)
	if content == "" {
		return Gate{Run: false, Reason: GateSkipEmpty}
	}
	tokens := gateTokens(strings.ToLower(q))
	allSocial := len(tokens) > 0
	for _, tok := range tokens {
		if !socialTokens[tok] {
			allSocial = false
			break
		}
	}
	if allSocial {
		return Gate{Run: false, Reason: GateSkipPleasantry}
	}

	n := len([]rune(content))
	if n <= 1 {
		return Gate{Run: false, Reason: GateSkipShortNoTech}
	}
	if n == 2 && !hasLatinOrDigit(content) && !techShortTokens[content] {
		return Gate{Run: false, Reason: GateSkipShortNoTech}
	}
	return Gate{Run: true, Reason: GateRun}
}

// gateContent 去掉空白与标点，只保留字母、数字、汉字。
func gateContent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case isHanRune(r), isASCIILetter(r), isASCIIDigit(r):
			b.WriteRune(r)
		}
	}
	return strings.ToLower(b.String())
}

// gateTokens 在原文上按「非字母数字汉字」切词，用于判定纯客套。
// 必须在原始 query 上切：先去掉空格再把 "ok thanks" 粘成 "okthanks"，
// 客套判定就永远失效。
func gateTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(isHanRune(r) || isASCIILetter(r) || isASCIIDigit(r))
	})
}

func isHanRune(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FFF
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func hasLatinOrDigit(s string) bool {
	for _, r := range s {
		if isASCIILetter(r) || isASCIIDigit(r) {
			return true
		}
	}
	return false
}

// WantsDeep 识别显式深查意图。只要用户明确说「深查 / 完整历史 / 所有相关」，
// 结构支持里就允许「显式深查」这一条；否则不给任何命中额外的通行证。
func WantsDeep(q string) bool {
	for _, marker := range []string{
		"深查", "详细查", "查全", "查一下全部", "完整历史", "全部相关", "所有相关",
		"都查一下", "翻一下记忆", "把所有相关",
	} {
		if strings.Contains(q, marker) {
			return true
		}
	}
	return false
}

// ---------- 结构支持 ----------

// QueryProfile 是查询侧的可匹配特征。
type QueryProfile struct {
	Identifiers []string // 需整体出现的精确技术标识符
}

// AnalyzeQuery 抽取查询里的技术标识符（ASCII 词、带数字/下划线/点/斜杠的串）。
func AnalyzeQuery(q string) QueryProfile {
	var out []string
	seen := map[string]bool{}
	var cur []rune
	flush := func() {
		if len(cur) < 2 {
			cur = cur[:0]
			return
		}
		tok := strings.Trim(strings.ToLower(string(cur)), "._-/:")
		cur = cur[:0]
		if len(tok) < 2 || !hasASCIILetter(tok) || stopIdentifiers[tok] || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}
	for _, r := range q {
		if isIdentRune(r) {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return QueryProfile{Identifiers: out}
}

func hasASCIILetter(s string) bool {
	for _, r := range s {
		if isASCIILetter(r) {
			return true
		}
	}
	return false
}

func isIdentRune(r rune) bool {
	return isASCIILetter(r) || isASCIIDigit(r) || r == '_' || r == '.' || r == '-' || r == '/' || r == ':'
}

var stopIdentifiers = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "how": true, "what": true,
	"why": true, "this": true, "that": true, "you": true, "are": true, "was": true,
	"not": true, "use": true, "using": true, "can": true, "from": true, "into": true,
	"about": true, "please": true, "when": true, "where": true, "which": true,
	"to": true, "of": true, "in": true, "on": true, "is": true, "it": true,
	"thing": true, "things": true, "something": true, "anything": true, "stuff": true,
}

// SupportContext 是判定的外部输入。
type SupportContext struct {
	Deep             bool
	PositiveSubjects map[string]bool
}

// SubjectKey 把记忆归到「同一类事」上：kind + project。project 空表示全局。
func SubjectKey(h store.Hit) string { return h.Kind + "|" + h.Project }

// Support 是一条命中的结构支持。
type Support struct {
	Score   float64  `json:"score"`
	Classes []string `json:"classes"`
}

// Evaluate 计算一条命中的结构支持。
//
// 五类支持：精确技术标识符、有效 query group 的 precise、两路以上共同支持、
// 显式深查、同 subject 正反馈。分值让「强多路」能压过「弱 precise」：
// 只被 precise 单路命中的条目是 1.0，而两路共同支持从 2.0 起。
// 精确标识符（3.0）与显式深查（3.0）是更强的证据，仍可越过普通多路。
func Evaluate(h store.Hit, p QueryProfile, ctx SupportContext) Support {
	var classes []string
	var score float64

	if len(p.Identifiers) > 0 && containsAllIdentifiers(h.Content, p.Identifiers) {
		classes = append(classes, "identifier")
		score += 3.0
	}
	if ctx.Deep {
		classes = append(classes, "deep")
		score += 3.0
	}
	if ctx.PositiveSubjects[SubjectKey(h)] {
		classes = append(classes, "subject")
		score += 1.5
	}
	routes := 0
	if h.Signals != nil {
		if h.Signals.Precise > 0 {
			routes++
		}
		if h.Signals.Broad > 0 {
			routes++
		}
		if h.Signals.Fuzzy > 0 {
			routes++
		}
	}
	if routes >= 2 {
		classes = append(classes, "multi")
		score += 2.0 + 0.25*float64(routes-2)
	}
	if h.Signals != nil && h.Signals.Precise > 0 {
		classes = append(classes, "precise")
		score += 1.0
	}
	return Support{Score: score, Classes: classes}
}

// MatchClass 把支持类别压成稳定的短串（写 read_trace_items.match_class）。
func (s Support) MatchClass() string { return strings.Join(s.Classes, "+") }

func containsAllIdentifiers(content string, ids []string) bool {
	lower := strings.ToLower(content)
	for _, id := range ids {
		if !containsIdent(lower, id) {
			return false
		}
	}
	return true
}

// containsIdent 做技术标识符的边界匹配：fts5 命中「fts5 索引」，但不命中 fts50。
func containsIdent(content, ident string) bool {
	from := 0
	for {
		i := strings.Index(content[from:], ident)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(ident)
		leftOK := start == 0 || !isWordRune(rune(content[start-1]))
		rightOK := end >= len(content) || !isWordRune(rune(content[end]))
		if leftOK && rightOK {
			return true
		}
		from = start + 1
	}
}

func isWordRune(r rune) bool { return isASCIILetter(r) || isASCIIDigit(r) || r == '_' }

// ---------- 供给 ----------

// Overfetch 是默认过取深度（18–24 区间取 24）。
const Overfetch = 24

func poolSize(limit int) int {
	if limit <= 6 {
		if limit*4 > Overfetch {
			return limit * 4
		}
		return Overfetch
	}
	return limit * 4
}

// QueryStore 是 recall 依赖的最小 store 面。用接口而非 *store.Store，
// 便于用假实现覆盖「无支持 / 批量证据 / 写轨迹失败」这些真实库难造的分支。
type QueryStore interface {
	Search(store.SearchQuery) ([]store.Hit, error)
	AdoptedSubjectKeys() (map[string]bool, error)
	EvidenceLinksBatch([]string) (map[string][]commit.Link, error)
	ContentHashes([]string) (map[string]string, error)
	WriteReadTrace(store.ReadTrace, []store.ReadTraceItem) (string, error)
}

// EvidenceStore 只需要批量证据读取（MCP/开场注入也会用到）。
type EvidenceStore interface {
	EvidenceLinksBatch([]string) (map[string][]commit.Link, error)
}

// QueryOptions 是一次检索型供给的参数。
type QueryOptions struct {
	Query        string
	Project      string
	Scope        string // current|global|all
	SessionID    string
	Harness      string
	Limit        int
	Budget       int // 可注入文本的字符预算（调用方已扣掉标题/提示）
	ContentRunes int // 单条正文截断长度
	Deep         bool
	// Explicit 表示这次是用户显式发起的检索（MCP recall_context / CLI），
	// 不套自动注入的 query gate；质量筛选与预算仍然生效。
	Explicit bool
	// NotesCovered 判断一条命中是否已被 notes 小字条覆盖；nil = 不去重。
	NotesCovered func(store.Hit) bool
}

func (o QueryOptions) withDefaults() QueryOptions {
	if o.Limit <= 0 {
		o.Limit = 3
	}
	if o.Budget == 0 {
		o.Budget = 1200
	}
	if o.ContentRunes <= 0 {
		o.ContentRunes = 120
	}
	if o.Scope == "" {
		o.Scope = "current"
	}
	return o
}

// Item 是池中一条候选的处置。
type Item struct {
	Hit           store.Hit
	Support       Support
	MatchClass    string
	Eligible      bool
	DropReason    string
	InjectionRank int
	Provenance    string
}

// Injected 把 Item 转成注入计划（保持顺序）。
type Injected struct {
	Hit        store.Hit
	Provenance string
	Rank       int
}

// Result 是一次供给的结果。Items 含被丢弃的候选，便于审计「为什么不是它」。
type Result struct {
	TraceID    string
	TraceError string
	Gate       Gate
	Outcome    string
	Items      []Item
	Injected   []Injected
	Overfetch  int
	RawHits    int
	Qualified  int

	hashes map[string]string
}

const (
	OutcomeInjected      = "injected"
	OutcomeNoHit         = "abstained_no_hit"
	OutcomeLowQuality    = "abstained_low_quality"
	OutcomeSkippedGate   = "skipped_gate"
	OutcomeError         = "error"
	dropUnsupported      = "unsupported_quality"
	dropDuplicateContent = "duplicate_content"
	dropNotes            = "notes_duplicate"
	dropScope            = "scope"
	dropScopeQuota       = "scope_quota"
	dropBudget           = "budget"
	dropLimit            = "limit"
)

// Query 执行一次读侧供给。
//
// 处理顺序是固定的（需求 T4）：过取 18–24 条 → 质量筛选 → notes/content 去重 →
// scope 配额 → 选择 → 预算检查 → 从合格池补位。预算不足时继续扫描池子，
// 绝不「先截 3 条再过滤、过滤掉也不回补」。
func Query(st QueryStore, opt QueryOptions) (Result, error) {
	opt = opt.withDefaults()
	started := time.Now()
	res := Result{Gate: QueryGate(opt.Query)}
	if opt.Explicit {
		res.Gate = Gate{Run: true, Reason: "explicit"}
	}
	if opt.Deep || WantsDeep(opt.Query) {
		opt.Deep = true
	}

	tr := store.ReadTrace{
		SessionID: opt.SessionID, Project: opt.Project, Harness: opt.Harness,
		QueryGate: "run", RawQuery: opt.Query, FocusQuery: strings.TrimSpace(opt.Query),
		Scope: opt.Scope, TargetLimit: opt.Limit, BudgetChars: opt.Budget,
		Overfetch: poolSize(opt.Limit),
	}
	finish := func() {
		tr.RawHits = res.RawHits
		tr.Qualified = res.Qualified
		tr.Injected = len(res.Injected)
		tr.Outcome = res.Outcome
		tr.ElapsedMS = time.Since(started).Milliseconds()
		items := make([]store.ReadTraceItem, 0, len(res.Items))
		for i, it := range res.Items {
			items = append(items, store.ReadTraceItem{
				Ordinal: i + 1, MemoryID: it.Hit.ID, ContentHash: res.hashOf(it.Hit.ID),
				SignalsJSON: signalsJSON(it.Hit.Signals), MatchClass: it.MatchClass,
				Eligible: it.Eligible, DropReason: it.DropReason,
				InjectionRank: it.InjectionRank, Provenance: it.Provenance,
			})
		}
		id, err := st.WriteReadTrace(tr, items)
		if err != nil {
			res.TraceError = err.Error()
			return
		}
		res.TraceID = id
	}

	tr.GateReason = res.Gate.Reason
	if res.Gate.Skipped() {
		res.Outcome = OutcomeSkippedGate
		tr.QueryGate = "skip"
		tr.GateReason = res.Gate.Reason
		finish()
		return res, nil
	}
	res.Overfetch = tr.Overfetch
	res.hashes = map[string]string{}

	positives, err := st.AdoptedSubjectKeys()
	if err != nil {
		positives = map[string]bool{}
	}
	ctx := SupportContext{Deep: opt.Deep, PositiveSubjects: positives}
	profile := AnalyzeQuery(opt.Query)

	hits, err := st.Search(store.SearchQuery{
		Query: opt.Query, Project: opt.Project, Scope: opt.Scope, Limit: tr.Overfetch,
	})
	if err != nil {
		res.Outcome = OutcomeError
		tr.GateReason = err.Error()
		finish()
		return res, err
	}
	res.RawHits = len(hits)
	if len(hits) == 0 {
		res.Outcome = OutcomeNoHit
		finish()
		return res, nil
	}

	res.hashes, _ = st.ContentHashes(hitIDs(hits))
	prov := ProvenanceFor(st, hits)

	res.Items = make([]Item, len(hits))
	for i, h := range hits {
		sup := Evaluate(h, profile, ctx)
		res.Items[i] = Item{
			Hit: h, Support: sup, MatchClass: sup.MatchClass(),
			Provenance: prov[h.ID],
		}
	}

	// 质量筛选 → notes/content 去重 → scope 合法性。
	seenContent := map[string]bool{}
	var eligible []int
	for i := range res.Items {
		it := &res.Items[i]
		cls := scopeClass(it.Hit, opt.Project)
		switch {
		case it.Support.Score <= 0:
			it.DropReason = dropUnsupported
		case opt.NotesCovered != nil && opt.NotesCovered(it.Hit):
			it.DropReason = dropNotes
		case seenContent[NormalizeContent(it.Hit.Content)]:
			it.DropReason = dropDuplicateContent
		case cls == "cross" && opt.Scope != "all":
			it.DropReason = dropScope
		default:
			it.Eligible = true
			seenContent[NormalizeContent(it.Hit.Content)] = true
			eligible = append(eligible, i)
		}
	}
	res.Qualified = len(eligible)
	if len(eligible) == 0 {
		res.Outcome = OutcomeLowQuality
		finish()
		return res, nil
	}

	// 选择：先按结构支持排序，再按 store 的融合分/名次破平。
	sortEligible(res.Items, eligible)

	// global 配额是硬上限：即便本工程没有候选，也不拿全局记忆凑满 limit。
	globalCap := 1

	used := 0
	globalCount := 0
	for _, idx := range eligible {
		if len(res.Injected) >= opt.Limit {
			break
		}
		it := &res.Items[idx]
		cls := scopeClass(it.Hit, opt.Project)
		if cls == "global" && globalCount >= globalCap {
			it.DropReason = dropScopeQuota
			continue
		}
		line := RenderLine(it.Hit, it.Provenance, opt.ContentRunes)
		cost := len([]rune(line))
		if used+cost > opt.Budget {
			it.DropReason = dropBudget
			continue
		}
		it.InjectionRank = len(res.Injected) + 1
		used += cost
		if cls == "global" {
			globalCount++
		}
		res.Injected = append(res.Injected, Injected{
			Hit: it.Hit, Provenance: it.Provenance, Rank: it.InjectionRank,
		})
	}

	if len(res.Injected) == 0 {
		res.Outcome = OutcomeLowQuality
		finish()
		return res, nil
	}
	for _, idx := range eligible {
		if res.Items[idx].InjectionRank == 0 && res.Items[idx].DropReason == "" {
			res.Items[idx].DropReason = dropLimit
		}
	}
	res.Outcome = OutcomeInjected
	finish()
	return res, nil
}

func (r *Result) hashOf(id string) string {
	if r.hashes == nil {
		return ""
	}
	return r.hashes[id]
}

func hitIDs(hits []store.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}

func scopeClass(h store.Hit, project string) string {
	if h.Project == "" {
		return "global"
	}
	if project != "" && h.Project == project {
		return "project"
	}
	return "cross"
}

func sortEligible(items []Item, idx []int) {
	// 插入排序避免为召回引入额外依赖；合格池规模 ≤ 过取深度，成本可忽略。
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && better(items[idx[j]], items[idx[j-1]]); j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
}

func better(a, b Item) bool {
	if a.Support.Score != b.Support.Score {
		return a.Support.Score > b.Support.Score
	}
	if a.Hit.Score != b.Hit.Score {
		return a.Hit.Score > b.Hit.Score
	}
	if a.Hit.Rank != b.Hit.Rank {
		return a.Hit.Rank < b.Hit.Rank
	}
	return a.Hit.ID < b.Hit.ID
}

// NormalizeContent 用于 notes/content 去重：忽略空白、大小写与句末标点。
func NormalizeContent(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(s, "。．.!！?？;；,，:：")
}

// RenderLine 渲染一条注入行。hook 与 recall 的预算检查共用它，
// 保证「算的」和「注入的」是同一串字符。
func RenderLine(h store.Hit, provenance string, contentRunes int) string {
	if contentRunes <= 0 {
		contentRunes = 120
	}
	line := fmt.Sprintf("- [%s] mem:%s %s", h.Kind, ShortID(h.ID), truncateRunes(oneLine(h.Content), contentRunes))
	if strings.TrimSpace(provenance) != "" {
		line += " · " + provenance
	}
	return line + "\n"
}

// ShortID 是注入与 CLI 共用的短 id（8 位）。
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n]) + "…"
}

// ProvenanceFor 批量取证据并压成一行紧凑 provenance；无证据时退回召回路径/作用域。
//
// 一次批量调用是硬要求：注入路径按条查证据会把 hook 延迟预算吃光。
func ProvenanceFor(st EvidenceStore, hits []store.Hit) map[string]string {
	out := make(map[string]string, len(hits))
	if len(hits) == 0 {
		return out
	}
	ids := make([]string, 0, len(hits))
	seen := map[string]bool{}
	for _, h := range hits {
		if h.ID == "" || seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		ids = append(ids, h.ID)
	}
	links, err := st.EvidenceLinksBatch(ids)
	for _, h := range hits {
		p := ""
		if err == nil {
			p = (commit.Compact{}).CompactProvenance(links[h.ID])
		}
		if strings.TrimSpace(p) == "" {
			p = fallbackProvenance(h)
		}
		out[h.ID] = p
	}
	return out
}

func fallbackProvenance(h store.Hit) string {
	if h.Signals != nil {
		var parts []string
		if h.Signals.Precise > 0 {
			parts = append(parts, "p")
		}
		if h.Signals.Broad > 0 {
			parts = append(parts, "b")
		}
		if h.Signals.Fuzzy > 0 {
			parts = append(parts, "f")
		}
		if len(parts) > 0 {
			return strings.Join(parts, "+")
		}
	}
	if h.Project == "" {
		return "global"
	}
	return "project:" + h.Project
}

func signalsJSON(s *store.HitSignals) string {
	if s == nil {
		return "{}"
	}
	// HitSignals 只有三个数值字段，手工拼比反射更稳（且不会带 NaN）。
	var parts []string
	if s.Precise != 0 {
		parts = append(parts, fmt.Sprintf(`"precise":%g`, s.Precise))
	}
	if s.Broad != 0 {
		parts = append(parts, fmt.Sprintf(`"broad":%g`, s.Broad))
	}
	if s.Fuzzy != 0 {
		parts = append(parts, fmt.Sprintf(`"fuzzy":%g`, s.Fuzzy))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
