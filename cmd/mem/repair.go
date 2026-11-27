package main

// mem repair —— 数据面修复入口。
//
// 当前只有 evidence 一个子命令：给记忆补证据链。为什么需要单独入口：
// 历史缺口（规则蒸馏路径的候选只写 obs_ids_json）已经落进库里，代码修好只
// 保证新数据不再断链，存量得显式修一次；批量/全量形态让换机、导入旧库后
// 也能一条命令收敛。

import (
	"flag"
	"fmt"
	"strings"

	"github.com/v2camp/v2mem/internal/store"
)

// repairAllThreshold 是「无参数时是否直接全量」的规模上限。
//
// 少于这个数量，全量修复的代价和误操作面都可忽略，直接执行更省一步；
// 达到这个数量，全量可能跑很久且会改动大量行，要求显式 --all 表达意图。
const repairAllThreshold = 100

func cmdRepair(args []string) error {
	if len(args) == 0 {
		return usageError{msg: "缺少子命令，用法: mem repair evidence|content [选项]；详见 mem repair help"}
	}
	switch args[0] {
	case "evidence":
		return cmdRepairEvidence(args[1:])
	case "content":
		return cmdRepairContent(args[1:])
	case "help", "-h", "--help":
		fmt.Println("用法: mem repair <子命令> [选项]")
		fmt.Println("  evidence [id...] [--all] [--dry-run] [--json]")
		fmt.Println("      为记忆补证据链（幂等）：传 id 修单条或批量，--all 修全部；")
		fmt.Printf("      不带参数时，库内少于 %d 条记忆按全量执行，否则要求显式 --all。\n", repairAllThreshold)
		fmt.Println("      --dry-run 只报告哪些记忆待修复，不写库。")
		fmt.Println("  content [id...] [--filter=narrative|overlong|all] [--dry-run|--preview] [--json]")
		fmt.Println("      用模型把过程叙述提炼成「触发场景 → 判断依据 → 处置」；")
		fmt.Println("      --dry-run 只列候选；--preview 出 before/after；默认改写并归档到 <db 目录>/enhance/。")
		fmt.Println("      --rollback <tag|file> [--dry-run] 按归档逐条校验后还原。")
		return nil
	default:
		return usageError{msg: fmt.Sprintf("未知子命令 %q，用法见 mem repair help", args[0])}
	}
}

func cmdRepairEvidence(args []string) error {
	var c common
	fs := flag.NewFlagSet("repair evidence", flag.ContinueOnError)
	c.register(fs)
	all := fs.Bool("all", false, "修复全部记忆")
	dry := fs.Bool("dry-run", false, "只报告，不写库")
	positional, flagArgs := splitLeadingArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	ids := make([]string, 0, len(positional)+len(fs.Args()))
	for _, id := range append(positional, fs.Args()...) {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	if len(ids) > 0 {
		if *all {
			return usageError{msg: "--all 与具体 id 不能同时使用"}
		}
		rep := repairByIDs(st, ids, *dry)
		if err := renderRepairReport(rep, c.json, *dry); err != nil {
			return err
		}
		if rep.Failed > 0 {
			return fmt.Errorf("%d 条记忆修复失败", rep.Failed)
		}
		return nil
	}

	// 无 id：先探规模，再决定是否直接全量。
	probe, err := st.RepairAllMemoryEvidence(true)
	if err != nil {
		return err
	}
	if probe.Scanned == 0 {
		if c.json {
			return printJSON(probe)
		}
		fmt.Println("库内没有记忆，无需修复")
		return nil
	}
	if !*all && probe.Scanned >= repairAllThreshold {
		return usageError{msg: fmt.Sprintf(
			"库内 %d 条记忆，达到批量阈值 %d，全量修复需显式 --all（或传入具体 id）",
			probe.Scanned, repairAllThreshold)}
	}
	if !*all && !c.json {
		fmt.Printf("库内 %d 条记忆（少于 %d），按全量执行\n", probe.Scanned, repairAllThreshold)
	}
	if *dry {
		return renderRepairReport(probe, c.json, true)
	}
	rep, err := st.RepairAllMemoryEvidence(false)
	if err != nil {
		return err
	}
	if err := renderRepairReport(rep, c.json, false); err != nil {
		return err
	}
	if rep.Failed > 0 {
		return fmt.Errorf("%d 条记忆修复失败", rep.Failed)
	}
	return nil
}

// repairByIDs 逐条修复：失败只记账，不阻断后面的 id。
func repairByIDs(st *store.Store, ids []string, dry bool) *store.MemoryRepairReport {
	rep := &store.MemoryRepairReport{Scanned: len(ids), DryRun: dry}
	for _, id := range ids {
		if dry {
			needs, err := st.MemoryNeedsEvidenceRepair(id)
			switch {
			case err != nil:
				rep.Failed++
				rep.Items = append(rep.Items, store.MemoryRepairItem{MemoryID: id, Status: "failed", Error: err.Error()})
			case needs:
				rep.Items = append(rep.Items, store.MemoryRepairItem{MemoryID: id, Status: "pending"})
			default:
				rep.Skipped++
			}
			continue
		}
		n, err := st.RepairMemoryEvidence(id)
		switch {
		case err != nil:
			rep.Failed++
			rep.Items = append(rep.Items, store.MemoryRepairItem{MemoryID: id, Status: "failed", Error: err.Error()})
		case n > 0:
			rep.Repaired++
			rep.AddedEvidence += n
			rep.Items = append(rep.Items, store.MemoryRepairItem{MemoryID: id, Status: "repaired", AddedEvidence: n})
		default:
			rep.Skipped++
		}
	}
	return rep
}

func renderRepairReport(rep *store.MemoryRepairReport, jsonOut, dry bool) error {
	if jsonOut {
		return printJSON(rep)
	}
	if dry {
		pending := 0
		for _, it := range rep.Items {
			if it.Status == "pending" {
				pending++
			}
		}
		fmt.Printf("预演（不写库）：扫描 %d 条，待修复 %d 条，跳过 %d，失败 %d\n",
			rep.Scanned, pending, rep.Skipped, rep.Failed)
	} else {
		fmt.Printf("修复证据链：扫描 %d 条，修复 %d 条（新增 %d 条证据），跳过 %d，失败 %d\n",
			rep.Scanned, rep.Repaired, rep.AddedEvidence, rep.Skipped, rep.Failed)
	}
	for _, it := range rep.Items {
		switch it.Status {
		case "pending":
			fmt.Printf("  %s  待修复\n", shortID(it.MemoryID))
		case "repaired":
			fmt.Printf("  %s  +%d 条证据\n", shortID(it.MemoryID), it.AddedEvidence)
		case "failed":
			fmt.Printf("  %s  失败: %s\n", shortID(it.MemoryID), it.Error)
		}
	}
	return nil
}
