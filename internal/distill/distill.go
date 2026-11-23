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
)

const (
	DefaultModel   = "qwen2.5:7b"
	DefaultTimeout = 45 * time.Second

	// RulesMaxFacts 是单次规则蒸馏产出的候选上限。
	// 上限不是质量阀，是**供需阀**：一次会话实测能产 6–7 条，而人工裁决
	// 速率是每窗口 0–2 条，不限流时候选积压只增不减（docs/ops-watch.md §3 R8）。
	// 取 3：一屏可裁决完，且够覆盖一次会话真正的决策点。
	RulesMaxFacts = 3
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
	// APIKey / BaseURL 是调用方（model.json / flag）给的显式覆盖，优先于环境变量。
	// 没有它们时，兼容 OpenAI 的接入只能靠 OPENAI_API_KEY + OPENAI_BASE_URL 三条变量拼，
	// 而这正是「换个模型要改一堆环境」的来源。
	APIKey  string
	BaseURL string
	// CLI / ModelTag 是 mode=cli 时的来源：CLI 是内置白名单里的名字（不是命令串），
	// ModelTag 形如 cli@<ver>+tpl<ver>，供 judge_model 按版本归因。由 ResolveOptions 填。
	CLI      string
	ModelTag string
}

// ModelSpec 是一次本地或兼容 OpenAI 的调用参数。
type ModelSpec struct {
	Name    string
	BaseURL string
	APIKey  string
	Kind    string // ollama | openai | cli
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
	// CLI 必须先于 provider 解析：mode=cli 时 Provider 是空的，落到 auto
	// 会去探测 ollama/env，既不必要，也会把 CLI 配置静默降级成别的来源。
	if strings.TrimSpace(opt.CLI) != "" {
		name := "cli:" + strings.TrimSpace(opt.ModelTag)
		if strings.TrimSpace(opt.ModelTag) == "" {
			name = "cli:" + strings.TrimSpace(opt.CLI)
		}
		return name, ModelSpec{Name: opt.CLI, Kind: "cli"}, nil
	}
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
		key := strings.TrimSpace(opt.APIKey)
		if key == "" {
			key = strings.TrimSpace(opt.Getenv("OPENAI_API_KEY"))
		}
		if key == "" {
			return "openai", ModelSpec{}, fmt.Errorf("未设置 OPENAI_API_KEY")
		}
		base := strings.TrimSpace(opt.BaseURL)
		if base == "" {
			base = strings.TrimSpace(opt.Getenv("OPENAI_BASE_URL"))
		}
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return "openai", ModelSpec{Name: opt.Model, BaseURL: base, APIKey: key, Kind: "openai"}, nil
	case "deepseek":
		// DeepSeek 是 OpenAI 兼容协议：同一套 chat/completions，只换 base 与 model 名。
		key := strings.TrimSpace(opt.APIKey)
		if key == "" {
			key = strings.TrimSpace(opt.Getenv("DEEPSEEK_API_KEY"))
		}
		if key == "" {
			return "deepseek", ModelSpec{}, fmt.Errorf("未设置 DEEPSEEK_API_KEY（或 ~/.v2mem/model.json 的 api_key）")
		}
		base := strings.TrimSpace(opt.BaseURL)
		if base == "" {
			base = strings.TrimSpace(opt.Getenv("DEEPSEEK_BASE_URL"))
		}
		if base == "" {
			base = "https://api.deepseek.com/v1"
		}
		name := opt.Model
		if strings.TrimSpace(name) == "" {
			name = "deepseek-flash"
		}
		return "deepseek", ModelSpec{Name: name, BaseURL: base, APIKey: key, Kind: "openai"}, nil
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
//   - 每条事实带 attributed_to，供裁决时看出「谁说的」。
func ExtractRules(obs []Observation) []Fact {
	var out []Fact
	seen := map[string]bool{}
	for _, o := range obs {
		if len(out) >= RulesMaxFacts {
			break
		}
		summary := strings.TrimSpace(o.Summary)
		if summary == "" {
			continue
		}
		speaker := SpeakerForEvent(o.Event)
		if speaker == "" {
			continue
		}
		kind := matchKind(summary)
		if kind == "" || (speaker == "assistant" && kind == "preference") {
			continue
		}
		content := truncate(oneLine(summary), 240)
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

func matchKind(summary string) string {
	for _, k := range kindRes {
		if k.re.MatchString(summary) {
			return k.kind
		}
	}
	return ""
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
