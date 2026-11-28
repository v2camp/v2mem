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

	// 批量归档专用：archive_hash 是 evidence_archives 的主键，一批只插一行账。
	// 批内第一条设 ToSeq / RawCount / RawHashConcat 并写账，其余条设
	// SkipArchiveLedger=true 只写 observations。零值时行为与单条完全一致。
	ToSeq             int
	SkipArchiveLedger bool
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
	SummaryRef       string
	SummaryHash      string
	PayloadBytes     int
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
	o.raw_record_hash, o.raw_ref, o.summary, o.summary_ref, o.summary_hash, o.payload_bytes,
	o.parser_version, o.meta_json`

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
	channel, meta, err := prepareEvidenceInput(in)
	if err != nil {
		return EvidenceRecord{}, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return EvidenceRecord{}, false, err
	}
	defer tx.Rollback()
	rec, created, err := captureEvidenceTx(tx, SummaryContentRoot(s.path), s.summaryExternal, in, channel, meta, time.Now().Unix())
	if err != nil {
		return EvidenceRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return EvidenceRecord{}, false, err
	}
	return rec, created, nil
}

// CaptureEvidenceBatch 在一个事务里写入一批证据。
//
// archive_hash 是 evidence_archives 的主键，故批内只有第一条写 archive 账
// （from_seq..to_seq 覆盖整批），其余条设 SkipArchiveLedger 只写 observations。
// 每条仍走与单条完全相同的判重、seq gap 校验与水位推进；同一事务内后一条
// 能看到前一条推进的 last_seq，因此连续序号不会被拒绝。
func (s *Store) CaptureEvidenceBatch(ins []EvidenceCaptureInput) ([]EvidenceRecord, []bool, error) {
	if len(ins) == 0 {
		return nil, nil, nil
	}
	type prepared struct {
		in      EvidenceCaptureInput
		channel string
		meta    []byte
	}
	items := make([]prepared, 0, len(ins))
	for _, in := range ins {
		channel, meta, err := prepareEvidenceInput(in)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, prepared{in: in, channel: channel, meta: meta})
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	root := SummaryContentRoot(s.path)
	recs := make([]EvidenceRecord, 0, len(items))
	created := make([]bool, 0, len(items))
	for _, it := range items {
		rec, ok, err := captureEvidenceTx(tx, root, s.summaryExternal, it.in, it.channel, it.meta, now)
		if err != nil {
			return nil, nil, err
		}
		recs = append(recs, rec)
		created = append(created, ok)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return recs, created, nil
}

// prepareEvidenceInput 校验身份并预计算 channel / meta_json，单条与批量共用。
func prepareEvidenceInput(in EvidenceCaptureInput) (string, []byte, error) {
	if strings.TrimSpace(in.EventUID) == "" {
		return "", nil, errors.New("event_uid 不能为空")
	}
	if strings.TrimSpace(in.SourceID) == "" {
		return "", nil, errors.New("source_id 不能为空")
	}
	if strings.TrimSpace(in.ArchiveHash) == "" {
		return "", nil, errors.New("archive_hash 不能为空")
	}
	channel, err := DefaultObsChannel(in.EventType, in.Channel)
	if err != nil {
		return "", nil, err
	}
	meta, err := json.Marshal(evidenceMeta{
		ProposalEligible: in.ProposalEligible,
		Fidelity:         in.Fidelity,
		Adapter:          in.Adapter,
	})
	if err != nil {
		return "", nil, err
	}
	return channel, meta, nil
}

// captureEvidenceTx 是单条与批量共用的事务体，不负责 Commit。
// 判重命中时返回已有行且不推进水位，与重构前行为逐字一致。
func captureEvidenceTx(tx *sql.Tx, root string, external bool, in EvidenceCaptureInput, channel string, meta []byte, now int64) (EvidenceRecord, bool, error) {
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

	if !in.SkipArchiveLedger {
		toSeq := in.ToSeq
		if toSeq <= 0 {
			toSeq = seq
		}
		rawCount := in.RawCount
		if rawCount <= 0 {
			rawCount = 1
		}
		if _, err := tx.Exec(`INSERT INTO evidence_archives(
			archive_hash, source_id, generation, from_seq, to_seq, raw_count, byte_size,
			path, raw_hash_concat, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			in.ArchiveHash, in.SourceID, gen, seq, toSeq, rawCount, in.ByteSize,
			in.ArchivePath, in.RawHashConcat, now); err != nil {
			return EvidenceRecord{}, false, fmt.Errorf("写 archive 账失败: %w", err)
		}
	}

	id := "ev_" + newID()
	// 事件名不是内容（design-capture §0/§4.3）：observations.event 列已记录
	// 事件类型，空摘要 + FidelityNone 共同表达「无正文」。Engine 传来空 Summary
	// 即代表确实没有正文，不得再用 eventType 或 "evidence" 伪造展示内容。
	// 这是与 internal/evidence/engine.go:summarizeRaw 同一语义的第二处收口。
	summary := strings.TrimSpace(in.Summary)
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
	idx := observationIndexText(summary)
	storedSummary, storedIdx := summary, idx
	var summaryRef, summaryHash string
	var payloadBytes int
	if external {
		day := time.Unix(now, 0).Format("2006/01/02")
		summarySeq, err := nextSummarySeqTx(tx, day)
		if err != nil {
			return EvidenceRecord{}, false, err
		}
		summaryRef, summaryHash, payloadBytes, err = AppendSummaryLine(root, now, SummaryLine{
			V: 1, Seq: summarySeq, ID: id, Summary: summary, SummaryIdx: idx,
		})
		if err != nil {
			return EvidenceRecord{}, false, err
		}
		storedSummary, storedIdx = "", ""
	}
	if _, err := tx.Exec(`INSERT INTO observations(
		id, session_id, project, harness, event, tool_name, summary, summary_idx,
		summary_ref, summary_hash, payload_bytes, raw_ref,
		content_hash, created_at, meta_json, channel, event_id, event_uid, source_id,
		source_generation, source_seq, occurred_at_ns, archive_hash, raw_record_hash, parser_version
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, sessionID, in.Project, in.Harness, in.EventType, "", storedSummary, storedIdx,
		summaryRef, summaryHash, payloadBytes, locator, obsHash(in.EventType, "", summary),
		now, string(meta), channel, "", in.EventUID, in.SourceID, gen, seq, occurred,
		in.ArchiveHash, in.RawRecordHash, in.ParserVersion); err != nil {
		return EvidenceRecord{}, false, fmt.Errorf("写 canonical observation 失败: %w", err)
	}
	if external {
		if err := obsFTSPut(tx, id, idx); err != nil {
			return EvidenceRecord{}, false, fmt.Errorf("写观测检索索引失败: %w", err)
		}
	}

	newTip := evidenceTip(tip, in.EventUID, in.ArchiveHash, seq)
	if _, err := tx.Exec(`UPDATE evidence_sources
		SET last_seq = ?, source_tip = ?, updated_at = ? WHERE id = ?`,
		seq, newTip, now, in.SourceID); err != nil {
		return EvidenceRecord{}, false, fmt.Errorf("推进证据源水位失败: %w", err)
	}
	return EvidenceRecord{
		ID: id, EventUID: in.EventUID, SourceID: in.SourceID, Generation: gen, Seq: seq,
		OccurredAtNS: occurred, SessionID: sessionID, Project: in.Project, Harness: in.Harness,
		EventType: in.EventType, ArchiveHash: in.ArchiveHash, RawRecordHash: in.RawRecordHash,
		RawLocator: locator, Summary: summary,
		SummaryRef: summaryRef, SummaryHash: summaryHash, PayloadBytes: payloadBytes,
		ParserVersion:    in.ParserVersion,
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
		&rec.SummaryRef, &rec.SummaryHash, &rec.PayloadBytes,
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
// 连续性：窗口必须从 cursor_seq+1 开始逐条递增；遇到序号洞时把缺失区间记入
// evidence_gaps 并把 cursor 推到洞尾，同一次调用内继续构建下一窗（最多
// maxEvidenceGapHops 次）。洞不跨源拼窗，也不阻塞健康源。
//
// 上限：limit（默认 50）与 maxBytes（默认 1MiB，按 gzip segment 的 byte_size 计）
// 先到先停；即使单条超过 maxBytes 也至少返回 1 条，避免永远卡住。
//
// byte_size 量的是归档段（原始语料），不是进模型的正文——正文由
// evidence.ProposalPayloadRunes(400) × limit 约束。所以调大 maxBytes 只让
// 单窗覆盖更多证据，不放大单次调用成本；它是「一窗代表多少语料」的开关。
func (s *Store) NextEvidenceBundle(limit int, maxBytes int64) (*EvidenceBundleRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
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
	// 最老源优先：与 NextRunnableProposalBundle 的「全局最老 + 按源公平」语义一致。
	// 不能按积压量升序（短积压优先）：活跃项目的 hook 源每轮都在新增，会长期占满
	// 队头，把 created_at 更早、积压几千条的 transcript 源永久挤出候选集——实测
	// 86 个 transcript 源 cursor 恒为 0，一个 Bootstrap 都没生成。单轮预算
	// （--max-bundles 600）足够时，大源会在本轮内连续推进，不存在「霸占队头」；
	// 积压相同再按 id 兜底，保证确定性。
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

// maxEvidenceGapHops 是单次 NextEvidenceBundle 连续跳过洞的次数上限。
//
// 每个洞都对应一次写事务；若上游出现大量互不相邻的缺失行（或并发删除），
// 不设上限会让一次取窗退化成全表扫描。超界时返回 nil，下一轮从这里继续。
const maxEvidenceGapHops = 8

// EvidenceGap 是一条已处置的序号洞：cursor 已越过 [FromSeq, ToSeq]，
// 这段区间不会再产出证据。它不是 Bundle，不参与 Claim/重试/隔离。
type EvidenceGap struct {
	ID         string
	SourceID   string
	Generation int
	FromSeq    int
	ToSeq      int
	DetectedAt int64
}

// ListEvidenceGaps 返回最近的序号洞（detected_at 倒序；同秒按 source/from 稳定排序）。
func (s *Store) ListEvidenceGaps(limit int) ([]EvidenceGap, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, source_id, generation, from_seq, to_seq, detected_at
		FROM evidence_gaps ORDER BY detected_at DESC, source_id ASC, from_seq ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvidenceGap
	for rows.Next() {
		var g EvidenceGap
		if err := rows.Scan(&g.ID, &g.SourceID, &g.Generation, &g.FromSeq, &g.ToSeq, &g.DetectedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// buildEvidenceBundle 构建窗口；若 cursor+1 处缺失，先记录序号洞并把 cursor 推到洞尾，
// 同一次调用内继续尝试下一窗（最多 maxEvidenceGapHops 次）。
//
// 洞的判定：expected 缺失到「下一条已存在 seq - 1」；一条更后的行都没有时，
// 缺失区间是 expected..last_seq（捕获水位仍大 = 行被删/丢，等待已无意义）。
// 洞的写入与 cursor 推进必须同事务，否则「跳过但没记账」不可审计。
func (s *Store) buildEvidenceBundle(src EvidenceSourceState, limit int, maxBytes int64) (*EvidenceBundleRecord, error) {
	cursor := src.CursorSeq
	for hop := 0; hop < maxEvidenceGapHops; hop++ {
		items, err := s.evidenceItemsAfter(src, cursor, limit)
		if err != nil {
			return nil, err
		}

		expected := cursor + 1
		var window []evidenceWindowItem
		var bytes int64
		gapFrom, gapTo := 0, 0
		for _, it := range items {
			if it.seq != expected {
				// 窗口必须连续：洞落在窗口起点时先记洞跳过；落在中段时先把
				// 已连续的部分发出去（同一次调用内不再跨越洞拼窗）。
				if len(window) == 0 {
					gapFrom, gapTo = expected, it.seq-1
				}
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
		if gapFrom > 0 {
			next, err := s.skipEvidenceGap(src, cursor, gapFrom, gapTo)
			if err != nil {
				return nil, err
			}
			cursor = next
			continue
		}
		if len(window) == 0 {
			if src.LastSeq > cursor {
				// 一条行都没有：整段 missing 到捕获水位，cursor 直接追平。
				next, err := s.skipEvidenceGap(src, cursor, cursor+1, src.LastSeq)
				if err != nil {
					return nil, err
				}
				cursor = next
				continue
			}
			// 真正没有新证据：与旧返回语义一致。
			return nil, nil
		}
		return s.commitEvidenceBundle(src, cursor, window)
	}
	return nil, nil
}

func (s *Store) evidenceItemsAfter(src EvidenceSourceState, cursor, limit int) ([]evidenceWindowItem, error) {
	rows, err := s.db.Query(`SELECT o.id, o.event_uid, o.source_seq, o.archive_hash,
			COALESCE(a.byte_size, 0)
		FROM observations o
		LEFT JOIN evidence_archives a ON a.archive_hash = o.archive_hash
		WHERE o.source_id = ? AND o.source_generation = ? AND o.source_seq > ?
		ORDER BY o.source_seq ASC LIMIT ?`,
		src.ID, src.Generation, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []evidenceWindowItem
	for rows.Next() {
		var it evidenceWindowItem
		if err := rows.Scan(&it.id, &it.eventUID, &it.seq, &it.archiveHash, &it.byteSize); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// skipEvidenceGap 在同一事务里记录洞并推进 cursor。cursor 用 CAS 校验，
// 与建窗共用 ErrEvidenceCursorConflict 语义。
func (s *Store) skipEvidenceGap(src EvidenceSourceState, cursor, from, to int) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var curSeq int
	if err := tx.QueryRow(`SELECT cursor_seq FROM evidence_sources WHERE id = ? AND generation = ?`,
		src.ID, src.Generation).Scan(&curSeq); err != nil {
		return 0, err
	}
	if curSeq != cursor {
		return 0, ErrEvidenceCursorConflict
	}
	now := time.Now().Unix()
	if err := recordEvidenceGapTx(tx, src.ID, src.Generation, from, to, now); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`UPDATE evidence_sources SET cursor_seq = ?, updated_at = ?
		WHERE id = ? AND generation = ? AND cursor_seq = ?`,
		to, now, src.ID, src.Generation, cursor)
	if err != nil {
		return 0, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, err
	} else if n == 0 {
		return 0, ErrEvidenceCursorConflict
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return to, nil
}

// recordEvidenceGapTx 幂等写洞：同一 (source, generation, from_seq) 只保留首条，
// 重复检测不会因 UNIQUE 失败而阻塞建窗。
func recordEvidenceGapTx(tx *sql.Tx, sourceID string, gen, from, to int, now int64) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO evidence_gaps(
		id, source_id, generation, from_seq, to_seq, detected_at
	) VALUES (?,?,?,?,?,?)`,
		evidenceGapID(sourceID, gen, from), sourceID, gen, from, to, now); err != nil {
		return fmt.Errorf("写序号洞记录失败: %w", err)
	}
	return nil
}

