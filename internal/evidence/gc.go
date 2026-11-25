package evidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// archiveFileSuffix 是归档段文件的扩展名；文件名本身（去掉它）就是 archive_hash。
const archiveFileSuffix = ".jsonl.gz"

// EvidenceGCResult 报告一次证据归档准出的结果。
//
// 字段分成三组，对应 gc 的三件事：账本内孤儿段（Segments/RawEvents/Bytes）、
// 账本外残渣（Untracked/TempFiles）、空转证据源（Sources）。
// Failed 只统计「文件删不掉」的段——它们留在账本里，下轮重试。
type EvidenceGCResult struct {
	Segments  int   `json:"segments"`
	RawEvents int   `json:"raw_events"`
	Bytes     int64 `json:"bytes"`
	Untracked int   `json:"untracked"`
	TempFiles int   `json:"temp_files"`
	Sources   int   `json:"sources"`
	Failed    int   `json:"failed"`
}

// GCEvidence 回收「已经没有任何引用指向」的证据归档，返回本次释放的量。
//
// cutoff 是写入保护期（unix 秒，只回收 created_at/modtime 早于它的东西）。
// 调用方传 time.Now().Add(-grace).Unix()；保护期的存在理由见 UnreferencedArchives。
//
// 四件事，顺序不可换：
//  0. 保留集回读校验（§8.1 顺序铁律）：仍被引用保留的段必须先用 VerifyRef 证明
//     读得回。任一读回失败 → 整体拒绝删除（返回错误且一个文件都不删）——
//     坏的保留集意味着库况已超出自动 GC 的理解范围，删除只会让损失扩大。
//  1. 账本内孤儿段：先删磁盘文件，再删账本行。反过来的话文件删失败就会留下
//     「账本说没有、磁盘还在」的残渣，而账本恰恰是找残渣的唯一线索。
//  2. 账本外残文件：进程在「落盘」与「写账」之间退出留下的段，以及写归档时的
//     .seg-*.tmp。它们不在任何账本里，只能靠扫目录找。
//  3. 空转证据源：既无归档也无观测的 evidence_sources 行。
//
// 只认领得清的垃圾：名字不是「64 位 hash + .jsonl.gz」的文件一律不碰。
func (e *Engine) GCEvidence(cutoff int64) (*EvidenceGCResult, error) {
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
	removed := make([]string, 0, len(segs))
	for _, seg := range segs {
		if rmErr := os.Remove(seg.Path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			// 删不掉就整行跳过：账本留着，下轮再试，绝不让账本先于文件消失。
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
			return res, fmt.Errorf("删归档账本失败: %w", err)
		}
	}

	untracked, temps, err := e.sweepArchiveDir(cutoff)
	res.Untracked, res.TempFiles = untracked, temps
	if err != nil {
		return res, err
	}

	n, err := e.st.DeleteEmptyEvidenceSources(cutoff)
	if err != nil {
		return res, err
	}
	res.Sources = n
	return res, nil
}

// sweepArchiveDir 扫归档目录，清掉账本外的残段与写一半的临时文件。
func (e *Engine) sweepArchiveDir(cutoff int64) (untracked, temps int, err error) {
	entries, rerr := os.ReadDir(e.archiveDir)
	if errors.Is(rerr, os.ErrNotExist) {
		return 0, 0, nil
	}
	if rerr != nil {
		return 0, 0, fmt.Errorf("读归档目录失败: %w", rerr)
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
				return untracked, temps, qerr
			}
			if tracked {
				continue
			}
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				continue
			}
			untracked++
		case strings.HasPrefix(name, ".seg-") && strings.HasSuffix(name, ".tmp"):
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				continue
			}
			temps++
		}
	}
	return untracked, temps, nil
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
