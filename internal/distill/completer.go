package distill

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// PromptCompleter 返回一个只做「prompt → 原文」的函数，供 Proposal 执行器
// 在本地模型上复用同一套 provider 解析逻辑。
//
// 与 Run 的区别：Run 负责「观测 → Fact」的完整流水线；这里只暴露模型调用，
// 因为 Proposal 的输入是证据 Bundle、输出是 Draft，解析与校验在 proposal 包里。
//
// 失败一律包 ErrLLMUnavailable：调用方据此降级到下一级 executor 或 backlog，
// 而不是把「模型没起来」误判成「模型说没有」。
func PromptCompleter(opt Options) (func(context.Context, string) (string, error), string, error) {
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
		return nil, name, fmt.Errorf("%w: %w", ErrLLMUnavailable, err)
	}
	if name == "none" {
		return nil, name, fmt.Errorf("%w: provider=none 没有可用模型", ErrLLMUnavailable)
	}
	if opt.Complete != nil {
		return func(ctx context.Context, prompt string) (string, error) {
			return opt.Complete(ctx, spec, prompt)
		}, name, nil
	}
	if name == "ollama" && !ollamaAlive(opt.Timeout) {
		return nil, name, fmt.Errorf("%w: ollama 未运行", ErrLLMUnavailable)
	}
	return func(ctx context.Context, prompt string) (string, error) {
		return completeLLM(ctx, spec, prompt)
	}, name, nil
}
