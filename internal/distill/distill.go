// Package distill 从观测里抽出候选事实。
//
// 它不写库。调用方把 Fact 放进 candidates；禁止在这里插入 memories。
// provider=none 用规则。LLM 失败时 Outcome.Status=fail 且 Facts 为空，由调用方决定退出码。
package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
)

// Observation 是蒸馏读到的一条观测。只取摘要，不取工具 stdout 全文。
type Observation struct {
	ID        string
	SessionID string
	Project   string
	Summary   string
}

// Fact 是一条候选事实，对应方案里的一行 JSON。
type Fact struct {
	Kind      string   `json:"kind"`
	Content   string   `json:"content"`
	Salience  float64  `json:"salience"`
	ObsIDs    []string `json:"obs_ids"`
	Rationale string   `json:"rationale"`
}

// Outcome 是一次蒸馏的结果。Status 取 ok|fail|skipped。
type Outcome struct {
	Provider string
	Status   string
	Error    string
	Facts    []Fact
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
		return Outcome{Provider: name, Status: "fail", Error: err.Error()}
	}
	if name == "none" {
		facts := ExtractRules(obs)
		return Outcome{Provider: "none", Status: "ok", Facts: facts}
	}
	ctx, cancel := context.WithTimeout(context.Background(), opt.Timeout)
	defer cancel()
	complete := opt.Complete
	if complete == nil {
		complete = completeLLM
	}
	raw, err := complete(ctx, spec, promptFor(obs))
	if err != nil {
		return Outcome{Provider: name, Status: "fail", Error: err.Error()}
	}
	facts, err := ParseFacts(raw)
	if err != nil {
		return Outcome{Provider: name, Status: "fail", Error: err.Error()}
	}
	if len(facts) == 0 {
		return Outcome{Provider: name, Status: "fail", Error: "模型没有返回合规事实"}
	}
	return Outcome{Provider: name, Status: "ok", Facts: facts}
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
func ExtractRules(obs []Observation) []Fact {
	var out []Fact
	seen := map[string]bool{}
	for _, o := range obs {
		summary := strings.TrimSpace(o.Summary)
		if summary == "" {
			continue
		}
		kind := matchKind(summary)
		if kind == "" {
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
	b.WriteString(`字段：{"kind":"decision|pitfall|preference|task","content":"原子事实","salience":0.6,"obs_ids":["id"],"rationale":"一句话"}` + "\n")
	b.WriteString("不要输出 fact。没有可抽取的事实就输出空行。\n")
	for _, o := range obs {
		fmt.Fprintf(&b, "- id=%s %s\n", o.ID, oneLine(o.Summary))
	}
	return b.String()
}

// ParseFacts 解析 JSONL 或单个 JSON 数组。不合规行跳过；一行都合规失败才报错。
func ParseFacts(raw string) ([]Fact, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("模型输出为空")
	}
	if strings.HasPrefix(raw, "[") {
		var arr []Fact
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, fmt.Errorf("JSON 不合规: %w", err)
		}
		return filterFacts(arr)
	}
	var facts []Fact
	var bad int
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "{") {
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
	kept, err := filterFacts(facts)
	if err != nil && bad > 0 && len(facts) == 0 {
		return nil, fmt.Errorf("JSON 不合规")
	}
	return kept, err
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
		return nil, fmt.Errorf("没有合规事实")
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
