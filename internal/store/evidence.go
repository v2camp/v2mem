package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// T1.1 原始证据账本。
//
// 分工（同一张 SQLite 里的三条路径，互不混淆）：
//   - observations 是 canonical 证据行：每条原始事件恰好一行，带 event_uid 与
//     (source_id, source_generation, source_seq) 身份；
//   - evidence_archives 是 capture 账：一行的 from_seq..to_seq 覆盖一段 gzip segment；
//   - evidence_sources 是两段水位：last_seq = 已归档到哪（capture 推进），
//     cursor_seq = 已发成 Bundle 到哪（NextBundle 推进）。两段必须分开，
//     否则「归档成功但 Bundle 还没发」的证据会被直接跳过。
//
// 旧 AppendObservation 的 content_hash 去重语义不在这里：证据行按 event_uid 与
// 序号判重，所以「同摘要、不同 event_uid」可以共存。对应唯一索引
// ux_obs_dedup_legacy 只覆盖 event_uid='' AND source_id='' 的 legacy 行。

var (
	// ErrEvidenceSourceBacklog 表示证据源还有未消费证据，拒绝轮转。
	ErrEvidenceSourceBacklog = errors.New("证据源仍有未消费证据，拒绝轮转")
	// ErrEvidenceSourceNotFound 表示证据源不存在。
	ErrEvidenceSourceNotFound = errors.New("证据源不存在")
	// ErrEvidenceGenerationMismatch 表示写入的 generation 不是证据源当前代。
	ErrEvidenceGenerationMismatch = errors.New("证据 generation 与证据源当前代不一致")
	// ErrEvidenceSeqGap 表示序号跳号：连续游标不接受空洞，也不接受补写历史。
	ErrEvidenceSeqGap = errors.New("证据序号不连续")
	// ErrEvidenceCursorConflict 表示并发写入者已推进 cursor。
	ErrEvidenceCursorConflict = errors.New("证据游标已被其他写入者推进")
	// ErrEvidenceBundleNotFound 表示 Bundle 不存在。
	ErrEvidenceBundleNotFound = errors.New("证据 Bundle 不存在")
)

// EvidenceCaptureInput 是一次 capture 的账本写入参数。
//
// ArchiveHash/ArchivePath/ByteSize/RawHashConcat 由 internal/evidence 先在文件系统
// 落地 gzip segment 后提供；本函数只负责在一个事务里写账。
type EvidenceCaptureInput struct {
	EventUID         string
	SourceID         string
	SourceKind       string
	SourceLocator    string
	Generation       int
	Seq              int
	OccurredAtNS     int64
	SessionID        string
	Project          string
	Harness          string
	EventType        string
	Channel          string
	ParserVersion    string
	ProposalEligible bool
	Summary          string
	// Fidelity / Adapter 是捕获层语义，只落 observations.meta_json，
	// 不进归档 segment。空 Fidelity 表示旧行/未声明，由读侧兼容。
	Fidelity string
	Adapter  string

	ArchiveHash   string
	ArchivePath   string
	RawRecordHash string
	RawHashConcat string
	ByteSize      int64
	RawCount      int
}

// EvidenceRecord 是一条证据行的最小引用（与 evidence.Ref 字段一一对应）。
type EvidenceRecord struct {
	ID               string
	EventUID         string
	SourceID         string
	Generation       int
	Seq              int
	OccurredAtNS     int64
	SessionID        string
	Project          string
	Harness          string
	EventType        string
	ArchiveHash      string
	RawRecordHash    string
	RawLocator       string
	Summary          string
	ParserVersion    string
	ProposalEligible bool
	Fidelity         string
	Adapter          string
}

// evidenceMeta 是 observations.meta_json 的结构化形态（W0 冻结）。
// 旧行只有 proposal_eligible 一个键，新增键一律 omitempty 保持兼容。
type evidenceMeta struct {
	ProposalEligible bool   `json:"proposal_eligible"`
	Fidelity         string `json:"fidelity,omitempty"`
	Adapter          string `json:"adapter,omitempty"`
}

// EvidenceSourceState 是证据源当前代与两段水位。
type EvidenceSourceState struct {
	ID         string
	Generation int
	LastSeq    int
	CursorSeq  int
	SourceTip  string
}

