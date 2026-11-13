package evidence

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// segmentRecord 是归档 segment 与 spool segment 共用的记录格式。
//
// Raw 以 []byte 存进 JSON（自动 base64），保证「原始字节」可逆，不做摘要替代。
// 记录里带 event_uid/generation/seq，因此相同原始字节但不同身份也会得到不同
// archive hash —— 不会在 evidence_archives 的主键上误合并两条证据。
type segmentRecord struct {
	Version          int    `json:"v"`
	EventUID         string `json:"event_uid"`
	SourceID         string `json:"source_id"`
	Generation       int    `json:"generation"`
	Seq              int    `json:"seq"`
	OccurredAtNS     int64  `json:"occurred_at_ns"`
	SessionID        string `json:"session_id,omitempty"`
	Project          string `json:"project,omitempty"`
	Harness          string `json:"harness,omitempty"`
	EventType        string `json:"event_type,omitempty"`
	ParserVersion    string `json:"parser_version,omitempty"`
	ProposalEligible bool   `json:"proposal_eligible,omitempty"`
	Raw              []byte `json:"raw"`
	// SummaryOverride 只写在「需要清洗查询源」的事件上（宿主把注入上下文与
	// 用户原话拼在一条消息里）。omitempty 保证其余 segment 字节不变，
	// 且 spool 回放能还原清洗后的查询源，而不是拿全文去生成摘要。
	SummaryOverride string `json:"summary_override,omitempty"`
}

func recordOf(ev RawEvent) segmentRecord {
	return segmentRecord{
		Version: 1, EventUID: ev.EventUID, SourceID: ev.SourceID,
		Generation: ev.Generation, Seq: ev.Seq, OccurredAtNS: ev.OccurredAtNS,
		SessionID: ev.SessionID, Project: ev.Project, Harness: ev.Harness,
		EventType: ev.EventType, ParserVersion: ev.ParserVersion,
		ProposalEligible: ev.ProposalEligible, Raw: ev.Raw,
		SummaryOverride: ev.SummaryOverride,
	}
}

func (r segmentRecord) event() RawEvent {
	return RawEvent{
		EventUID: r.EventUID, SourceID: r.SourceID, Generation: r.Generation, Seq: r.Seq,
		OccurredAtNS: r.OccurredAtNS, SessionID: r.SessionID, Project: r.Project,
		Harness: r.Harness, EventType: r.EventType, ParserVersion: r.ParserVersion,
		ProposalEligible: r.ProposalEligible, Raw: r.Raw,
		SummaryOverride: r.SummaryOverride,
	}
}

type archivedSegment struct {
	hash string
	path string
	size int64
}

