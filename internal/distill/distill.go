// Package distill 从观测里抽出候选事实。
//
// 它不写库。调用方把 Fact 放进 candidates；禁止在这里插入 memories。
// provider=none 用规则。
//
// 三种结局必须分得清：Status=ok（有事实）、Status=empty（合法 0 条）、
// Status=fail（模型不可用 / 超时 / 响应坏）。把 empty 并进 fail 会让调用方
// 无法区分「模型说没有」和「模型没跑」，只能二选一地误报或漏报。
// 失败的结构化原因放在 Outcome.Err，用 errors.Is 判断 ErrLLMUnavailable /
// ErrBadResponse / ErrNoFacts。
package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	DefaultModel   = "qwen2.5:7b"
	DefaultTimeout = 45 * time.Second
)

// 蒸馏的四种状态。为什么要有 empty：
//
// 「模型说没有可抽取的事实」和「模型压根没跑起来」是两件完全不同的事。
// 旧实现把两者都压成 fail + Facts 为空，调用方无法区分，于是只能把
// 合法的 0 条也当成故障——要么误报失败，要么为了不误报而把真失败也放过。
// 把 empty 显式拆出来，调用方才能对 fail 做告警、对 empty 记账后放过。
const (
	StatusOK      = "ok"      // 成功，Facts 非空
	StatusEmpty   = "empty"   // 成功，但模型明确表示没有可抽取的事实（合法 0 条）
	StatusFail    = "fail"    // 模型不可用 / 超时 / 响应不可解析
	StatusSkipped = "skipped" // 没有可蒸馏的观测，或窗口已蒸馏过
)

// 类型化错误。为什么不用裸 error：
//
// 调用方需要按「原因」决定行为，而不是按错误文案做字符串匹配：
//   - ErrLLMUnavailable：模型没起来 / 没配 key / 超时 → 值得重试或降级到规则
//   - ErrBadResponse：模型说话了，但说的不是 JSON → 改 prompt 或换模型
//   - ErrNoFacts：响应合法、只是没有事实 → 不是故障，记账后跳过
var (
	ErrLLMUnavailable = errors.New("模型不可用")
	ErrBadResponse    = errors.New("模型响应不可解析")
	ErrNoFacts        = errors.New("没有可抽取的事实")
)

// Observation 是蒸馏读到的一条观测。只取摘要，不取工具 stdout 全文。
type Observation struct {
	ID        string
	SessionID string
	Project   string
	Summary   string
	// Event 是观测事件类型（user-prompt / assistant-text / stop / post-tool-use…）。
	// 空串表示旧夹具未声明事件，按 user 兼容，避免历史窗口永远收敛不了。
	Event string
}

// Fact 是一条候选事实，对应方案里的一行 JSON。
type Fact struct {
	Kind         string   `json:"kind"`
	Content      string   `json:"content"`
	Salience     float64  `json:"salience"`
	ObsIDs       []string `json:"obs_ids"`
	Rationale    string   `json:"rationale"`
	AttributedTo string   `json:"attributed_to,omitempty"`
}

// SpeakerForEvent 把观测事件类型映射到事实的原始来源。
//
// 与 internal/proposal.SpeakerOf 同源规则，但不跨包依赖（distill 是更底层的一层）。
// 工具事件与未解析行返回空串：它们不是事实陈述，规则蒸馏一律跳过。
func SpeakerForEvent(event string) string {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "", "user-prompt", "user.prompt":
		return "user" // 空串 = 旧夹具兼容
	case "assistant-text", "assistant.final", "stop", "session-end":
		return "assistant"
	default:
		return ""
	}
}

// Outcome 是一次蒸馏的结果。Status 取 ok|empty|fail|skipped。
//
// Error 是给人看的文本，Err 是给代码判断的结构化原因（errors.Is/As 用）。
// 两者并存：日志要能读，调用方要能分支。
type Outcome struct {
	Provider string
	Status   string
	Error    string
	Err      error
	Facts    []Fact
}

// Failed 报告这次蒸馏是否真的失败。empty 是成功的一种，不算失败。
func (o Outcome) Failed() bool { return o.Status == StatusFail }

// failOutcome 统一构造失败结果，保证 Status/Error/Err 三者一致。
func failOutcome(provider string, err error) Outcome {
	return Outcome{Provider: provider, Status: StatusFail, Error: err.Error(), Err: err}
}

