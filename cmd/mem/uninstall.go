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

// 本文件实现 `mem uninstall`——init 的逆操作：
// 把之前注入各工具的 v2mem 钩子（hooks 配置 / 指令块）完整移除。
//
// 沿用 init 的三条纪律：
//  1. **合并，不覆盖**。只摘掉带 v2mem 标记的条目，用户自己的 hook 与
//     无关配置键原样保留；解析不了就报错退出，绝不拿默认值盖掉用户文件。
//  2. **幂等**。重复执行不报错、也不产生副作用。
//  3. **不动用户额外的文件**。目标文件不存在即跳过并报告，绝不新建。

// cmdUninstall 卸载钩子。与 init 对称：--harness 指定工具，--all 全部处理。
func cmdUninstall(args []string) error {
	var c common
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	c.register(fs)
	harnessFlag := fs.String("harness", "", "要卸载的工具，逗号分隔")
	all := fs.Bool("all", false, "处理全部工具")
	project := fs.String("project", "", "该目录的项目级配置（默认用户级）")
	scope := fs.String("scope", "auto", "auto|global|project|instruction")
	dryRun := fs.Bool("dry-run", false, "只显示将移除的内容，不落盘")
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
		project: *project, scope: *scope, fileFlag: *file, dryRun: *dryRun,
	}

	names := splitList(*harnessFlag)
	switch {
	case len(names) > 0 && *all:
		return errors.New("--harness 与 --all 不可同时使用")
	case len(names) == 0 && !*all:
		return errors.New("请指定要卸载的工具：--harness <名字> 或 --all")
	case *all:
		for _, h := range harness.All() {
			names = append(names, h.Name)
		}
	}

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
		if err := uninstallOne(h, opts); err != nil {
			if errors.Is(err, errSkipUnsupported) {
				skipped = append(skipped, name)
				continue
			}
			return err
		}
	}
	if len(skipped) > 0 {
		fmt.Printf("\n跳过 %d 个（当前范围下没有可卸载位置）：%s\n", len(skipped), strings.Join(skipped, "、"))
	}
	return nil
}

// uninstallTargets 决定从哪些文件卸载，规则与 init 的 resolveConfigTarget 对称，
// 保证「装到哪就从哪卸」。
func uninstallTargets(h *harness.Harness, opts initOptions) []string {
	if strings.TrimSpace(opts.fileFlag) != "" {
		return []string{filepath.Clean(opts.fileFlag)}
	}
	instruction := opts.scope == "instruction" ||
		(h.Tier == harness.TierInstruction && opts.scope == "auto")
	if instruction {
		rel := "AGENTS.md"
		if len(h.Instruction) > 0 {
			rel = h.Instruction[0]
		}
		if strings.HasPrefix(rel, "~") {
			return []string{harness.ExpandHome(rel)}
		}
		if opts.project != "" {
			return []string{filepath.Join(opts.project, rel)}
		}
		return nil
	}
	if opts.scope == "project" || (opts.project != "" && len(h.ProjectConfig) > 0) {
		if len(h.ProjectConfig) > 0 {
			return []string{filepath.Join(opts.project, h.ProjectConfig[0])}
		}
	}
	if p, _ := h.ResolveGlobalConfig(); p != "" {
		return []string{p}
	}
	return nil
}

// uninstallOne 处理单个工具：对每个目标文件尝试摘除 v2mem 内容。
func uninstallOne(h *harness.Harness, opts initOptions) error {
	targets := uninstallTargets(h, opts)
	if len(targets) == 0 {
		if opts.skipNoPos {
			return errSkipUnsupported
		}
		return fmt.Errorf("%s 在当前范围下没有可卸载的位置；请用 --project、--scope 或 --file 指定", h.Name)
	}

	fmt.Printf("harness : %s\n", h.LabelOrName())
	for _, target := range targets {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			fmt.Printf("文件    : %s（未找到，跳过）\n", target)
			continue
		}
		instruction := parseConfigTarget(target, h, opts.scope)
		changed, err := uninstallAt(target, instruction, opts.dryRun)
		if err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
		reportUninstallResult(opts.dryRun, changed, target)
	}
	return nil
}

