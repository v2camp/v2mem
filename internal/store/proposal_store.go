package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// T2 Proposal 执行器的落库面：Bundle 租约、运行账目、候选与 candidate_evidence。
//
// 分工：
//   - evidence 包负责「取连续窗口」并把 cursor 推进到窗口末尾；
//   - 本文件负责「窗口是否已被消费」——这是一个独立于 cursor 的完成水位：
//     ok/noop 才算消费，fail/partial/backlog 保持可重试，绝不跳过。
//
// 为什么要租约而不是直接跑：两个进程同时消费同一窗口会生成两套候选，
// 其中一套的 evidence 引用还会指向已消费的窗口。租约把「谁在跑」写进
// lease_owner/lease_until，attempt 单调递增，not_before 负责退避。

var (
	// ErrProposalBundleLeased 表示 Bundle 仍被租约占用，或还没到 not_before。
	ErrProposalBundleLeased = errors.New("证据 Bundle 正被租用或未到重试时间")
	// ErrProposalBundleLeaseLost 表示完成时租约已不在自己手里。
	ErrProposalBundleLeaseLost = errors.New("证据 Bundle 租约已丢失")
)

// proposalOpenStatuses 是「尚未消费、还需要重试」的 Bundle 状态。
//
// quarantined 不在其中：它需要人工处理，不能让一个坏窗口永久堵住整条队列。
const proposalOpenStatuses = `('pending','leased','fail','partial','backlog')`

// ProposalBundle 是一段被执行器认领的连续窗口。
type ProposalBundle struct {
	ID               string
	SourceID         string
	Generation       int
	FromSeqExclusive int
	ToSeqInclusive   int
	EvidenceHash     string
	SourceTip        string
	Status           string
	Attempt          int
	NotBefore        int64
	LeaseUntil       int64
	LeaseOwner       string
	LastRunID        string
	EvidenceIDs      []string
}

const proposalBundleColumns = `id, source_id, generation, from_seq_exclusive,
	to_seq_inclusive, evidence_hash, source_tip, status, attempt, not_before,
	lease_until, lease_owner, last_run_id`