// Options 控制蒸馏。Complete 非空时替代真实 LLM，便于测试超时与坏 JSON。
type Options struct {
	Provider string
	Model    string
	Timeout  time.Duration
	LookPath func(string) (string, error)
	Getenv   func(string) string
	Complete func(ctx context.Context, spec ModelSpec, prompt string) (string, error)
}

// ModelSpec 是一次本地或兼容 OpenAI 的调用参数。
type ModelSpec struct {
	Name    string
	BaseURL string
	APIKey  string
	Kind    string // ollama | openai
}

var kindRes = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"pitfall", regexp.MustCompile(`踩坑|报错|失败|坑了|不要再|(?i)\berror\b`)},
	{"decision", regexp.MustCompile(`决策|决定|定为|之所以|拍板`)},
	{"preference", regexp.MustCompile(`偏好|默认|禁用|优先用|不要用`)},
	{"task", regexp.MustCompile(`待办|还剩|下次|做到哪|(?i)\bTODO\b`)},
}

// progressRe 命中表示「过程性叙述」：一次性的进度、状态、完成汇报，会话一结束就失效。
// 这类内容进候选只会抬高裁决成本——它是 R9 噪音里篇幅最大的一类（18 条同一任务的
// 「已完成 52/65」「65/65 全部完成」）。
var progressRe = regexp.MustCompile(`(?i)进度|已完成|全部完成|已经完成|校验通过|已落盘|已生成|已合并|已提交|已修复|已通过|已加上|已处理|已更新|本轮|本次|当前状态|正常退出|无异常|剩余|还剩|进行中|改动[:：]|\bDONE\b|\d+\s*/\s*\d+`)

// durableRe 命中表示「可复用结论」：因果、规则、教训，换一个会话仍然成立。
// 只有这类 assistant 正文才值得进候选（对齐 mem0 的 additive extraction：
// 助手只贡献用户没说过的新增信息，而「新增信息」的可机器判定形态就是这类断言）。
var durableRe = regexp.MustCompile(`根因|原因|之所以|教训|规律|凡是|以后|下次|必须|不要|应该|应当|避免|注意`)

// declarationRe 认「决策：X」这类**结构化声明**。它不依赖 必须/不要 等词就已是
// 可复用结论，但只认行首或句读之后的冒号形态——口语化的「决定先跑一遍测试」不算。
var declarationRe = regexp.MustCompile(`(?:^|[\s，。；])决策[:：]`)

// negationWindow 是判定否定语境时前后各看多少个字符。
const negationWindow = 12

// negTokens 是出现在关键词**紧邻前方**的否定词。
// 不含裸「不」：「不过」「不仅」会把真踩坑误判成否定。
var negTokens = []string{"无", "没有", "没", "未", "非", "从未", "零", "不是", "不会", "不再", "一次也没有"}

// negatedMatch 判断某次关键词命中是否处于否定语境。
//
// 起因（2026-11-17）：18 条候选全部靠否定句命中 pitfall——「日志无 `FAIL`/`ERROR`」
// 「一次也没有**失败**」「失败：0 个」。它们说的是「一切正常」，却被 kindRes 当成
// 踩坑记录。判定层不看语境，是这类噪音的直接根因，因此在命中点做前置判断。
func negatedMatch(s string, start, end int) bool {
	return negatedBefore(s[:start]) || negatedAfter(s[end:])
}

// englishNegRe 认英文否定：「no errors found」「0 failures」。
var englishNegRe = regexp.MustCompile(`(?i)(?:^|[^a-z])(?:no|not|zero|without)\s+$`)

// negatedBefore 看关键词之前是否紧跟否定词或「0 个」这类计数零。
//
// 先把尾部非汉字剥掉再判：枚举式列举（「无 `FAIL`/`ERROR`」）的否定词只出现一次，
// 后面的英文/数字项挡在中间；同样也让「2026-11-10 报错」这类日期不会被当成计数零。
func negatedBefore(prefix string) bool {
	r := []rune(prefix)
	if len(r) == 0 {
		return false
	}
	if len(r) > negationWindow {
		r = r[len(r)-negationWindow:]
	}
	window := string(r)
	if englishNegRe.MatchString(window) {
		return true
	}
	t := trimEnumeration(window)
	if t == "" {
		return false
	}
	for _, tok := range negTokens {
		if strings.HasSuffix(t, tok) {
			return true
		}
	}
	return endsWithZeroCount(t)
}

// cjkPunct 是「小句边界」。中文逗号/分号之后换了语义，否定不再向左传递；
// 顿号与斜杠是枚举连接，允许继续回看。
const cjkPunct = "，。；：！？（）【】「」『』《》〈〉…·"

