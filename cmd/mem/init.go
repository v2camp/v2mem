package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wanghui/v2mem/internal/harness"
)

// 本文件负责「把钩子装进各工具」。两条纪律：
//
//  1. **合并，不覆盖**。用户已有的 hook 与无关配置键必须原样保留；
//     解析不了就报错退出，绝不拿默认值盖掉用户文件。
//  2. **幂等**。重复执行不得累积重复条目（否则钩子会被调用多次、注入翻倍），
//     因此写入前先摘掉自己上次写的条目。
const (
	// instrBegin/instrEnd 是指令级接入的标记块边界，靠它们实现「就地替换」。
	instrBegin = "<!-- >>> v2mem 自动生成，勿手改；重跑 `mem init` 覆盖本块 >>> -->"
	instrEnd   = "<!-- <<< v2mem <<< -->"
	// hookTimeoutSec 是钩子超时。记忆查询是本地 SQLite 读，20 秒绰绰有余，
	// 又不至于在库异常时把宿主拖住。
	hookTimeoutSec = 20
)

func cmdHarness(args []string) error {
	var c common
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.json {
		return printJSON(harness.All())
	}

	fmt.Println("v2mem 支持的 harness 接入方式（* = 检测到本机已安装）")
	fmt.Println()
	for _, h := range harness.All() {
		mark := " "
		target := "（无配置文件，见说明）"
		if existing := h.ExistingGlobalConfigs(); len(existing) > 0 {
			mark = "*"
			target = existing[0]
		} else if len(h.GlobalConfig) > 0 {
			target = harness.ExpandHome(h.GlobalConfig[0])
		} else if len(h.Instruction) > 0 {
			target = h.Instruction[0]
		}
		fmt.Printf("  %s %-16s %-12s %s\n", mark, h.Name, h.Tier, target)
	}
	fmt.Println()
	fmt.Println("native      = 事件触发即执行，代码强制（推荐）")
	fmt.Println("bridge      = 通过官方桥接包复用他家的 hook 协议")
	fmt.Println("instruction = 无原生钩子，只能把指令写进会话级文件靠模型遵守（可靠性低一档）")
	fmt.Println()
	fmt.Println("查看某家的详情：mem init --harness <name> --dry-run")
	fmt.Println("写入配置：      mem init --harness <name> [--project <项目目录>]")
	return nil
}

