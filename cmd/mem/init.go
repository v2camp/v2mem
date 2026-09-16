package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wanghui/v2mem/internal/harness"
)

// 本文件负责「把钩子装进各工具」。三条纪律：
//
//  1. **合并，不覆盖**。用户已有的 hook 与无关配置键必须原样保留；
//     解析不了就报错退出，绝不拿默认值盖掉用户文件。
//  2. **幂等**。重复执行不得累积重复条目（否则钩子会被调用多次、注入翻倍），
//     因此写入前先摘掉自己上次写的条目。
//  3. **选择权在用户**。不带 --harness 时进入「扫描 → 选择」流程，
//     不无差别地往所有工具里写。
const (
	// instrBegin/instrEnd 是指令级接入的标记块边界，靠它们实现「就地替换」。
	instrBegin = "<!-- >>> v2mem 自动生成，勿手改；重跑 `mem init` 覆盖本块 >>> -->"
	instrEnd   = "<!-- <<< v2mem <<< -->"
	// hookTimeoutSec 是钩子超时。记忆查询是本地 SQLite 读，20 秒绰绰有余，
	// 又不至于在库异常时把宿主拖住。
	hookTimeoutSec = 20
	// hookMarker 是写在 hook 命令末尾的标记，用于识别「这条是本工具生成的」。
	//
	// 为什么不用「可执行文件名 == mem」来识别：一旦二进制改名（mem-0.3、v2mem，
	// 或 go test 生成的 mem.test），识别就失效，重复 init 会不断累积条目、
	// 让钩子被调用多次。实测被幂等用例抓过。
	//
	// 写成 shell 注释是安全的：各家文档的 command 都经 shell 执行
	// （bash / zsh / sh / powershell 皆以 # 为注释），且不污染实际参数。
	hookMarker = "# v2mem"
)

// errSkipUnsupported 表示该工具在当前范围下没有可写位置（非错误，应报告跳过）。
var errSkipUnsupported = errors.New("该工具在当前范围下无配置文件")

type initOptions struct {
	project   string
	scope     string
	fileFlag  string
	command   string
	dryRun    bool
	skipNoPos bool // 无写入位置时返回 errSkipUnsupported 而不是报错
}

