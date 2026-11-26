package main

// S8：`mem maintain` —— 统一维护门面的第一段（design-data-lifecycle §3、§10 P2）。
//
// 本期承接的是「故障恢复」这一半：
//   - 过期租约不必专门恢复：ClaimProposalBundle / NextRunnableProposalBundle
//     已把 `lease_until <= now` 视为可重认领（进程被杀留下的 leased 会被接管）；
//   - 最老优先连续处理，预算内推进到队列清空、全源让位或出现新隔离为止；
//   - `--force` 只解除退避（not_before=0），不改 attempt——attempt 是「试过几次」的事实；
//   - 成功后写 maintain:last_ok，与 gc/consolidate 同一水位键。
//
// **提交 / 拒绝本期恒为 0**：候选的确定性提交矩阵 + 模型自动裁决由 S10 接入
// （§6），这里既不能假装提交过，也不能省略字段——输出里显式列 0 并注明，
// 让「还没接」与「接了但为 0」在机器侧可区分。
//
// 单实例租约（§3 第 1 步）不在本切片：当前并发保护是 Bundle 级租约，
// 两个 maintain 同时跑只会有一次认领成功，不会重复消费同一窗口。

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/commit"
	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/proposal"
	"github.com/v2camp/v2mem/internal/store"
)

// maintainDefaultMaxBundles 是一次唤醒的默认预算：Hook 唤醒应该短命，
// 剩下的留给下一轮；显式 `--max-bundles 0` 也落回默认，而不是变成「不处理」。
const maintainDefaultMaxBundles = 20

// maintainDefaultMaxAdjudications 是单次自动裁决的默认上限：
// 与 accept --all-pending 的 1000 不同，maintain 是常驻小步，200 条足够一轮灰度，
// 剩下的下一轮继续，避免一次唤醒写库过久。
const maintainDefaultMaxAdjudications = 200

// 裁决范围：this-run 只覆盖本轮产出的 Bundle（默认，最小爆炸半径）；
// all 覆盖当前全部 pending（供灰度/排障显式 opt-in）。
const (
	maintainAdjudicateThisRun = "this-run"
	maintainAdjudicateAll     = "all"
)

// 维护阶段（S15）：`--stage` 只决定「本次跑哪些阶段」，默认 proposal。
// 执行顺序固定为 proposal → adjudicate → consolidate → gc，与书写顺序无关。
const (
	maintainStageProposal    = "proposal"
	maintainStageAdjudicate  = "adjudicate"
	maintainStageConsolidate = "consolidate"
	maintainStageGC          = "gc"
	maintainStageAll         = "all"
)

// maintainStageOrder 是唯一允许的执行顺序：依赖关系（裁决要在候选之后、
// GC 要在归并之后）都编码在这里，不让调用方自己拼顺序。
var maintainStageOrder = []string{
	maintainStageProposal, maintainStageAdjudicate, maintainStageConsolidate, maintainStageGC,
}