// uninstallAt 按文件语义执行移除：指令块走 stripMarkedBlock，否则走 hooks 配置。
func uninstallAt(target string, instruction, dryRun bool) (changed bool, err error) {
	if instruction {
		return stripMarkedBlock(target, dryRun)
	}
	return stripHooksConfig(target, dryRun)
}

func reportUninstallResult(dryRun, changed bool, target string) {
	switch {
	case dryRun:
		fmt.Printf("模式    : --dry-run，未写盘\n")
	case changed:
		fmt.Printf("文件    : %s（已移除 v2mem 内容；原文件已备份为 <文件>.v2mem.bak）\n", target)
	default:
		fmt.Printf("文件    : %s（无 v2mem 内容，未改动）\n", target)
	}
}

// stripHooksConfig 从 hooks 配置里摘除所有 v2mem 钩子，返回是否发生了改动。
func stripHooksConfig(path string, dryRun bool) (changed bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var cfg map[string]any
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			// 绝不拿默认值盖掉解析不了的用户文件
			return false, fmt.Errorf("现有配置文件不是合法 JSON，已中止以免破坏：%w", err)
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	if !stripOursHooks(cfg) {
		return false, nil
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')

	if dryRun {
		fmt.Printf("--- 将移除 %s 中的 v2mem 钩子 ---\n%s", path, out)
		return false, nil
	}
	if string(raw) == string(out) {
		return false, nil
	}
	if err := backupOnce(path, raw); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, out, 0o644)
}

// stripOursHooks 遍历所有事件，逐条移除带 v2mem 标记的 hook；
// 组内清空则丢弃整组，事件内无剩余组则删除该事件键。
func stripOursHooks(cfg map[string]any) bool {
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		return false
	}
	changed := false
	for ev, v := range hooks {
		groups, _ := v.([]any)
		kept := make([]any, 0, len(groups))
		for _, g := range groups {
			gm, ok := g.(map[string]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			hs, _ := gm["hooks"].([]any)
			var keptHooks []any
			groupChanged := false
			for _, h := range hs {
				hm, ok := h.(map[string]any)
				if !ok {
					keptHooks = append(keptHooks, h)
					continue
				}
				if c, ok := hm["command"].(string); ok && isV2memHookCommand(c) {
					changed, groupChanged = true, true
					continue
				}
				keptHooks = append(keptHooks, h)
			}
			if groupChanged {
				if len(keptHooks) == 0 {
					continue // 整组仅由 v2mem 钩子构成，整体移除
				}
				gm["hooks"] = keptHooks
			}
			kept = append(kept, gm)
		}
		if len(kept) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = kept
		}
	}
	return changed
}

// stripMarkedBlock 从文件中移除 v2mem 指令块（instrBegin..instrEnd），保留其余内容。
func stripMarkedBlock(path string, dryRun bool) (changed bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	s := string(raw)
	begin := strings.Index(s, instrBegin)
	if begin < 0 {
		return false, nil
	}
	end := strings.Index(s[begin:], instrEnd)
	if end < 0 {
		return false, fmt.Errorf("有起始标记但缺少结束标记，可能被手工改坏，已中止")
	}
	end += begin

	next := s[:begin] + s[end+len(instrEnd):]
	next = strings.TrimLeft(next, "\n")
	next = strings.TrimRight(next, "\n")
	if next != "" {
		next += "\n"
	}

	if dryRun {
		fmt.Printf("--- 将移除 %s 中的 v2mem 指令块 ---\n", path)
		return false, nil
	}
	if next == s {
		return false, nil
	}
	if err := backupOnce(path, raw); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(next), 0o644)
}