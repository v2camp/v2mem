package proposal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/scrub"
	"github.com/v2camp/v2mem/internal/store"
)

// Outcome 是一次 Proposal 步骤的完整账目。
//
// Result 是冻结契约里的运行结果；这里补上落库与 Bundle 状态，便于 CLI、
// 审计和测试看到「生成了几条 / 入了几条 / 窗口最后是什么状态」。
type Outcome struct {
	Result
	BundleID     string
	SessionID    string
	BundleStatus string
	Inserted     int
	Merged       int
	Blocked      string
	Error        string
	NotBefore    int64
	Attempt      int
}

// Runner 消费连续 Bundle 并产候选。它只写 candidates / candidate_evidence /
// distill_runs 与 evidence_bundles 的完成状态，绝不写 memories。
type Runner struct {
	St     *store.Store
	Reader evidence.Reader
	Chain  []Executor

	Owner        string
	LeaseTTL     time.Duration
	RetryDelay   time.Duration
	BacklogDelay time.Duration
	Limit        int
	MaxBytes     int64
	NoScrub      bool
	Now          func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) retryDelay(attempt int) time.Duration {
	base := r.RetryDelay
	if base <= 0 {
		base = time.Minute
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return base * time.Duration(attempt)
}

func (r *Runner) backlogDelay() time.Duration {
	if r.BacklogDelay > 0 {
		return r.BacklogDelay
	}
	return 10 * time.Minute
}

// Step 认领并处理一个 Bundle。
//
// 队列顺序是严格的：只要还有未终结的 Bundle，就先重试最老的那一个，
// 成功了才允许调用 NextBundle 取下一窗。这样 fail/partial 不会因为
// 「后面还有新证据」而被越过，连续水位实际上由 Bundle 状态守护。
func (r *Runner) Step(_ context.Context) (*Outcome, error) {
	if r.St == nil || r.Reader == nil {
		return nil, errors.New("proposal: Runner 缺少 Store 或 Reader")
	}
	now := r.now().Unix()

	head, ok, err := r.St.NextOpenProposalBundle()
	if err != nil {
		return nil, err
	}
	var id string
	if ok {
		id = head.ID
	} else {
		b, err := r.Reader.NextBundle(r.Limit, r.MaxBytes)
		if err != nil {
			return nil, err
		}
		if b == nil {
			return &Outcome{Result: Result{Status: "skipped"}, Blocked: "没有待处理的连续证据窗口"}, nil
		}
		id = b.ID
	}

	claimed, err := r.St.ClaimProposalBundle(id, r.Owner, now, r.LeaseTTL)
	if err != nil {
		if errors.Is(err, store.ErrProposalBundleLeased) {
			return &Outcome{
				Result: Result{Status: StatusBacklog}, BundleID: id,
				Blocked: "Bundle 仍在租约中或未到 not_before",
			}, nil
		}
		return nil, err
	}
	return r.runClaimed(claimed, false)
}

// StepBundle 认领指定 Bundle。显式 bundle 用于补跑/排障；
// 它不改变队列顺序，也不会跳过更老的未终结窗口（调用方明确要求除外）。
func (r *Runner) StepBundle(_ context.Context, bundleID string) (*Outcome, error) {
	if r.St == nil || r.Reader == nil {
		return nil, errors.New("proposal: Runner 缺少 Store 或 Reader")
	}
	claimed, err := r.St.ClaimProposalBundle(strings.TrimSpace(bundleID), r.Owner, r.now().Unix(), r.LeaseTTL)
	if err != nil {
		if errors.Is(err, store.ErrProposalBundleLeased) {
			return &Outcome{Result: Result{Status: StatusBacklog}, BundleID: bundleID,
				Blocked: "Bundle 仍在租约中或未到 not_before"}, nil
		}
		return nil, err
	}
	return r.runClaimed(claimed, false)
}

// Peek 是 dry-run：不认领、不落库、不改 Bundle 状态，只报告 would_generate。
//
// 必须指定已存在的 Bundle：dry-run 也不允许通过 NextBundle 推进 cursor，
// 否则「只看一眼」会真实消费掉一个窗口。
func (r *Runner) Peek(_ context.Context, bundleID string) (*Outcome, error) {
	if r.St == nil || r.Reader == nil {
		return nil, errors.New("proposal: Runner 缺少 Store 或 Reader")
	}
	b, err := r.St.LoadProposalBundle(strings.TrimSpace(bundleID))
	if err != nil {
		return nil, err
	}
	if b.Status == "ok" || b.Status == "noop" || b.Status == "quarantined" {
		return &Outcome{Result: Result{Status: StatusNoop}, BundleID: b.ID,
			BundleStatus: b.Status, Blocked: "Bundle 已终结，dry-run 不再生成"}, nil
	}
	return r.runClaimed(b, true)
}

func (r *Runner) runClaimed(b *store.ProposalBundle, dry bool) (*Outcome, error) {
	out := &Outcome{BundleID: b.ID, Attempt: b.Attempt}
	refs, err := r.Reader.ListEvidence(b.ID)
	if err != nil {
		out.Status = StatusFail
		out.Error = err.Error()
		if dry {
			return out, err
		}
		return r.finish(b, out, "fail", 0)
	}
	if len(b.EvidenceIDs) > 0 && len(refs) != len(b.EvidenceIDs) {
		out.Status = StatusFail
		out.Error = fmt.Sprintf("Bundle %s 条目数不一致: refs=%d ids=%d", b.ID, len(refs), len(b.EvidenceIDs))
		if dry {
			return out, errors.New(out.Error)
		}
		return r.finish(b, out, "fail", 0)
	}
	out.SessionID = bundleSession(refs)
	out.RunID = newRunID()

	if countEligible(refs) == 0 {
		// 没有可提案的证据（例如 Proposal 子会话事件）：合法空结果。
		out.Status = StatusNoop
		if dry {
			out.Status = StatusWouldGenerate
			return out, nil
		}
		return r.finish(b, out, "noop", 0)
	}

	res, lastErr, used := r.execute(b.ID)
	out.Result = res
	if !used {
		// 所有 provider 都不可用：这是 backlog，不是 empty。
		out.Status = StatusBacklog
		out.Error = lastErrString(lastErr)
		if dry {
			return out, nil
		}
		return r.finish(b, out, "backlog", r.backlogDelay())
	}

	if dry {
		out.Inserted = len(res.Drafts)
		out.Status = StatusWouldGenerate
		out.Error = lastErrString(lastErr)
		return out, nil
	}

	inserted, merged, duplicate, insertErrs := r.persist(b, refs, res.Drafts, out.RunID)
	out.Inserted, out.Merged, out.Duplicate = inserted, merged, duplicate
	if len(insertErrs) > 0 {
		out.Error = errors.Join(insertErrs...).Error()
	}
	if res.Status == StatusInvalid || res.Status == StatusFail {
		out.Error = lastErrString(lastErr)
	}

	status, bundleStatus := classifyOutcome(res.Status, inserted, len(insertErrs))
	out.Status = status
	var delay time.Duration
	switch bundleStatus {
	case "fail", "partial":
		delay = r.retryDelay(b.Attempt)
	case "backlog":
		delay = r.backlogDelay()
	}
	return r.finish(b, out, bundleStatus, delay)
}

func (r *Runner) execute(bundleID string) (Result, error, bool) {
	var last Result
	var lastErr error
	for _, ex := range r.Chain {
		res, err := ex.Run(bundleID)
		if res.Executor != "" {
			last = res
		}
		if err != nil && isUnavailable(err) {
			lastErr = err
			continue
		}
		// provider 跑起来了：不管结果好坏都不再降级——坏 JSON 不是「没 provider」。
		lastErr = err
		return res, err, true
	}
	return last, lastErr, false
}

func (r *Runner) persist(b *store.ProposalBundle, refs []evidence.Ref, drafts []Draft, runID string) (int, int, int, []error) {
	inserted, merged, duplicate := 0, 0, 0
	var errs []error
	for i, d := range drafts {
		in, err := r.candidateInput(b, refs, d, i+1, runID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		result, err := r.St.InsertProposalCandidate(in)
		if err != nil {
			errs = append(errs, fmt.Errorf("第 %d 条候选落库失败: %w", i+1, err))
			continue
		}
		if result.Created {
			inserted++
		} else {
			duplicate++
			merged += result.MergedEvidence
		}
	}
	return inserted, merged, duplicate, errs
}

func (r *Runner) candidateInput(b *store.ProposalBundle, refs []evidence.Ref, d Draft, idx int, runID string) (store.ProposalCandidateInput, error) {
	ids, err := evidenceIDsFor(d.EvidenceIdx, b.EvidenceIDs)
	if err != nil {
		return store.ProposalCandidateInput{}, err
	}
	evs := make([]store.CandidateEvidence, 0, len(ids))
	sessionID, project := "", ""
	for i, n := range d.EvidenceIdx {
		ref := refs[n-1]
		if i == 0 {
			sessionID, project = ref.SessionID, ref.Project
		}
		evs = append(evs, store.CandidateEvidence{
			EvidenceID: ids[i], EventUID: ref.EventUID, SourceID: ref.SourceID,
			SourceSeq: ref.Seq, ArchiveHash: ref.ArchiveHash,
			RawRecordHash: ref.RawRecordHash, EvidenceRole: "direct",
		})
	}
	if sessionID == "" {
		sessionID = "unknown"
	}
	return store.ProposalCandidateInput{
		SessionID: sessionID, Project: project, BundleID: b.ID, RunID: runID,
		ProposalIdx: idx, Kind: d.Kind, Scope: d.Scope,
		Content:    scrub.Apply(d.Content, r.NoScrub),
		SubjectKey: scrub.Apply(d.SubjectKey, r.NoScrub),
		TaskState:  scrub.Apply(d.TaskState, r.NoScrub),
		Rationale:  scrub.Apply(d.Rationale, r.NoScrub),
		Salience:   0.6, ObsIDs: ids, Evidence: evs,
	}, nil
}

// finish 统一收口：写运行账 → 释放租约。顺序不能反：
// 先落 run 才能让 Bundle 的 last_run_id 指向一条真实账目。
func (r *Runner) finish(b *store.ProposalBundle, out *Outcome, bundleStatus string, delay time.Duration) (*Outcome, error) {
	now := r.now()
	notBefore := int64(0)
	if delay > 0 {
		notBefore = now.Add(delay).Unix()
	}
	out.BundleStatus = bundleStatus
	out.NotBefore = notBefore
	runStatus := out.Status
	if runStatus == StatusWouldGenerate {
		runStatus = StatusOK
	}
	err := r.St.SaveProposalRun(store.ProposalRunRecord{
		SessionID: out.SessionID, Provider: out.Executor, Status: runStatus,
		Error: out.Error, CandCount: out.Inserted,
		StartedAt: now.Unix(), FinishedAt: r.now().Unix(),
		BundleID: b.ID, Executor: out.Executor, SchemaVersion: 1,
		GeneratedCount: out.Generated, ValidCount: out.Valid,
		InvalidCount: out.Invalid, DuplicateCount: out.Duplicate,
		Attempt: b.Attempt, SourceFingerprint: b.EvidenceHash,
	})
	if err != nil {
		return out, err
	}
	if err := r.St.FinishProposalBundle(b.ID, r.owner(), bundleStatus, notBefore, out.RunID); err != nil {
		return out, err
	}
	return out, nil
}

func (r *Runner) owner() string {
	if strings.TrimSpace(r.Owner) != "" {
		return r.Owner
	}
	return "proposal"
}

// classifyOutcome 把执行器状态与落库结果合成最终语义。
//
// 关键：invalid/fail/partial/backlog 都不是终态；只有 ok/noop 消费窗口。
func classifyOutcome(execStatus string, inserted, insertErrCount int) (string, string) {
	switch execStatus {
	case StatusNoop:
		return StatusNoop, "noop"
	case StatusOK:
		if insertErrCount == 0 {
			return StatusOK, "ok"
		}
		if inserted == 0 {
			return StatusFail, "fail"
		}
		return StatusPartial, "partial"
	case StatusPartial:
		if inserted == 0 && insertErrCount > 0 {
			return StatusFail, "fail"
		}
		return StatusPartial, "partial"
	case StatusInvalid:
		return StatusInvalid, "fail"
	case StatusBacklog:
		return StatusBacklog, "backlog"
	default:
		return StatusFail, "fail"
	}
}

func isUnavailable(err error) bool {
	return errors.Is(err, ErrNoProvider) ||
		errors.Is(err, ErrHostUnavailable) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

func lastErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func countEligible(refs []evidence.Ref) int {
	n := 0
	for _, r := range refs {
		if r.ProposalEligible {
			n++
		}
	}
	return n
}

func bundleSession(refs []evidence.Ref) string {
	for _, r := range refs {
		if r.ProposalEligible && strings.TrimSpace(r.SessionID) != "" {
			return r.SessionID
		}
	}
	for _, r := range refs {
		if strings.TrimSpace(r.SessionID) != "" {
			return r.SessionID
		}
	}
	return "unknown"
}

func newRunID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("run_%d", time.Now().UnixNano())
	}
	return "run_" + hex.EncodeToString(b)
}
