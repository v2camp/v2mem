package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// 证据归档准出的数据面。
//
// 设计约束（docs/VALUE.md「证据不能被悬空」的同一根）：删除一段归档的唯一
// 合法理由是「已经没有任何引用指向它」。引用还在、归档没了，读侧就会显示
// 「有证据」却打不开原始记录，比没有证据更具误导性。因此这里的每一处删除
// 都以引用图反查为前置条件，绝不做「按年龄直接删段」。
//
// 与 GCObservations 的配合：先回收观测（--obs），被删观测对应的归档段下一轮
// 才成为孤儿（--evidence）。两步分开是有意的——中间态可观察，也能只跑一步。

// ArchivedSegment 是证据归档账本里的一行（只取准出所需字段）。
// 刻意不带 raw_hash_concat 等大字段：这个结构只用于「删不删」的判断与统计。
type ArchivedSegment struct {
	Hash     string `json:"archive_hash"`
	Path     string `json:"path"`
	RawCount int    `json:"raw_count"`
	ByteSize int64  `json:"byte_size"`
}

// archiveRefGuard 是「全量引用保护」的红线口径（design-data-lifecycle §8.1）：
// 对归档段 a，以下任一成立即「有引用」，绝不进回收集合。
//
//  1. observations.archive_hash —— canonical 观测仍指向它（现状口径，读侧
//     「有证据」的直接依据）；
//  2. memory_evidence.archive_hash —— 最终记忆的证据链。跨设备同步来的行本机
//     可能没有对应观测，archive_hash 是唯一锚点；
//  3. candidate_evidence.archive_hash —— 候选（含长期 pending）的证据链；
//  4. Bundle 引用 —— evidence_bundle_items 引用的 evidence_id 对应观测的
//     archive_hash；观测行已被回收时 evidence_id join 不到观测，改由 Bundle
//     窗口序号范围 (from_seq_exclusive, to_seq_inclusive] 兜底。完成 Bundle 的
//     保留回收是后续切片的事，本切片宁可多保护；
//  5. 未 Bundle 化活跃序号范围 —— per-source seq ∈ (cursor_seq, last_seq] 尚未
//     发窗。观测行可能已被 GCObservations 按年龄回收，但这些序号还没发过
//     Bundle，段是这些事件仅存的原始字节。判定落在归档自身的 [from_seq, to_seq]
//     与该范围相交上，观测行在不在都不漏。
//
// 口径是红线：放宽任何一条都等于允许自动 GC 挖断还在用的证据链。
const archiveRefGuard = `EXISTS (SELECT 1 FROM observations o WHERE o.archive_hash = a.archive_hash)
	OR EXISTS (SELECT 1 FROM memory_evidence m WHERE m.archive_hash = a.archive_hash)
	OR EXISTS (SELECT 1 FROM candidate_evidence c WHERE c.archive_hash = a.archive_hash)
	OR EXISTS (SELECT 1 FROM evidence_bundle_items i
	            JOIN observations o ON o.id = i.evidence_id
	            WHERE o.archive_hash = a.archive_hash)
	OR EXISTS (SELECT 1 FROM evidence_bundles b
	            WHERE b.source_id = a.source_id
	              AND b.generation = a.generation
	              AND a.from_seq <= b.to_seq_inclusive
	              AND a.to_seq > b.from_seq_exclusive)
	OR EXISTS (SELECT 1 FROM evidence_sources s
	            WHERE s.id = a.source_id
	              AND s.generation = a.generation
	              AND a.from_seq <= s.last_seq
	              AND a.to_seq > s.cursor_seq)`

