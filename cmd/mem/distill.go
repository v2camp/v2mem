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
	"github.com/v2camp/v2mem/internal/scrub"
	"github.com/v2camp/v2mem/internal/store"
)

// 蒸馏只写 candidates 与 distill_runs。本文件没有 store.Add。
// 采纳在 cmdAccept，那是唯一把候选升格进 memories 的入口。

func cmdDistill(args []string) error {
	var c common
	fs := flag.NewFlagSet("distill", flag.ContinueOnError)
	c.register(fs)
	session := fs.String("session", "", "只蒸馏这个会话")
	since := fs.Duration("since", 0, "只看这段时间之内的观测，如 24h")
	provider := fs.String("provider", "auto", "auto|none|openai|ollama")
	dry := fs.Bool("dry-run", false, "只打印候选，不写库")
	strict := fs.Bool("strict", false, "失败时非零退出（默认 fail-open，退出码 0）")
	pending := fs.Bool("pending", false, "蒸馏所有尚未处理的会话")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	noScrub := fs.Bool("no-scrub", false, "关闭密钥打码（仅本地调试）")
	proposalMode := fs.Bool("proposal", false, "从证据账本取连续 Bundle 生成候选（T2）")
	bundleID := fs.String("bundle", "", "只处理指定证据 Bundle（隐含 --proposal）")
	executor := fs.String("executor", "auto", "Proposal executor: auto|host|local|byok|none")
	maxBundles := fs.Int("max-bundles", 1, "--proposal 本次最多处理几个 Bundle")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*pending && strings.TrimSpace(*session) == "" && strings.TrimSpace(*bundleID) == "" && !*proposalMode {
		return errors.New("需要 --session、--pending、--proposal 或 --bundle")
	}
	model, timeout := distill.LoadConfig(distill.ConfigPath())
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
		unavailable := func(label string, cause error) proposal.Generator {
			return proposal.UnavailableGenerator(label, cause)
		}
		local := func() proposal.Generator {
			fn, name, cerr := distill.PromptCompleter(distill.Options{
				Provider: "ollama", Model: model, Timeout: timeout,
			})
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
			fn, name, cerr := distill.PromptCompleter(distill.Options{
				Provider: "openai", Model: model, Timeout: timeout,
			})
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
			proposal.StatusFail: 6, proposal.StatusInvalid: 5, proposal.StatusPartial: 4,
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
				_ = audit.Append(auditPath(*auditFile), audit.Record{
					TS: time.Now().Unix(), Event: "distill", SessionID: o.SessionID,
					Provider: o.Executor, Mode: o.Status, Count: o.Inserted,
					Failed: o.Invalid, Source: "proposal",
					Empty: o.Status == proposal.StatusNoop,
				})
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

	sessions := []string{strings.TrimSpace(*session)}
	if *pending {
		sessions, err = st.SessionsNeedingDistill(20)
		if err != nil {
			return err
		}
	}
	if len(sessions) == 0 || (len(sessions) == 1 && sessions[0] == "") {
		if c.json {
			return printJSON(map[string]any{
				"status": "skipped", "candidates": 0, "inserted": 0, "failed": 0, "sessions": 0,
			})
		}
		fmt.Println("没有需要蒸馏的会话")
		return nil
	}

	reports := make([]*distillReport, 0, len(sessions))
	// 逐个会话独立计数。不能只留第一个 failed —— 其余会话的失败会被静默吞掉，
	// 用户看到的「退出码 0」与「有 3 个会话失败」的事实不符。
	var failures []error
	inserted, failed := 0, 0
	for _, sid := range sessions {
		rep, err := distillOne(st, distillJob{
			Session: sid, Provider: *provider, Model: model, Timeout: timeout,
			Since: *since, Dry: *dry, Audit: *auditFile, DB: c.db, NoScrub: *noScrub,
		})
		reports = append(reports, rep)
		inserted += rep.Inserted
		failed += rep.Failed
		if err != nil {
			failures = append(failures, err)
		}
		if c.json {
			continue
		}
		fmt.Printf("会话 %s  provider=%s status=%s 候选 %d\n", sid, rep.Provider, rep.Status, rep.Inserted)
		if rep.Failed > 0 {
			// 落库失败必须单独说，不能并进「候选」数字里。
			fmt.Fprintf(os.Stderr, "v2mem distill: %s 有 %d 条候选未能落库\n", sid, rep.Failed)
		}
		if rep.Error != "" {
			fmt.Fprintf(os.Stderr, "v2mem distill: %s\n", rep.Error)
		}
		if *dry {
			for _, f := range rep.Facts {
				fmt.Printf("  [%s] %s\n", f.Kind, f.Content)
			}
		}
	}
	if c.json {
		if err := printJSON(summarizeDistill(reports, inserted, failed, len(sessions))); err != nil {
			return err
		}
	}
	if len(failures) > 0 && *strict {
		// errors.Join 保留每个会话的原因，而不是只报第一个。
		return errors.Join(failures...)
	}
	return nil
}

// distillReport 是一次蒸馏的完整账目。
//
// 为什么 inserted 与 failed 必须同时存在：候选是逐条插入的，中途失败时前 n 条已经落库。
// 只报 inserted 会掩盖丢数据，只报 failed 会让人以为一条都没成——两种单数字都在说谎。
type distillReport struct {
	Session  string         `json:"session"`
	Provider string         `json:"provider"`
	Status   string         `json:"status"` // ok|empty|partial|fail|skipped
	Inserted int            `json:"inserted"`
	Failed   int            `json:"failed"`
	Error    string         `json:"error,omitempty"`
	Facts    []distill.Fact `json:"-"`
	Err      error          `json:"-"`
}

// summarizeDistill 汇总多次蒸馏。顶层 status 取最坏的那个，
// 让 `--json` 的调用方不必遍历 results 也能判断整批是否可信。
func summarizeDistill(reports []*distillReport, inserted, failed, sessions int) map[string]any {
	var emptyN, skippedN, failN, partialN int
	for _, r := range reports {
		switch r.Status {
		case distill.StatusEmpty:
			emptyN++
		case distill.StatusSkipped:
			skippedN++
		case statusPartial:
			partialN++
		case distill.StatusFail:
			failN++
		}
	}
	status := distill.StatusOK
	switch {
	case failN == len(reports) && failN > 0:
		status = distill.StatusFail
	case failN > 0 || partialN > 0:
		status = statusPartial
	case emptyN == len(reports):
		status = distill.StatusEmpty
	case skippedN == len(reports):
		status = distill.StatusSkipped
	}
	return map[string]any{
		"status": status,
		// candidates 保留旧字段名，语义与 inserted 相同（真正落库的条数）。
		"candidates":      inserted,
		"inserted":        inserted,
		"failed":          failed,
		"failed_sessions": failN,
		"partial":         partialN,
		"sessions":        sessions,
		"empty":           emptyN,
		"skipped":         skippedN,
		"results":         reports,
	}
}

// statusPartial 表示「部分落库」：既不是成功也不是全败，必须单独一个状态，
// 否则调用方只能在 inserted>0 且 failed>0 之间自己推断。
const statusPartial = "partial"

type distillJob struct {
	Session  string
	Provider string
	Model    string
	Timeout  time.Duration
	Since    time.Duration
	Dry      bool
	Audit    string
	DB       string
	NoScrub  bool
}

func distillOne(st *store.Store, job distillJob) (*distillReport, error) {
	started := time.Now()
	report := &distillReport{Session: job.Session, Status: distill.StatusSkipped}
	var sinceUnix int64
	if job.Since > 0 {
		sinceUnix = time.Now().Add(-job.Since).Unix()
	}
	obs, err := st.ObservationsForDistill(job.Session, sinceUnix, 200)
	if err != nil {
		return failReport(report, err), err
	}
	provider := job.Provider
	if provider == "" {
		provider = "auto"
	}
	report.Provider = provider
	if len(obs) == 0 {
		writeDistillAudit(job, report, time.Since(started))
		return report, nil
	}
	ids := make([]string, len(obs))
	project := ""
	in := make([]distill.Observation, len(obs))
	for i, o := range obs {
		ids[i] = o.ID
		if project == "" {
			project = o.Project
		}
		in[i] = distill.Observation{
			ID: o.ID, SessionID: o.SessionID, Project: o.Project,
			Summary: scrub.Apply(o.Summary, job.NoScrub),
		}
	}
	window := distill.WindowKey(ids)
	done, err := st.DistillWindowDone(job.Session, window)
	if err != nil {
		return failReport(report, err), err
	}
	if done {
		return report, nil
	}
	out := distill.Run(in, distill.Options{Provider: provider, Model: job.Model, Timeout: job.Timeout})
	report.Provider = out.Provider
	report.Facts = out.Facts

	if job.Dry {
		// dry-run 也要如实反射三态：原来是 fail 被静默吞成 (0, nil)，
		// 于是「模型没起来」在 dry-run 下看起来和「没有候选」一模一样。
		switch out.Status {
		case distill.StatusOK:
			report.Status = distill.StatusOK
			report.Inserted = len(out.Facts)
		case distill.StatusEmpty:
			report.Status = distill.StatusEmpty
		default:
			failReport(report, runErr(out))
		}
		writeDistillAudit(job, report, time.Since(started))
		return report, report.Err
	}
	switch out.Status {
	case distill.StatusFail:
		failReport(report, runErr(out))
		_ = st.SaveDistillRun(store.DistillRun{
			SessionID: job.Session, Provider: report.Provider, Status: distill.StatusFail,
			Error: report.Error, StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
		})
		writeDistillAudit(job, report, time.Since(started))
		return report, report.Err
	case distill.StatusEmpty:
		// 合法 0 条：这是成功结论。仍然记 run 并占住窗口，
		// 否则同一批观测每次启动都会被重新蒸馏，白白再问一次模型。
		report.Status = distill.StatusEmpty
		if err := st.SaveDistillRun(store.DistillRun{
			SessionID: job.Session, Provider: report.Provider, Status: distill.StatusEmpty,
			CandCount: 0, WindowKey: window,
			StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
		}); err != nil && !errors.Is(err, store.ErrAlreadyDistilled) {
			return failReport(report, err), err
		}
		writeDistillAudit(job, report, time.Since(started))
		return report, nil
	}

	// 逐条插入。失败不中断：能落库的先落库，再把失败的条数与原因一起报出来。
	var insertErrs []error
	for _, f := range out.Facts {
		res, err := st.InsertCandidate(store.CandidateInput{
			SessionID: job.Session, Project: project, Kind: f.Kind,
			Content: scrub.Apply(f.Content, job.NoScrub), Source: "distill", Channel: store.ChannelAgent,
			Salience: f.Salience, ObsIDs: f.ObsIDs,
		})
		if err != nil {
			report.Failed++
			insertErrs = append(insertErrs, err)
			continue
		}
		if res.Created {
			report.Inserted++
		}
	}
	runStatus := distill.StatusOK
	switch {
	case len(insertErrs) == 0:
	case report.Inserted == 0:
		runStatus = distill.StatusFail
	default:
		runStatus = statusPartial
	}
	report.Status = runStatus
	if len(insertErrs) > 0 {
		report.Err = errors.Join(insertErrs...)
		report.Error = report.Err.Error()
		_ = st.SaveDistillRun(store.DistillRun{
			SessionID: job.Session, Provider: report.Provider, Status: runStatus,
			Error: report.Error, CandCount: report.Inserted,
			StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
		})
		writeDistillAudit(job, report, time.Since(started))
		return report, report.Err
	}
	if err := st.SaveDistillRun(store.DistillRun{
		SessionID: job.Session, Provider: report.Provider, Status: distill.StatusOK,
		CandCount: report.Inserted, WindowKey: window,
		StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
	}); err != nil && !errors.Is(err, store.ErrAlreadyDistilled) {
		return failReport(report, err), err
	}
	writeDistillAudit(job, report, time.Since(started))
	return report, nil
}

// runErr 取结构化原因；老代码路径只给了文本时补一个兜底 error，避免 Err 为空。
func runErr(out distill.Outcome) error {
	if out.Err != nil {
		return out.Err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	return errors.New("蒸馏失败")
}

// failReport 把一次失败写进报告，保持 Status/Inserted/Failed/Error/Err 自洽。
func failReport(report *distillReport, err error) *distillReport {
	report.Status = distill.StatusFail
	if err != nil {
		report.Error = err.Error()
		report.Err = err
	}
	return report
}

// writeDistillAudit 写一条蒸馏审计。
//
// Empty 只表示「合法空结果」：旧实现写 n == 0，把 fail 和 ok-空混成一类，
// 事后统计空结果率时会把模型故障算成「没有可记的事实」，指标直接失真。
func writeDistillAudit(job distillJob, report *distillReport, d time.Duration) {
	if job.Audit == "-" {
		return
	}
	// Mode 取报告的最终状态（ok|empty|partial|fail|skipped），而不是 distill.Run 的原始状态：
	// 逐条插入失败时 Run 仍然是 ok，只有报告知道这次其实是 partial。
	_ = audit.Append(auditPath(job.Audit), audit.Record{
		TS: time.Now().Unix(), Event: "distill", SessionID: job.Session,
		Provider: report.Provider, Mode: report.Status, Count: report.Inserted,
		Failed: report.Failed, Source: "distill",
		MS: d.Milliseconds(), Empty: report.Status == distill.StatusEmpty,
	})
}

func cmdAccept(args []string) error {
	var c common
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	c.register(fs)
	all := fs.Bool("all-pending", false, "采纳当前筛选下的全部 pending")
	session := fs.String("session", "", "配合 --all-pending，只采纳这个会话")
	auto := fs.Bool("auto", false, "按提交矩阵自动裁决：只有 auto_commit 才落库，人工类型保持 pending")
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
		if *auto {
			res, err = st.AutoCommitCandidate(id, rev)
		} else {
			res, err = st.CommitAccept(id, rev, "human")
		}
		if err != nil {
			failedItems = append(failedItems, acceptFailure{Candidate: id, Error: err.Error()})
			fmt.Fprintf(os.Stderr, "v2mem accept: %s 采纳失败: %v\n", shortID(id), err)
			continue
		}
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
		if err := printJSON(map[string]any{
			"accepted": accepted, "failed": failedItems, "deferred": deferred,
			"inserted": len(accepted), "failed_count": len(failedItems),
			"deferred_count": len(deferred),
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
		return printJSON(cand)
	}
	fmt.Printf("已拒绝 %s\n", shortID(cand.ID))
	return nil
}

func cmdCandidates(args []string) error {
	if len(args) == 0 || args[0] != "ls" {
		return errors.New("用法: mem candidates ls [--status pending] [--session S] [--policy]")
	}
	var c common
	fs := flag.NewFlagSet("candidates ls", flag.ContinueOnError)
	c.register(fs)
	status := fs.String("status", "pending", "pending|accepted|rejected|expired|空为全部")
	session := fs.String("session", "", "限定会话")
	limit := fs.Int("limit", 50, "条数上限")
	policy := fs.Bool("policy", false, "显示提交矩阵裁决（只读，不写库）")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	list, err := st.ListCandidates(*status, *session, *limit)
	if err != nil {
		return err
	}
	if list == nil {
		list = []store.Candidate{}
	}
	if *policy {
		views := make([]candidatePolicyView, 0, len(list))
		for _, cand := range list {
			vc, d, derr := st.DecideCandidate(cand.ID)
			if derr != nil {
				return derr
			}
			views = append(views, candidatePolicyView{
				Candidate: cand, Revision: vc.Revision,
				Action: string(d.Action), ReasonCodes: d.ReasonCodes,
			})
		}
		if c.json {
			return printJSON(views)
		}
		if len(views) == 0 {
			fmt.Println("（没有候选）")
			return nil
		}
		for _, v := range views {
			fmt.Printf("%s  [%s] %s  rev=%d  action=%s  %s\n",
				shortID(v.Candidate.ID), v.Candidate.Kind, v.Candidate.Status,
				v.Revision, v.Action, oneLine(v.Candidate.Content))
			if len(v.ReasonCodes) > 0 {
				fmt.Printf("    理由: %s\n", strings.Join(v.ReasonCodes, ", "))
			}
		}
		return nil
	}
	if c.json {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println("（没有候选）")
		return nil
	}
	for _, cand := range list {
		fmt.Printf("%s  [%s] %s  %s\n", shortID(cand.ID), cand.Kind, cand.Status, oneLine(cand.Content))
	}
	return nil
}

// candidatePolicyView 是 candidates ls --policy 的只读视图。
type candidatePolicyView struct {
	Candidate   store.Candidate `json:"candidate"`
	Revision    int             `json:"revision"`
	Action      string          `json:"action"`
	ReasonCodes []string        `json:"reason_codes,omitempty"`
}
