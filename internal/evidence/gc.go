package evidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

// archiveFileSuffix 是归档段文件的扩展名；文件名本身（去掉它）就是 archive_hash。
const archiveFileSuffix = ".jsonl.gz"

// EvidenceGCResult 报告一次证据归档准出的结果。
//
// 字段分成三组，对应 gc 的三件事：账本内孤儿段（Segments/RawEvents/Bytes）、
// 账本外残渣（Untracked/TempFiles）、空转证据源（Sources）。
// Failed 只统计「文件删不掉」的段——它们留在账本里，下轮重试（仅无备份逃生口会出现）。
// BackupDir / BackedUp 只在做了文件级备份时出现（omitempty：旧输出不变）。
type EvidenceGCResult struct {
	Segments  int   `json:"segments"`
	RawEvents int   `json:"raw_events"`
	Bytes     int64 `json:"bytes"`
	Untracked int   `json:"untracked"`
	TempFiles int   `json:"temp_files"`
	Sources   int   `json:"sources"`
	Failed    int   `json:"failed"`

	BackupDir string `json:"backup_dir,omitempty"`
	BackedUp  int    `json:"backed_up,omitempty"`
}

// GCEvidence 回收「已经没有任何引用指向」的证据归档，返回本次释放的量。
//
// 保留原签名：不带文件级备份（测试与旧调用方零改动）。生产入口走
// GCEvidenceBackup（cmd/mem 的 gc 一律传备份根）。
func (e *Engine) GCEvidence(cutoff int64) (*EvidenceGCResult, error) {
	return e.GCEvidenceBackup(cutoff, "")
}

// GCEvidenceBackup 是 GCEvidence 的备份版：backupRoot 非空时，把本轮要从归档目录
// 移走的所有文件（账本内孤儿段 + 账本外残段）移进
// `<backupRoot>/evidence-gc-<时间戳>/` 并写 manifest.json，全部落位后才删账本行。
//
// cutoff 是写入保护期（unix 秒，只回收 created_at/modtime 早于它的东西）。
// 调用方传 time.Now().Add(-grace).Unix()；保护期的存在理由见 UnreferencedArchives。
//
// 四件事，顺序不可换：
//  0. 保留集回读校验（§8.1 顺序铁律）：仍被引用保留的段必须先用 VerifyRef 证明
//     读得回。任一读回失败 → 整体拒绝删除（返回错误且一个文件都不删）——
//     坏的保留集意味着库况已超出自动 GC 的理解范围，删除只会让损失扩大。
//  1. 全部 move（含账本外残段）→ 写 manifest：备份没落盘前不碰账本；
//     任一步失败回滚已移文件，账本不动。跨盘（EXDEV）走 copy+fsync+sha256+删源。
//  2. 删账本行：文件已经在备份里，账本是最后一个索引，必须最后收口。
//     反过来的话文件移动失败就会留下「账本说没有、磁盘还在/备份里」的不一致。
//  3. .seg-*.tmp 是写一半的临时文件，直接删不备份（它本来就不是可读归档）。
//  4. 空转证据源：既无归档也无观测的 evidence_sources 行。
//
// 只认领得清的垃圾：名字不是「64 位 hash + .jsonl.gz」的文件一律不碰。
func (e *Engine) GCEvidenceBackup(cutoff int64, backupRoot string) (*EvidenceGCResult, error) {
	res := &EvidenceGCResult{}

	segs, err := e.st.UnreferencedArchives(cutoff)
	if err != nil {
		return res, fmt.Errorf("查孤儿归档段失败: %w", err)
	}
	keep, err := e.st.ReferencedArchives()
	if err != nil {
		return res, fmt.Errorf("查保留归档段失败: %w", err)
	}
	for _, seg := range keep {
		if _, vErr := VerifyRef(seg.Path, seg.Hash); vErr != nil {
			return res, fmt.Errorf("保留集回读校验失败，拒绝删除（%s）: %w", seg.Hash, vErr)
		}
	}

	// 只扫不删：把「账本外残段」与「写一半的临时文件」先认领出来，
	// 具体动作按备份/不备份两种口径分流。
	scan, err := e.scanArchiveDir(cutoff)
	if err != nil {
		return res, err
	}

	if backupRoot != "" {
		if err := checkBackupRoot(e.archiveDir, backupRoot); err != nil {
			return res, err
		}
		if err := e.backupAndReclaim(res, segs, scan, backupRoot); err != nil {
			return res, err
		}
	} else {
		if err := e.removeAndReclaim(res, segs, scan); err != nil {
			return res, err
		}
	}

	// 临时文件不是归档，直接删；删不掉就留在盘上，下轮再试（不计失败，与旧行为一致）。
	for _, p := range scan.temps {
		if rmErr := os.Remove(p); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			continue
		}
		res.TempFiles++
	}

	n, err := e.st.DeleteEmptyEvidenceSources(cutoff)
	if err != nil {
		return res, err
	}
	res.Sources = n
	return res, nil
}