// commitEvidenceBundle 在一个事务里插 Bundle、条目并推进 cursor（fromSeq 为窗口起点前的游标）。
func (s *Store) commitEvidenceBundle(src EvidenceSourceState, fromSeq int, window []evidenceWindowItem) (*EvidenceBundleRecord, error) {
	toSeq := window[len(window)-1].seq
	bundle := EvidenceBundleRecord{
		ID:               evidenceBundleID(src.ID, src.Generation, fromSeq, toSeq),
		SourceID:         src.ID,
		Generation:       src.Generation,
		FromSeqExclusive: fromSeq,
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
	if curSeq != fromSeq {
		return nil, ErrEvidenceCursorConflict
	}

	now := time.Now().Unix()
	res, err := tx.Exec(`INSERT OR IGNORE INTO evidence_bundles(
		id, source_id, generation, from_seq_exclusive, to_seq_inclusive, evidence_hash,
		source_tip, status, attempt, not_before, lease_until, lease_owner, last_run_id,
		created_at, updated_at
	) VALUES (?,?,?,?,?,?,?,'pending',0,0,0,'','',?,?)`,
		bundle.ID, src.ID, src.Generation, fromSeq, toSeq, bundle.EvidenceHash,
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
		toSeq, now, src.ID, src.Generation, fromSeq)
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

func evidenceGapID(sourceID string, gen, from int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("gap|%s|%d|%d", sourceID, gen, from)))
	return "gap_" + hex.EncodeToString(sum[:])[:24]
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
