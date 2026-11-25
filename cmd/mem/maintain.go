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
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/proposal"
	"github.com/v2camp/v2mem/internal/store"
)

// maintainDefaultMaxBundles 是一次唤醒的默认预算：Hook 唤醒应该短命，
// 剩下的留给下一轮；显式 `--max-bundles 0` 也落回默认，而不是变成「不处理」。
const maintainDefaultMaxBundles = 20

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
// Committed/Rejected 本期恒 0（S10 接入自动提交），保留字段是为了让巡检与自动化
// 现在就能按稳定键解析，而不是等接入时再改契约。
type maintainReport struct {
	DryRun        bool           `json:"dry_run"`
	CatchUp       bool           `json:"catch_up"`
	Forced        int            `json:"forced_retry"`
	Processed     int            `json:"processed"`
	OK            int            `json:"ok"`
	Noop          int            `json:"noop"`
	Fail          int            `json:"fail"`
	Partial       int            `json:"partial"`
	Quarantine    int            `json:"quarantine"`
	Backlog       int            `json:"backlog"`
	WouldGenerate int            `json:"would_generate"`
	Committed     int            `json:"committed"`
	Rejected      int            `json:"rejected"`
	Remaining     int            `json:"remaining"`
	Blocked       string         `json:"blocked,omitempty"`
	Results       []maintainStep `json:"results,omitempty"`
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

func cmdMaintain(args []string) error {
	var c common
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	c.register(fs)
	// --catch-up 与默认行为等价：保留它是为了让 Harness / SOP 明示「这是故障恢复」，
	// 而不是新增一条只有名字不同的路径。
	catchUp := fs.Bool("catch-up", false, "故障恢复：恢复过期租约后最老优先重试可恢复窗口（与默认行为等价）")
	dry := fs.Bool("dry-run", false, "只读预演：不认领、不落候选、不写库")
	force := fs.Bool("force", false, "把退避中的 fail/partial/backlog 清零 not_before，本轮立即重试（不改 attempt）")
	maxBundles := fs.Int("max-bundles", maintainDefaultMaxBundles, "本次最多处理几个 Bundle")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dry && *force {
		return errors.New("--dry-run 与 --force 不能同用：前者只读，后者会写 not_before")
	}
	limit := *maxBundles
	if limit <= 0 {
		limit = maintainDefaultMaxBundles
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	model, timeout := distill.LoadConfig(distill.ConfigPath())
	cfg, err := distill.LoadModelConfig("")
	if err != nil {
		// 与 distill 同一条 fail-closed：非法 model.json 当「没配」会静默外发。
		return fmt.Errorf("读取模型配置失败（%s）: %w", distill.DefaultModelConfigPath(), err)
	}
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
	runner := &proposal.Runner{
		St: st, Reader: ev, Chain: chain, Owner: "cli",
		LeaseTTL: 2 * time.Minute, Limit: evidence.DefaultBundleLimit,
		MaxBytes: evidence.DefaultBundleBytes,
	}

	rep := &maintainReport{DryRun: *dry, CatchUp: *catchUp}
	if *force {
		n, ferr := st.ForceRetryHeads(time.Now().Unix())
		if ferr != nil {
			return ferr
		}
		rep.Forced = n
	}

	if *dry {
		if err := maintainPreview(st, runner, rep, limit); err != nil {
			return err
		}
	} else if err := maintainRun(st, runner, rep, limit); err != nil {
		return err
	}

	now := time.Now().Unix()
	bl, err := st.Backlog(now)
	if err != nil {
		return err
	}
	rep.Remaining = bl.OpenBundles

	if !*dry {
		// 只有命令成功返回才打点；任何分支已提前 return，不会写水位。
		markMaintainOK(st)
	}
	return printMaintainReport(rep, c.json)
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

// printMaintainReport 输出文本或 JSON。文本固定一行主账 + 可选让位/强制说明：
// 巡检与事后 grep 都按这一行读，不让字段散落成多行。
func printMaintainReport(rep *maintainReport, asJSON bool) error {
	if asJSON {
		return printJSON(rep)
	}
	if rep.DryRun {
		fmt.Printf("已维护（dry-run，未认领） 预演 %d（would_generate %d noop %d fail %d backlog %d）｜剩余 %d\n",
			rep.Processed, rep.WouldGenerate, rep.Noop, rep.Fail, rep.Backlog, rep.Remaining)
	} else {
		fmt.Printf("已维护 处理 %d（ok %d noop %d fail %d partial %d quarantine %d）｜提交 %d 拒绝 %d（自动提交待 S10 接入）｜剩余 %d\n",
			rep.Processed, rep.OK, rep.Noop, rep.Fail, rep.Partial, rep.Quarantine,
			rep.Committed, rep.Rejected, rep.Remaining)
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
