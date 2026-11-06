// Package evidence 实现原始证据归档（T1.1）：gzip segment + 连续游标。
//
// 关键不变量：
//   - 原始字节先落盘（临时文件 → fsync → 原子 rename），再写账；
//     归档失败不动游标，DB 失败保留归档并落独立 spool segment。
//   - 证据身份是 event_uid 与 (source_id, generation, seq)，不是摘要；
//     同摘要不同 uid 可以共存，不复用旧 AppendObservation 的内容去重。
//   - Bundle 只从 evidence_sources.cursor_seq 取连续窗口，绝不拿
//     distill_runs.window_key / finished_at 当水位。
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/v2camp/v2mem/internal/store"
)

// ParserVersion 是当前归档记录格式版本。格式变更必须递增，旧字节仍可解。
const ParserVersion = "evidence/v1"

// 默认窗口参数：约 50 条 / 64KB（见 NextEvidenceBundle）。
const (
	DefaultBundleLimit = 50
	DefaultBundleBytes = 64 << 10
)

// Engine 同时实现 Writer 与 Reader。
type Engine struct {
	st         *store.Store
	archiveDir string
	spoolRoot  string
}

var (
	_ Writer = (*Engine)(nil)
	_ Reader = (*Engine)(nil)
)

// Open 打开（必要时创建）证据库，并挂上同目录下的归档/ spool 路径。
func Open(dbPath string) (*Engine, error) {
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return New(st), nil
}

// New 复用已打开的 Store。归档与 spool 都放在库文件所在目录下：
//
//	<db-dir>/evidence/archives/<archive_hash>.jsonl.gz
//	<db-dir>/spool/<session>/seg-*.jsonl
func New(st *store.Store) *Engine {
	dir := filepath.Dir(st.Path())
	return &Engine{
		st:         st,
		archiveDir: filepath.Join(dir, "evidence", "archives"),
		spoolRoot:  filepath.Join(dir, "spool"),
	}
}

func (e *Engine) Close() error { return e.st.Close() }

// ArchiveDir 暴露归档目录（诊断与测试用）。
func (e *Engine) ArchiveDir() string { return e.archiveDir }

// SpoolRoot 暴露 spool 根目录（诊断与测试用）。
func (e *Engine) SpoolRoot() string { return e.spoolRoot }

// Capture 归档一条原始事件。
//
// 顺序固定：解析身份 → 写 gzip segment（临时文件→fsync→rename）→ 单事务写账。
// DB 失败时保留 segment 并把事件写进独立 spool segment；archive 失败时直接返回错误，
// 既不写账也不推进水位。
func (e *Engine) Capture(ev RawEvent) (Ref, error) {
	return e.capture(ev, true)
}

func (e *Engine) capture(ev RawEvent, allowSpool bool) (Ref, error) {
	ev.EventUID = strings.TrimSpace(ev.EventUID)
	ev.SourceID = strings.TrimSpace(ev.SourceID)
	if ev.EventUID == "" {
		return Ref{}, errors.New("event_uid 不能为空")
	}
	if ev.SourceID == "" {
		return Ref{}, errors.New("source_id 不能为空")
	}
	if ev.EventType == "" {
		ev.EventType = "raw"
	}
	if ev.ParserVersion == "" {
		ev.ParserVersion = ParserVersion
	}
	if ev.OccurredAtNS == 0 {
		ev.OccurredAtNS = time.Now().UnixNano()
	}
	// 空 Fidelity 按事实源 Raw 是否有可见内容兜底，写回本地 ev：传给账本的
	// 值与 Capture 返回的 Ref、ListEvidence 读回保持一致。该字段只进 meta_json，
	// 不进归档 segment，归档字节级幂等不受影响。
	if ev.Fidelity == "" {
		ev.Fidelity = resolveFidelity(ev.Raw)
	}

	// 先只读判重：命中就不必再分配序号/写新 segment，也不会留下孤儿归档。
	// 库不可用时，只有显式给了 gen+seq 才继续（这正是「DB 失败保留归档 + spool」
	// 要覆盖的场景）。
	if rec, ok, err := e.st.LookupEvidence(ev.EventUID, ev.SourceID, ev.Generation, ev.Seq); err != nil {
		if ev.Generation == 0 || ev.Seq <= 0 {
			return Ref{}, fmt.Errorf("查证据身份失败: %w", err)
		}
	} else if ok {
		rawHash := sha256Hex(ev.Raw)
		// 重放时顺手校验归档：文件损坏但字节一致 → 用库里的身份与原始字节原子重建。
		if rec.RawRecordHash == rawHash {
			ev.Generation, ev.Seq = rec.Generation, rec.Seq
			ev.SessionID, ev.Project, ev.Harness = rec.SessionID, rec.Project, rec.Harness
			ev.EventType, ev.ParserVersion = rec.EventType, rec.ParserVersion
			ev.OccurredAtNS, ev.ProposalEligible = rec.OccurredAtNS, rec.ProposalEligible
			seg, aerr := e.writeArchive(ev)
			if aerr != nil {
				return Ref{}, fmt.Errorf("校验归档失败: %w", aerr)
			}
			if seg.hash != rec.ArchiveHash {
				return Ref{}, fmt.Errorf("归档无法按库内身份重建：算出 %s，账本 %s", seg.hash, rec.ArchiveHash)
			}
		}
		return toRef(rec), nil
	}

	// 身份没给全才读库；显式 gen+seq 时即便库暂时不可用也先把字节归档下来。
	if ev.Generation == 0 || ev.Seq <= 0 {
		head, _, err := e.st.EvidenceSourceHead(ev.SourceID)
		if err != nil {
			return Ref{}, fmt.Errorf("读证据源水位失败: %w", err)
		}
		if ev.Generation == 0 {
			ev.Generation = head.Generation
		}
		if ev.Seq <= 0 {
			ev.Seq = head.LastSeq + 1
		}
	}

	rawHash := sha256Hex(ev.Raw)
	summary := summarizeRaw(ev.Raw, ev.EventType)
	seg, err := e.writeArchive(ev)
	if err != nil {
		return Ref{}, fmt.Errorf("归档原始证据失败: %w", err)
	}

	channel, err := store.DefaultObsChannel(ev.EventType, "")
	if err != nil {
		return Ref{}, err
	}
	rec, _, err := e.st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: ev.EventUID, SourceID: ev.SourceID, SourceKind: "hook",
		Generation: ev.Generation, Seq: ev.Seq, OccurredAtNS: ev.OccurredAtNS,
		SessionID: ev.SessionID, Project: ev.Project, Harness: ev.Harness,
		EventType: ev.EventType, Channel: channel, ParserVersion: ev.ParserVersion,
		ProposalEligible: ev.ProposalEligible, Summary: summary,
		Fidelity: string(ev.Fidelity), Adapter: ev.Harness,
		ArchiveHash: seg.hash, ArchivePath: seg.path, RawRecordHash: rawHash,
		RawHashConcat: rawHash, ByteSize: seg.size, RawCount: 1,
	})
	if err != nil {
		if !allowSpool {
			return Ref{}, err
		}
		spoolPath, spoolErr := e.writeSpool(ev)
		if spoolErr != nil {
			return Ref{}, errors.Join(err, fmt.Errorf("落 spool segment 失败: %w", spoolErr))
		}
		return Ref{}, fmt.Errorf("%w（归档已保留：%s；已落 spool：%s）", err, seg.path, spoolPath)
	}
	return toRef(rec), nil
}

