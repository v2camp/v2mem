package evidence

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
}

func recordOf(ev RawEvent) segmentRecord {
	return segmentRecord{
		Version: 1, EventUID: ev.EventUID, SourceID: ev.SourceID,
		Generation: ev.Generation, Seq: ev.Seq, OccurredAtNS: ev.OccurredAtNS,
		SessionID: ev.SessionID, Project: ev.Project, Harness: ev.Harness,
		EventType: ev.EventType, ParserVersion: ev.ParserVersion,
		ProposalEligible: ev.ProposalEligible, Raw: ev.Raw,
	}
}

func (r segmentRecord) event() RawEvent {
	return RawEvent{
		EventUID: r.EventUID, SourceID: r.SourceID, Generation: r.Generation, Seq: r.Seq,
		OccurredAtNS: r.OccurredAtNS, SessionID: r.SessionID, Project: r.Project,
		Harness: r.Harness, EventType: r.EventType, ParserVersion: r.ParserVersion,
		ProposalEligible: r.ProposalEligible, Raw: r.Raw,
	}
}

type archivedSegment struct {
	hash string
	path string
	size int64
}

// writeArchive 把一条事件写成 <archive_hash>.jsonl.gz。
//
// archive_hash 是压缩后字节的 sha256。流程：同目录临时文件 → 写 gzip → fsync →
// 原子 rename；rename 前再 fsync 目录，保证掉电后目录项也可见。
//
// 同 hash 的既有文件会被校验：内容损坏时用本次原始字节原子重写
// （测试与真实故障都覆盖），而不是继续引用坏文件。
func (e *Engine) writeArchive(ev RawEvent) (archivedSegment, error) {
	if err := os.MkdirAll(e.archiveDir, 0o700); err != nil {
		return archivedSegment{}, err
	}
	rec := recordOf(ev)
	payload, err := json.Marshal(rec)
	if err != nil {
		return archivedSegment{}, err
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
	if _, err := zw.Write(payload); err != nil {
		cleanup()
		return archivedSegment{}, err
	}
	if _, err := zw.Write([]byte("\n")); err != nil {
		cleanup()
		return archivedSegment{}, err
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
