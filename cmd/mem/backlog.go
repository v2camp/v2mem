package main

// S4：积压五指标的 CLI 侧（design-data-lifecycle §3.2）。
//
//	积压：pending / 未消费证据 / open Bundle / spool 段
//	隔离：candidates(quarantined|failed) + bundles(quarantined)
//	维护：maintain:last_ok（从未维护显示 null）
//
// 只做原始值展示，不判级；阈值仍由巡检的 R ID 承担。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

// statsView 把 backlog 并进 stats 的 JSON：Stats 指针嵌入保持旧键全部留在顶层，
// 巡检脚本只读 size_bytes / 总条数 / 按工程，不因多一层而失明。
type statsView struct {
	*store.Stats
	Backlog *backlogView `json:"backlog"`
}

// backlogView 是 JSON 里的 backlog 对象：store 口径 + spool 扫描结果。
type backlogView struct {
	*store.Backlog
	SpoolBacklogFiles     int    `json:"spool_backlog_files"`
	OldestSpoolBacklogAge *int64 `json:"oldest_spool_backlog_age_seconds,omitempty"`
}

// scanSpoolBacklog 扫描 <db-dir>/spool/<session>/ 下的补写段。
//
// 只认 seg-*.jsonl 与 seg-*.jsonl.processing，与 internal/evidence.isSpoolSegment 同口径；
// .tmp（写到一半）与非 seg 文件不是积压。root 不存在 → 0/零值/nil（首次运行正常），
// 其它读失败（如 root 是普通文件）→ error，把「看不见」和「没有」分开。
// oldest 为零值表示没有段。
func scanSpoolBacklog(root string) (files int, oldest time.Time, err error) {
	dirs, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("读取 spool 目录 %s: %w", root, err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		sub := filepath.Join(root, d.Name())
		entries, err := os.ReadDir(sub)
		if err != nil {
			return files, oldest, fmt.Errorf("读取 spool 会话目录 %s: %w", sub, err)
		}
		for _, f := range entries {
			if f.IsDir() || !isSpoolSegmentName(f.Name()) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				return files, oldest, fmt.Errorf("读取 spool 段 %s: %w", filepath.Join(sub, f.Name()), err)
			}
			files++
			mt := info.ModTime()
			if oldest.IsZero() || mt.Before(oldest) {
				oldest = mt
			}
		}
	}
	return files, oldest, nil
}

// isSpoolSegmentName 与 evidence.isSpoolSegment 同口径（内部包不导出，只能复刻）。
func isSpoolSegmentName(name string) bool {
	if !strings.HasPrefix(name, "seg-") {
		return false
	}
	return strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.processing")
}

// markMaintainOK 在维护命令成功返回后写水位。写失败只告警：维护动作已经完成，
// 不能因为一行元数据反过来把退出码改成失败。
func markMaintainOK(st *store.Store) {
	if err := st.MetaSet(store.MetaMaintainLastOK, strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		fmt.Fprintf(os.Stderr, "警告：记录最后维护时间失败: %v\n", err)
	}
}

// markMaintainOKPath 供已经按库路径分发的子命令使用（那里没有现成的 Store）。
func markMaintainOKPath(db string) {
	st, err := store.Open(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告：记录最后维护时间失败: %v\n", err)
		return
	}
	defer st.Close()
	markMaintainOK(st)
}

// formatAge 是面向人的粗粒度：≥48h 按天、≥1h 按小时、否则按分钟。
// 只用于展示，不参与判级。
func formatAge(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d天", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%d小时", int(d/time.Hour))
	default:
		return fmt.Sprintf("%d分钟", int(d/time.Minute))
	}
}

// printBacklog 在「按工程」之后追加三行。前缀刻意避开「总条数」「按工程」——
// 巡检脚本按这两个前缀做行解析，新行不得抢走它们。
// S6c：洞数并入「积压」行，口径是「已跳过并记录」，不是待办积压。
func printBacklog(b *store.Backlog, spoolFiles int, spoolOldestAge *int64) {
	fmt.Printf("积压     pending %d%s ｜ 未消费证据 %d 源%s ｜ open Bundle %d%s ｜ spool %d 段%s ｜ 洞 %d（序号缺失已跳过，记录在案）\n",
		b.PendingCount, ageSuffix(b.OldestPendingAge),
		b.EvidenceBacklogSources, ageSuffix(b.OldestUnconsumedEvidenceAge),
		b.OpenBundles, ageSuffix(b.OldestOpenBundleAge),
		spoolFiles, ageSuffix(spoolOldestAge),
		b.EvidenceGaps,
	)
	fmt.Printf("隔离     候选 %d ｜ Bundle %d（Bundle 隔离由重试超限自动写入（S6a）；候选隔离待 S10）\n",
		b.CandidatesIsolated, b.BundlesIsolated)
	if b.LastMaintainOKAt == nil {
		fmt.Printf("维护     从未维护（无 %s 记录）\n", store.MetaMaintainLastOK)
		return
	}
	fmt.Printf("维护     最后成功 %s前（unix %d）\n", formatAge(*b.LastMaintainOKAge), *b.LastMaintainOKAt)
}

// ageSuffix 无数据时不打印年龄，绝不把「没有」显示成「0 分钟」。
func ageSuffix(age *int64) string {
	if age == nil {
		return ""
	}
	return "（最老 " + formatAge(*age) + "）"
}