// backupAndReclaim 走「先备份、后删账本」的路径。
func (e *Engine) backupAndReclaim(res *EvidenceGCResult, segs []store.ArchivedSegment, scan archiveScan, backupRoot string) error {
	paths := make([]string, 0, len(segs)+len(scan.untracked))
	for _, seg := range segs {
		if _, statErr := os.Stat(seg.Path); errors.Is(statErr, os.ErrNotExist) {
			// 上一轮已把文件移走、账本没删成：文件不在归档目录即已备份，账本可以收口。
			continue
		}
		paths = append(paths, seg.Path)
	}
	paths = append(paths, scan.untracked...)

	if len(paths) > 0 {
		dir, err := newBackupDir(backupRoot, time.Now())
		if err != nil {
			return err
		}
		items, err := moveToBackupDir(paths, dir)
		if err != nil {
			return err
		}
		if err := writeManifest(dir, items, time.Now()); err != nil {
			// 清单没写成 = 备份不成立：把已移出的文件按逆序搬回原位，账本一个都不动。
			for i := len(items) - 1; i >= 0; i-- {
				_ = moveFile(filepath.Join(dir, items[i].File), paths[i])
			}
			return err
		}
		res.BackupDir = dir
		res.BackedUp = len(items)
		res.Bytes = 0
		for _, it := range items {
			res.Bytes += it.Bytes
		}
		res.Untracked = len(scan.untracked)
	}

	// 账本行在文件全部落位（含清单）之后才删。
	hashes := make([]string, 0, len(segs))
	for _, seg := range segs {
		hashes = append(hashes, seg.Hash)
		res.Segments++
		res.RawEvents += seg.RawCount
	}
	if len(hashes) > 0 {
		if _, err := e.st.DeleteArchivedSegments(hashes); err != nil {
			return fmt.Errorf("删归档账本失败: %w", err)
		}
	}
	return nil
}

// removeAndReclaim 是 `--no-backup` 的逃生口：直接删文件，删不掉就整行跳过
// （账本留着，下轮再试，绝不让账本先于文件消失）。
func (e *Engine) removeAndReclaim(res *EvidenceGCResult, segs []store.ArchivedSegment, scan archiveScan) error {
	removed := make([]string, 0, len(segs))
	for _, seg := range segs {
		if rmErr := os.Remove(seg.Path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			res.Failed++
			continue
		}
		removed = append(removed, seg.Hash)
		res.Segments++
		res.RawEvents += seg.RawCount
		res.Bytes += seg.ByteSize
	}
	if len(removed) > 0 {
		if _, err := e.st.DeleteArchivedSegments(removed); err != nil {
			return fmt.Errorf("删归档账本失败: %w", err)
		}
	}
	for _, p := range scan.untracked {
		if rmErr := os.Remove(p); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			continue
		}
		res.Untracked++
	}
	return nil
}

// archiveScan 是扫归档目录的结果：只认领，不执行。
type archiveScan struct {
	untracked []string // 账本没有、但名字是归档段的文件（保护期外）
	temps     []string // 写一半的 .seg-*.tmp（保护期外）
}

// scanArchiveDir 扫归档目录，挑出账本外的残段与写一半的临时文件。
// 只读：删除与搬移由调用方按「备份 / 不备份」决定。
func (e *Engine) scanArchiveDir(cutoff int64) (archiveScan, error) {
	var out archiveScan
	entries, rerr := os.ReadDir(e.archiveDir)
	if errors.Is(rerr, os.ErrNotExist) {
		return out, nil
	}
	if rerr != nil {
		return out, fmt.Errorf("读归档目录失败: %w", rerr)
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, ierr := ent.Info()
		if ierr != nil || info.ModTime().Unix() >= cutoff {
			continue // 保护期内：可能是正在写的段
		}
		name := ent.Name()
		path := filepath.Join(e.archiveDir, name)
		switch {
		case isArchiveFileName(name):
			tracked, qerr := e.st.ArchiveExists(strings.TrimSuffix(name, archiveFileSuffix))
			if qerr != nil {
				return out, qerr
			}
			if tracked {
				continue
			}
			out.untracked = append(out.untracked, path)
		case strings.HasPrefix(name, ".seg-") && strings.HasSuffix(name, ".tmp"):
			out.temps = append(out.temps, path)
		}
	}
	return out, nil
}

// isArchiveFileName 只认「64 位小写十六进制 hash + .jsonl.gz」。
// 别的文件不碰：归档目录出现未知文件应被看见，而不是被 GC 静默当垃圾清掉。
func isArchiveFileName(name string) bool {
	if !strings.HasSuffix(name, archiveFileSuffix) {
		return false
	}
	base := strings.TrimSuffix(name, archiveFileSuffix)
	if len(base) != 64 {
		return false
	}
	for _, c := range base {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