// NextBundle 取最老未消费源的一段连续窗口。没有可出的窗口时返回 (nil, nil)。
func (e *Engine) NextBundle(limit int, maxBytes int64) (*Bundle, error) {
	if limit <= 0 {
		limit = DefaultBundleLimit
	}
	if maxBytes <= 0 {
		maxBytes = DefaultBundleBytes
	}
	rec, err := e.st.NextEvidenceBundle(limit, maxBytes)
	if err != nil {
		if errors.Is(err, store.ErrEvidenceCursorConflict) {
			return nil, nil
		}
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	return &Bundle{
		ID: rec.ID, SourceID: rec.SourceID, Generation: rec.Generation,
		FromSeqExclusive: rec.FromSeqExclusive, ToSeqInclusive: rec.ToSeqInclusive,
		EvidenceIDs: rec.EvidenceIDs, EvidenceHash: rec.EvidenceHash,
		SourceTip: rec.SourceTip, Status: rec.Status, Attempt: rec.Attempt,
	}, nil
}

// ListEvidence 返回 Bundle 内的证据引用。
func (e *Engine) ListEvidence(bundleID string) ([]Ref, error) {
	recs, err := e.st.ListEvidenceRecords(bundleID)
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(recs))
	for _, rec := range recs {
		out = append(out, toRef(rec))
	}
	return out, nil
}

// RotateSource 轮转证据源（generation+1，序号归零）；有未消费证据时拒绝。
func (e *Engine) RotateSource(sourceID string) (int, error) {
	return e.st.RotateEvidenceSource(sourceID)
}

func toRef(rec store.EvidenceRecord) Ref {
	return Ref{
		EventUID: rec.EventUID, SourceID: rec.SourceID, Generation: rec.Generation,
		Seq: rec.Seq, OccurredAtNS: rec.OccurredAtNS, SessionID: rec.SessionID,
		Project: rec.Project, Harness: rec.Harness, EventType: rec.EventType,
		ArchiveHash: rec.ArchiveHash, RawRecordHash: rec.RawRecordHash,
		RawLocator: rec.RawLocator, Summary: rec.Summary, ParserVersion: rec.ParserVersion,
		ProposalEligible: rec.ProposalEligible, Fidelity: Fidelity(rec.Fidelity),
	}
}

// resolveFidelity 为空 Fidelity 按事实源 Raw 是否有可见内容兜底。
// 有内容 → full，无内容（含纯空白）→ none；结果只落 observations.meta_json。
func resolveFidelity(raw []byte) Fidelity {
	if strings.TrimSpace(string(raw)) != "" {
		return FidelityFull
	}
	return FidelityNone
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// summarizeRaw 给 canonical observation 一个有界摘要。
//
// 原始字节仍是唯一事实来源，摘要只用于检索与展示：折叠空白、去掉控制字符、
// 截到 512 字节（按 rune 边界回退）。
//
// 空内容返回空摘要，绝不回落到事件类型（"stop" / "session-end"）或 "evidence"
// 这类占位字面量：事件名不是内容（design-capture §0/§4.3）；observations.event
// 列已经记录事件类型；FidelityNone / 空 Raw 已经表达「无正文」。再把事件名塞进
// 摘要只会把伪造内容当正文展示给使用者。eventType 参数保留只为固定调用点签名
// （engine.Capture 仍传 ev.EventType），本函数不再消费它。
func summarizeRaw(raw []byte, _ string) string {
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(raw))
	s = strings.Join(strings.Fields(s), " ")
	const max = 512
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}