// EvidenceBundleRecord 是一段连续证据窗口。
type EvidenceBundleRecord struct {
	ID               string
	SourceID         string
	Generation       int
	FromSeqExclusive int
	ToSeqInclusive   int
	EvidenceIDs      []string
	EvidenceHash     string
	SourceTip        string
	Status           string
	Attempt          int
}

const evidenceRecordColumns = `o.id, o.event_uid, o.source_id, o.source_generation, o.source_seq,
	o.occurred_at_ns, o.session_id, o.project, o.harness, o.event, o.archive_hash,
	o.raw_record_hash, o.raw_ref, o.summary, o.parser_version, o.meta_json`

// EvidenceSourceHead 返回证据源当前代与捕获水位；源不存在时返回 generation=1、
// last_seq=0（不创建行）。
func (s *Store) EvidenceSourceHead(sourceID string) (EvidenceSourceState, bool, error) {
	var st EvidenceSourceState
	err := s.db.QueryRow(`SELECT id, generation, last_seq, cursor_seq, source_tip
		FROM evidence_sources WHERE id = ?`, sourceID).
		Scan(&st.ID, &st.Generation, &st.LastSeq, &st.CursorSeq, &st.SourceTip)
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceSourceState{ID: sourceID, Generation: 1}, false, nil
	}
	if err != nil {
		return EvidenceSourceState{}, false, err
	}
	return st, true, nil
}

// LookupEvidence 只读地按身份查已有证据，供归档层在写字节前判断重放。
//
// 先查 event_uid；uid 为空时才查 (source_id, generation, seq)。
func (s *Store) LookupEvidence(eventUID, sourceID string, gen, seq int) (EvidenceRecord, bool, error) {
	if strings.TrimSpace(eventUID) != "" {
		rec, err := scanEvidenceRecord(s.db.QueryRow(
			`SELECT `+evidenceRecordColumns+` FROM observations o WHERE o.event_uid = ?`, eventUID))
		if errors.Is(err, sql.ErrNoRows) {
			return EvidenceRecord{}, false, nil
		}
		if err != nil {
			return EvidenceRecord{}, false, err
		}
		return rec, true, nil
	}
	if sourceID == "" || seq <= 0 {
		return EvidenceRecord{}, false, nil
	}
	rec, err := scanEvidenceRecord(s.db.QueryRow(`SELECT `+evidenceRecordColumns+`
		FROM observations o
		WHERE o.source_id = ? AND o.source_generation = ? AND o.source_seq = ?`,
		sourceID, gen, seq))
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceRecord{}, false, nil
	}
	if err != nil {
		return EvidenceRecord{}, false, err
	}
	return rec, true, nil
}

