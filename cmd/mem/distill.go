package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/proposal"
	"github.com/v2camp/v2mem/internal/store"
)

// distill 只写 candidates 与 distill_runs。本文件没有 store.Add。
// 采纳在 cmdAccept，那是唯一把候选升格进 memories 的入口。
//
// S12 命令面收编（design-data-lifecycle §12.1）：旧规则蒸馏单轨（--session /
// --pending / distillOne）已退役——自动写侧只剩 SessionStart 的 proposal spawn
// 与一次性冷启动，手动入口只有 --proposal / --bundle / 三个运维出口。
// distillOne、SessionsNeedingDistill、ObservationsForDistill 等旧轨代码一并删除。

// proposalOptions 把 model.json 的 mode 语义带进 proposal 的 local/byok 两条来源。
//
// cli 与 off 都必须走 ResolveOptions：只判断 cli 会让 mode=off 落回硬编码的
// ollama/openai，`mem distill --proposal` 就还会外发，与配置说的「关闭」不符。
// api 保持原行为：local 试 ollama、byok 试 openai，模型取 config.toml 的 model。
func proposalOptions(cfg distill.ModelConfig, provider, model string, timeout time.Duration) distill.Options {
	if cfg.Mode == "cli" || cfg.Mode == "off" {
		// 只读 model.json 的 cli/model_tag：config.toml 的 model 不是显式 --model，
		// 传进去会把归因标签覆盖成 cli:<config.toml 的模型名>。
		opt := distill.ResolveOptions(cfg, "", "")
		opt.Timeout = timeout
		return opt
	}
	return distill.Options{Provider: provider, Model: model, Timeout: timeout}
}

