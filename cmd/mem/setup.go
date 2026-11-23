package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

%s回退：mem setup --undo。
`, name, tag, autoSpawnNotice())
}

// autoSpawnNotice 是配好模型后必须打印的自动触发告知：开着什么、边界在哪、怎么关。
// 宿主钩子不继承 shell 环境，所以关闭方式以写进 model.json 的命令为主，环境变量为辅。
func autoSpawnNotice() string {
	return fmt.Sprintf("积压自动判定已开启：候选 ≥%d 条时在会话结束时后台跑一次判定，默认影子（只记判定，不改记忆）。\n"+
		"关闭自动判定：mem setup --mode off；临时关闭可设 MEM_JUDGE_SPAWN=0。\n", judgeDefaultMinPending)
}

// noHeadlessHint 点名「本机装了、但已知不能当非交互后端」的 CLI。
//
// 静默跳过会让人以为探测坏了（例如装了免费的 freebuff 却查不到）；明确说出
// 原因，用户才知道该换 API 还是换工具。它只影响文案，不影响探测与写盘。
func noHeadlessHint() string {
	items := distill.DetectNoHeadless()
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "\n检测到 %s，但%s，不能作为判定器/蒸馏来源。", it.Name, it.Why)
	}
	return b.String()
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "只探测与验证，不写盘")
	status := fs.Bool("status", false, "显示当前配置（不联网、不调用 CLI）")
	mode := fs.String("mode", "", "显式指定来源: api|cli|off")
	provider := fs.String("provider", "", "--mode api 的供应商: deepseek|openai（缺省读 model.json，仍为空则 deepseek）")
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
	if *provider != "" && *mode != "api" {
		return errors.New("--provider 只用于 --mode api")
	}
	switch strings.ToLower(*provider) {
	case "", "deepseek", "openai":
	default:
		return fmt.Errorf("未知 --provider %q（api 模式可选 deepseek|openai）", *provider)
	}
	// --status / --undo 是只读动作，和「改配置」互斥：同时给只会让人猜不透要执行哪个。
	if *status && *undo {
		return errors.New("--status 与 --undo 不能同时使用")
	}
	if (*status || *undo) && *mode != "" {
		return errors.New("--status / --undo 与 --mode 互斥：查看或回退时不要改配置")
	}
	// --undo 会写回配置并删除备份，不可逆；不能让它披着 --dry-run 的外衣真的执行。
	// --status 是只读的，与 --dry-run 并存无害，保持允许。
	if *undo && *dryRun {
		return errors.New("--undo 与 --dry-run 不可同时使用：--undo 会写回配置并删除备份")
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
		next.AutoSpawn = false // 关掉模型来源就不能留着自动外发的开关
		if *dryRun {
			fmt.Printf("dry-run：将写入 mode=off（不写盘）\npath      %s\n", path)
			return nil
		}
		if err := distill.SaveModelConfig(path, next); err != nil {
			return err
		}
		fmt.Printf("已关闭判定器与蒸馏的模型来源（mode=off），积压自动判定同时关闭。\n回退：mem setup --undo。\n")
		return nil
	case "api":
		// 流程第 1 步：没有 model.json 就先建一个空壳（0600），后续只往这个文件里存密钥。
		if !setupConfigExists(path) {
			if *dryRun {
				fmt.Printf("dry-run：将创建 %s（不写盘）\n", path)
			} else if err := distill.SaveModelConfig(path, distill.ModelConfig{Mode: "api"}); err != nil {
				return fmt.Errorf("创建 %s 失败: %w", path, err)
			} else {
				fmt.Printf("已创建 %s（0600）。\n", path)
			}
		}
		// 第 2 步：只认 model.json 里已有的 key 或用户当场输入，不读任何 API key 环境变量。
		next, err := apiConfigFromModelJSON(path, cfg, strings.ToLower(*provider), os.Stdin, os.Stdout)
		if err != nil {
			return err
		}
		next.AutoSpawn = true
		// 第 3 步：真实调用一次 judge 端点；没通过就不把密钥落盘。
		if err := apiDryRun(next); err != nil {
			return fmt.Errorf("API dry-run 未通过，未写入 %s: %w", path, err)
		}
		if *dryRun {
			fmt.Printf("dry-run 成功：provider=%s model=%s api_key=%s auto_spawn=true。未写盘。\npath      %s\n",
				next.Provider, next.Model, redactKey(next.APIKey), path)
			return nil
		}
		if err := distill.SaveModelConfig(path, next); err != nil {
			return err
		}
		fmt.Printf("已将模型来源切换为 API：%s / %s。\n密钥只写入 %s（0600），不经命令行参数或环境变量传入。\n%s回退：mem setup --undo。\n",
			next.Provider, next.Model, path, autoSpawnNotice())
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
		msg := fmt.Sprintf("没有探测到可用的 CLI（白名单：%s）。判定器与蒸馏的模型来源未改变。", strings.Join(distill.CLIWhitelist(), " / "))
		msg += noHeadlessHint()
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
			return fmt.Errorf("所有候选 CLI 验证均未通过，未写盘:\n  %s%s", strings.Join(reasons, "\n  "), noHeadlessHint())
		}
		fmt.Printf("未配置：%d 个候选 CLI 全部未通过验证，未写盘。\n  %s%s\n可改用 mem setup --mode api，或 mem setup --mode off。\n",
			len(candidates), strings.Join(reasons, "\n  "), noHeadlessHint())
		return nil
	}

	next := cfg
	next.Mode = "cli"
	next.CLI = name
	next.ModelTag = tag
	next.AutoSpawn = true

	if *dryRun {
		fmt.Printf("dry-run 通过：%s（%s）。将写入 auto_spawn=true。未写盘。\npath      %s\n", name, tag, path)
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
		return fmt.Errorf("judge dry-run 输出不可解析: %w；原始响应: %s", err, setupRawSnippet(raw))
	}

	// 观测要足够明确：模型面对「偏好：默认使用短句。」这类弱信号时可能回一段
	// 散文式说明而不是空行或 JSON，于是合法的 dry-run 也会报 ErrBadResponse。
	out := distill.Run([]distill.Observation{
		{ID: "setup", Event: "user-prompt", Summary: "我偏好所有回复都用短句，不要长段落；每次代码改动后都要跑测试。"},
	}, distill.Options{CLI: name, ModelTag: tag, Timeout: setupDryRunTimeout})
	// empty 是合法 0 条，只有 fail（模型没起来 / 响应坏）才算验证失败。
	if out.Failed() {
		msg := fmt.Sprintf("distill dry-run 失败: %s", out.Error)
		if raw := setupRawSnippet(out.Raw); raw != "" {
			msg += "；原始响应: " + raw
		}
		return errors.New(msg)
	}
	return nil
}

// setupRawSnippet 把模型原始响应截到可诊断的长度。没有原文时「响应不可解析」
// 分不清是 CLI 未登录、被限流，还是模型输出了散文（2026-11-23 的真实卡点）。
func setupRawSnippet(raw string) string {
	s := strings.TrimSpace(raw)
	r := []rune(s)
	if len(r) > 300 {
		return string(r[:300]) + "…（已截断）"
	}
	return s
}

// apiDryRun / readAPIKey 是测试替身入口：生产实现分别打真实端点、读真实终端，
// 测试替换后不必有网络或 TTY。签名即契约，别在调用点各自判断来源。
var apiDryRun = dryRunAPIConfig
var readAPIKey = promptSecretLine

// apiConfigFromModelJSON 是 --mode api 的唯一配置来源：model.json + 当场输入。
// 刻意不读 DEEPSEEK_API_KEY / OPENAI_API_KEY —— 密钥必须确定性落盘，
// 否则定时任务与宿主钩子继承不到环境时，判定器会以「没配」静默失效。
func apiConfigFromModelJSON(path string, cfg distill.ModelConfig, flagProvider string, in *os.File, out io.Writer) (distill.ModelConfig, error) {
	next := cfg
	next.Mode = "api"

	provider := strings.TrimSpace(flagProvider)
	if provider == "" {
		provider = strings.TrimSpace(cfg.Provider)
	}
	if provider == "" {
		provider = "deepseek"
		fmt.Fprintln(out, "model.json 未指定 provider，按 deepseek 配置；换家用 --provider deepseek|openai 重跑。")
	}
	switch provider {
	case "deepseek":
		next.Provider, next.Model, next.BaseURL = provider, distill.DefaultDeepSeekModel, "https://api.deepseek.com/v1"
	case "openai":
		next.Provider, next.Model, next.BaseURL = provider, "gpt-4o-mini", "https://api.openai.com/v1"
	default:
		return distill.ModelConfig{}, fmt.Errorf("--mode api 只支持 --provider deepseek|openai，当前为 %q", provider)
	}
	// 用户自己维护的 model / base_url 优先于默认值（自建网关、代理）；
	// 但换 provider 时不带走旧值，避免把 deepseek 端点配到 openai 上。
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), provider) {
		if strings.TrimSpace(cfg.Model) != "" {
			next.Model = cfg.Model
		}
		if strings.TrimSpace(cfg.BaseURL) != "" {
			next.BaseURL = cfg.BaseURL
		}
	}

	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		fmt.Fprintln(out, "model.json 里没有 api_key；本流程不读环境变量，请直接输入。")
		var err error
		key, err = readAPIKey(in, out, fmt.Sprintf("请输入 %s API Key（输入不回显）: ", provider))
		if err != nil {
			return distill.ModelConfig{}, fmt.Errorf("读取 API Key 失败: %w", err)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return distill.ModelConfig{}, errors.New("API Key 不能为空，未写盘")
		}
	} else {
		fmt.Fprintf(out, "已读取 %s 里的 api_key（%s，不回显全量）。\n", path, redactKey(key))
	}
	next.APIKey = key
	return next, nil
}

// dryRunAPIConfig 用真实 judge prompt 打一次 API，验证 key / 端点 / 模型三者可用，
// 并且返回值能被现有解析器吃下。Getenv 显式返回空：dry-run 也不得借环境变量补 key。
func dryRunAPIConfig(cfg distill.ModelConfig) error {
	complete, name, err := distill.PromptCompleter(distill.Options{
		Provider: cfg.Provider,
		Model:    cfg.Model,
		APIKey:   cfg.APIKey,
		BaseURL:  cfg.BaseURL,
		Timeout:  setupDryRunTimeout,
		Getenv:   func(string) string { return "" },
	})
	if err != nil {
		return fmt.Errorf("调用器不可用: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupDryRunTimeout)
	defer cancel()
	raw, err := complete(ctx, fmt.Sprintf(judgePrompt,
		`[{"id":"aaaaaaaa","kind":"fact","scope":"project","project":"setup","content":"测试候选"}]`))
	if err != nil {
		return fmt.Errorf("%s 调用失败: %w", name, err)
	}
	if _, err := parseJudgeVerdicts(raw); err != nil {
		return fmt.Errorf("输出不可解析: %w（原文前 200 字节：%s）", err, truncateRunes(raw, 200))
	}
	return nil
}

// promptSecretLine 的实现在同包的 secret.go：真终端临时关 ECHO，管道逐行读。
// 放在独立文件是为了把终端 ioctl 的 OS 差异挡在 setup 主流程之外。

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
	fmt.Printf("mode       %s\n", mode)
	fmt.Printf("cli        %s\n", setupOrDash(cfg.CLI))
	fmt.Printf("model_tag  %s\n", setupOrDash(cfg.ModelTag))
	fmt.Printf("provider   %s\n", setupOrDash(cfg.Provider))
	fmt.Printf("model      %s\n", setupOrDash(cfg.Model))
	if cfg.APIKey == "" {
		fmt.Printf("api_key    —\n")
	} else {
		fmt.Printf("api_key    %s（来自 model.json）\n", redactKey(cfg.APIKey))
	}
	fmt.Printf("auto_spawn %s\n", setupOnOff(cfg.AutoSpawn))
	fmt.Printf("perm       %s\n", perm)
	return nil
}

func setupOnOff(on bool) string {
	if on {
		return "开"
	}
	return "关"
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
	// 恢复后回读一次：cli→api 切换后 --undo 回到 cli，用户要能立刻看到当前来源。
	// 先回读再删备份：内容解析不了时还留着可重试的那份。
	restored, err := distill.LoadModelConfig(path)
	if err != nil {
		return fmt.Errorf("已恢复 %s，但内容无法解析（可用 mem setup --status 查看）: %w", path, err)
	}
	if err := os.Remove(bak); err != nil {
		return err
	}
	if restored.Mode == "cli" {
		fmt.Printf("已从备份恢复 %s（mode=cli，cli=%s）\n", path, restored.CLI)
	} else {
		fmt.Printf("已从备份恢复 %s（mode=%s）\n", path, restored.Mode)
	}
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
