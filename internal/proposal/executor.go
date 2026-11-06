package proposal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/v2camp/v2mem/internal/evidence"
)

var (
	// ErrNoProvider 表示这个 executor 拿不到模型/凭据，应降级到下一级。
	ErrNoProvider = errors.New("没有可用的 Proposal provider")
	// ErrHostUnavailable 表示 Host executor 不可用（host_safe=false 时的默认）。
	//
	// 必须与「合法空结果」分开：Host 没起来不能报成 noop，否则这个窗口会被
	// 标记成已消费，证据永久漏提炼。
	ErrHostUnavailable = errors.New("Host executor 不可用")
)

// GenerateRequest 是一次生成调用的全部上下文。
//
// Repair=true 时是同一窗口的第二次（也是唯一一次）修复尝试，Previous 是
// 上次原文；provider 可以据此换更严格的提示词，而不是重复第一次的请求。
type GenerateRequest struct {
	BundleID string
	Evidence []evidence.Ref
	Prompt   string
	Repair   bool
	Previous string
}

// Generator 是模型侧的最小契约。测试用假实现即可覆盖全部状态机，
// 不需要真实模型或网络。
type Generator interface {
	Name() string
	Generate(ctx context.Context, req GenerateRequest) (string, error)
}

// FuncGenerator 把一个普通函数适配成 Generator（供 CLI 与测试使用）。
type FuncGenerator struct {
	Label string
	Fn    func(ctx context.Context, req GenerateRequest) (string, error)
}

func (g FuncGenerator) Name() string { return g.Label }

func (g FuncGenerator) Generate(ctx context.Context, req GenerateRequest) (string, error) {
	if g.Fn == nil {
		return "", fmt.Errorf("%w: %s 没有实现", ErrNoProvider, g.Label)
	}
	return g.Fn(ctx, req)
}

// PromptGenerator 把「prompt → 原文」函数（如 distill.PromptCompleter）
// 适配成 Generator；修复提示已经拼在 req.Prompt 里。
func PromptGenerator(name string, fn func(context.Context, string) (string, error)) Generator {
	if fn == nil {
		return UnavailableGenerator(name, ErrNoProvider)
	}
	return FuncGenerator{Label: name, Fn: func(ctx context.Context, req GenerateRequest) (string, error) {
		return fn(ctx, req.Prompt)
	}}
}

// UnavailableGenerator 是一个永远不可用的 provider 占位。
//
// 用途：显式选择 Host/BYOK 但环境不具备条件时，让链路走到下一级，
// 而不是把一个「没跑」伪装成「跑出了 0 条」。
func UnavailableGenerator(name string, cause error) Generator {
	if cause == nil {
		cause = ErrNoProvider
	}
	return FuncGenerator{Label: name, Fn: func(context.Context, GenerateRequest) (string, error) {
		return "", fmt.Errorf("%w: %v", ErrNoProvider, cause)
	}}
}

// modelExecutor 把 Generator 适配成冻结的 Executor 接口。
type modelExecutor struct {
	gen    Generator
	reader evidence.Reader
}

// NewExecutor 构造一个读取 Bundle 证据并调用 Generator 的执行器。
func NewExecutor(reader evidence.Reader, gen Generator) Executor {
	return &modelExecutor{gen: gen, reader: reader}
}

func (e *modelExecutor) name() string {
	if e.gen == nil {
		return "none"
	}
	return e.gen.Name()
}

// Run 实现 Executor。错误只用于「这一级不可用/失败」，候选级问题记在 Result 里。
func (e *modelExecutor) Run(bundleID string) (Result, error) {
	res := Result{Executor: e.name()}
	if e.gen == nil || e.reader == nil {
		return res, fmt.Errorf("%w: executor 未配置", ErrNoProvider)
	}
	refs, err := e.reader.ListEvidence(bundleID)
	if err != nil {
		res.Status = StatusFail
		return res, err
	}
	prompt := BuildPrompt(refs)
	eligible := 0
	for _, r := range refs {
		// 与 BuildPrompt 共用同一判定：Fidelity 驱动的准入，两者必须一致。
		if proposalEligible(r) {
			eligible++
		}
	}
	if eligible == 0 {
		// 窗口里没有可提案的证据（例如 Proposal 子会话事件）：合法空结果。
		res.Status = StatusNoop
		return res, nil
	}

	ctx := context.Background()
	raw, err := e.gen.Generate(ctx, GenerateRequest{
		BundleID: bundleID, Evidence: refs, Prompt: prompt,
	})
	if err != nil {
		res.Status = StatusFail
		return res, err
	}
	parsed, perr := ParseDrafts(raw, len(refs))
	if errors.Is(perr, ErrBadOutput) {
		// 一次有界 repair：只重问一次，第二次仍坏就判 invalid。
		raw, err = e.gen.Generate(ctx, GenerateRequest{
			BundleID: bundleID, Evidence: refs,
			Prompt: RepairPrompt(prompt, raw), Repair: true, Previous: raw,
		})
		if err != nil {
			res.Status = StatusFail
			return res, err
		}
		parsed, perr = ParseDrafts(raw, len(refs))
	}
	if perr != nil {
		res.Status = StatusInvalid
		return res, perr
	}

	res.Generated = len(parsed.Drafts) + parsed.Invalid
	res.Valid = len(parsed.Drafts)
	res.Invalid = parsed.Invalid
	res.Drafts = parsed.Drafts
	switch {
	case res.Valid == 0 && res.Invalid == 0:
		res.Status = StatusNoop
	case res.Invalid > 0 && res.Valid > 0:
		res.Status = StatusPartial
	case res.Invalid > 0:
		res.Status = StatusInvalid
		return res, fmt.Errorf("%w: %d 条草案均未通过校验", ErrBadOutput, res.Invalid)
	default:
		res.Status = StatusOK
	}
	return res, nil
}

// ExecutorNames 返回链路里的 executor 名称（诊断与测试用）。
func ExecutorNames(chain []Executor) []string {
	out := make([]string, 0, len(chain))
	for _, ex := range chain {
		if n, ok := ex.(interface{ name() string }); ok {
			out = append(out, n.name())
			continue
		}
		out = append(out, "unknown")
	}
	return out
}

// Deps 是可选的 provider 集合。nil 表示该级在当前环境不可用。
type Deps struct {
	Host  Generator
	Local Generator
	BYOK  Generator
}

// BuildChain 按「Host → Local → backlog」的固定顺序组装链路。
//
// 两条硬约束：
//   - auto/local 绝不包含 Host：Host 会从 Stop 拉起子 Agent，递归风险未清（host_safe=false）；
//   - auto 绝不包含 BYOK：把证据发到远端必须是显式选择，不能默认外发。
func BuildChain(mode string, reader evidence.Reader, deps Deps) ([]Executor, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	add := func(chain []Executor, gen Generator) []Executor {
		if gen == nil {
			return chain
		}
		return append(chain, NewExecutor(reader, gen))
	}
	var chain []Executor
	switch mode {
	case "auto", "local":
		chain = add(chain, deps.Local)
	case "host":
		chain = add(chain, deps.Host)
		chain = add(chain, deps.Local)
	case "byok":
		// BYOK 只在显式选择时外发；失败后仍可退回本地，不继续外发。
		chain = add(chain, deps.BYOK)
		chain = add(chain, deps.Local)
	case "none":
		// 显式 none = 没有 provider，终端状态是 backlog。
	default:
		return nil, fmt.Errorf("未知 executor: %s（auto|host|local|byok|none）", mode)
	}
	return chain, nil
}