func cmdDistill(args []string) error {
	var c common
	fs := flag.NewFlagSet("distill", flag.ContinueOnError)
	c.register(fs)
	dry := fs.Bool("dry-run", false, "只打印候选，不写库（需配合 --bundle：dry-run 不得推进连续游标）")
	strict := fs.Bool("strict", false, "失败时非零退出（默认 fail-open，退出码 0）")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	noScrub := fs.Bool("no-scrub", false, "关闭密钥打码（仅本地调试）")
	proposalMode := fs.Bool("proposal", false, "从证据账本取连续 Bundle 生成候选")
	bundleID := fs.String("bundle", "", "只处理指定证据 Bundle")
	quarantined := fs.Bool("quarantined", false, "列出被隔离（quarantined）的 Bundle，需单独使用")
	requeue := fs.String("requeue", "", "把指定隔离 Bundle 放回队列（attempt 归零），需单独使用")
	gaps := fs.Bool("gaps", false, "列出已跳过并记录的序号洞，需单独使用（洞不是 Bundle，不提供回捞）")
	executor := fs.String("executor", "auto", "Proposal executor: auto|host|local|byok|none")
	maxBundles := fs.Int("max-bundles", 1, "--proposal 本次最多处理几个 Bundle")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 隔离查看/回捞/序号洞是运维入口：不生成候选，也不要求 --proposal 等必填项；
	// 与正常蒸馏路径互斥，避免「顺手带个 --proposal」把库写脏。
	requeueID := strings.TrimSpace(*requeue)
	outlets := 0
	for _, on := range []bool{*quarantined, requeueID != "", *gaps} {
		if on {
			outlets++
		}
	}
	if outlets > 1 {
		return errors.New("--quarantined、--requeue 与 --gaps 需单独使用，不能同时指定")
	}
	if outlets == 1 {
		if *proposalMode || strings.TrimSpace(*bundleID) != "" {
			return errors.New("--quarantined/--requeue/--gaps 需单独使用，不能与 --proposal/--bundle 同用")
		}
		st, err := store.Open(c.db)
		if err != nil {
			return err
		}
		defer st.Close()
		switch {
		case *quarantined:
			return printQuarantinedBundles(st, c.json)
		case *gaps:
			// 序号洞是「已跳过并记录」的事实，不是 Bundle：没有回捞入口。
			return printEvidenceGaps(st, c.json)
		default:
			return requeueProposalBundle(st, requeueID, c.json, *auditFile)
		}
	}
	if strings.TrimSpace(*bundleID) == "" && !*proposalMode {
		return errors.New("需要 --proposal 或 --bundle（--quarantined/--requeue/--gaps 为运维出口，需单独使用）")
	}
	model, timeout := distill.LoadConfig(distill.ConfigPath())
	// model.json 的 mode=cli 要同时接管 proposal 的 local/byok 两条来源，
	// 否则 `mem distill --proposal` 还会去连 ollama/openai，与配置说的不符。
	// 读不出来必须 fail-closed：把非法配置当「没配」会让用户选的 CLI 静默变成外发。
	cfg, err := distill.LoadModelConfig("")
	if err != nil {
		return fmt.Errorf("读取模型配置失败（%s）: %w", distill.DefaultModelConfigPath(), err)
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	// T2：证据 Bundle 路径。默认 auto = Local → backlog，绝不默认外发 BYOK，
	// 也不把 Host 放进自动链路（host_safe=false，见 design-write-governance §11）。
	if *bundleID != "" || *proposalMode {
		mode := strings.ToLower(strings.TrimSpace(*executor))
		if mode == "" {
			mode = "auto"
		}
		if *dry && strings.TrimSpace(*bundleID) == "" {
			return errors.New("--dry-run 需要配合 --bundle：dry-run 不得推进连续游标")
		}
		var deps proposal.Deps
		// 规则兜底恒挂链尾：模型全部不可用时仍能把决策/踩坑类证据落成候选，
		// 而不是让窗口停在 backlog。none 模式 BuildChain 不消费它，语义不变。
		deps.Rules = proposal.NewRulesGenerator()
		unavailable := func(label string, cause error) proposal.Generator {
			return proposal.UnavailableGenerator(label, cause)
		}
		localOpt := proposalOptions(cfg, "ollama", model, timeout)
		byokOpt := proposalOptions(cfg, "openai", model, timeout)
		local := func() proposal.Generator {
			fn, name, cerr := distill.PromptCompleter(localOpt)
			if cerr != nil {
				return unavailable("local", cerr)
			}
			return proposal.PromptGenerator(name, fn)
		}
		switch mode {
		case "auto", "local":
			deps.Local = local()
		case "host":
			deps.Host = unavailable("host", proposal.ErrHostUnavailable)
			deps.Local = local()
		case "byok":
			deps.Local = local()
			fn, name, cerr := distill.PromptCompleter(byokOpt)
			if cerr != nil {
				deps.BYOK = unavailable("byok", cerr)
			} else {
				deps.BYOK = proposal.PromptGenerator(name, fn)
			}
		case "none":
			// 显式 none：没有 provider，终态是 backlog（不是 empty）。
		default:
			return fmt.Errorf("未知 executor: %s（auto|host|local|byok|none）", *executor)
		}
		ev := evidence.New(st)
		chain, err := proposal.BuildChain(mode, ev, deps)
		if err != nil {
			return err
		}
		runner := &proposal.Runner{
			St: st, Reader: ev, Chain: chain, Owner: "cli",
			LeaseTTL: 2 * time.Minute, Limit: evidence.DefaultBundleLimit,
			MaxBytes: evidence.DefaultBundleBytes, NoScrub: *noScrub,
		}
		limit := *maxBundles
		if limit <= 0 {
			limit = 1
		}
		outs := make([]*proposal.Outcome, 0, limit)
		var failures []error
		for i := 0; i < limit; i++ {
			var out *proposal.Outcome
			if id := strings.TrimSpace(*bundleID); id != "" {
				if *dry {
					out, err = runner.Peek(context.Background(), id)
				} else {
					out, err = runner.StepBundle(context.Background(), id)
				}
			} else {
				out, err = runner.Step(context.Background())
			}
			if err != nil {
				failures = append(failures, err)
				break
			}
			if out.Status == "skipped" {
				break
			}
			outs = append(outs, out)
			if *bundleID != "" {
				break
			}
			// 只有窗口真正被消费（ok/noop）才继续下一窗；fail/partial/backlog 停在这里。
			if out.BundleStatus != "ok" && out.BundleStatus != "noop" {
				break
			}
		}
		if len(outs) == 0 {
			if len(failures) > 0 {
				return errors.Join(failures...)
			}
			if c.json {
				return printJSON(map[string]any{"status": "skipped", "bundles": 0, "inserted": 0})
			}
			fmt.Println("没有待处理的证据窗口")
			return nil
		}

		rank := map[string]int{
			proposal.StatusQuarantined: 7,
			proposal.StatusFail:        6, proposal.StatusInvalid: 5, proposal.StatusPartial: 4,
			proposal.StatusBacklog: 3, proposal.StatusOK: 2, proposal.StatusNoop: 1,
			proposal.StatusWouldGenerate: 0, "skipped": -1,
		}
		status := outs[0].Status
		var generated, valid, invalid, duplicate, inserted, merged int
		results := make([]map[string]any, 0, len(outs))
		for _, o := range outs {
			if rank[o.Status] > rank[status] {
				status = o.Status
			}
			generated += o.Generated
			valid += o.Valid
			invalid += o.Invalid
			duplicate += o.Duplicate
			inserted += o.Inserted
			merged += o.Merged
			results = append(results, map[string]any{
				"bundle": o.BundleID, "session": o.SessionID, "executor": o.Executor,
				"status": o.Status, "bundle_status": o.BundleStatus,
				"generated": o.Generated, "valid": o.Valid, "invalid": o.Invalid,
				"duplicate": o.Duplicate, "inserted": o.Inserted, "merged": o.Merged,
				"blocked": o.Blocked, "error": o.Error, "attempt": o.Attempt,
			})
			if *auditFile != "-" {
				writeProposalAudit(*auditFile, o)
			}
			if o.Error != "" {
				fmt.Fprintf(os.Stderr, "v2mem distill: %s\n", o.Error)
			}
			if c.json {
				continue
			}
			fmt.Printf("bundle %s executor=%s status=%s 生成 %d 有效 %d 无效 %d 新增 %d 重复 %d\n",
				shortID(o.BundleID), o.Executor, o.Status, o.Generated, o.Valid,
				o.Invalid, o.Inserted, o.Duplicate)
			if o.Blocked != "" {
				fmt.Printf("  %s\n", o.Blocked)
			}
			if *dry {
				for _, d := range o.Drafts {
					fmt.Printf("  would_generate [%s] %s\n", d.Kind, d.Content)
				}
			}
		}
		if c.json {
			if err := printJSON(map[string]any{
				"status": status, "bundles": len(outs), "generated": generated,
				"valid": valid, "invalid": invalid, "duplicate": duplicate,
				"inserted": inserted, "merged": merged,
				"would_generate": *dry, "results": results,
			}); err != nil {
				return err
			}
		}
		if *strict {
			for _, o := range outs {
				switch o.Status {
				case proposal.StatusFail, proposal.StatusInvalid, proposal.StatusPartial:
					failures = append(failures, fmt.Errorf("bundle %s status=%s: %s",
						o.BundleID, o.Status, o.Error))
				}
			}
		}
		if len(failures) > 0 {
			return errors.Join(failures...)
		}
		return nil
	}
	return nil
}

// quarantinedBundleView 是 `mem distill --quarantined --json` 的稳定字段面。
// 用独立视图而不是直接序列化 store.ProposalBundle：后者带 EvidenceIDs 等内部字段，
// 且没有 json tag，改库结构会意外改变 CLI 契约。
type quarantinedBundleView struct {
	ID               string `json:"id"`
	SourceID         string `json:"source_id"`
	Generation       int    `json:"generation"`
	FromSeqExclusive int    `json:"from_seq_exclusive"`
	ToSeqInclusive   int    `json:"to_seq_inclusive"`
	Attempt          int    `json:"attempt"`
	NotBefore        int64  `json:"not_before"`
	LastRunID        string `json:"last_run_id"`
	UpdatedAt        int64  `json:"updated_at"`
}

// printQuarantinedBundles 输出隔离队列。列表只读，不写审计、不改状态。
func printQuarantinedBundles(st *store.Store, asJSON bool) error {
	list, err := st.ListProposalBundles("quarantined", 0)
	if err != nil {
		return err
	}
	views := make([]quarantinedBundleView, 0, len(list))
	for _, b := range list {
		views = append(views, quarantinedBundleView{
			ID: b.ID, SourceID: b.SourceID, Generation: b.Generation,
			FromSeqExclusive: b.FromSeqExclusive, ToSeqInclusive: b.ToSeqInclusive,
			Attempt: b.Attempt, NotBefore: b.NotBefore, LastRunID: b.LastRunID,
			UpdatedAt: b.UpdatedAt,
		})
	}
	if asJSON {
		return printJSON(views)
	}
	if len(views) == 0 {
		fmt.Println("（没有隔离 Bundle）")
		return nil
	}
	for _, v := range views {
		fmt.Printf("bundle %s source=%s seq=%d..%d attempt=%d updated=%d\n",
			shortID(v.ID), v.SourceID, v.FromSeqExclusive, v.ToSeqInclusive, v.Attempt, v.UpdatedAt)
	}
	return nil
}

// evidenceGapView 是 `mem distill --gaps --json` 的稳定字段面。
// 与 quarantinedBundleView 同理：独立视图，避免把 store 内部结构直接变成 CLI 契约。
type evidenceGapView struct {
	ID         string `json:"id"`
	SourceID   string `json:"source_id"`
	Generation int    `json:"generation"`
	FromSeq    int    `json:"from_seq"`
	ToSeq      int    `json:"to_seq"`
	DetectedAt int64  `json:"detected_at"`
}

// printEvidenceGaps 输出已处置的序号洞。列表只读、不写审计、不改 cursor。
func printEvidenceGaps(st *store.Store, asJSON bool) error {
	list, err := st.ListEvidenceGaps(0)
	if err != nil {
		return err
	}
	views := make([]evidenceGapView, 0, len(list))
	for _, g := range list {
		views = append(views, evidenceGapView{
			ID: g.ID, SourceID: g.SourceID, Generation: g.Generation,
			FromSeq: g.FromSeq, ToSeq: g.ToSeq, DetectedAt: g.DetectedAt,
		})
	}
	if asJSON {
		return printJSON(views)
	}
	if len(views) == 0 {
		fmt.Println("（没有序号洞）")
		return nil
	}
	for _, v := range views {
		fmt.Printf("gap %s source=%s gen=%d seq=%d..%d detected=%d（已跳过并记录，不是 Bundle，无法回捞）\n",
			shortID(v.ID), v.SourceID, v.Generation, v.FromSeq, v.ToSeq, v.DetectedAt)
	}
	return nil
}

// requeueProposalBundle 把隔离窗口放回 pending 并写 bundle-requeue 审计。
// 回捞本身不自动重跑：新退避从 0 重新计，是否立即重放由用户显式 --bundle 决定。
func requeueProposalBundle(st *store.Store, id string, asJSON bool, auditFile string) error {
	b, err := st.RequeueProposalBundle(id, time.Now().Unix())
	if err != nil {
		return err
	}
	if auditFile != "-" {
		_ = audit.Append(auditPath(auditFile), audit.Record{
			TS: time.Now().Unix(), Event: "bundle-requeue", Mode: "requeue",
			Source: "proposal", Count: 1,
		})
	}
	if asJSON {
		return printJSON(map[string]any{
			"status": b.Status, "id": b.ID, "attempt": b.Attempt,
		})
	}
	fmt.Printf("已重新入队 %s（attempt 归零）；用 --bundle 立即重放\n", b.ID)
	return nil
}

// writeProposalAudit 写一次 Proposal 结果的审计。
//
// 隔离事件由 CLI 写而不是 runner 写：internal/proposal 是纯逻辑包，
// 引入 audit 依赖会把「执行器可单测」的边界破坏掉。
func writeProposalAudit(auditFile string, o *proposal.Outcome) {
	if auditFile == "-" {
		return
	}
	_ = audit.Append(auditPath(auditFile), audit.Record{
		TS: time.Now().Unix(), Event: "distill", SessionID: o.SessionID,
		Provider: o.Executor, Mode: o.Status, Count: o.Inserted,
		Failed: o.Invalid, Source: "proposal",
		Empty: o.Status == proposal.StatusNoop,
	})
	if o.BundleStatus == proposal.StatusQuarantined {
		_ = audit.Append(auditPath(auditFile), audit.Record{
			TS: time.Now().Unix(), Event: "bundle-quarantine", SessionID: o.SessionID,
			Provider: o.Executor, Mode: o.BundleStatus, Source: "proposal",
		})
	}
}

func cmdAccept(args []string) error {
	var c common
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	c.register(fs)
	all := fs.Bool("all-pending", false, "采纳当前筛选下的全部 pending")
	session := fs.String("session", "", "配合 --all-pending，只采纳这个会话")
	auto := fs.Bool("auto", false, "按提交矩阵自动裁决：只有 auto_commit 才落库，人工类型保持 pending")
	modelAuto := fs.Bool("model-auto", false, "允许模型自动采纳（默认关；Conflict/Defer 仍保留，裁决 actor=model-auto）")
	modelTag := fs.String("model-tag", "", "归因 model@promptver（--model-auto 时写入 commit_decisions，供按版本回滚）")
	revision := fs.Int("revision", -1, "期望的候选 revision（默认按当前 revision；并发下用过期的值会冲突）")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all && *revision >= 0 {
		return errors.New("--all-pending 不能与 --revision 同用：批量采纳的每条候选 revision 不同")
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	var ids []string
	if *all {
		list, err := st.ListCandidates("pending", *session, 1000)
		if err != nil {
			return err
		}
		for _, cand := range list {
			ids = append(ids, cand.ID)
		}
		if len(ids) == 0 {
			fmt.Println("没有 pending 候选")
			return nil
		}
	} else {
		id := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if id == "" {
			return errors.New("缺少 ID，用法: mem accept <id|前缀> 或 mem accept --all-pending [--session S]")
		}
		ids = []string{id}
	}

	// 逐条采纳，失败不中断循环。
	//
	// 旧实现在第一条失败时直接 return，把前面已经采纳成功的记忆从输出里丢掉——
	// 但它们**已经落库**了（审计也逐条写了），于是审计数 > 用户看到数。
	// 现在把成功的留下来照常打印/输出，失败的单列，最后再返回聚合错误。
	//
	// revision 在调用前显式读出并传入：两个并发 accept 会拿到同一个 revision，
	// 随后由 store 的 CAS 判定唯一赢家；不传就会退化成「事务内自读自写」，
	// 并发双方都能成功，CAS 失去意义。
	var accepted []store.CommitResult
	var failedItems []acceptFailure
	var deferred []acceptDeferred
	decisions := make([]decisionView, 0, len(ids))
	for _, id := range ids {
		view, err := st.CommitCandidate(id)
		if err != nil {
			failedItems = append(failedItems, acceptFailure{Candidate: id, Error: err.Error()})
			fmt.Fprintf(os.Stderr, "v2mem accept: %s 采纳失败: %v\n", shortID(id), err)
			continue
		}
		rev := *revision
		if rev < 0 {
			rev = view.Revision
		}
		var res *store.CommitResult
		switch {
		case *modelAuto:
			// S10b 生产开关：默认关，不改既有 accept 行为。开启后仍走提交矩阵
			// （Conflict/Defer 保留）与 CommitAccept 的 CAS/幂等路径，只是 actor
			// 记 model-auto、model_tag 落裁决账（§6.3 / §7 第 7 条）。
			res, err = st.ModelAutoCommit(id, rev, *modelTag)
		case *auto:
			res, err = st.AutoCommitCandidate(id, rev)
		default:
			res, err = st.CommitAccept(id, rev, "human")
		}
		if err != nil {
			failedItems = append(failedItems, acceptFailure{Candidate: id, Error: err.Error()})
			fmt.Fprintf(os.Stderr, "v2mem accept: %s 采纳失败: %v\n", shortID(id), err)
			continue
		}
		// 无论落库、重放还是「必须人工」，都产出一条裁决视图：
		// 自动化要靠 decision 判断结果，而不是去猜 accepted/deferred 数组。
		decisions = append(decisions, decisionOf(res, rev))
		if !res.Committed {
			d := acceptDeferred{
				Candidate: res.Candidate.ID, Action: string(res.Action),
				ReasonCodes: res.ReasonCodes,
			}
			deferred = append(deferred, d)
			fmt.Fprintf(os.Stderr, "v2mem accept: %s 需人工裁决 action=%s reasons=%s\n",
				shortID(d.Candidate), d.Action, strings.Join(d.ReasonCodes, ","))
			continue
		}
		accepted = append(accepted, *res)
		if *auditFile != "-" {
			mode := "create"
			if res.Already {
				mode = "overwrite"
			}
			_ = audit.Append(auditPath(*auditFile), audit.Record{
				TS: time.Now().Unix(), Event: "accept", SessionID: res.Candidate.SessionID,
				Project: res.Candidate.Project, Source: "distill-accept", Mode: mode,
				Count: 1, Hashes: []string{res.MemoryID}, Kinds: []string{res.Candidate.Kind},
			})
		}
	}
	if c.json {
		// 成功与失败分开列：数组里混着两种语义就必然有人把失败当成功数。
		// deferred 是第三类：矩阵判为「必须人工」，既不是成功也不是故障。
		// decision 是计划 §4.4 的必交付字段：单条 --revision N 场景直接读它；
		// --all-pending 批量时它取最后一笔，全量在 decisions。
		var last any
		if len(decisions) > 0 {
			last = decisions[len(decisions)-1]
		}
		if err := printJSON(map[string]any{
			"accepted": accepted, "failed": failedItems, "deferred": deferred,
			"inserted": len(accepted), "failed_count": len(failedItems),
			"deferred_count": len(deferred),
			"decision":       last, "decisions": decisions,
		}); err != nil {
			return err
		}
	} else {
		for _, res := range accepted {
			fmt.Printf("已采纳 %s → 记忆 %s source=distill-accept\n", shortID(res.Candidate.ID), shortID(res.MemoryID))
		}
	}
	if len(failedItems) > 0 {
		errs := make([]error, 0, len(failedItems))
		for _, f := range failedItems {
			errs = append(errs, fmt.Errorf("%s: %s", f.Candidate, f.Error))
		}
		return errors.Join(errs...)
	}
	return nil
}

// decisionView 是 accept/reject --json 的裁决视图（计划 §4.4 必交付）。
// 它把「矩阵怎么判的、有没有真的写库、用的是哪版 revision」合成一处，
// 自动化不必再从 accepted/deferred/reason_codes 里反推。
type decisionView struct {
	Candidate      string   `json:"candidate"`
	Action         string   `json:"action"`
	Actor          string   `json:"actor"`
	Revision       int      `json:"revision"`
	ReasonCodes    []string `json:"reason_codes,omitempty"`
	Committed      bool     `json:"committed"`
	Already        bool     `json:"already"`
	EvidenceHash   string   `json:"evidence_hash,omitempty"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
}

func decisionOf(res *store.CommitResult, revision int) decisionView {
	return decisionView{
		Candidate: res.Candidate.ID, Action: string(res.Action), Actor: res.Actor,
		Revision: revision, ReasonCodes: res.ReasonCodes,
		Committed: res.Committed, Already: res.Already,
		EvidenceHash: res.EvidenceHash, IdempotencyKey: res.IdempotencyKey,
	}
}

// rejectResultView 匿名嵌入 Candidate，保持原有字段平铺，只新增 decision。
type rejectResultView struct {
	store.Candidate
	Decision decisionView `json:"decision"`
}

// acceptFailure 是一条采纳失败的记录。保留候选 id，方便重试时定位。
type acceptFailure struct {
	Candidate string `json:"candidate"`
	Error     string `json:"error"`
}

// acceptDeferred 是矩阵判为「必须人工」的候选。它没有落库，也不是错误：
// 把它混进 failed 会让自动化把「设计如此」当成故障重试。
type acceptDeferred struct {
	Candidate   string   `json:"candidate"`
	Action      string   `json:"action"`
	ReasonCodes []string `json:"reason_codes,omitempty"`
}

func cmdReject(args []string) error {
	var c common
	fs := flag.NewFlagSet("reject", flag.ContinueOnError)
	c.register(fs)
	revision := fs.Int("revision", -1, "期望的候选 revision（默认按当前 revision）")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		return errors.New("缺少 ID，用法: mem reject <id|前缀>")
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	view, err := st.CommitCandidate(id)
	if err != nil {
		return err
	}
	rev := *revision
	if rev < 0 {
		rev = view.Revision
	}
	res, err := st.CommitReject(id, rev, "human", "")
	if err != nil {
		return err
	}
	cand := res.Candidate
	if *auditFile != "-" {
		_ = audit.Append(auditPath(*auditFile), audit.Record{
			TS: time.Now().Unix(), Event: "reject", SessionID: cand.SessionID,
			Project: cand.Project, Source: "distill", Count: 1, Mode: "rejected",
		})
	}
	if c.json {
		return printJSON(rejectResultView{Candidate: cand, Decision: decisionOf(res, rev)})
	}
	fmt.Printf("已拒绝 %s\n", shortID(cand.ID))
	return nil
}