// scanEntry 是扫描结果的一行。
type scanEntry struct {
	Idx       int
	H         harness.Harness
	Detected  bool   // 用户级配置目录已存在，即本机很可能装了
	Target    string // 当前 --project 下会写到哪
	Skippable bool   // 当前范围下没有可写位置
}

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

	fmt.Println("v2mem 支持的 harness 接入方式（● = 检测到本机已安装）")
	fmt.Println()
	for _, h := range harness.All() {
		mark := " "
		target := "（无配置文件，见说明）"
		if existing := h.ExistingGlobalConfigs(); len(existing) > 0 {
			mark = "●"
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
	fmt.Println("装钩子：mem init              进入扫描选择流程")
	fmt.Println("        mem init --harness <名字> [--project <项目目录>]")
	fmt.Println("        mem init --all        不询问，处理全部已检测到的")
	return nil
}

func cmdInit(args []string) error {
	var c common
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	c.register(fs)
	harnessFlag := fs.String("harness", "", "要接入的工具，逗号分隔（留空则进入扫描选择）")
	all := fs.Bool("all", false, "不询问，处理全部已检测到的工具")
	project := fs.String("project", "", "写入该目录的项目级配置（默认写用户级）")
	scope := fs.String("scope", "auto", "auto|global|project|instruction")
	dryRun := fs.Bool("dry-run", false, "只显示将写入的内容，不落盘")
	command := fs.String("command", "", "覆盖 hook 命令（默认用当前二进制的绝对路径）")
	file := fs.String("file", "", "覆盖目标文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *scope {
	case "auto", "global", "project", "instruction":
	default:
		return fmt.Errorf("未知 --scope: %q（可选 auto|global|project|instruction）", *scope)
	}
	opts := initOptions{
		project: *project, scope: *scope, fileFlag: *file,
		command: *command, dryRun: *dryRun,
	}

	names := splitList(*harnessFlag)
	switch {
	case len(names) > 0 && *all:
		return errors.New("--harness 与 --all 不可同时使用")
	case len(names) == 0 && *all:
		selected, err := selectAll(*project)
		if err != nil {
			return err
		}
		names = selected
	case len(names) == 0:
		selected, err := promptSelection(*project)
		if err != nil {
			return err
		}
		if len(selected) == 0 {
			fmt.Println("已取消，未做任何改动。")
			return nil
		}
		names = selected
	}

	// 多选时容忍「某些工具在当前范围下没有落点」，报告跳过而不是整批失败
	opts.skipNoPos = len(names) > 1
	var skipped []string
	for i, name := range names {
		h, err := harness.Get(name)
		if err != nil {
			return err
		}
		if i > 0 {
			fmt.Println()
		}
		if err := initOne(h, opts); err != nil {
			if errors.Is(err, errSkipUnsupported) {
				skipped = append(skipped, name)
				continue
			}
			return err
		}
	}
	if len(skipped) > 0 {
		fmt.Printf("\n跳过 %d 个（当前范围下没有可写位置）：%s\n", len(skipped), strings.Join(skipped, "、"))
		fmt.Println("如需接入：去掉 --project 写用户级配置，或用 --file 指定落点。")
	}
	return nil
}

// initOne 处理单个工具。
func initOne(h *harness.Harness, opts initOptions) error {
	if strings.TrimSpace(opts.fileFlag) != "" {
		return initAtExplicitFile(h, opts.fileFlag, opts.command, opts.dryRun,
			parseConfigTarget(opts.fileFlag, h, opts.scope))
	}

	switch opts.scope {
	case "instruction":
		return initInstruction(h, opts.project, opts.dryRun, opts.skipNoPos)
	case "project":
		if len(h.ProjectConfig) == 0 {
			return fmt.Errorf("%s 官方未提供项目级配置文件，只能写用户级（去掉 --scope project）", h.Name)
		}
	}

	if h.Tier == harness.TierInstruction && opts.scope == "auto" {
		return initInstruction(h, opts.project, opts.dryRun, opts.skipNoPos)
	}

	target, isNew, err := resolveConfigTarget(h, opts.project, opts.scope)
	if err != nil {
		if opts.skipNoPos {
			return errSkipUnsupported
		}
		return err
	}
	cmd := opts.command
	if strings.TrimSpace(cmd) == "" {
		cmd = defaultHookCommand(h.Name)
	} else {
		// 自定义命令也补标记，否则重复 init 认不出它、会不断累积
		cmd = withMarker(cmd)
	}

	changed, err := upsertHooksConfig(target, cmd, opts.dryRun)
	if err != nil {
		return err
	}

	fmt.Printf("harness : %s（%s，%s）\n", h.Name, h.LabelOrName(), h.Tier)
	fmt.Printf("文件    : %s%s\n", target, map[bool]string{true: "（新建）", false: ""}[isNew])
	fmt.Printf("命令    : %s\n", cmd)
	fmt.Printf("事件    : %s、%s\n", harness.EventNames[harness.EventSessionStart], harness.EventNames[harness.EventPromptSubmit])
	reportWriteResult(opts.dryRun, changed)
	// 只在写「用户级」配置时才提示未检测到 —— 项目级写入本就该在项目里新建文件，
	// 那时报「未检测到配置目录」纯属误导。
	printFollowUp(h, isNew && opts.project == "")
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
		return "", false, fmt.Errorf("%s 没有 shell hook 配置，请用 --scope instruction 或 --file 指定落点", h.Name)
	}
	path, isNew = h.ResolveGlobalConfig()
	if path == "" {
		return "", false, fmt.Errorf("%s 无可写入的配置路径", h.Name)
	}
	return path, isNew, nil
}

// ---------- 扫描与选择 ----------

