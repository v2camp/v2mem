package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/distill"
)

// mem setup：零填写地把本机已登录的 harness CLI 配成判定器与蒸馏的模型来源。
//
// 权限边界（spec §5/§6/§7）：
//   - 只读/写 ~/.v2mem/model.json；不打开库、不写 hooks / MCP / memories / candidates、
//     不启动常驻进程。
//   - 写盘前必须 dry-run 通过：judge 合成候选能过 parseJudgeVerdicts、蒸馏真实 prompt
//     能过 ParseFacts。模板 argv 未实测，dry-run 是唯一的硬闸门，不得放宽。
//   - 用户主动执行本命令构成外发授权，但必须当场告知外发范围与回退命令。
const (
	setupDryRunTimeout = 60 * time.Second
	setupBackupSuffix  = ".v2mem.bak"
)

// setupNotice 是写盘成功后必须打印的告知（不是提问）。
func setupNotice(name, tag string) string {
	return fmt.Sprintf(`已将判定器与蒸馏的模型来源设为 %s（%s）。
judge 的候选正文与蒸馏的观测摘要会发往该 CLI 所连的远端模型；本机不另存副本。
回退：mem setup --undo。
自动触发仍默认关（MEM_JUDGE_SPAWN 未设 1）。
`, name, tag)
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "只探测与验证，不写盘")
	status := fs.Bool("status", false, "显示当前配置（不联网、不调用 CLI）")
	mode := fs.String("mode", "", "显式指定来源: api|cli|off")
	undo := fs.Bool("undo", false, "恢复最近一次写盘前的备份")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("未知参数: %s（mem setup 不接受位置参数）", strings.Join(fs.Args(), " "))
	}
	switch *mode {
	case "", "api", "cli", "off":
	default:
		return fmt.Errorf("未知 --mode %q（可选 api|cli|off）", *mode)
	}
	// --status / --undo 是只读动作，和「改配置」互斥：同时给只会让人猜不透要执行哪个。
	if *status && *undo {
		return errors.New("--status 与 --undo 不能同时使用")
	}
	if (*status || *undo) && *mode != "" {
		return errors.New("--status / --undo 与 --mode 互斥：查看或回退时不要改配置")
	}

	path := distill.DefaultModelConfigPath()
	if *status {
		return printSetupStatus(path)
	}
	if *undo {
		return undoSetupConfig(path)
	}

	// 读现状。JSON 非法 / 未知 mode 一律报错：不能拿默认值盖掉用户的文件。
	cfg, err := distill.LoadModelConfig(path)
	if err != nil {
		return fmt.Errorf("读取模型配置失败（%s）: %w", path, err)
	}

	switch *mode {
	case "off":
		next := cfg
		next.Mode = "off"
		if *dryRun {
			fmt.Printf("dry-run：将写入 mode=off（不写盘）\npath      %s\n", path)
			return nil
		}
		if err := distill.SaveModelConfig(path, next); err != nil {
			return err
		}
		fmt.Printf("已关闭判定器与蒸馏的模型来源（mode=off）。\n回退：mem setup --undo。\n")
		return nil
	case "api":
		next, err := apiConfigFromEnv(cfg)
		if err != nil {
			return err
		}
		if *dryRun {
			fmt.Printf("dry-run：将写入 mode=api provider=%s model=%s api_key=%s（不写盘）\npath      %s\n",
				next.Provider, next.Model, redactKey(next.APIKey), path)
			return nil
		}
		if err := distill.SaveModelConfig(path, next); err != nil {
			return err
		}
		fmt.Printf("已将模型来源切换为 API：%s / %s。\n密钥已写入 %s（0600），不会进 shell history。\n回退：mem setup --undo。\n",
			next.Provider, next.Model, path)
		return nil
	}

	// 默认流程：已有配置且没有显式要求时幂等跳过 —— 不探测、不改写。
	if *mode == "" && !*dryRun && setupConfigExists(path) {
		return printSetupStatus(path)
	}

	// 按白名单顺序逐个 dry-run，首个通过者胜出：某个模板可能探测得到却跑不通
	// （argv 未实测），此时换下一个，而不是让用户手工排查。
	candidates := distill.ProbeAll()
	if len(candidates) == 0 {
		msg := "没有探测到可用的 CLI（白名单：claude / codex）。判定器与蒸馏的模型来源未改变。"
		if *mode == "cli" {
			return errors.New(msg + "\n装了其中任一个后重跑 mem setup，或改用 mem setup --mode api / --mode off。")
		}
		fmt.Printf("%s\n可改用 mem setup --mode api（读 DEEPSEEK_API_KEY / OPENAI_API_KEY），或 mem setup --mode off 关闭。\n", msg)
		return nil
	}

	var name, tag string
	var reasons []string
	for _, cand := range candidates {
		candTag := distill.VersionTag(cand)
		if err := verifySetupCLI(cand, candTag); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", cand, err))
			continue
		}
		name, tag = cand, candTag
		break
	}
	if name == "" {
		// 全部候选失败：不写盘。默认流程不阻塞安装（退出 0），显式要求 cli 则报错。
		if *mode == "cli" {
			return fmt.Errorf("所有候选 CLI 验证均未通过，未写盘:\n  %s", strings.Join(reasons, "\n  "))
		}
		fmt.Printf("未配置：%d 个候选 CLI 全部未通过验证，未写盘。\n  %s\n可改用 mem setup --mode api，或 mem setup --mode off。\n",
			len(candidates), strings.Join(reasons, "\n  "))
		return nil
	}

	next := cfg
	next.Mode = "cli"
	next.CLI = name
	next.ModelTag = tag

	if *dryRun {
		fmt.Printf("dry-run 通过：%s（%s）。未写盘。\npath      %s\n", name, tag, path)
		return nil
	}
	if err := distill.SaveModelConfig(path, next); err != nil {
		return err
	}
	fmt.Print(setupNotice(name, tag))
	return nil
}

