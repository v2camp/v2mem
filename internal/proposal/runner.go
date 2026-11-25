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

// MaxAttempts 是内容类失败（fail/partial）允许的最大尝试次数。
//
// 只约束「模型跑起来了但结果不可用」：backlog 表示模型全局不可用，
// 那不是这一窗的错，累计次数再高也不能把整条队列隔离。
const MaxAttempts = 5

// retryDelay 是指数退避：base=1m 时 1/2/3/4 次为 1/2/4/8m，之后封顶 16×base。
//
// 线性退避在反复坏输出时很快退到「每分钟重试一次」，既浪费调用也刷不出新结论；
// 指数退避让第 5 次隔离前的总等待落在半小时量级，配合 MaxAttempts 收敛。
func (r *Runner) retryDelay(attempt int) time.Duration {
	base := r.RetryDelay
	if base <= 0 {
		base = time.Minute
	}
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 4 {
		shift = 4
	}
	return base * time.Duration(1<<shift)
}

func (r *Runner) backlogDelay() time.Duration {
	if r.BacklogDelay > 0 {
		return r.BacklogDelay
	}
	return 10 * time.Minute
}

// maxRunnableHeads 是一次 Step 内最多试几个可运行头。
//
// NextRunnableProposalBundle 已经把退避与租约过滤掉了，所以认领失败只剩竞态
// （另一个 worker 在同一瞬间抢走了同一个头）。重试同一个头没有意义，按候选
// 个数收敛即可：抢输 3 个就交回调用方，下一轮凭年龄重新排队。
const maxRunnableHeads = 3

// Step 认领并处理一个 Bundle。
//
// 调度口径是「按源头」而不是「全局队首」（design-data-lifecycle §10 P1）：
// 每个源只看自己的最老头，头在退避或租约中就整源让位，绝不越过它去新建窗口。
// 这样既保住了 §8.1「连续水位不得越过失败窗」——失败窗被同源新窗越过就等于
// 永久跳过证据；也避免一个源 16 分钟的退避把全部源一起挡住。
//
// 三层顺序不能换：先找可运行的头 → 再判断「还有积压但全不可运行」→
// 最后才允许 Reader.NextBundle 新建窗口。中间那层若误调 NextBundle，
// 就会在失败头之后造出新窗口，连续语义直接打穿。
func (r *Runner) Step(_ context.Context) (*Outcome, error) {
	if r.St == nil || r.Reader == nil {
		return nil, errors.New("proposal: Runner 缺少 Store 或 Reader")
	}
	now := r.now().Unix()

	var contended string
	for i := 0; i < maxRunnableHeads; i++ {
		head, ok, err := r.St.NextRunnableProposalBundle(now)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		contended = head.ID
		claimed, err := r.St.ClaimProposalBundle(head.ID, r.Owner, now, r.LeaseTTL)
		if err == nil {
			return r.runClaimed(claimed, false)
		}
		if !errors.Is(err, store.ErrProposalBundleLeased) {
			return nil, err
		}
		// 竞态：这个头已被别人抢走，继续试下一个可运行头，而不是整批停摆。
	}
	if contended != "" {
		return &Outcome{
			Result: Result{Status: StatusBacklog}, BundleID: contended,
			Blocked: "可运行的头在认领时被其它 worker 抢走（认领竞态），本轮让位",
		}, nil
	}

	// 没有可运行的头，但仍有 open 积压：所有源的头都在退避或租约中。
	// 这里绝不能调 Reader.NextBundle——那会越过失败头新建窗口。
	if head, ok, err := r.St.NextOpenProposalBundle(); err != nil {
		return nil, err
	} else if ok {
		return &Outcome{
			Result: Result{Status: StatusBacklog}, BundleID: head.ID,
			Blocked: "所有源的头都在退避或租约中",
		}, nil
	}

	b, err := r.Reader.NextBundle(r.Limit, r.MaxBytes)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return &Outcome{Result: Result{Status: "skipped"}, Blocked: "没有待处理的连续证据窗口"}, nil
	}
	claimed, err := r.St.ClaimProposalBundle(b.ID, r.Owner, now, r.LeaseTTL)
	if err != nil {
		if errors.Is(err, store.ErrProposalBundleLeased) {
			return &Outcome{
				Result: Result{Status: StatusBacklog}, BundleID: b.ID,
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
	// attributed_to 以被引证据为准，不信任模型回填：同一事实若同时引到
	// 用户与助手，按「用户原话优先」记 user（mem0 No Echo Extraction 同义）。
	// 证据类型认不出来时才退回模型给的标注。
	attributedTo := ""
	for _, n := range d.EvidenceIdx {
		s := SpeakerOf(refs[n-1].EventType)
		if s == "" {
			continue
		}
		attributedTo = s
		if s == SpeakerUser {
			break
		}
	}
	if attributedTo == "" {
		attributedTo = d.AttributedTo
	}
	return store.ProposalCandidateInput{
		SessionID: sessionID, Project: project, BundleID: b.ID, RunID: runID,
		ProposalIdx: idx, Kind: d.Kind, Scope: d.Scope,
		Content:      scrub.Apply(d.Content, r.NoScrub),
		SubjectKey:   scrub.Apply(d.SubjectKey, r.NoScrub),
		TaskState:    scrub.Apply(d.TaskState, r.NoScrub),
		Rationale:    scrub.Apply(d.Rationale, r.NoScrub),
		AttributedTo: attributedTo,
		Salience:     0.6, ObsIDs: ids, Evidence: evs,
	}, nil
}

// finish 统一收口：写运行账 → 释放租约。顺序不能反：
// 先落 run 才能让 Bundle 的 last_run_id 指向一条真实账目。
//
// 隔离判定放在这里而不是 classifyOutcome：所有 fail/partial 出口（含
// 证据不一致这类早退）都必须经过同一道超限闸门，避免某个分支绕过它。
func (r *Runner) finish(b *store.ProposalBundle, out *Outcome, bundleStatus string, delay time.Duration) (*Outcome, error) {
	if (bundleStatus == "fail" || bundleStatus == "partial") && b.Attempt >= MaxAttempts {
		bundleStatus = "quarantined"
		out.Status = StatusQuarantined
		delay = 0
	}
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
// 超限的 fail/partial 由 finish 统一改写成 quarantined 终态。
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
		// 与 BuildPrompt / executor 共用单一判定源，避免 Fidelity 接入后出现
		// 「runner 算有、执行器算无」的双轨语义。
		if proposalEligible(r) {
			n++
		}
	}
	return n
}

func bundleSession(refs []evidence.Ref) string {
	for _, r := range refs {
		if proposalEligible(r) && strings.TrimSpace(r.SessionID) != "" {
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