// trimEnumeration 剥掉尾部的英文/数字/半角符号（枚举项），遇到汉字或小句边界就停。
// 这样「无 `FAIL`/`ERROR`」能回看到「无」，而「日志无 FAIL，决策是…」不会误判。
func trimEnumeration(s string) string {
	return strings.TrimRightFunc(s, func(x rune) bool {
		if unicode.Is(unicode.Han, x) || strings.ContainsRune(cjkPunct, x) {
			return false
		}
		return true
	})
}

// endsWithZeroCount 判断文本是否以「0」或「0 个」这类计数零结尾。
// 排除 10、-0、11/0 这类非计数零（日期、比值）。
func endsWithZeroCount(t string) bool {
	for _, unit := range []string{"个", "条", "次", "处", "份"} {
		if strings.HasSuffix(t, unit) {
			t = strings.TrimRight(strings.TrimSuffix(t, unit), " ")
			break
		}
	}
	r := []rune(t)
	if len(r) == 0 || (r[len(r)-1] != '0' && r[len(r)-1] != '０') {
		return false
	}
	if len(r) == 1 {
		return true
	}
	prev := r[len(r)-2]
	return !unicode.IsDigit(prev) && prev != '-' && prev != '/' && prev != ':'
}

// negatedAfter 看关键词之后是否紧跟「：0」「均为 0」这类「没有发生」的表述。
func negatedAfter(suffix string) bool {
	r := []rune(suffix)
	if len(r) > negationWindow {
		r = r[:negationWindow]
	}
	t := strings.TrimLeftFunc(string(r), func(x rune) bool { return !isWordish(x) })
	for _, lead := range []string{"均为", "为"} {
		if strings.HasPrefix(t, lead) {
			t = strings.TrimLeft(strings.TrimPrefix(t, lead), " ")
			break
		}
	}
	r = []rune(t)
	if len(r) == 0 {
		return false
	}
	switch r[0] {
	case '0', '０':
		// 「0x8007…」是错误码不是计数零。
		return len(r) == 1 || (r[1] != 'x' && r[1] != 'X')
	case '零':
		// 「零常驻」是实义词，只有「零 / 零个 / 零次」才是计数零。
		return len(r) == 1 || strings.ContainsRune("个条次处份项", r[1])
	case '无':
		// 「无法」「无需」是实义词，只有独立成词的「无」才是否定。
		return len(r) == 1
	}
	return false
}