// parseMaintainStages 把 `--stage` 规整成固定顺序的阶段列表。
//
// 规则：
//   - 默认 / 空值 = proposal；逗号可多选，去重后按 maintainStageOrder 输出；
//   - all = proposal + consolidate + gc，显式 opt-in；开启 --model-auto 时追加 adjudicate；
//   - 含 proposal 时，--model-auto 在它之后追加 adjudicate（默认路径行为不变）；
//   - 显式点名 adjudicate 必须同时给 --model-auto，否则是用例错误——单跑裁决没有
//     「本轮 Bundle」可裁，必须由用户显式确认归因与范围。
func parseMaintainStages(spec string, modelAuto bool) ([]string, error) {
	if strings.TrimSpace(spec) == "" {
		spec = maintainStageProposal
	}
	parts := strings.Split(spec, ",")
	want := map[string]bool{}
	all := false
	for _, raw := range parts {
		name := strings.ToLower(strings.TrimSpace(raw))
		switch name {
		case "":
			return nil, fmt.Errorf("--stage 含空值（%q），用法：proposal|adjudicate|consolidate|gc|all", spec)
		case maintainStageAll:
			all = true
			want[maintainStageProposal] = true
			want[maintainStageConsolidate] = true
			want[maintainStageGC] = true
		case maintainStageProposal, maintainStageAdjudicate, maintainStageConsolidate, maintainStageGC:
			want[name] = true
		default:
			return nil, fmt.Errorf("--stage 只支持 proposal|adjudicate|consolidate|gc|all，got %q", raw)
		}
	}
	if all && len(parts) > 1 {
		return nil, errors.New("--stage=all 不能与其他阶段同用（它已包含 proposal/consolidate/gc）")
	}
	if want[maintainStageProposal] && modelAuto {
		// proposal 之后的追加动作：显式 --stage 与默认路径共用同一条规则。
		want[maintainStageAdjudicate] = true
	}
	if want[maintainStageAdjudicate] && !modelAuto {
		return nil, errors.New("--stage=adjudicate 需要同时给 --model-auto 与可归因的 --model-tag")
	}
	out := make([]string, 0, len(want))
	for _, name := range maintainStageOrder {
		if want[name] {
			out = append(out, name)
		}
	}
	return out, nil
}

func hasMaintainStage(stages []string, name string) bool {
	for _, s := range stages {
		if s == name {
			return true
		}
	}
	return false
}