func cmdInit(args []string) error {
	var c common
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	c.register(fs)
	harnessName := fs.String("harness", "", "要接入的工具名（见 mem harness）")
	project := fs.String("project", "", "写入该目录的项目级配置（默认写用户级）")
	scope := fs.String("scope", "auto", "auto|global|project|instruction")
	dryRun := fs.Bool("dry-run", false, "只显示将写入的内容，不落盘")
	command := fs.String("command", "", "覆盖 hook 命令（默认用当前二进制的绝对路径）")
	file := fs.String("file", "", "覆盖目标文件路径（用于把指令块放到不挤占记忆预算的位置）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	h, err := harness.Get(*harnessName)
	if err != nil {
		return err
	}
	switch *scope {
	case "auto", "global", "project", "instruction":
	default:
		return fmt.Errorf("未知 --scope: %q（可选 auto|global|project|instruction）", *scope)
	}

	if strings.TrimSpace(*file) != "" {
		return initAtExplicitFile(h, *file, *command, *dryRun, *scope == "instruction" || h.Tier == harness.TierInstruction)
	}

	switch *scope {
	case "instruction":
		return initInstruction(h, *project, *dryRun)
	case "project":
		if len(h.ProjectConfig) == 0 {
			return fmt.Errorf("%s 官方未提供项目级配置文件，只能写用户级（去掉 --scope project）", h.Name)
		}
	}

	if h.Tier == harness.TierInstruction && *scope == "auto" {
		return initInstruction(h, *project, *dryRun)
	}

	target, isNew, err := resolveConfigTarget(h, *project, *scope)
	if err != nil {
		return err
	}
	cmd := *command
	if strings.TrimSpace(cmd) == "" {
		cmd = defaultHookCommand(h.Name)
	} else {
		// 自定义命令也补标记，否则重复 init 认不出它、会不断累积
		cmd = withMarker(cmd)
	}

	changed, err := upsertHooksConfig(target, cmd, *dryRun)
	if err != nil {
		return err
	}

	fmt.Printf("harness : %s（%s，%s）\n", h.Name, h.LabelOrName(), h.Tier)
	fmt.Printf("文件    : %s%s\n", target, map[bool]string{true: "（新建）", false: ""}[isNew])
	fmt.Printf("命令    : %s\n", cmd)
	fmt.Printf("事件    : %s、%s\n", harness.EventNames[harness.EventSessionStart], harness.EventNames[harness.EventPromptSubmit])
	if *dryRun {
		fmt.Println("模式    : --dry-run，未写盘")
	} else if changed {
		fmt.Println("结果    : 已写入（原文件已备份为 <文件>.v2mem.bak）")
	} else {
		fmt.Println("结果    : 内容已是最新，未改动")
	}
	printFollowUp(h, isNew)
	return nil
}

// resolveConfigTarget 决定写到哪个文件。
func resolveConfigTarget(h *harness.Harness, project, scope string) (path string, isNew bool, err error) {
	if scope == "project" || (project != "" && len(h.ProjectConfig) > 0) {
		if len(h.ProjectConfig) == 0 {
			return "", false, fmt.Errorf("%s 官方未提供项目级配置文件（只能写用户级）", h.Name)
		}
		p := filepath.Join(project, h.ProjectConfig[0])
		_, statErr := os.Stat(p)
		return p, errors.Is(statErr, os.ErrNotExist), nil
	}
	if len(h.GlobalConfig) == 0 {
		if len(h.Instruction) > 0 {
			return "", false, fmt.Errorf("%s 没有 shell hook 配置，请用 --scope instruction（或直接 mem init --harness %s）", h.Name, h.Name)
		}
		return "", false, fmt.Errorf("%s 无可写入的配置路径", h.Name)
	}
	path, isNew = h.ResolveGlobalConfig()
	if path == "" {
		return "", false, fmt.Errorf("%s 无可写入的配置路径", h.Name)
	}
	return path, isNew, nil
}

// hookMarker 是写在 hook 命令末尾的标记，用于识别「这条是本工具生成的」。
//
// 为什么不用「可执行文件名 == mem」来识别：一旦二进制改名（mem-0.3、v2mem，
// 或 go test 生成的 mem.test），识别就失效，重复 init 会不断累积条目、
// 让钩子被调用多次。实测被幂等用例抓过。
//
// 写成 shell 注释是安全的：各家文档的 command 都经 shell 执行
// （bash / zsh / sh / powershell 皆以 # 为注释），且不污染实际参数。
const hookMarker = "# v2mem"

// defaultHookCommand 用当前二进制的绝对路径，而不是裸 `mem`。
//
// harness 拉起 hook 时的环境往往很干净（可能没有用户 shell 的 PATH），
// 写裸命令会静默失效 —— 这是「配置生效须可证」里最典型的坑。
func defaultHookCommand(harnessName string) string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "mem"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return withMarker(fmt.Sprintf("%s hook --harness %s", exe, harnessName))
}

// withMarker 在命令末尾补上标记（已存在则原样返回）。
func withMarker(cmd string) string {
	if strings.Contains(cmd, hookMarker) {
		return cmd
	}
	return cmd + "  " + hookMarker
}

