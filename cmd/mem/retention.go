package main

// S13「保留与终态出口」：文件侧出口与 mem gc --retention 入口。
//
// 口径来自 docs/design-data-lifecycle.md §2.2 终态保留表、§8.1 顺序铁律、
// §8.2 各类出口表。只清终态、只清过保留期；--dry-run 必须零写；物理删之前
// 必须确认备份有效（cmdGC 的备份前置 + 本文件 sweepRetentionFiles 的函数级守卫）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

// 文件侧保留口径（常量 + 注释，不许散落魔法数）：
//   - PlansAge 与候选/决策账的 90 天对齐：plans/*.jsonl 是批量裁决的回滚凭证，
//     凭证活得和它要回滚的账一样久（§2.2）。
//   - BackupsKeep / BackupsAge：库快照按代数 + 年龄双条件轮转；归档备份目录按年龄。
//   - SpawnMaxBytes 与 audit.MaxArchiveBytes 同一量级（5MiB），避免一处 5MiB、一处无限。
//   - SpoolTmpAge 24h：seg-*.tmp 是「写了一半」的临时段，24h 未转成 .jsonl 即视为死段。
//   - CursorAge 30 天：cursor 年龄线；再叠加「transcript 已不存在 / 已完全消费」
//     才删除（见 sweepRetentionFiles 第 (5) 步）。
const (
	plansRetention    = 90 * 24 * time.Hour
	backupsKeep       = 3
	backupsRetention  = 30 * 24 * time.Hour
	spawnRotateBytes  = 5 << 20
	spoolTmpRetention = 24 * time.Hour
	cursorReportAge   = 30 * 24 * time.Hour
)

// fileRetentionConfig 是文件侧保留口径，全部有默认值。
type fileRetentionConfig struct {
	PlansAge      time.Duration // plans/*.jsonl 回滚凭证，与候选/决策账一致的 90 天
	BackupsKeep   int           // backup/ 保留代数
	BackupsAge    time.Duration // backup/ 超过年龄也回收
	SpawnMaxBytes int64         // spawn.log 轮转阈值
	SpoolTmpAge   time.Duration // spool seg-*.tmp 安全回收年龄
	CursorAge     time.Duration // transcript cursor 终态回收年龄线（30 天 + 完全消费）
}

func defaultFileRetentionConfig() fileRetentionConfig {
	return fileRetentionConfig{
		PlansAge:      plansRetention,
		BackupsKeep:   backupsKeep,
		BackupsAge:    backupsRetention,
		SpawnMaxBytes: spawnRotateBytes,
		SpoolTmpAge:   spoolTmpRetention,
		CursorAge:     cursorReportAge,
	}
}

// fileSweepReport 是一次文件侧回收的预览/结果计数。
type fileSweepReport struct {
	Plans          int `json:"plans"`
	Backups        int `json:"backups"`
	SpawnRotated   int `json:"spawn_rotated"`
	SpoolTmp       int `json:"spool_tmp"`
	CursorsDeleted int `json:"cursors_deleted"`
	CursorsListed  int `json:"cursors_listed"`
}