// scanHarnesses 列出全部已知入口及其在当前范围下的落点。
//
// 刻意列出**全部**而不是只列检测到的：索引因此稳定（不受本机环境影响），
// 也允许用户选一个我们没探到路径的工具（它可能装在别处）。
func scanHarnesses(project string) []scanEntry {
	all := harness.All()
	out := make([]scanEntry, 0, len(all))
	for i, h := range all {
		e := scanEntry{Idx: i + 1, H: h}
		e.Detected = len(h.ExistingGlobalConfigs()) > 0
		switch {
		case project != "" && len(h.ProjectConfig) > 0:
			e.Target = filepath.Join(project, h.ProjectConfig[0])
		case project != "" && len(h.Instruction) > 0:
			e.Target = filepath.Join(project, h.Instruction[0])
		case len(h.GlobalConfig) > 0:
			e.Target, _ = h.ResolveGlobalConfig()
		case len(h.Instruction) > 0:
			e.Target = harness.ExpandHome(h.Instruction[0])
		default:
			e.Target = "（无）"
		}
		e.Skippable = project != "" && len(h.ProjectConfig) == 0 && len(h.Instruction) == 0
		out = append(out, e)
	}
	return out
}

func countDetected(entries []scanEntry) int {
	n := 0
	for _, e := range entries {
		if e.Detected {
			n++
		}
	}
	return n
}

// selectAll 返回 --all 应处理的工具：默认只处理检测到已安装的。
//
// 被排除的必须**逐条报告**，否则用户以为「全都装了」，实际有工具被静默跳过。
func selectAll(project string) ([]string, error) {
	entries := scanHarnesses(project)
	fmt.Printf("扫描本机：检测到 %d 个已安装的工具（共 %d 个已知入口）\n\n", countDetected(entries), len(entries))

	var names, skipped []string
	for _, e := range entries {
		if e.Detected || (project != "" && !e.Skippable) {
			names = append(names, e.H.Name)
			continue
		}
		if e.Skippable {
			skipped = append(skipped, e.H.Name+"（当前范围无落点）")
		} else {
			skipped = append(skipped, e.H.Name+"（未检测到）")
		}
	}
	if len(names) == 0 {
		return nil, errors.New("未检测到任何已安装的工具；可用 --harness 指定，或确认工具是否装在本机")
	}
	if len(skipped) > 0 {
		fmt.Printf("跳过 %d 个：%s\n\n", len(skipped), strings.Join(skipped, "、"))
	}
	return names, nil
}

// promptSelection 打印扫描结果并读取用户选择。
func promptSelection(project string) ([]string, error) {
	entries := scanHarnesses(project)
	fmt.Printf("扫描本机：检测到 %d 个已安装的工具（共 %d 个已知入口）\n\n", countDetected(entries), len(entries))
	for _, e := range entries {
		mark := " "
		if e.Detected {
			mark = "●"
		}
		note := ""
		if e.Skippable {
			note = "   ← 当前范围无落点"
		}
		fmt.Printf("  [%2d] %s %-16s %-11s %s%s\n", e.Idx, mark, e.H.Name, e.H.Tier, e.Target, note)
	}
	fmt.Println()
	fmt.Print("选择要注入钩子的工具（编号或名字，逗号分隔；a=全部已检测到；q=取消）：")

	line, _ := readLine()
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "q", "quit", "n", "no":
		return nil, nil
	case "a", "all", "*":
		return selectAll(project)
	}
	return parseSelection(line, entries)
}

// readLine 读一行；EOF 时返回已读到的内容（管道输入不会有结尾换行）。
func readLine() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return line, err
	}
	return line, nil
}

// parseSelection 把「编号或名字」的列表解析成 harness 名，去重保序。
func parseSelection(line string, entries []scanEntry) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, tok := range splitList(line) {
		if n, err := strconv.Atoi(tok); err == nil {
			found := false
			for _, e := range entries {
				if e.Idx == n {
					add(e.H.Name)
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("编号 %d 超出范围（可用 1-%d）", n, len(entries))
			}
			continue
		}
		h, err := harness.Get(tok)
		if err != nil {
			return nil, err
		}
		add(h.Name)
	}
	return out, nil
}