// upsertHooksConfig 合并写入 hooks 配置，返回是否发生了改动。
func upsertHooksConfig(path, command string, dryRun bool) (changed bool, err error) {
	var cfg map[string]any
	raw, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				// 绝不拿默认值盖掉解析不了的用户文件
				return false, fmt.Errorf("现有配置文件不是合法 JSON，已中止以免覆盖：%s: %w", path, err)
			}
		}
	case os.IsNotExist(readErr):
		// 首次写入
	default:
		return false, readErr
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	for _, ev := range []harness.Event{harness.EventSessionStart, harness.EventPromptSubmit} {
		name := harness.EventNames[ev]
		groups := withoutOurs(hooks[name])
		groups = append(groups, map[string]any{
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": command,
					"timeout": hookTimeoutSec,
				},
			},
		})
		hooks[name] = groups
	}
	cfg["hooks"] = hooks

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')

	if dryRun {
		fmt.Printf("--- 将写入 %s ---\n%s", path, out)
		return false, nil
	}
	if readErr == nil && string(raw) == string(out) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if readErr == nil {
		if err := backupOnce(path, raw); err != nil {
			return false, err
		}
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// withoutOurs 摘掉本工具上次写入的 hook 组，实现幂等与可升级。
func withoutOurs(v any) []any {
	groups, _ := v.([]any)
	kept := make([]any, 0, len(groups))
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			kept = append(kept, g)
			continue
		}
		hs, _ := gm["hooks"].([]any)
		if len(hs) > 0 && allOurs(hs) {
			continue
		}
		kept = append(kept, g)
	}
	return kept
}

func allOurs(hooks []any) bool {
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			return false
		}
		c, _ := hm["command"].(string)
		if !isV2memHookCommand(c) {
			return false
		}
	}
	return true
}

// isV2memHookCommand 判定一条 command 是不是本工具生成的（靠显式标记，见 hookMarker）。
func isV2memHookCommand(cmd string) bool {
	return strings.Contains(cmd, hookMarker)
}

// backupOnce 只备份一次，保证 .v2mem.bak 永远是「本工具首次触碰前」的原始状态。
func backupOnce(path string, raw []byte) error {
	bak := path + ".v2mem.bak"
	if _, err := os.Stat(bak); err == nil {
		return nil
	}
	return os.WriteFile(bak, raw, 0o644)
}

// initAtExplicitFile 把内容写到用户指定的文件（--file）。
// 用途：WorkBuddy / TraeWork 这类指令级接入，其默认指令文件常是「有注入预算的
// 受限文件」，把 v2mem 的说明塞进去会挤占预算，因此需要能改指到别处（如 AGENTS.md）。
func initAtExplicitFile(h *harness.Harness, path, command string, dryRun, instruction bool) error {
	if instruction || len(h.GlobalConfig) == 0 {
		changed, err := upsertMarkedBlock(path, instructionBlock(h.Name), dryRun)
		if err != nil {
			return err
		}
		fmt.Printf("harness : %s（%s，指令级 · 指定文件）\n", h.Name, h.LabelOrName())
		fmt.Printf("文件    : %s\n", path)
		reportWriteResult(dryRun, changed)
		fmt.Println("说明    : ⚠️ 指令级接入 —— 靠模型遵守，没有代码强制")
		printFollowUp(h, false)
		return nil
	}

	cmd := command
	if strings.TrimSpace(cmd) == "" {
		cmd = defaultHookCommand(h.Name)
	} else {
		cmd = withMarker(cmd)
	}
	changed, err := upsertHooksConfig(path, cmd, dryRun)
	if err != nil {
		return err
	}
	fmt.Printf("harness : %s（%s，%s · 指定文件）\n", h.Name, h.LabelOrName(), h.Tier)
	fmt.Printf("文件    : %s\n", path)
	fmt.Printf("命令    : %s\n", cmd)
	fmt.Printf("事件    : %s、%s\n", harness.EventNames[harness.EventSessionStart], harness.EventNames[harness.EventPromptSubmit])
	reportWriteResult(dryRun, changed)
	printFollowUp(h, false)
	return nil
}

func reportWriteResult(dryRun, changed bool) {
	switch {
	case dryRun:
		fmt.Println("模式    : --dry-run，未写盘")
	case changed:
		fmt.Println("结果    : 已写入（原文件已备份为 <文件>.v2mem.bak）")
	default:
		fmt.Println("结果    : 内容已是最新，未改动")
	}
}