// olderThan 判断文件 mtime 是否早于 now-age。保留期未配置（<=0）时一律不删，
// 宁可保守；stat 不到的文件也跳过，不让一个坏条目毁掉整轮扫描。
func olderThan(path string, now time.Time, age time.Duration) bool {
	if age <= 0 {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.ModTime().Before(now.Add(-age))
}

// sweepRetentionFiles 扫描并（在非 dry-run 且备份有效时）回收文件侧终态出口。
//
// 先只读扫描、把待删项收进列表，最后统一执行：dry-run 天然零写，实删也不会
// 边遍历边删。§8.1 守卫放在最前面，备份未确认有效时一个字节都不碰。
func sweepRetentionFiles(root string, now time.Time, cfg fileRetentionConfig, dryRun, backupReady bool) (*fileSweepReport, error) {
	// §8.1：破坏性 GC 前必须确认备份有效；--dry-run 零写，不受此限。
	if !dryRun && !backupReady {
		return nil, errors.New("§8.1：备份未确认有效，拒绝物理删（先做库备份再执行 mem gc --retention）")
	}

	rep := &fileSweepReport{}
	var toDelete []string     // 待删普通文件
	var toDeleteDirs []string // 待删目录（backup/ 下的 evidence-gc-*）
	rotateSpawn := false

	// (1) plans/*.jsonl：批量裁决的回滚凭证，按与候选一致的 90 天回收。
	planDir := filepath.Join(root, "plans")
	planEntries, err := readDirIfExists(planDir)
	if err != nil {
		return nil, err
	}
	for _, e := range planEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(planDir, e.Name())
		if olderThan(p, now, cfg.PlansAge) {
			rep.Plans++
			toDelete = append(toDelete, p)
		}
	}

	// (2) backup/：库快照 mem-*.db 按「代数 + 年龄」双条件轮转；归档备份目录
	//     evidence-gc-* 按年龄回收。最新一代永不被代数规则挤掉（它就是刚做的备份）。
	backupDir := filepath.Join(root, "backup")
	backupEntries, err := readDirIfExists(backupDir)
	if err != nil {
		return nil, err
	}
	type snapshot struct {
		path string
		mod  time.Time
	}
	var snaps []snapshot
	for _, e := range backupEntries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "mem-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, snapshot{filepath.Join(backupDir, name), fi.ModTime()})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].mod.After(snaps[j].mod) })
	for i, s := range snaps {
		byGen := cfg.BackupsKeep > 0 && i >= cfg.BackupsKeep
		byAge := cfg.BackupsAge > 0 && s.mod.Before(now.Add(-cfg.BackupsAge))
		if byGen || byAge {
			rep.Backups++
			toDelete = append(toDelete, s.path)
		}
	}
	for _, e := range backupEntries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "evidence-gc-") {
			continue
		}
		p := filepath.Join(backupDir, e.Name())
		if olderThan(p, now, cfg.BackupsAge) {
			rep.Backups++
			toDeleteDirs = append(toDeleteDirs, p)
		}
	}

	// (3) spawn.log：超过体量阈值就轮转为 .1（写入侧 openSpawnLog 直接 append，
	//     重建空当前日志维持「当前日志恒存在」）。
	spawnPath := filepath.Join(root, "spawn.log")
	if fi, err := os.Stat(spawnPath); err == nil && cfg.SpawnMaxBytes > 0 && fi.Size() > cfg.SpawnMaxBytes {
		rotateSpawn = true
		rep.SpawnRotated = 1
	}

	// (4) spool/seg-*.tmp：只回收「半截段」临时文件；完整 .jsonl / .processing
	//     与保护期内的 .tmp 一律不动。递归到 spool/<session>/ 子目录。
	spoolRoot := filepath.Join(root, "spool")
	_ = filepath.WalkDir(spoolRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // 不存在/不可读的分支跳过，不让一个坏目录毁掉整轮
		}
		name := d.Name()
		if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".tmp") {
			return nil
		}
		if olderThan(path, now, cfg.SpoolTmpAge) {
			rep.SpoolTmp++
			toDelete = append(toDelete, path)
		}
		return nil
	})

	// (5) evidence/transcripts/*.cursor：游标只有 Path/Offset/Tools，没有显式「会话
	//     终态」字段。用「文件年龄 + transcript 消费状态」判定终态：超过 CursorAge
	//     且（transcript 已不存在，或 stat.Size() <= Offset 即完全消费）才回收。
	//     30 天远大于任何活动会话；Size > Offset（有未读余量）或文件不可读时保留，
	//     只删明确终态，绝不猜着删。dry-run 下同样只计数（下面统一跳过物理删）。
	cursorRoot := filepath.Join(root, "evidence", "transcripts")
	_ = filepath.WalkDir(cursorRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".cursor") || !olderThan(path, now, cfg.CursorAge) {
			return nil
		}
		rep.CursorsListed++
		if cursorReclaimable(path) {
			rep.CursorsDeleted++
			toDelete = append(toDelete, path)
		}
		return nil
	})

	// --dry-run：报告已就绪，直接返回，盘上一个字节都不改。
	if dryRun {
		return rep, nil
	}

	for _, p := range toDelete {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return rep, err
		}
	}
	for _, p := range toDeleteDirs {
		if err := os.RemoveAll(p); err != nil {
			return rep, err
		}
	}
	if rotateSpawn {
		if err := rotateSpawnLog(spawnPath); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// cursorReclaimable 判断超龄 cursor 是否已到终态：transcript 不存在，或已被
// 完全消费（Size <= Offset）。坏 JSON、读不到 cursor、transcript 不可读时一律
// 返回 false（保留）——判不准就不删。
func cursorReclaimable(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cur transcriptCursor
	if err := json.Unmarshal(b, &cur); err != nil {
		return false
	}
	fi, err := os.Stat(cur.Path)
	if err != nil {
		return os.IsNotExist(err) // 只有「确实不存在」算终态；权限等不可读保留
	}
	return fi.Size() <= cur.Offset
}

// readDirIfExists 读目录；目录不存在视为空，不报错（新库还没建 plans/backup 是常态）。
func readDirIfExists(dir string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// rotateSpawnLog 把当前 spawn.log 轮转为 .1 并重建空文件（与 audit.rotate 同口径）。
func rotateSpawnLog(path string) error {
	if err := os.Remove(path + ".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// dbRetentionReport 是库侧三个回收方法的结果汇总。
type dbRetentionReport struct {
	Candidates *store.CandGCResult     `json:"candidates"`
	Bundles    *store.BundleGCResult   `json:"bundles"`
	Decisions  *store.DecisionGCResult `json:"decisions"`
}

// cmdGCRetention 是 mem gc --retention 的执行体。
//
// dry-run：只做文件侧预览，库侧三个方法没有预览模式，一律跳过（零写）。
// 实删：cmdGC 只在备份前置通过（或 --no-backup 显式豁免，既有逃生口）后才派发
// 到这里，故非 dry-run 视为 §8.1 已满足；backupRoot 非空只是「真做过备份」的凭证。
func cmdGCRetention(c common, dryRun bool, backupRoot string) error {
	cfg := defaultFileRetentionConfig()
	dbPath := c.db
	if dbPath == "" {
		dbPath = store.DefaultPath()
	}
	root := filepath.Dir(dbPath)

	if dryRun {
		rep, err := sweepRetentionFiles(root, time.Now(), cfg, true, false)
		if err != nil {
			return err
		}
		return printRetentionReport(c, rep, nil, true, backupRoot)
	}

	// 顺序按 §8.1「全量引用保护」：先跑文件侧/库侧里引用判断最全的 Bundle，
	// 再 GC 候选，最后 GC 决策账（它要求「候选已不存在」，必须排在候选之后）。
	rep, err := sweepRetentionFiles(root, time.Now(), cfg, false, true)
	if err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	bundles, err := st.GCTerminalBundles(0)
	if err != nil {
		return err
	}
	cand, err := st.GCCandidates(14*24*time.Hour, 90*24*time.Hour)
	if err != nil {
		return err
	}
	decisions, err := st.GCCommitDecisions(0)
	if err != nil {
		return err
	}
	return printRetentionReport(c, rep, &dbRetentionReport{
		Candidates: cand, Bundles: bundles, Decisions: decisions,
	}, false, backupRoot)
}

// printRetentionReport 输出文件侧 + 库侧的预览/结果计数。
func printRetentionReport(c common, rep *fileSweepReport, db *dbRetentionReport, dryRun bool, backupRoot string) error {
	if c.json {
		return printJSON(struct {
			DryRun     bool               `json:"dry_run"`
			BackupRoot string             `json:"backup_root,omitempty"`
			Files      *fileSweepReport   `json:"files"`
			DB         *dbRetentionReport `json:"db,omitempty"`
		}{dryRun, backupRoot, rep, db})
	}
	if dryRun {
		fmt.Println("dry-run 预览（零写：不删文件、不写库、不生成备份快照）")
	} else {
		shown := backupRoot
		if shown == "" {
			shown = "（--no-backup 逃生口）"
		}
		fmt.Printf("备份根：%s\n", shown)
	}
	fmt.Printf("文件侧：plans %d，backup %d，spawn.log 轮转 %d，spool .tmp %d，cursor 报数 %d（删除 %d）\n",
		rep.Plans, rep.Backups, rep.SpawnRotated, rep.SpoolTmp, rep.CursorsListed, rep.CursorsDeleted)
	if db == nil {
		fmt.Println("库侧：dry-run 跳过（GCCandidates / GCTerminalBundles / GCCommitDecisions 无预览模式）")
		return nil
	}
	fmt.Printf("库侧：候选 过期%d/删已采纳%d/删过期%d/删已拒%d，完成 Bundle %d（items %d，引用保护跳过 %d），孤立决策 %d\n",
		db.Candidates.Expired, db.Candidates.Deleted, db.Candidates.DeletedExpired, db.Candidates.DeletedRejected,
		db.Bundles.Bundles, db.Bundles.Items, db.Bundles.SkippedReferenced, db.Decisions.Deleted)
	return nil
}