// splitList 拆分逗号/空格/顿号分隔的名字列表。
func splitList(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '、'
	}) {
		if tok = strings.TrimSpace(tok); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// ---------- 写入 ----------

// parseConfigTarget 决定 --file 的语义：写指令块还是写 hooks 配置。
//
// 起因：把 workbuddy / traework 从指令级更正为 native 之后，
// `mem init --harness workbuddy --file AGENTS.md` 不再写指令块，而是把 .md 当 JSON 解析
// 并报「不是合法 JSON」—— 同一个 --file 因 tier 变化而语义翻转，是个哑陷阱。
//
// 现在按扩展名推断：`.md` 视为指令级落点，其余视为 hooks 配置。
// 显式 --scope 仍可覆盖推断。
// initAtExplicitFile 把内容写到用户指定的文件（--file）。
// 用途：指令级接入的默认落点常是「有注入预算的受限文件」，
// 把 v2mem 的说明塞进去会挤占预算，因此需要能改指到别处（如 AGENTS.md）。
func parseConfigTarget(fileFlag string, h *harness.Harness, scope string) bool {
	if scope == "instruction" {
		return true
	}
	if h.Tier == harness.TierInstruction {
		return true
	}
	if scope == "auto" && strings.EqualFold(filepath.Ext(fileFlag), ".md") {
		return true
	}
	return false
}

// 用途：指令级接入的默认落点常是「有注入预算的受限文件」，
// 把 v2mem 的说明塞进去会挤占预算，因此需要能改指到别处（如 AGENTS.md）。
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

// initInstruction 为「无原生钩子」或「原生钩子暂不可用」的工具写入指令级钩子块。
//
// 注意：更正注册表后，所有已知工具的默认接入都是 native/bridge，
// 这条路径因此只在显式 `--scope instruction` 时走到 —— 它是有意保留的**兜底通道**：
// 当原生钩子未生效（需 trust / 需重启 / 被企业策略禁用）时，仍能靠指令块工作。
// 故落点缺省为 AGENTS.md，而不是让命令报错。
func initInstruction(h *harness.Harness, project string, dryRun, skipNoPos bool) error {
	rel := "AGENTS.md"
	if len(h.Instruction) > 0 {
		rel = h.Instruction[0]
	}
	var target string
	if strings.HasPrefix(rel, "~") {
		target = harness.ExpandHome(rel)
	} else if project != "" {
		target = filepath.Join(project, rel)
	} else {
		if skipNoPos {
			return errSkipUnsupported
		}
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

// instructionBlock 是指令级钩子的正文。
//
// 除了「告诉模型有这么一个库」，还必须给出**容量纪律**：
// 会话级文件每轮注入且有上限，若允许把细节写进去，它必然再度膨胀、
// 再度被截断、再度需要人工压缩 —— 这正是要解决的问题本身。
func instructionBlock(_ string) string {
	lines := []string{
		instrBegin,
		"## 分层记忆（v2mem · 由 `mem init` 生成）",
		"",
		"本机有一个跨会话的本地记忆库（Level 2），正文不进上下文，按需检索。",
		"",
		"### 容量纪律（防止本文件再度膨胀）",
		"",
		"本文件每轮注入且有容量上限，灌满即被截断。因此只留三样：跨工程硬规则、",
		"当前任务态、本段钩子；细节 / 踩坑 / 数据 / 历史决策一律写进记忆库。",
		"",
		"- 查历史决策 / 踩坑 / 约定：",
		"  `mem search --scope current --json \"<关键词>\"`",
		"- 记下新的原子事实（一条只写一个事实，不写长段落）：",
		"  `mem add --kind decision|pitfall|preference|fact \"<原子事实>\"`",
		"- 把既有 md 文件的条目一次性搬进库：`mem ingest --project <工程> <文件>`",
		"- 用上了某条记忆后反馈：`mem touch <id前8位>`",
		"",
		"### 任务收尾：输出 mem 评测结论（临时约定）",
		"",
		"每次任务结束时，在收尾清单里**单独给一段** mem 评测结论，取自：",
		"",
		"    mem report --since <本次任务时长，如 2h>",
		"",
		"写清三行：① 读/写两侧活动次数 ② 有无空命中（列出查询原文）③ 这次记忆库到底起没起作用。",
		"结论分四种且含义不同，**不要合并成一句「有用」**：",
		"只查未记 / 只记未查 / 两侧都有活动 / 未使用记忆库。",
		"",
		"> 这是**临时评测约定**，为判断 v2mem 是否值得继续投入而设。",
		"> 达标判据：连续 10 次任务里多数为「两侧都有活动」且空命中率 <30%；达标后本段移除。",
		"",
		"不要凭空断言历史决策，不确定就先 `mem search`；查不到再问用户。",
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