// verifySetupCLI 是写盘前的硬闸门：同一份配置下跑通 judge 与 distill 两条真实路径。
//
// judge 用合成候选（正文含「候选」，与真实 prompt 同形），蒸馏用一条真实形状的观测。
// 只要任一环节解析不了，就说明模板 argv 或输出格式不合约，该候选不得写盘。
func verifySetupCLI(name, tag string) error {
	opt := distill.Options{CLI: name, ModelTag: tag, Timeout: setupDryRunTimeout}
	complete, _, err := distill.PromptCompleter(opt)
	if err != nil {
		return fmt.Errorf("调用器不可用: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupDryRunTimeout)
	defer cancel()
	raw, err := complete(ctx, fmt.Sprintf(judgePrompt,
		`[{"id":"aaaaaaaa","kind":"fact","scope":"project","project":"setup","content":"测试候选"}]`))
	if err != nil {
		return fmt.Errorf("judge dry-run 调用失败: %w", err)
	}
	if _, err := parseJudgeVerdicts(raw); err != nil {
		return fmt.Errorf("judge dry-run 输出不可解析: %w", err)
	}

	out := distill.Run([]distill.Observation{
		{ID: "setup", Event: "user-prompt", Summary: "偏好：默认使用短句。"},
	}, distill.Options{CLI: name, ModelTag: tag, Timeout: setupDryRunTimeout})
	// empty 是合法 0 条，只有 fail（模型没起来 / 响应坏）才算验证失败。
	if out.Failed() {
		return fmt.Errorf("distill dry-run 失败: %s", out.Error)
	}
	return nil
}

// apiConfigFromEnv 只用环境变量构造 API 配置：不读 stdin，避免密钥回显与依赖终端。
// 已有配置的其它字段（cli/model_tag）保留不删，方便再切回 cli。
func apiConfigFromEnv(cfg distill.ModelConfig) (distill.ModelConfig, error) {
	next := cfg
	next.Mode = "api"
	if key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")); key != "" {
		next.Provider = "deepseek"
		next.Model = distill.DefaultDeepSeekModel
		next.BaseURL = "https://api.deepseek.com/v1"
		next.APIKey = key
		return next, nil
	}
	if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
		next.Provider = "openai"
		next.Model = "gpt-4o-mini"
		next.BaseURL = "https://api.openai.com/v1"
		next.APIKey = key
		return next, nil
	}
	return distill.ModelConfig{}, errors.New(
		"--mode api 需要环境变量 DEEPSEEK_API_KEY 或 OPENAI_API_KEY；请先 export 其中之一后重跑 mem setup --mode api（不读 stdin，密钥不回显）")
}

// printSetupStatus 只读本地文件：不探测 PATH、不调用 CLI、不出网。
func printSetupStatus(path string) error {
	cfg, err := distill.LoadModelConfig(path)
	if err != nil {
		return fmt.Errorf("读取模型配置失败（%s）: %w", path, err)
	}
	perm := "—"
	if st, err := os.Stat(path); err == nil {
		perm = fmt.Sprintf("%04o", st.Mode().Perm())
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	mode := cfg.Mode
	if mode == "" {
		mode = "未配置（无 model.json）"
	}
	fmt.Printf("path      %s\n", path)
	fmt.Printf("mode      %s\n", mode)
	fmt.Printf("cli       %s\n", setupOrDash(cfg.CLI))
	fmt.Printf("model_tag %s\n", setupOrDash(cfg.ModelTag))
	fmt.Printf("perm      %s\n", perm)
	return nil
}

// undoSetupConfig 用备份内容覆盖 model.json 后删除备份。
// 不用 os.Rename 覆盖目标：Windows 上会失败，而这里要的只是「写回原内容」。
func undoSetupConfig(path string) error {
	bak := path + setupBackupSuffix
	b, err := os.ReadFile(bak)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("没有可回退的备份（%s 不存在）；要关闭判定器与蒸馏可用 mem setup --mode off", bak)
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	// WriteFile 不改已存在文件的权限，这里显式收敛到 0600（配置里可能有密钥）。
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if err := os.Remove(bak); err != nil {
		return err
	}
	fmt.Printf("已从备份恢复 %s\n", path)
	return nil
}

func setupConfigExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func setupOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// redactKey 只在 dry-run 的预览里露出密钥的前缀：够确认「用的是哪把」，不落全量。
func redactKey(key string) string {
	if len(key) <= 6 {
		return "***"
	}
	return key[:3] + "…" + key[len(key)-3:]
}