// maintainStageReport 是单个阶段的账目。status: ok|error|skipped。
// counts 的键随阶段而定（proposal: processed/ok/…；gc: plans/backups/…）。
type maintainStageReport struct {
	Name   string         `json:"name"`
	Status string         `json:"status"`
	Counts map[string]int `json:"counts,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// maintainLocalGenerator 是「本地模型来源」的构造点。
//
// 生产与 `mem distill --proposal --executor auto` 同一条路径：model.json / 环境变量
// 解析出 provider，失败则返回不可用占位，让链路降级到规则兜底（BuildChain 的链尾）。
// 单测替换它，就能在无模型环境里覆盖 ok / partial / quarantined 分支，
// 不把「本机恰好装了 ollama」变成用例结果的一部分。
var maintainLocalGenerator = func(cfg distill.ModelConfig, model string, timeout time.Duration) proposal.Generator {
	fn, name, err := distill.PromptCompleter(proposalOptions(cfg, "ollama", model, timeout))
	if err != nil {
		return proposal.UnavailableGenerator("local", err)
	}
	return proposal.PromptGenerator(name, fn)
}

// maintainReport 是文本与 `--json` 的共同账目。
//
// Committed 在未开 --model-auto 时恒 0（S10 之前的口径保留）；开启后是自动裁决
// 真正落库的条数。AdjudicateScope 为空即「本轮没跑裁决」，与「跑了但为 0」可区分。
type maintainReport struct {
	DryRun             bool                  `json:"dry_run"`
	CatchUp            bool                  `json:"catch_up"`
	Forced             int                   `json:"forced_retry"`
	Processed          int                   `json:"processed"`
	OK                 int                   `json:"ok"`
	Noop               int                   `json:"noop"`
	Fail               int                   `json:"fail"`
	Partial            int                   `json:"partial"`
	Quarantine         int                   `json:"quarantine"`
	Backlog            int                   `json:"backlog"`
	WouldGenerate      int                   `json:"would_generate"`
	Committed          int                   `json:"committed"`
	Rejected           int                   `json:"rejected"`
	AdjudicateScope    string                `json:"adjudicate_scope"`
	WouldAdjudicate    int                   `json:"would_adjudicate"`
	Adjudicated        int                   `json:"adjudicated"`
	Deferred           int                   `json:"deferred"`
	Conflict           int                   `json:"conflict"`
	AdjudicationErrors int                   `json:"adjudication_errors"`
	Remaining          int                   `json:"remaining"`
	Blocked            string                `json:"blocked,omitempty"`
	Stages             []maintainStageReport `json:"stages,omitempty"`
	Results            []maintainStep        `json:"results,omitempty"`
}

// maintainStep 是单个 Bundle 的处理账目。error 必须逐窗保留：
// 汇成总数就只剩「失败 2」，没人知道是哪一窗、为什么。
type maintainStep struct {
	Bundle       string `json:"bundle"`
	Session      string `json:"session,omitempty"`
	Executor     string `json:"executor,omitempty"`
	Status       string `json:"status"`
	BundleStatus string `json:"bundle_status,omitempty"`
	Inserted     int    `json:"inserted"`
	Attempt      int    `json:"attempt"`
	Error        string `json:"error,omitempty"`
}

func cmdMaintain(args []string) (retErr error) {
	var c common
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	c.register(fs)
	// --catch-up 与默认行为等价：保留它是为了让 Harness / SOP 明示「这是故障恢复」，
	// 而不是新增一条只有名字不同的路径。
	catchUp := fs.Bool("catch-up", false, "故障恢复：恢复过期租约后最老优先重试可恢复窗口（与默认行为等价）")
	dry := fs.Bool("dry-run", false, "只读预演：不认领、不落候选、不写库")
	force := fs.Bool("force", false, "把退避中的 fail/partial/backlog 清零 not_before，本轮立即重试（不改 attempt）")
	maxBundles := fs.Int("max-bundles", maintainDefaultMaxBundles, "本次最多处理几个 Bundle")
	// S10：proposal 之后的自动裁决。默认关——灰度通过前保持 proposal-only。
	modelAuto := fs.Bool("model-auto", false, "proposal 之后按提交矩阵自动裁决候选（默认关；Conflict/Defer 保留）")
	modelTag := fs.String("model-tag", "", "裁决归因 model@promptver（默认取 model.json 的 model_tag）")
	adjudicateScope := fs.String("adjudicate-scope", maintainAdjudicateThisRun, "裁决范围：this-run（只本轮 Bundle）| all（全部 pending）")
	maxAdjudications := fs.Int("max-adjudications", maintainDefaultMaxAdjudications, "单次最多裁决多少条候选")
	// S15：阶段选择。默认 proposal（与历史行为一致）；逗号可多选，顺序固定。
	stageSpec := fs.String("stage", maintainStageProposal, "阶段：proposal|adjudicate|consolidate|gc|all（默认 proposal；逗号可多选）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	stageExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "stage" {
			stageExplicit = true
		}
	})
	stages, err := parseMaintainStages(*stageSpec, *modelAuto)
	if err != nil {
		return err
	}
	runProposal := hasMaintainStage(stages, maintainStageProposal)
	runAdjudicate := hasMaintainStage(stages, maintainStageAdjudicate)

	if *dry && *force {
		return errors.New("--dry-run 与 --force 不能同用：前者只读，后者会写 not_before")
	}
	if *adjudicateScope != maintainAdjudicateThisRun && *adjudicateScope != maintainAdjudicateAll {
		return fmt.Errorf("--adjudicate-scope 只支持 %s|%s，got %q",
			maintainAdjudicateThisRun, maintainAdjudicateAll, *adjudicateScope)
	}
	limit := *maxBundles
	if limit <= 0 {
		limit = maintainDefaultMaxBundles
	}
	adjLimit := *maxAdjudications
	if adjLimit <= 0 {
		adjLimit = maintainDefaultMaxAdjudications
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	// 失败点亮（S11 触达面）：任何已开库后的失败都写 last_fail_at + 简短原因，
	// 下一次 SessionStart 就能在注入行里看到「维护失败」。成功路径由 markMaintainOK
	// 写 last_ok，触达面按 last_ok >= last_fail 判恢复，不需要手动清键。
	defer func() {
		if retErr != nil {
			markMaintainFail(st, retErr)
		}
	}()

	// 只有 proposal/adjudicate 需要模型来源；consolidate/gc 单跑时不为无关的
	// model.json 买单（配置坏了也不该挡住纯文件回收）。
	var tag string
	var runner *proposal.Runner
	if runProposal || runAdjudicate {
		model, timeout := distill.LoadConfig(distill.ConfigPath())
		cfg, err := distill.LoadModelConfig("")
		if err != nil {
			// 与 distill 同一条 fail-closed：非法 model.json 当「没配」会静默外发。
			return fmt.Errorf("读取模型配置失败（%s）: %w", distill.DefaultModelConfigPath(), err)
		}
		// 归因优先级：显式 --model-tag > model.json.model_tag。开启自动裁决却没有可
		// 归因的 tag 时必须报错——写空 tag 等于放弃「按版本回滚」的唯一凭据。
		tag = strings.TrimSpace(*modelTag)
		if tag == "" && *modelAuto {
			tag = strings.TrimSpace(cfg.ModelTag)
		}
		if runAdjudicate && tag == "" {
			return errors.New("--model-auto 需要可归因的 model tag：请传 --model-tag 或在 model.json 配置 model_tag")
		}
		if runProposal {
			ev := evidence.New(st)
			// auto（本地模型）+ 规则兜底：模型不可用时仍能把决策/踩坑类证据落成候选，
			// 而不是让整条队列停在 backlog。
			chain, err := proposal.BuildChain("auto", ev, proposal.Deps{
				Local: maintainLocalGenerator(cfg, model, timeout),
				Rules: proposal.NewRulesGenerator(),
			})
			if err != nil {
				return err
			}
			runner = &proposal.Runner{
				St: st, Reader: ev, Chain: chain, Owner: "cli",
				LeaseTTL: 2 * time.Minute, Limit: evidence.DefaultBundleLimit,
				MaxBytes: evidence.DefaultBundleBytes,
			}
		}
	}

	rep := &maintainReport{DryRun: *dry, CatchUp: *catchUp}
	if *force && runProposal {
		n, ferr := st.ForceRetryHeads(time.Now().Unix())
		if ferr != nil {
			return ferr
		}
		rep.Forced = n
	}

	// 阶段按固定顺序执行；任一步出错立即停止，已完成阶段的账目原样保留。
	var stageErr error
	failedStage := ""
	for _, stage := range stages {
		entry := maintainStageReport{Name: stage, Counts: map[string]int{}}
		var serr error
		switch stage {
		case maintainStageProposal:
			// dry-run 用 Peek 预演，不认领、不落库。
			if *dry {
				serr = maintainPreview(st, runner, rep, limit)
			} else {
				serr = maintainRun(st, runner, rep, limit)
			}
			if serr == nil {
				bl, berr := st.Backlog(time.Now().Unix())
				if berr != nil {
					serr = berr
				} else {
					rep.Remaining = bl.OpenBundles
				}
			}
			entry.Counts = map[string]int{
				"processed": rep.Processed, "ok": rep.OK, "noop": rep.Noop,
				"fail": rep.Fail, "partial": rep.Partial, "quarantine": rep.Quarantine,
				"backlog": rep.Backlog, "would_generate": rep.WouldGenerate,
				"remaining": rep.Remaining,
			}
		case maintainStageAdjudicate:
			// 只有本轮真的产出过候选，this-run 才有东西可裁；单条失败聚合返回、
			// 不阻断其余，已成功的裁决留在库里（与 accept 逐条语义一致）。
			serr = adjudicateRun(st, rep, adjudicateOptions{
				Scope: *adjudicateScope, Tag: tag, Limit: adjLimit, DryRun: *dry,
				Bundles: maintainProcessedBundles(rep),
			})
			entry.Counts = map[string]int{
				"would_adjudicate": rep.WouldAdjudicate, "adjudicated": rep.Adjudicated,
				"committed": rep.Committed, "deferred": rep.Deferred,
				"conflict": rep.Conflict, "errors": rep.AdjudicationErrors,
			}
		case maintainStageConsolidate:
			var cres *consolidateReport
			cres, serr = runConsolidate(st, consolidateOptions{DryRun: *dry})
			if cres != nil {
				if cres.Skipped {
					entry.Status = "skipped"
				}
				entry.Counts = map[string]int{
					"scanned": cres.Result.Scanned, "groups": cres.Result.Groups,
					"superseded": cres.Result.Superseded,
				}
			}
		case maintainStageGC:
			serr = maintainRetentionStage(st, c.db, *dry, entry.Counts)
		}
		if serr != nil {
			entry.Status = "error"
			entry.Error = serr.Error()
			rep.Stages = append(rep.Stages, entry)
			stageErr = serr
			failedStage = stage
			break
		}
		if entry.Status == "" {
			entry.Status = "ok"
		}
		rep.Stages = append(rep.Stages, entry)
	}

	if !*dry && stageErr == nil {
		// 只有命令成功返回才打点；阶段有错时留给 markMaintainFail，不写成功水位。
		markMaintainOK(st)
	}
	// 默认 proposal-only 的错误路径保持历史行为：不打印半截报告，直接返回错误。
	if stageErr != nil && !stageExplicit && failedStage == maintainStageProposal {
		return stageErr
	}
	if err := printMaintainReport(rep, c.json, stageExplicit); err != nil {
		return err
	}
	return stageErr
}

// maintainRetentionStage 执行 gc 阶段：先备份（§8.1），再走终态保留回收。
//
// dry-run 不备份、不删；非 dry-run 备份失败即整阶段失败，后续阶段不再执行。
func maintainRetentionStage(st *store.Store, dbPath string, dryRun bool, counts map[string]int) error {
	backupRoot := ""
	if !dryRun {
		if dbPath == "" {
			dbPath = store.DefaultPath()
		}
		bak, err := backupBeforeMutating(dbPath, false, "")
		if err != nil {
			return err
		}
		if bak != "" {
			backupRoot = backupDirFor(dbPath, "")
		}
	}
	res, err := runRetention(st, dbPath, dryRun, backupRoot)
	if err != nil {
		return err
	}
	if res.Files != nil {
		counts["plans"] = res.Files.Plans
		counts["backups"] = res.Files.Backups
		counts["spawn_rotated"] = res.Files.SpawnRotated
		counts["spool_tmp"] = res.Files.SpoolTmp
		counts["cursors_deleted"] = res.Files.CursorsDeleted
		counts["cursors_listed"] = res.Files.CursorsListed
	}
	if res.DB != nil {
		counts["candidates_expired"] = res.DB.Candidates.Expired
		counts["candidates_deleted"] = res.DB.Candidates.Deleted
		counts["candidates_deleted_expired"] = res.DB.Candidates.DeletedExpired
		counts["candidates_deleted_rejected"] = res.DB.Candidates.DeletedRejected
		counts["bundles"] = res.DB.Bundles.Bundles
		counts["bundle_items"] = res.DB.Bundles.Items
		counts["bundles_skipped_referenced"] = res.DB.Bundles.SkippedReferenced
		counts["decisions_deleted"] = res.DB.Decisions.Deleted
	}
	return nil
}

// adjudicateOptions 是一次自动裁决的输入。
type adjudicateOptions struct {
	Scope   string
	Tag     string
	Limit   int
	DryRun  bool
	Bundles []string
}

// adjudicateRun 逐条调用 ModelAutoCommit 并汇总账目。
//
// dry-run 只数 would_adjudicate：不调用 ModelAutoCommit、不写 commit_decisions、
// 不改候选（统计口径与真跑完全相同，避免「预演说 0、真跑写 20」）。
func adjudicateRun(st *store.Store, rep *maintainReport, opts adjudicateOptions) error {
	rep.AdjudicateScope = opts.Scope
	refs, err := pendingRefsForAdjudication(st, opts)
	if err != nil {
		return err
	}
	rep.WouldAdjudicate = len(refs)
	if opts.DryRun {
		return nil
	}
	var errs []error
	for _, ref := range refs {
		rev := ref.Revision
		if rev < 0 {
			// all 走 ListCandidates：它不返回 revision，这里按当前值再读一次，
			// 与 accept 单条路径一致（CAS 仍由事务内的 revision 判定）。
			view, verr := st.CommitCandidate(ref.ID)
			if verr != nil {
				rep.AdjudicationErrors++
				errs = append(errs, fmt.Errorf("%s: %w", shortID(ref.ID), verr))
				fmt.Fprintf(os.Stderr, "v2mem maintain: 裁决 %s 失败: %v\n", shortID(ref.ID), verr)
				continue
			}
			rev = view.Revision
		}
		res, cerr := st.ModelAutoCommit(ref.ID, rev, opts.Tag)
		if cerr != nil {
			rep.AdjudicationErrors++
			errs = append(errs, fmt.Errorf("%s: %w", shortID(ref.ID), cerr))
			fmt.Fprintf(os.Stderr, "v2mem maintain: 裁决 %s 失败: %v\n", shortID(ref.ID), cerr)
			continue
		}
		rep.Adjudicated++
		switch {
		case res.Committed:
			rep.Committed++
		case res.Action == commit.ActionConflict:
			rep.Conflict++
		default:
			// Defer（以及任何非提交动作）原样保留，等价于「需人工」。
			rep.Deferred++
		}
	}
	return errors.Join(errs...)
}

// pendingRefsForAdjudication 按 scope 取候选：this-run 只认本轮 Bundle；
// all 复用 ListCandidates("pending")，revision 用 -1 表示「执行时读当前值」。
func pendingRefsForAdjudication(st *store.Store, opts adjudicateOptions) ([]store.PendingCandidateRef, error) {
	if opts.Scope == maintainAdjudicateAll {
		list, err := st.ListCandidates("pending", "", opts.Limit)
		if err != nil {
			return nil, err
		}
		refs := make([]store.PendingCandidateRef, 0, len(list))
		for _, c := range list {
			refs = append(refs, store.PendingCandidateRef{ID: c.ID, Revision: -1})
		}
		return refs, nil
	}
	return st.ListPendingCandidatesByBundles(opts.Bundles, opts.Limit)
}

// maintainProcessedBundles 去重收集本轮 rep.Results 的 BundleID。
// 只取本轮真正处理过的窗口（含 noop/fail/quarantined），不读历史。
func maintainProcessedBundles(rep *maintainReport) []string {
	seen := make(map[string]bool, len(rep.Results))
	out := make([]string, 0, len(rep.Results))
	for _, r := range rep.Results {
		if r.Bundle == "" || seen[r.Bundle] {
			continue
		}
		seen[r.Bundle] = true
		out = append(out, r.Bundle)
	}
	return out
}

// maintainRun 连续 Step 到预算耗尽 / 队列清空（skipped）/ 全源让位（backlog）/
// 出现新隔离（quarantined）为止。
func maintainRun(st *store.Store, runner *proposal.Runner, rep *maintainReport, limit int) error {
	ctx := context.Background()
	var stepErrs []error
	for i := 0; i < limit; i++ {
		out, err := runner.Step(ctx)
		if err != nil {
			stepErrs = append(stepErrs, err)
			rep.Blocked = "处理中断: " + err.Error()
			break
		}
		if out == nil || out.Status == "skipped" {
			// 队列已清空是正常收尾，不算「本轮停止」：只在真的被让位
			// （退避/租约/竞态）时把原因留给输出。
			if out != nil && out.Blocked != "没有待处理的连续证据窗口" {
				rep.Blocked = out.Blocked
			}
			break
		}
		if out.Status == proposal.StatusBacklog {
			// 全源让位（头在退避/租约中）或认领竞态：本轮到此为止。
			// 这一窗没有被消费，remaining 会如实把它算回去。
			rep.Backlog++
			rep.Blocked = out.Blocked
			break
		}

		rep.Processed++
		rep.Results = append(rep.Results, maintainStep{
			Bundle: out.BundleID, Session: out.SessionID, Executor: out.Executor,
			Status: out.Status, BundleStatus: out.BundleStatus,
			Inserted: out.Inserted, Attempt: out.Attempt, Error: out.Error,
		})
		switch out.BundleStatus {
		case "ok":
			rep.OK++
		case "noop":
			rep.Noop++
		case "partial":
			rep.Partial++
		case "quarantined":
			rep.Quarantine++
		default:
			rep.Fail++
		}
		if out.Error != "" {
			// 逐窗写到 stderr：汇总数字不承担「哪一窗、为什么」。
			fmt.Fprintf(os.Stderr, "v2mem maintain: bundle %s status=%s: %s\n",
				shortID(out.BundleID), out.Status, out.Error)
		}
		if out.BundleStatus == proposal.StatusQuarantined {
			// 隔离是「需要人工」的终点：继续跑后面的窗口会把一次需要判断的失败
			// 埋在批量输出里，故立即停并如实汇报。
			rep.Blocked = fmt.Sprintf("bundle %s 已隔离，需人工处理（mem distill --quarantined）", shortID(out.BundleID))
			break
		}
	}
	if len(stepErrs) > 0 {
		if rep.Blocked == "" {
			rep.Blocked = "处理中断"
		}
		return errors.Join(stepErrs...)
	}
	return nil
}

// maintainPreview 是 --dry-run：只 Peek（不认领、不落库、不改 Bundle 状态）。
//
// 按最老优先遍历未终结窗口，而不是用 NextRunnable 找头：Peek 不推进队列，
// 重复查同一个头会死循环；预演要回答的是「队列里这些窗口若现在跑会怎样」。
func maintainPreview(st *store.Store, runner *proposal.Runner, rep *maintainReport, limit int) error {
	list, err := st.ListOpenProposalBundles(limit)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, b := range list {
		out, err := runner.Peek(ctx, b.ID)
		step := maintainStep{Bundle: b.ID, Attempt: b.Attempt}
		if err != nil {
			step.Status = proposal.StatusFail
			step.Error = err.Error()
			rep.Fail++
		} else {
			step.Status = out.Status
			step.BundleStatus = out.BundleStatus
			step.Session = out.SessionID
			step.Executor = out.Executor
			step.Inserted = out.Inserted
			step.Error = out.Error
			switch out.Status {
			case proposal.StatusWouldGenerate:
				rep.WouldGenerate++
			case proposal.StatusNoop:
				rep.Noop++
			case proposal.StatusBacklog:
				rep.Backlog++
			default:
				rep.Fail++
			}
		}
		rep.Processed++
		rep.Results = append(rep.Results, step)
	}
	return nil
}

// printMaintainReport 输出文本或 JSON。默认（无 --stage）文本固定一行主账 +
// 可选让位/强制说明，保持历史格式不变；显式 --stage 时改为逐阶段输出。
func printMaintainReport(rep *maintainReport, asJSON, stageExplicit bool) error {
	if asJSON {
		return printJSON(rep)
	}
	if stageExplicit {
		for _, s := range rep.Stages {
			printMaintainStage(s)
		}
		if rep.Forced > 0 {
			fmt.Printf("已清除 %d 个退避窗口的 not_before（attempt 不变），本轮立即重试\n", rep.Forced)
		}
		if rep.Blocked != "" {
			fmt.Printf("本轮停止: %s\n", strings.TrimSpace(rep.Blocked))
		}
		return nil
	}
	if rep.DryRun {
		fmt.Printf("已维护（dry-run，未认领） 预演 %d（would_generate %d noop %d fail %d backlog %d）｜剩余 %d\n",
			rep.Processed, rep.WouldGenerate, rep.Noop, rep.Fail, rep.Backlog, rep.Remaining)
	} else if rep.AdjudicateScope == "" {
		fmt.Printf("已维护 处理 %d（ok %d noop %d fail %d partial %d quarantine %d）｜提交 %d 拒绝 %d（自动提交待 S10 接入）｜剩余 %d\n",
			rep.Processed, rep.OK, rep.Noop, rep.Fail, rep.Partial, rep.Quarantine,
			rep.Committed, rep.Rejected, rep.Remaining)
	} else {
		fmt.Printf("已维护 处理 %d（ok %d noop %d fail %d partial %d quarantine %d）｜提交 %d 拒绝 %d（model-auto）｜剩余 %d\n",
			rep.Processed, rep.OK, rep.Noop, rep.Fail, rep.Partial, rep.Quarantine,
			rep.Committed, rep.Rejected, rep.Remaining)
	}
	if rep.AdjudicateScope != "" {
		fmt.Printf("自动裁决（%s）检查 %d 裁决 %d（committed %d deferred %d conflict %d）错误 %d\n",
			rep.AdjudicateScope, rep.WouldAdjudicate, rep.Adjudicated,
			rep.Committed, rep.Deferred, rep.Conflict, rep.AdjudicationErrors)
	}
	if rep.Forced > 0 {
		fmt.Printf("已清除 %d 个退避窗口的 not_before（attempt 不变），本轮立即重试\n", rep.Forced)
	}
	if rep.Blocked != "" {
		fmt.Printf("本轮停止: %s\n", strings.TrimSpace(rep.Blocked))
	}
	if rep.Remaining > 0 && !rep.DryRun {
		// 预算耗尽与「全源让位」在账面上都表现为剩余 > 0：下一轮凭年龄重新排队，
		// 所以只提示「还有未终结窗口」，不承诺它们此刻可运行。
		fmt.Printf("提示: 仍有 %d 个未终结窗口（预算耗尽或让位），再跑一次 mem maintain 继续追账\n", rep.Remaining)
	}
	return nil
}

// printMaintainStage 输出单个阶段的摘要行（仅在显式 --stage 时使用）。
func printMaintainStage(s maintainStageReport) {
	c := s.Counts
	switch s.Name {
	case maintainStageProposal:
		fmt.Printf("阶段 proposal %s：处理 %d（ok %d noop %d fail %d partial %d quarantine %d backlog %d）｜剩余 %d\n",
			s.Status, c["processed"], c["ok"], c["noop"], c["fail"], c["partial"],
			c["quarantine"], c["backlog"], c["remaining"])
	case maintainStageAdjudicate:
		fmt.Printf("阶段 adjudicate %s：检查 %d 裁决 %d（committed %d deferred %d conflict %d）错误 %d\n",
			s.Status, c["would_adjudicate"], c["adjudicated"], c["committed"],
			c["deferred"], c["conflict"], c["errors"])
	case maintainStageConsolidate:
		if s.Status == "skipped" {
			fmt.Println("阶段 consolidate skipped：dry-run 无预览模式（零写）")
		} else {
			fmt.Printf("阶段 consolidate %s：扫描 %d 簇 %d 取代 %d\n",
				s.Status, c["scanned"], c["groups"], c["superseded"])
		}
	case maintainStageGC:
		fmt.Printf("阶段 gc %s：文件 plans %d / backup %d / spawn 轮转 %d / spool %d / cursor 报数 %d；库侧候选 过期 %d 删 %d，Bundle %d，决策 %d\n",
			s.Status, c["plans"], c["backups"], c["spawn_rotated"], c["spool_tmp"],
			c["cursors_listed"], c["candidates_expired"], c["candidates_deleted"],
			c["bundles"], c["decisions_deleted"])
	}
	if s.Error != "" {
		fmt.Printf("  原因: %s\n", s.Error)
	}
}

// markMaintainFail 记录最近一次维护失败（S11 触达面读它决定是否点亮「维护失败」）。
//
// 与 markMaintainOK 一样：写失败只告警，不改退出码。失败本身已经返回给调用方，
// 一行元数据不该把错误类型再改一次。
// 原因只留一行、截到 120 rune：注入行只显示「维护失败」，原因供 `mem stats`/排障读取，
// 不塞进会话上下文。
func markMaintainFail(st *store.Store, cause error) {
	if err := st.MetaSet(store.MetaMaintainLastFailAt, strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		fmt.Fprintf(os.Stderr, "警告：记录维护失败时间失败: %v\n", err)
		return
	}
	reason := strings.TrimSpace(oneLine(truncateRunes(cause.Error(), 120)))
	if err := st.MetaSet(store.MetaMaintainLastFailReason, reason); err != nil {
		fmt.Fprintf(os.Stderr, "警告：记录维护失败原因失败: %v\n", err)
	}
}