// writeArchiveRecords 把 N 条记录写成单个 <archive_hash>.jsonl.gz（每记录一行）。
//
// archive_hash 是压缩后字节的 sha256。流程：同目录临时文件 → 写 gzip → fsync →
// 原子 rename；rename 前再 fsync 目录，保证掉电后目录项也可见。
// N=1 时压缩字节与重构前的单条实现逐字节一致（header 固定、写入顺序不变），
// 因此存量归档的 hash 不受影响。
//
// 同 hash 的既有文件会被校验：内容损坏时用本次原始字节原子重写
// （测试与真实故障都覆盖），而不是继续引用坏文件。
func (e *Engine) writeArchiveRecords(recs []segmentRecord) (archivedSegment, error) {
	if len(recs) == 0 {
		return archivedSegment{}, errors.New("归档批次不能为空")
	}
	if err := os.MkdirAll(e.archiveDir, 0o700); err != nil {
		return archivedSegment{}, err
	}
	payloads := make([][]byte, 0, len(recs))
	for _, rec := range recs {
		payload, err := json.Marshal(rec)
		if err != nil {
			return archivedSegment{}, err
		}
		payloads = append(payloads, payload)
	}

	tmp, err := os.CreateTemp(e.archiveDir, ".seg-*.tmp")
	if err != nil {
		return archivedSegment{}, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	hasher := sha256.New()
	zw, err := gzip.NewWriterLevel(io.MultiWriter(tmp, hasher), gzip.BestSpeed)
	if err != nil {
		cleanup()
		return archivedSegment{}, err
	}
	// 固定 header 才能让同样的记录得到同样的压缩字节（archive_hash 可复现）。
	zw.Header.ModTime = time.Time{}
	zw.Header.OS = 255
	for _, payload := range payloads {
		if _, err := zw.Write(payload); err != nil {
			cleanup()
			return archivedSegment{}, err
		}
		if _, err := zw.Write([]byte("\n")); err != nil {
			cleanup()
			return archivedSegment{}, err
		}
	}
	if err := zw.Close(); err != nil {
		cleanup()
		return archivedSegment{}, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return archivedSegment{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return archivedSegment{}, err
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	final := filepath.Join(e.archiveDir, hash+".jsonl.gz")
	ok, err := fileHashMatches(final, hash)
	if err != nil {
		os.Remove(tmpName)
		return archivedSegment{}, err
	}
	if ok {
		os.Remove(tmpName)
	} else {
		if err := os.Rename(tmpName, final); err != nil {
			os.Remove(tmpName)
			return archivedSegment{}, err
		}
		if err := syncDir(e.archiveDir); err != nil {
			return archivedSegment{}, err
		}
	}
	fi, err := os.Stat(final)
	if err != nil {
		return archivedSegment{}, err
	}
	return archivedSegment{hash: hash, path: final, size: fi.Size()}, nil
}

// writeArchive 是单条写入路径，等价于 len(recs)==1 的批量写入。
func (e *Engine) writeArchive(ev RawEvent) (archivedSegment, error) {
	return e.writeArchiveRecords([]segmentRecord{recordOf(ev)})
}

// writeArchiveBatch 把一批事件写成单个 segment，文件 fsync 与目录 fsync 各一次。
//
// seq 由调用方预分配，本函数不推进证据源水位。P1 阶段尚未接入 Engine.Capture；
// 账本改造与灰度见 docs/design-write-batch.md。
func (e *Engine) writeArchiveBatch(evs []RawEvent) (archivedSegment, error) {
	recs := make([]segmentRecord, 0, len(evs))
	for _, ev := range evs {
		recs = append(recs, recordOf(ev))
	}
	return e.writeArchiveRecords(recs)
}

func fileHashMatches(path, want string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// readArchive 解回一条归档记录（测试与 T1.2 回放共用）。
// 只适用于单条 segment；批量归档请用 ReadArchiveRecord 按 #seq 定位。
func readArchive(path string) (segmentRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return segmentRecord{}, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return segmentRecord{}, fmt.Errorf("打开 gzip segment 失败: %w", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		return segmentRecord{}, fmt.Errorf("读 gzip segment 失败: %w", err)
	}
	var rec segmentRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return segmentRecord{}, fmt.Errorf("解析 segment 记录失败: %w", err)
	}
	return rec, nil
}

// ReadArchiveRecord 按 "<path>#<seq>" 从事实源归档解出一条原始事件。
//
// 单条归档只有一行，可不带 #seq（兼容既有 readArchive 语义）；批量归档含 N 行，
// 必须按 seq 定位。locator 的 path 部分需是可打开的归档文件路径。
func ReadArchiveRecord(locator string) (RawEvent, error) {
	rec, err := readArchiveRecord(locator)
	if err != nil {
		return RawEvent{}, err
	}
	return rec.event(), nil
}

// readArchiveRecord 逐行扫描 gzip JSONL，按 seq 定位，而不是把整个流当成一个
// JSON 对象。返回 segmentRecord 以便包内校验身份字段。
func readArchiveRecord(locator string) (segmentRecord, error) {
	path := strings.TrimSpace(locator)
	seq := 0
	if i := strings.LastIndex(path, "#"); i >= 0 {
		s := strings.TrimSpace(path[i+1:])
		path = path[:i]
		if s != "" {
			v, err := strconv.Atoi(s)
			if err != nil {
				return segmentRecord{}, fmt.Errorf("归档 locator 的 seq 无效: %q", s)
			}
			seq = v
		}
	}
	if path == "" {
		return segmentRecord{}, errors.New("归档 locator 缺少路径")
	}
	f, err := os.Open(path)
	if err != nil {
		return segmentRecord{}, fmt.Errorf("打开归档失败: %w", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return segmentRecord{}, fmt.Errorf("打开 gzip segment 失败: %w", err)
	}
	defer zr.Close()
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec segmentRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return segmentRecord{}, fmt.Errorf("解析 segment 记录失败: %w", err)
		}
		if seq == 0 || rec.Seq == seq {
			return rec, nil
		}
	}
	if err := sc.Err(); err != nil {
		return segmentRecord{}, fmt.Errorf("读 gzip segment 失败: %w", err)
	}
	if seq > 0 {
		return segmentRecord{}, fmt.Errorf("归档中找不到 seq=%d: %s", seq, path)
	}
	return segmentRecord{}, fmt.Errorf("归档为空: %s", path)
}