// initInstruction 为无原生钩子的工具写入指令级钩子块。
func initInstruction(h *harness.Harness, project string, dryRun bool) error {
	if len(h.Instruction) == 0 {
		return fmt.Errorf("%s 既无 shell hook 配置也无指令文件，无法接入", h.Name)
	}
	rel := h.Instruction[0]
	var target string
	if strings.HasPrefix(rel, "~") {
		target = harness.ExpandHome(rel)
	} else if project != "" {
		target = filepath.Join(project, rel)
	} else {
		return fmt.Errorf("%s 的指令文件 %s 属项目级，请用 --project <项目目录> 指定位置", h.Name, rel)
	}

	changed, err := upsertMarkedBlock(target, instructionBlock(h.Name), dryRun)
	if err != nil {
		return err
	}

	fmt.Printf("harness : %s（%s，%s）\n", h.Name, h.LabelOrName(), h.Tier)
	fmt.Printf("文件    : %s\n", target)
	reportWriteResult(dryRun, changed)
	fmt.Println("说明    : ⚠️ 指令级接入 —— 本块靠模型遵守，没有代码强制，可靠性低于原生钩子")
	printFollowUp(h, false)
	return nil
}

// upsertMarkedBlock 在标记块范围内就地替换；没有标记块则追加。
// 因此重复执行不会堆积多份指令。
func upsertMarkedBlock(path, block string, dryRun bool) (changed bool, err error) {
	raw, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, readErr
	}
	old := string(raw)

	var next string
	begin := strings.Index(old, instrBegin)
	end := strings.Index(old, instrEnd)
	switch {
	case begin >= 0 && end > begin:
		next = old[:begin] + block + old[end+len(instrEnd):]
	case strings.TrimSpace(old) == "":
		next = block + "\n"
	default:
		next = strings.TrimRight(old, "\n") + "\n\n" + block + "\n"
	}

	if dryRun {
		fmt.Printf("--- 将写入 %s ---\n%s", path, next)
		return false, nil
	}
	if next == old {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if readErr == nil {
		if err := backupOnce(path, raw); err != nil {
			return false, err
		}
	}
	return true, os.WriteFile(path, []byte(next), 0o644)
}

// instructionBlock 是指令级钩子的正文：明确告诉模型「有这么一个库」以及怎么用。
func instructionBlock(harnessName string) string {
	lines := []string{
		instrBegin,
		"## 分层记忆（v2mem · 由 `mem init` 生成）",
		"",
		"本机有一个跨会话的本地记忆库（Level 2，正文不进上下文，按需检索）。",
		"接入工具：`" + harnessName + "`",
		"",
		"- 需要历史决策 / 踩坑 / 约定时：",
		"  `mem search \"<关键词>\" --scope current --json`",
		"- 学到新的原子事实时（一条只写一个事实，不要写长段落）：",
		"  `mem add --kind decision|pitfall|preference|fact \"<原子事实>\"`",
		"- 用上了某条记忆后反馈一次：",
		"  `mem touch <id前8位>`",
		"",
		"规则：不要凭空断言历史决策，不确定就先 `mem search`；查不到再问用户。",
		instrEnd,
	}
	return strings.Join(lines, "\n")
}

// printFollowUp 打印「怎么确认真的生效」与已知注意事项。
// 配置写进不生效的位置是静默失败，所以验证方法必须给出。
func printFollowUp(h *harness.Harness, isNew bool) {
	if h.Verify != "" {
		fmt.Printf("验证    : %s\n", h.Verify)
	}
	for _, e := range h.ExtraConfig {
		fmt.Printf("还需    : %s\n", e)
	}
	for _, n := range h.Notes {
		fmt.Printf("注意    : %s\n", n)
	}
	if isNew {
		fmt.Printf("提示    : 未检测到 %s 的配置目录，写入前请确认该工具确实装在本机\n", h.LabelOrName())
	}
	fmt.Printf("文档    : %s\n", h.Docs)
}