func scanProposalBundle(row rowScanner) (*ProposalBundle, error) {
	var b ProposalBundle
	err := row.Scan(&b.ID, &b.SourceID, &b.Generation, &b.FromSeqExclusive,
		&b.ToSeqInclusive, &b.EvidenceHash, &b.SourceTip, &b.Status, &b.Attempt,
		&b.NotBefore, &b.LeaseUntil, &b.LeaseOwner, &b.LastRunID)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// NextOpenProposalBundle 返回队列里最老的、尚未终结的 Bundle。
//
// 只返回最老的一条，而不是「所有可跑」：fail/partial 的窗口必须挡住后面的窗口，
// 否则失败的证据会被新证据永久越过——连续语义就没了。
func (s *Store) NextOpenProposalBundle() (*ProposalBundle, bool, error) {
	b, err := scanProposalBundle(s.db.QueryRow(`SELECT ` + proposalBundleColumns + `
		FROM evidence_bundles WHERE status IN ` + proposalOpenStatuses + `
		ORDER BY created_at ASC, id ASC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	ids, err := s.proposalBundleEvidenceIDs(b.ID)
	if err != nil {
		return nil, false, err
	}
	b.EvidenceIDs = ids
	return b, true, nil
}

// LoadProposalBundle 按 id 读 Bundle（含条目 id）。
func (s *Store) LoadProposalBundle(id string) (*ProposalBundle, error) {
	b, err := scanProposalBundle(s.db.QueryRow(`SELECT `+proposalBundleColumns+`
		FROM evidence_bundles WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrEvidenceBundleNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	ids, err := s.proposalBundleEvidenceIDs(id)
	if err != nil {
		return nil, err
	}
	b.EvidenceIDs = ids
	return b, nil
}

func (s *Store) proposalBundleEvidenceIDs(bundleID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT evidence_id FROM evidence_bundle_items
		WHERE bundle_id = ? ORDER BY ordinal ASC`, bundleID)
	return scanStringColumn(rows, err)
}

// ClaimProposalBundle 用一条 UPDATE 原子认领 Bundle。
//
// 幂等关键：attempt 只在认领成功时 +1；重复认领（租约未过期）返回
// ErrProposalBundleLeased，而不是悄悄再跑一次。owner 为空时用固定值，
// 保证「没给 owner」也不会把租约写成空串而永久可抢。
func (s *Store) ClaimProposalBundle(id, owner string, now int64, ttl time.Duration) (*ProposalBundle, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("bundle id 不能为空")
	}
	if now <= 0 {
		now = time.Now().Unix()
	}
	if owner == "" {
		owner = "proposal"
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	until := now + int64(ttl/time.Second)
	if until <= now {
		until = now + 1
	}
	res, err := s.db.Exec(`UPDATE evidence_bundles
		SET status='leased', lease_until=?, lease_owner=?, attempt=attempt+1, updated_at=?
		WHERE id = ? AND status IN `+proposalOpenStatuses+`
		  AND not_before <= ?
		  AND (lease_until <= ? OR lease_owner = '')`,
		until, owner, now, id, now, now)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// 区分「不存在」与「被租用/未到时间」：两者的调用方处置完全不同。
		if _, lerr := s.LoadProposalBundle(id); lerr != nil {
			return nil, lerr
		}
		return nil, ErrProposalBundleLeased
	}
	return s.LoadProposalBundle(id)
}

// FinishProposalBundle 落完成状态并释放租约。
//
// ok/noop 是终态，notBefore 传 0；fail/partial/backlog 保留重试资格，
// notBefore 由调用方按 attempt 退避。重复完成同一 run 是幂等的。
func (s *Store) FinishProposalBundle(id, owner, status string, notBefore int64, runID string) error {
	switch status {
	case "ok", "noop", "fail", "partial", "backlog":
	default:
		return fmt.Errorf("非法的 Bundle 完成状态: %q", status)
	}
	if owner == "" {
		owner = "proposal"
	}
	now := time.Now().Unix()
	cur, err := s.LoadProposalBundle(id)
	if err != nil {
		return err
	}
	if cur.Status == status && cur.LastRunID == runID && runID != "" {
		return nil
	}
	if cur.Status != "leased" || cur.LeaseOwner != owner {
		return fmt.Errorf("%w: status=%s owner=%s", ErrProposalBundleLeaseLost, cur.Status, cur.LeaseOwner)
	}
	res, err := s.db.Exec(`UPDATE evidence_bundles
		SET status=?, not_before=?, lease_until=0, lease_owner='', last_run_id=?, updated_at=?
		WHERE id=? AND status='leased' AND lease_owner=?`,
		status, notBefore, runID, now, id, owner)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrProposalBundleLeaseLost, id)
	}
	return nil
}

// ProposalRunRecord 是一次 Proposal 运行（distill_runs 的扩展列）。
type ProposalRunRecord struct {
	ID                string
	SessionID         string
	Provider          string
	Status            string
	Error             string
	CandCount         int
	StartedAt         int64
	FinishedAt        int64
	BundleID          string
	Executor          string
	Model             string
	ModelVersion      string
	PromptVersion     string
	SchemaVersion     int
	GeneratedCount    int
	ValidCount        int
	InvalidCount      int
	DuplicateCount    int
	Attempt           int
	SourceFingerprint string
}

// SaveProposalRun 写一条带 Bundle/计数/attempt 的运行账。
//
// 与旧 SaveDistillRun 分开：旧函数的 window_key 是「会话窗口」语义，
// 新 Proposal 用 bundle_id + attempt 表达「哪个连续窗口的第几次尝试」，
// 两者混用会让「是否已消费」重新退回窗口键，正是 T1.1 明令禁止的。
func (s *Store) SaveProposalRun(r ProposalRunRecord) error {
	if r.ID == "" {
		r.ID = newID()
	}
	if r.SessionID == "" {
		r.SessionID = ""
	}
	now := time.Now().Unix()
	if r.StartedAt == 0 {
		r.StartedAt = now
	}
	if r.FinishedAt == 0 {
		r.FinishedAt = now
	}
	for i := 0; i < 3; i++ {
		_, err := s.db.Exec(`INSERT INTO distill_runs(
			id, session_id, provider, status, error, cand_count, started_at, finished_at,
			bundle_id, executor, model, model_version, prompt_version, schema_version,
			generated_count, valid_count, invalid_count, duplicate_count, attempt,
			source_fingerprint
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.SessionID, r.Provider, r.Status, r.Error, r.CandCount,
			r.StartedAt, r.FinishedAt, r.BundleID, r.Executor, r.Model, r.ModelVersion,
			r.PromptVersion, r.SchemaVersion, r.GeneratedCount, r.ValidCount,
			r.InvalidCount, r.DuplicateCount, r.Attempt, r.SourceFingerprint)
		if err == nil {
			return nil
		}
		if strings.Contains(err.Error(), "started_at") {
			r.StartedAt++
			r.FinishedAt = r.StartedAt
			r.ID = newID()
			continue
		}
		return err
	}
	return errors.New("写 Proposal 运行账失败：started_at 冲突重试耗尽")
}

// ProposalRuns 按 Bundle 读运行账，按开始时间正序。
func (s *Store) ProposalRuns(bundleID string) ([]ProposalRunRecord, error) {
	rows, err := s.db.Query(`SELECT id, session_id, provider, status, error, cand_count,
			started_at, finished_at, bundle_id, executor, model, model_version,
			prompt_version, schema_version, generated_count, valid_count,
			invalid_count, duplicate_count, attempt, source_fingerprint
		FROM distill_runs WHERE bundle_id = ? ORDER BY started_at ASC, id ASC`, bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProposalRunRecord
	for rows.Next() {
		var r ProposalRunRecord
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Provider, &r.Status, &r.Error,
			&r.CandCount, &r.StartedAt, &r.FinishedAt, &r.BundleID, &r.Executor,
			&r.Model, &r.ModelVersion, &r.PromptVersion, &r.SchemaVersion,
			&r.GeneratedCount, &r.ValidCount, &r.InvalidCount, &r.DuplicateCount,
			&r.Attempt, &r.SourceFingerprint); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CandidateEvidence 是一条候选的证据引用。
type CandidateEvidence struct {
	CandidateID   string
	EvidenceID    string
	EventUID      string
	SourceID      string
	SourceSeq     int
	ArchiveHash   string
	RawRecordHash string
	EvidenceRole  string
	QuoteHash     string
	CreatedAt     int64
}

// ProposalCandidateInput 是 Proposal 候选的写入参数。
//
// 与 CandidateInput 分开：Proposal 候选必须携带 Bundle/Run/证据引用，
// 缺了它们就退化成「无来源的候选」，T3 无法做自动提交判定。
type ProposalCandidateInput struct {
	SessionID   string
	Project     string
	BundleID    string
	RunID       string
	ProposalIdx int
	Kind        string
	Scope       string
	Content     string
	SubjectKey  string
	TaskState   string
	Rationale   string
	ReviewClass string
	Revision    int
	Salience    float64
	ObsIDs      []string
	Evidence    []CandidateEvidence
}

// ProposalCandidateResult 描述写入结果。Created=false 表示命中同会话同内容的去重。
type ProposalCandidateResult struct {
	ID             string
	Created        bool
	MergedEvidence int
}

// InsertProposalCandidate 在单事务里写 candidates 与 candidate_evidence。
//
// 重复候选（同会话同内容 hash，且 pending/accepted）不新增行，只把新证据
// INSERT OR IGNORE 合并进去；重复运行不会让同一候选的证据链丢失。
func (s *Store) InsertProposalCandidate(in ProposalCandidateInput) (*ProposalCandidateResult, error) {
	in.Content = strings.TrimSpace(in.Content)
	if in.Content == "" {
		return nil, errors.New("候选内容为空")
	}
	switch in.Kind {
	case "preference", "decision", "pitfall", "task", "fact":
	default:
		return nil, fmt.Errorf("候选 kind 无效: %q", in.Kind)
	}
	for _, ev := range in.Evidence {
		if strings.TrimSpace(ev.EvidenceID) == "" {
			return nil, errors.New("候选证据引用缺少 evidence_id")
		}
	}
	salience := in.Salience
	if salience <= 0 || salience > 1 {
		salience = 0.5
	}
	hash := HashOf(in.Content)
	for attempt := 0; attempt < 2; attempt++ {
		res, err := s.insertProposalCandidateTx(in, hash, salience)
		if err == nil {
			return res, nil
		}
		// 并发插入命中唯一索引：退回「合并证据」路径，而不是把重复报成失败。
		if !isUniqueViolation(err) {
			return nil, err
		}
	}
	return nil, errors.New("写 Proposal 候选失败：重复冲突重试耗尽")
}

func (s *Store) insertProposalCandidateTx(in ProposalCandidateInput, hash string, salience float64) (*ProposalCandidateResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	var existing string
	err = tx.QueryRow(`SELECT id FROM candidates
		WHERE session_id = ? AND content_hash = ? AND status IN ('pending','accepted')
		ORDER BY created_at DESC, id DESC LIMIT 1`, in.SessionID, hash).Scan(&existing)
	switch {
	case err == nil:
		merged, err := mergeCandidateEvidenceTx(tx, existing, in.Evidence, in.ObsIDs, now)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &ProposalCandidateResult{ID: existing, Created: false, MergedEvidence: merged}, nil
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, err
	}

	id := newID()
	if _, err := tx.Exec(`INSERT INTO candidates(
		id, session_id, project, kind, content, content_hash, status, source, channel,
		salience, obs_ids_json, created_at, run_id, bundle_id, proposal_index, scope,
		subject_key, task_state, rationale, review_class, revision, schema_version
	) VALUES (?,?,?,?,?,?, 'pending', 'distill', 'agent', ?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, in.SessionID, in.Project, in.Kind, in.Content, hash, salience,
		marshalObsIDs(in.ObsIDs), now, in.RunID, in.BundleID, in.ProposalIdx,
		in.Scope, in.SubjectKey, in.TaskState, in.Rationale, in.ReviewClass,
		in.Revision, 1); err != nil {
		return nil, err
	}
	for _, ev := range in.Evidence {
		role := ev.EvidenceRole
		if role == "" {
			role = "direct"
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO candidate_evidence(
			candidate_id, evidence_id, event_uid, source_id, source_seq,
			archive_hash, raw_record_hash, evidence_role, quote_hash, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, ev.EvidenceID, ev.EventUID, ev.SourceID, ev.SourceSeq,
			ev.ArchiveHash, ev.RawRecordHash, role, ev.QuoteHash, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ProposalCandidateResult{ID: id, Created: true}, nil
}

func mergeCandidateEvidenceTx(tx *sql.Tx, candidateID string, evs []CandidateEvidence, obsIDs []string, now int64) (int, error) {
	merged := 0
	for _, ev := range evs {
		role := ev.EvidenceRole
		if role == "" {
			role = "direct"
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO candidate_evidence(
			candidate_id, evidence_id, event_uid, source_id, source_seq,
			archive_hash, raw_record_hash, evidence_role, quote_hash, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			candidateID, ev.EvidenceID, ev.EventUID, ev.SourceID, ev.SourceSeq,
			ev.ArchiveHash, ev.RawRecordHash, role, ev.QuoteHash, now)
		if err != nil {
			return merged, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return merged, err
		} else {
			merged += int(n)
		}
	}
	if len(obsIDs) == 0 {
		return merged, nil
	}
	var raw string
	if err := tx.QueryRow(`SELECT obs_ids_json FROM candidates WHERE id = ?`, candidateID).Scan(&raw); err != nil {
		return merged, err
	}
	cur := unmarshalObsIDs(raw)
	seen := make(map[string]bool, len(cur))
	for _, id := range cur {
		seen[id] = true
	}
	changed := false
	for _, id := range obsIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		cur = append(cur, id)
		changed = true
	}
	if changed {
		if _, err := tx.Exec(`UPDATE candidates SET obs_ids_json = ? WHERE id = ?`,
			marshalObsIDs(cur), candidateID); err != nil {
			return merged, err
		}
	}
	return merged, nil
}

// ListCandidateEvidence 按创建顺序读候选的证据引用。
func (s *Store) ListCandidateEvidence(candidateID string) ([]CandidateEvidence, error) {
	rows, err := s.db.Query(`SELECT candidate_id, evidence_id, event_uid, source_id,
			source_seq, archive_hash, raw_record_hash, evidence_role, quote_hash, created_at
		FROM candidate_evidence WHERE candidate_id = ? ORDER BY created_at ASC, evidence_id ASC`,
		candidateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CandidateEvidence
	for rows.Next() {
		var ev CandidateEvidence
		if err := rows.Scan(&ev.CandidateID, &ev.EvidenceID, &ev.EventUID, &ev.SourceID,
			&ev.SourceSeq, &ev.ArchiveHash, &ev.RawRecordHash, &ev.EvidenceRole,
			&ev.QuoteHash, &ev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