func isWordish(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// WindowKey 由观测 id 集合决定。同一集合不重复蒸馏。
func WindowKey(ids []string) string {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	sum := sha256.Sum256([]byte(strings.Join(cp, "\n")))
	return hex.EncodeToString(sum[:16])
}

// Run 抽出事实。不会因为缺模型而 panic；失败写在 Outcome 里。
func Run(obs []Observation, opt Options) Outcome {
	if opt.LookPath == nil {
		opt.LookPath = exec.LookPath
	}
	if opt.Getenv == nil {
		opt.Getenv = os.Getenv
	}
	if opt.Timeout <= 0 {
		opt.Timeout = DefaultTimeout
	}
	if opt.Model == "" {
		opt.Model = DefaultModel
	}
	name, spec, err := resolve(opt)
	if err != nil {
		// 解析不出 provider = 模型不可用。这不是「抽到 0 条」。
		return failOutcome(name, fmt.Errorf("%w: %w", ErrLLMUnavailable, err))
	}
	if name == "none" {
		facts := ExtractRules(obs)
		if len(facts) == 0 {
			// 规则抽不到也是合法 0 条：观测里确实没有决策/踩坑/偏好/待办。
			return Outcome{Provider: "none", Status: StatusEmpty}
		}
		return Outcome{Provider: "none", Status: StatusOK, Facts: facts}
	}
	ctx, cancel := context.WithTimeout(context.Background(), opt.Timeout)
	defer cancel()
	complete := opt.Complete
	if complete == nil {
		complete = completeLLM
	}
	raw, err := complete(ctx, spec, promptFor(obs))
	if err != nil {
		// 超时、连不上、HTTP 非 2xx：一律归为「模型不可用」，与「响应坏」分开。
		return failOutcome(name, fmt.Errorf("%w: %w", ErrLLMUnavailable, err))
	}
	facts, err := ParseFacts(raw)
	if err != nil {
		if errors.Is(err, ErrNoFacts) {
			return Outcome{Provider: name, Status: StatusEmpty}
		}
		return failOutcome(name, err)
	}
	return Outcome{Provider: name, Status: StatusOK, Facts: attributeFacts(facts, obs)}
}

// attributeFacts 把模型没填或填错的 attributed_to 按被引观测的事件类型补齐。
// 来源以证据为准，不信任模型自报；用户优先（同一事实同时引到双方时记 user）。
func attributeFacts(facts []Fact, obs []Observation) []Fact {
	byID := make(map[string]string, len(obs))
	for _, o := range obs {
		byID[o.ID] = SpeakerForEvent(o.Event)
	}
	for i := range facts {
		want := ""
		for _, id := range facts[i].ObsIDs {
			s := byID[id]
			if s == "" {
				continue
			}
			want = s
			if s == "user" {
				break
			}
		}
		if want == "" {
			want = facts[i].AttributedTo
		}
		switch want {
		case "user", "assistant":
			facts[i].AttributedTo = want
		default:
			facts[i].AttributedTo = ""
		}
	}
	return facts
}

func resolve(opt Options) (string, ModelSpec, error) {
	p := strings.ToLower(strings.TrimSpace(opt.Provider))
	if p == "" {
		p = "auto"
	}
	switch p {
	case "none":
		return "none", ModelSpec{}, nil
	case "ollama":
		if _, err := opt.LookPath("ollama"); err != nil {
			return "ollama", ModelSpec{}, fmt.Errorf("找不到 ollama: %w", err)
		}
		return "ollama", ModelSpec{Name: opt.Model, Kind: "ollama"}, nil
	case "openai":
		key := strings.TrimSpace(opt.Getenv("OPENAI_API_KEY"))
		if key == "" {
			return "openai", ModelSpec{}, fmt.Errorf("未设置 OPENAI_API_KEY")
		}
		base := strings.TrimSpace(opt.Getenv("OPENAI_BASE_URL"))
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return "openai", ModelSpec{Name: opt.Model, BaseURL: base, APIKey: key, Kind: "openai"}, nil
	case "auto":
		if _, err := opt.LookPath("ollama"); err == nil && ollamaAlive(opt.Timeout) {
			return "ollama", ModelSpec{Name: opt.Model, Kind: "ollama"}, nil
		}
		if key := strings.TrimSpace(opt.Getenv("OPENAI_API_KEY")); key != "" {
			base := strings.TrimSpace(opt.Getenv("OPENAI_BASE_URL"))
			if base == "" {
				base = "https://api.openai.com/v1"
			}
			return "openai", ModelSpec{Name: opt.Model, BaseURL: base, APIKey: key, Kind: "openai"}, nil
		}
		return "none", ModelSpec{}, nil
	default:
		return p, ModelSpec{}, fmt.Errorf("未知 provider: %s", p)
	}
}

// ollamaAlive 用很短的时间看本地服务在不在。不在就让 auto 落到下一家，避免空等 45s。
func ollamaAlive(parent time.Duration) bool {
	budget := 1500 * time.Millisecond
	if parent > 0 && parent < budget {
		budget = parent
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ollama", "list")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// ExtractRules 用启发式从观测摘要里抽决策 / 踩坑 / 偏好 / 待办。
// 纯 stop / session-end 套话且没有这些词时不产出。
//
// 分源约束（对齐 mem0 的 USER/AGENT 双提示词）：
//   - 工具载荷 / 未解析行不产事实（它们不是事实陈述，R9 的噪音根因）；
//   - preference 只从用户原话产，助手不该替用户定义偏好；
//   - assistant 需通过 assistantDurable 的「可复用结论」判据才产事实（见该函数）；
//   - 每条事实带 attributed_to，供裁决时看出「谁说的」。
func ExtractRules(obs []Observation) []Fact {
	var out []Fact
	seen := map[string]bool{}
	for _, o := range obs {
		summary := strings.TrimSpace(o.Summary)
		if summary == "" {
			continue
		}
		speaker := SpeakerForEvent(o.Event)
		if speaker == "" {
			continue
		}
		// 判定对象必须与存储对象一致：候选落库时只保留 oneLine + 240 rune。
		// 若按全文放行、按截断存储，就会产出「存下来的那段没有任何可复用标记」的候选——
		// 2026-11-18 17:41 实例：`根因是……` 在 300 字处，存的 240 字全是
		// 「全量 Java 测试已执行 / 通过 / 失败」的进度报表，裁决时无法据其判断可复用性。
		content := truncate(oneLine(summary), 240)
		if speaker == "assistant" && !assistantDurable(content) {
			continue
		}
		kind := matchKind(summary)
		if kind == "" || (speaker == "assistant" && kind == "preference") {
			continue
		}
		if seen[kind+"\n"+content] {
			continue
		}
		seen[kind+"\n"+content] = true
		out = append(out, Fact{
			Kind: kind, Content: content, Salience: 0.6,
			ObsIDs: []string{o.ID}, Rationale: "规则抽取",
			AttributedTo: speaker,
		})
	}
	return out
}

// assistantDurable 判断 assistant 观测能否作为候选来源。
//
// 默认**不能**：assistant 正文的主体是过程叙述——进度汇报、巡检报告、「已修复…」的
// 收尾确认。这些内容一次为真、会话结束即失效，进候选只会抬高裁决成本，实测 45 条
// pending 里 35 条来自这一类（2026-11-17 巡检：其中 1 条是巡检报告本身，属自我喂养）。
//
// 放行条件取「可复用结论（含结构化声明）且不含过程性标记」：根因、教训、必须/不要
// 这类规则断言，以及「决策：X」的声明形态，正是 docs/design-speaker-attribution.md §3
// 决定「不整体排除助手」时要保住的那部分。
// 代价是漏掉不带这些标记的助手结论，这条取舍已知，见该文档 §5。
func assistantDurable(s string) bool {
	s = unquoted(s)
	if progressRe.MatchString(s) {
		return false
	}
	return durableRe.MatchString(s) || declarationRe.MatchString(s)
}

// quoteSpans 是「引用片段」的成对定界符：围栏代码块、行内代码、中文直角/双引号、英文双引号。
// 引号里的是被引述的字面（命令、字段名、报错原文），不是叙述者自己的断言。
var quoteSpans = []*regexp.Regexp{
	regexp.MustCompile("(?s)```.*?```"),
	regexp.MustCompile("`[^`]*`"),
	regexp.MustCompile("「[^」]*」"),
	regexp.MustCompile("『[^』]*』"),
	regexp.MustCompile("“[^”]*”"),
	regexp.MustCompile(`"[^"\n]*"`),
}

// unquoted 把引用片段替换成空格，只留叙述者自己的表述。
//
// 起因（2026-11-17 13:08 蒸馏）：一条**讨论判据本身**的助手正文
// 「`决策：…` 这类结构化声明本身就可复用，不该依赖「必须/不要」等词」
// 因为引号里带了「必须/不要」而通过 assistantDurable，成了该轮唯一一条 assistant 候选——
// 判据被自己的引用文本满足，属于自证。反向的错也在：真正的结论如果写成
// 「决策：候选必须带 `已完成` 字样」，引号里的过程词又会把结论判成过程叙述。
//
// 只剥成对定界符：不成对的半个引号原样保留，避免「一个反引号吞掉整句」。
func unquoted(s string) string {
	for _, re := range quoteSpans {
		s = re.ReplaceAllString(s, " ")
	}
	return s
}

// matchKind 返回首个「非否定语境」命中的 kind。
//
// 第一处命中被否定时继续找下一个 kind，而不是整条丢弃：
// 「日志无 FAIL，决策是继续跑」应当落 decision，而不是因为没有踩坑就一并跳过。
//
// 枚举式列举（「无 FAIL/ERROR/Traceback」）里，否定词只出现在第一个关键词前面，
// 后续关键词靠分隔符连缀；因此上一处被否定的命中会沿分隔符向后传递。
func matchKind(summary string) string {
	for _, k := range kindRes {
		negated := false
		prevEnd := -1
		for _, loc := range k.re.FindAllStringIndex(summary, -1) {
			if prevEnd >= 0 && !onlySeparators(summary[prevEnd:loc[0]]) {
				negated = false
			}
			if !negated && negatedMatch(summary, loc[0], loc[1]) {
				negated = true
			}
			if !negated {
				return k.kind
			}
			prevEnd = loc[1]
		}
	}
	return ""
}

// onlySeparators 判断两处命中之间是否只有枚举连接（`无 FAIL/ERROR/Traceback`、`A|B`、`A、B`）。
// 允许英文与数字：枚举项本身可能就是 `FAIL`、`timeout`、`5xx` 这类拉丁串。
// 只要出现中文等其它字符，就认为换了语义，否定不再向后传递。
func onlySeparators(s string) bool {
	for _, r := range s {
		if strings.ContainsRune(" \t/|、,，;；*`\"'()（）[]_", r) ||
			unicode.Is(unicode.Latin, r) || unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

func promptFor(obs []Observation) string {
	var b strings.Builder
	b.WriteString("从下面的会话观测抽出原子事实。一行一个 JSON，不要其它文字。\n")
	b.WriteString(`字段：{"kind":"decision|pitfall|preference|task","content":"原子事实","salience":0.6,"obs_ids":["id"],"rationale":"一句话","attributed_to":"user|assistant"}` + "\n")
	b.WriteString("不要输出 fact。没有可抽取的事实就输出空行。\n")
	b.WriteString("用户原话是主来源；偏好只从 user 观测提取。assistant 观测只贡献用户没说过的新增信息，\n")
	b.WriteString("仅复述/总结/确认用户原话的内容一律不提取（No Echo Extraction）。\n")
	b.WriteString("attributed_to 标事实的原始来源；工具载荷不产事实。\n")
	for _, o := range obs {
		fmt.Fprintf(&b, "- id=%s (%s/%s) %s\n", o.ID, SpeakerForEvent(o.Event), o.Event,
			oneLine(o.Summary))
	}
	return b.String()
}

// ParseFacts 解析 JSONL 或单个 JSON 数组。
//
// 三种结局分开表达，调用方据此区分「没事实」和「模型坏了」：
//   - 解析成功且有合规事实 → (facts, nil)
//   - 响应合法但 0 条（空输出 / [] / 全部被过滤）→ ErrNoFacts
//   - 响应根本不像 JSON（坏 JSON、纯散文）→ ErrBadResponse
func ParseFacts(raw string) ([]Fact, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// 空输出是 prompt 明确允许的「没有事实」，不是坏响应。
		return nil, fmt.Errorf("%w: 模型输出为空", ErrNoFacts)
	}
	if strings.HasPrefix(raw, "[") {
		var arr []Fact
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, fmt.Errorf("%w: JSON 数组不合规: %w", ErrBadResponse, err)
		}
		return filterFacts(arr)
	}
	var facts []Fact
	var bad, lines int
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines++
		if !strings.Contains(line, "{") {
			continue
		}
		start := strings.Index(line, "{")
		end := strings.LastIndex(line, "}")
		if start < 0 || end <= start {
			bad++
			continue
		}
		var f Fact
		if err := json.Unmarshal([]byte(line[start:end+1]), &f); err != nil {
			bad++
			continue
		}
		facts = append(facts, f)
	}
	if len(facts) == 0 {
		// 有内容却一条对象都没解析出来：模型说了话，但不是我们约定的格式。
		if bad > 0 || lines > 0 {
			return nil, fmt.Errorf("%w: 响应里没有可解析的 JSON 行", ErrBadResponse)
		}
		return nil, fmt.Errorf("%w: 模型输出为空", ErrNoFacts)
	}
	return filterFacts(facts)
}

func filterFacts(in []Fact) ([]Fact, error) {
	var out []Fact
	for _, f := range in {
		f.Content = strings.TrimSpace(f.Content)
		switch f.Kind {
		case "preference", "decision", "pitfall", "task":
		default:
			continue
		}
		if f.Content == "" {
			continue
		}
		if f.Salience <= 0 || f.Salience > 1 {
			f.Salience = 0.6
		}
		if f.ObsIDs == nil {
			f.ObsIDs = []string{}
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		// JSON 是完整的，只是没有一条满足 kind/内容要求 —— 合法 0 条。
		return nil, fmt.Errorf("%w: 没有一条合规事实", ErrNoFacts)
	}
	return out, nil
}

// LoadConfig 读 ~/.v2mem/config.toml 里的 [distill] model。缺文件用默认。
func LoadConfig(path string) (model string, timeout time.Duration) {
	model, timeout = DefaultModel, DefaultTimeout
	b, err := os.ReadFile(path)
	if err != nil {
		return model, timeout
	}
	inDistill := false
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if strings.HasPrefix(s, "[") {
			inDistill = s == "[distill]"
			continue
		}
		if !inDistill {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "model":
			if v != "" {
				model = v
			}
		case "timeout_sec":
			var n int
			if _, err := fmt.Sscan(v, &n); err == nil && n > 0 {
				timeout = time.Duration(n) * time.Second
			}
		}
	}
	return model, timeout
}

// ConfigPath 是默认配置路径。
func ConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return strings.TrimRight(home, "/") + "/.v2mem/config.toml"
}