// CaptureEvidence 在一个事务里写 archive 账、canonical observation 与证据源水位。
//
// 幂等键有两个：
//   - event_uid：命中已有行直接返回（先写赢，不覆盖摘要/字节）；
//   - (source_id, generation, seq)：同一位置的重复写入返回已有行。
//
// 注意 seq：调用方传 0 表示追加，由本函数取 last_seq+1；传非 0 必须等于 last_seq+1，
// 否则返回 ErrEvidenceSeqGap —— 连续游标不能靠跳号或补历史来「修」。
func (s *Store) CaptureEvidence(in EvidenceCaptureInput) (EvidenceRecord, bool, error) {
	if strings.TrimSpace(in.EventUID) == "" {
		return EvidenceRecord{}, false, errors.New("event_uid 不能为空")
	}
	if strings.TrimSpace(in.SourceID) == "" {
		return EvidenceRecord{}, false, errors.New("source_id 不能为空")
	}
	if strings.TrimSpace(in.ArchiveHash) == "" {
		return EvidenceRecord{}, false, errors.New("archive_hash 不能为空")
	}
	channel, err := DefaultObsChannel(in.EventType, in.Channel)
	if err != nil {
		return EvidenceRecord{}, false, err
	}
	meta, err := json.Marshal(evidenceMeta{
		ProposalEligible: in.ProposalEligible,
		Fidelity:         in.Fidelity,
		Adapter:          in.Adapter,
	})
	if err != nil {
		return EvidenceRecord{}, false, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return EvidenceRecord{}, false, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	gen, lastSeq, tip, err := ensureEvidenceSourceTx(tx, in, now)
	if err != nil {
		return EvidenceRecord{}, false, err
	}

	if rec, ok, err := evidenceByEventUID(tx, in.EventUID); err != nil {
		return EvidenceRecord{}, false, err
	} else if ok {
		return rec, false, nil
	}

	seq := in.Seq
	if seq == 0 {
		seq = lastSeq + 1
	}
	writeGen := in.Generation
	if writeGen == 0 {
		writeGen = gen
	}
	if rec, ok, err := evidenceByPosition(tx, in.SourceID, writeGen, seq); err != nil {
		return EvidenceRecord{}, false, err
	} else if ok {
		return rec, false, nil
	}
	if writeGen != gen {
		return EvidenceRecord{}, false, fmt.Errorf("%w: got %d want %d", ErrEvidenceGenerationMismatch, writeGen, gen)
	}
	if seq != lastSeq+1 {
		return EvidenceRecord{}, false, fmt.Errorf("%w: got seq %d, 当前 last_seq %d", ErrEvidenceSeqGap, seq, lastSeq)
	}

	rawCount := in.RawCount
	if rawCount <= 0 {
		rawCount = 1
	}
	if _, err := tx.Exec(`INSERT INTO evidence_archives(
		archive_hash, source_id, generation, from_seq, to_seq, raw_count, byte_size,
		path, raw_hash_concat, created_at
	) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		in.ArchiveHash, in.SourceID, gen, seq, seq, rawCount, in.ByteSize,
		in.ArchivePath, in.RawHashConcat, now); err != nil {
		return EvidenceRecord{}, false, fmt.Errorf("写 archive 账失败: %w", err)
	}

	id := "ev_" + newID()
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = strings.TrimSpace(in.EventType)
	}
	if summary == "" {
		summary = "evidence"
	}
	sessionID := strings.TrimSpace(in.SessionID)
	if sessionID == "" {
		sessionID = "unknown"
	}
	locator := in.ArchivePath
	if locator == "" {
		locator = in.ArchiveHash
	}
	if seq > 0 {
		locator = fmt.Sprintf("%s#%d", locator, seq)
	}
	occurred := in.OccurredAtNS
	if occurred == 0 {
		occurred = time.Now().UnixNano()
	}
	if _, err := tx.Exec(`INSERT INTO observations(
		id, session_id, project, harness, event, tool_name, summary, summary_idx, raw_ref,
		content_hash, created_at, meta_json, channel, event_id, event_uid, source_id,
		source_generation, source_seq, occurred_at_ns, archive_hash, raw_record_hash, parser_version
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, sessionID, in.Project, in.Harness, in.EventType, "", summary,
		observationIndexText(summary), locator, obsHash(in.EventType, "", summary),
		now, string(meta), channel, "", in.EventUID, in.SourceID, gen, seq, occurred,
		in.ArchiveHash, in.RawRecordHash, in.ParserVersion); err != nil {
		return EvidenceRecord{}, false, fmt.Errorf("写 canonical observation 失败: %w", err)
	}

	newTip := evidenceTip(tip, in.EventUID, in.ArchiveHash, seq)
	if _, err := tx.Exec(`UPDATE evidence_sources
		SET last_seq = ?, source_tip = ?, updated_at = ? WHERE id = ?`,
		seq, newTip, now, in.SourceID); err != nil {
		return EvidenceRecord{}, false, fmt.Errorf("推进证据源水位失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return EvidenceRecord{}, false, err
	}
	return EvidenceRecord{
		ID: id, EventUID: in.EventUID, SourceID: in.SourceID, Generation: gen, Seq: seq,
		OccurredAtNS: occurred, SessionID: sessionID, Project: in.Project, Harness: in.Harness,
		EventType: in.EventType, ArchiveHash: in.ArchiveHash, RawRecordHash: in.RawRecordHash,
		RawLocator: locator, Summary: summary, ParserVersion: in.ParserVersion,
		ProposalEligible: in.ProposalEligible, Fidelity: in.Fidelity, Adapter: in.Adapter,
	}, true, nil
}

func ensureEvidenceSourceTx(tx *sql.Tx, in EvidenceCaptureInput, now int64) (gen, lastSeq int, tip string, err error) {
	err = tx.QueryRow(`SELECT generation, last_seq, source_tip FROM evidence_sources WHERE id = ?`,
		in.SourceID).Scan(&gen, &lastSeq, &tip)
	if err == nil {
		return gen, lastSeq, tip, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, "", err
	}
	gen = in.Generation
	if gen <= 0 {
		gen = 1
	}
	kind := strings.TrimSpace(in.SourceKind)
	if kind == "" {
		kind = "hook"
	}
	if _, err := tx.Exec(`INSERT INTO evidence_sources(
		id, kind, locator, generation, status, last_seq, cursor_seq, source_tip, created_at, updated_at
	) VALUES (?,?,?,?,'active',0,0,'',?,?)`,
		in.SourceID, kind, in.SourceLocator, gen, now, now); err != nil {
		return 0, 0, "", fmt.Errorf("建证据源失败: %w", err)
	}
	return gen, 0, "", nil
}

func evidenceByEventUID(tx *sql.Tx, eventUID string) (EvidenceRecord, bool, error) {
	row := tx.QueryRow(`SELECT `+evidenceRecordColumns+` FROM observations o WHERE o.event_uid = ?`, eventUID)
	rec, err := scanEvidenceRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceRecord{}, false, nil
	}
	if err != nil {
		return EvidenceRecord{}, false, err
	}
	return rec, true, nil
}

func evidenceByPosition(tx *sql.Tx, sourceID string, gen, seq int) (EvidenceRecord, bool, error) {
	if sourceID == "" || seq <= 0 {
		return EvidenceRecord{}, false, nil
	}
	row := tx.QueryRow(`SELECT `+evidenceRecordColumns+`
		FROM observations o
		WHERE o.source_id = ? AND o.source_generation = ? AND o.source_seq = ?`,
		sourceID, gen, seq)
	rec, err := scanEvidenceRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EvidenceRecord{}, false, nil
	}
	if err != nil {
		return EvidenceRecord{}, false, err
	}
	return rec, true, nil
}

func scanEvidenceRecord(row interface{ Scan(...any) error }) (EvidenceRecord, error) {
	var rec EvidenceRecord
	var meta string
	err := row.Scan(&rec.ID, &rec.EventUID, &rec.SourceID, &rec.Generation, &rec.Seq,
		&rec.OccurredAtNS, &rec.SessionID, &rec.Project, &rec.Harness, &rec.EventType,
		&rec.ArchiveHash, &rec.RawRecordHash, &rec.RawLocator, &rec.Summary,
		&rec.ParserVersion, &meta)
	if err != nil {
		return EvidenceRecord{}, err
	}
	var m evidenceMeta
	if json.Unmarshal([]byte(meta), &m) == nil {
		rec.ProposalEligible = m.ProposalEligible
		rec.Fidelity = m.Fidelity
		rec.Adapter = m.Adapter
	}
	return rec, nil
}

func evidenceTip(prev, eventUID, archiveHash string, seq int) string {
	sum := sha256.Sum256([]byte(prev + "\n" + eventUID + "\n" + archiveHash + "\n" + strconv.Itoa(seq)))
	return hex.EncodeToString(sum[:])
}

// NextEvidenceBundle 从「最老且有未消费证据」的源取一段连续窗口。
//
// 水位只认 evidence_sources.cursor_seq，绝不读 distill_runs.window_key/finished_at：
// 后者是旧蒸馏窗口，和证据是否被消费没有关系。
//
// 连续性：窗口必须从 cursor_seq+1 开始逐条递增；遇到空洞立即停止并在该源上
// 返回 nil（不去下一源“跳过空洞”），下一次调用会重新尝试同一 position。
//
// 上限：limit（默认 50）与 maxBytes（默认 64KB，按 gzip segment 的 byte_size 计）
// 先到先停；即使单条超过 maxBytes 也至少返回 1 条，避免永远卡住。
func (s *Store) NextEvidenceBundle(limit int, maxBytes int64) (*EvidenceBundleRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	sources, err := s.evidenceSourcesWithBacklog(8)
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		b, err := s.buildEvidenceBundle(src, limit, maxBytes)
		if err != nil {
			return nil, err
		}
		if b != nil {
			return b, nil
		}
	}
	return nil, nil
}