// UnreferencedArchives 列出没有任何引用指向、且创建时间早于 cutoff 的归档段。
//
// cutoff 是写入保护期（unix 秒）：真实捕获顺序是「先落归档文件，再写账」，
// 中间那一瞬段还没有观测指向它。没有保护期就会误删正在提交的证据。
// 引用判定见 archiveRefGuard（红线口径，一处收口）。
func (s *Store) UnreferencedArchives(cutoff int64) ([]ArchivedSegment, error) {
	rows, err := s.db.Query(`
		SELECT a.archive_hash, a.path, a.raw_count, a.byte_size
		  FROM evidence_archives a
		 WHERE a.created_at < ?
		   AND NOT (`+archiveRefGuard+`)
		 ORDER BY a.created_at ASC, a.archive_hash ASC`, cutoff)
	if err != nil {
		return nil, err
	}
	return scanArchivedSegments(rows)
}

// ReferencedArchives 列出仍被引用保护的归档段（UnreferencedArchives 的引用侧补集）。
//
// 不带写入保护期过滤：cutoff 只拦「回收」，不改变「是否被引用」。
// 用途是 GCEvidence 删除前的保留集回读校验——留下的段必须先证明读得回，
// 证明不了就整轮拒删（§8.1 顺序铁律）。
func (s *Store) ReferencedArchives() ([]ArchivedSegment, error) {
	rows, err := s.db.Query(`
		SELECT a.archive_hash, a.path, a.raw_count, a.byte_size
		  FROM evidence_archives a
		 WHERE ` + archiveRefGuard + `
		 ORDER BY a.created_at ASC, a.archive_hash ASC`)
	if err != nil {
		return nil, err
	}
	return scanArchivedSegments(rows)
}

func scanArchivedSegments(rows *sql.Rows) ([]ArchivedSegment, error) {
	defer rows.Close()
	var out []ArchivedSegment
	for rows.Next() {
		var seg ArchivedSegment
		if err := rows.Scan(&seg.Hash, &seg.Path, &seg.RawCount, &seg.ByteSize); err != nil {
			return nil, err
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

// DeleteArchivedSegments 按 archive_hash 删除账本行，返回实际删除的行数。
//
// 只删账本，不动磁盘文件：文件删除由 internal/evidence 负责（它才知道归档目录），
// 顺序也由它保证「先删文件、再删账本」——反过来会出现「账本说没有、文件还在」
// 的残渣，下次只能靠扫目录找回来。
func (s *Store) DeleteArchivedSegments(hashes []string) (int, error) {
	if len(hashes) == 0 {
		return 0, nil
	}
	const batch = 500 // 留在 SQLite 参数上限（默认 999）以内
	total := 0
	for start := 0; start < len(hashes); start += batch {
		end := start + batch
		if end > len(hashes) {
			end = len(hashes)
		}
		chunk := hashes[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, h := range chunk {
			ph[i] = "?"
			args[i] = h
		}
		res, err := s.db.Exec(`DELETE FROM evidence_archives WHERE archive_hash IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += int(n)
	}
	return total, nil
}

// ArchiveExists 报告某 archive_hash 是否还在账本里。
//
// 用途是识别「账本外残文件」：归档文件先于账本落盘，进程在两步之间退出就会
// 留下一个磁盘上有、账本里没有的 segment。这类残渣只能靠扫目录找出来。
func (s *Store) ArchiveExists(hash string) (bool, error) {
	if strings.TrimSpace(hash) == "" {
		return false, nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM evidence_archives WHERE archive_hash = ?`, hash).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeleteEmptyEvidenceSources 删除既无归档、也无观测的证据源行，返回删除行数。
//
// evidence_sources 是序号账本（generation / last_seq / cursor_seq）。留下一条
// 「空转」的源没有价值（它守护的数据已经全没了），但删掉仍有历史的源会让下一次
// 捕获从 seq=1 重新分配、与留存的观测撞号——所以两个 NOT EXISTS 缺一不可。
func (s *Store) DeleteEmptyEvidenceSources(cutoff int64) (int, error) {
	res, err := s.db.Exec(`DELETE FROM evidence_sources
		 WHERE updated_at < ?
		   AND NOT EXISTS (SELECT 1 FROM evidence_archives a WHERE a.source_id = evidence_sources.id)
		   AND NOT EXISTS (SELECT 1 FROM observations o WHERE o.source_id = evidence_sources.id)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("删除空转证据源失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