func (s *Store) evidenceSourcesWithBacklog(limit int) ([]EvidenceSourceState, error) {
	rows, err := s.db.Query(`SELECT id, generation, last_seq, cursor_seq, source_tip
		FROM evidence_sources
		WHERE status = 'active' AND last_seq > cursor_seq
		ORDER BY created_at ASC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvidenceSourceState
	for rows.Next() {
		var st EvidenceSourceState
		if err := rows.Scan(&st.ID, &st.Generation, &st.LastSeq, &st.CursorSeq, &st.SourceTip); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

type evidenceWindowItem struct {
	id          string
	eventUID    string
	seq         int
	archiveHash string
	byteSize    int64
}

func (s *Store) buildEvidenceBundle(src EvidenceSourceState, limit int, maxBytes int64) (*EvidenceBundleRecord, error) {
	rows, err := s.db.Query(`SELECT o.id, o.event_uid, o.source_seq, o.archive_hash,
			COALESCE(a.byte_size, 0)
		FROM observations o
		LEFT JOIN evidence_archives a ON a.archive_hash = o.archive_hash
		WHERE o.source_id = ? AND o.source_generation = ? AND o.source_seq > ?
		ORDER BY o.source_seq ASC LIMIT ?`,
		src.ID, src.Generation, src.CursorSeq, limit)
	if err != nil {
		return nil, err
	}
	var items []evidenceWindowItem
	for rows.Next() {
		var it evidenceWindowItem
		if err := rows.Scan(&it.id, &it.eventUID, &it.seq, &it.archiveHash, &it.byteSize); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	expected := src.CursorSeq + 1
	var window []evidenceWindowItem
	var bytes int64
	for _, it := range items {
		if it.seq != expected {
			break
		}
		if len(window) > 0 && bytes+it.byteSize > maxBytes {
			break
		}
		window = append(window, it)
		bytes += it.byteSize
		expected++
		if len(window) >= limit {
			break
		}
	}
	if len(window) == 0 {
		// cursor+1 缺失：不跳空洞，等写入方补齐（或在源头修复）后再取。
		return nil, nil
	}

	toSeq := window[len(window)-1].seq
	bundle := EvidenceBundleRecord{
		ID:               evidenceBundleID(src.ID, src.Generation, src.CursorSeq, toSeq),
		SourceID:         src.ID,
		Generation:       src.Generation,
		FromSeqExclusive: src.CursorSeq,
		ToSeqInclusive:   toSeq,
		SourceTip:        src.SourceTip,
		Status:           "pending",
	}
	for _, it := range window {
		bundle.EvidenceIDs = append(bundle.EvidenceIDs, it.id)
	}
	bundle.EvidenceHash = evidenceHash(window)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var curSeq int
	if err := tx.QueryRow(`SELECT cursor_seq FROM evidence_sources WHERE id = ? AND generation = ?`,
		src.ID, src.Generation).Scan(&curSeq); err != nil {
		return nil, err
	}
	if curSeq != src.CursorSeq {
		return nil, ErrEvidenceCursorConflict
	}

	now := time.Now().Unix()
	res, err := tx.Exec(`INSERT OR IGNORE INTO evidence_bundles(
		id, source_id, generation, from_seq_exclusive, to_seq_inclusive, evidence_hash,
		source_tip, status, attempt, not_before, lease_until, lease_owner, last_run_id,
		created_at, updated_at
	) VALUES (?,?,?,?,?,?,?,'pending',0,0,0,'','',?,?)`,
		bundle.ID, src.ID, src.Generation, src.CursorSeq, toSeq, bundle.EvidenceHash,
		src.SourceTip, now, now)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		// 同一窗口已发过：不重复插条目，也不回退 cursor；返回既有 Bundle。
		return evidenceLoadBundleTx(tx, bundle.ID)
	}
	for i, it := range window {
		if _, err := tx.Exec(`INSERT INTO evidence_bundle_items(bundle_id, ordinal, evidence_id, event_uid)
			VALUES (?,?,?,?)`, bundle.ID, i+1, it.id, it.eventUID); err != nil {
			return nil, err
		}
	}
	res, err = tx.Exec(`UPDATE evidence_sources SET cursor_seq = ?, updated_at = ?
		WHERE id = ? AND generation = ? AND cursor_seq = ?`,
		toSeq, now, src.ID, src.Generation, src.CursorSeq)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, ErrEvidenceCursorConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &bundle, nil
}

func evidenceBundleID(sourceID string, gen, from, to int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d", sourceID, gen, from, to)))
	return "bnd_" + hex.EncodeToString(sum[:])[:24]
}

func evidenceHash(items []evidenceWindowItem) string {
	h := sha256.New()
	for _, it := range items {
		fmt.Fprintf(h, "%s|%s|%d\n", it.id, it.eventUID, it.seq)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func evidenceLoadBundleTx(tx *sql.Tx, id string) (*EvidenceBundleRecord, error) {
	var b EvidenceBundleRecord
	err := tx.QueryRow(`SELECT id, source_id, generation, from_seq_exclusive, to_seq_inclusive,
			evidence_hash, source_tip, status, attempt
		FROM evidence_bundles WHERE id = ?`, id).
		Scan(&b.ID, &b.SourceID, &b.Generation, &b.FromSeqExclusive, &b.ToSeqInclusive,
			&b.EvidenceHash, &b.SourceTip, &b.Status, &b.Attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEvidenceBundleNotFound
	}
	if err != nil {
		return nil, err
	}
	items, qerr := tx.Query(`SELECT evidence_id FROM evidence_bundle_items WHERE bundle_id = ? ORDER BY ordinal`, id)
	ids, err := scanStringColumn(items, qerr)
	if err != nil {
		return nil, err
	}
	b.EvidenceIDs = ids
	// 复用既有窗口时也要把 cursor 推到窗口末尾（只进不退），否则同一窗口会被
	// 反复取出。正常情况下 bundle 与 cursor 同事务提交，这里只兜故障恢复。
	if _, err := tx.Exec(`UPDATE evidence_sources SET cursor_seq = ?, updated_at = ?
		WHERE id = ? AND generation = ? AND cursor_seq < ?`,
		b.ToSeqInclusive, time.Now().Unix(), b.SourceID, b.Generation, b.ToSeqInclusive); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &b, nil
}

// ListEvidenceRecords 返回 Bundle 内的证据引用（按 ordinal 顺序）。
func (s *Store) ListEvidenceRecords(bundleID string) ([]EvidenceRecord, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM evidence_bundles WHERE id = ?`, bundleID).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: %s", ErrEvidenceBundleNotFound, bundleID)
	}
	rows, err := s.db.Query(`SELECT `+evidenceRecordColumns+`
		FROM evidence_bundle_items i
		JOIN observations o ON o.id = i.evidence_id
		WHERE i.bundle_id = ? ORDER BY i.ordinal ASC`, bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvidenceRecord
	for rows.Next() {
		rec, err := scanEvidenceRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// RotateEvidenceSource 把证据源推进到下一代：generation+1、序号归零。
//
// 仍有未消费证据时拒绝轮转（ErrEvidenceSourceBacklog）：旧代的 cursor 只存在
// 这一行里，清掉就再也发不出旧代 Bundle，等于静默丢证据。
func (s *Store) RotateEvidenceSource(sourceID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var gen, lastSeq, cursor int
	err = tx.QueryRow(`SELECT generation, last_seq, cursor_seq FROM evidence_sources WHERE id = ?`,
		sourceID).Scan(&gen, &lastSeq, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrEvidenceSourceNotFound
	}
	if err != nil {
		return 0, err
	}
	if cursor < lastSeq {
		return 0, fmt.Errorf("%w: last_seq=%d cursor_seq=%d", ErrEvidenceSourceBacklog, lastSeq, cursor)
	}
	next := gen + 1
	if _, err := tx.Exec(`UPDATE evidence_sources
		SET generation = ?, last_seq = 0, cursor_seq = 0, source_tip = '', updated_at = ?
		WHERE id = ?`, next, time.Now().Unix(), sourceID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return next, nil
}

func scanStringColumn(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
