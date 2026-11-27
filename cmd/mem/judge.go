package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/store"
)

// 准入判定（docs/design-admission.md）。
//
// 定位：判定器只回答「这条要不要」，不回答「这条该怎么写」——后者是蒸馏的活。
// 权限：只写痕迹，不改状态。VALUE.md 的红线要求「未标定 + 不可回滚的推测不回写主库」；
// 回滚命令与 ≥95%/≥200 的标定门槛都还没落地，所以 --apply 入口一并撤掉，
// 判定器目前没有任何改库路径。要恢复落库，必须先把这两件事同批交付。
const (
	judgePromptVersion = "judge-v3"
	judgeDefaultBatch  = 20
	judgeContentRunes  = 600
)

// judgeReasons 是 reason_code 的闭集。自由文本无法聚合，也挡不住提示词漂移。
var judgeReasons = map[string]bool{
	"durable":           true, // 值得长期记住
	"process_narrative": true, // 过程叙述：已完成/已构建/已合入
	"transient_state":   true, // 一次性回执、临时状态
	"payload_residue":   true, // 纯代码/日志/路径清单
	"no_generalizable":  true, // 只对这一条会话有意义
	"unsure":            true, // verdict=unsure 时统一用这个，不沿用模型的猜测理由
}

const judgePrompt = `你是记忆准入判定器。对每条候选给出 verdict。

accept：可复用的知识或任务状态，换一个会话仍然有用（例：某服务的默认端口；某迁移还差哪几张表）。
reject：过程叙述（已完成/已构建/已合并/巡检完毕/报告已输出）、一次性操作回执、纯代码或日志或路径清单、无跨会话价值的临时状态。
unsure：以上都不是，或信息不足以判断。

硬约束：
- kind 只有 fact 或 task 才可能 accept。pitfall / preference / decision **不能 accept，但可以 reject**：内容明显是过程叙述、回执或载荷残渣时，照常 reject。
- scope 为 global 或 cross_project 不能 accept；同样是「不能要」，不是「不能否」。
- unsure 只用于「可能是知识，但信息不足」或「既不像知识也不像噪音」。能明确判为过程叙述的，不要给 unsure。
- 只判断「是否值得长期记住」，不判断内容是否正确。
- evidence 是候选引用的原始证据摘要；task_state/revision 仅作上下文，不要回显。

候选：
%s

只输出 JSON 数组，不要解释，不要代码块：
[{"id":"候选 id","verdict":"accept|reject|unsure","reason":"durable|process_narrative|transient_state|payload_residue|no_generalizable"}]`

type judgeVerdict struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

func cmdJudge(args []string) error {
	var c common
	fs := flag.NewFlagSet("judge", flag.ContinueOnError)
	c.register(fs)
	provider := fs.String("provider", "", "模型来源：deepseek|openai|ollama|auto|none（空=读 model.json，再退回 auto）")
	model := fs.String("model", "", "模型名（空=读 model.json；deepseek 默认 deepseek-flash）")
	cfgPath := fs.String("config", "", "模型配置文件（默认 ~/.v2mem/model.json）")
	timeout := fs.Duration("timeout", 90*time.Second, "单次模型调用超时")
	limit := fs.Int("limit", judgeDefaultBatch, "本批判定条数上限")
	project := fs.String("project", "", "只判这个工程的候选（空=不过滤）")
	// S12 退役（§12.1）：--stats 保留注册只为给出一句话引导，统计面已并入 review stats。
	stats := fs.Bool("stats", false, "（已退役）标定统计请用 mem review stats")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	if *stats {
		// 影子期一致率的维护入口退役：标定统计是 review stats 的职责（§12.1），
		// 这里只指路，不再打印旧表——留着旧入口只会让人在两处看到不同的数。
		return errors.New("mem judge --stats 已退役：标定统计与阈值提示并入 `mem review stats`；影子判定的 accept/reject/unsure 计数随 `mem judge` 批次输出")
	}

	// 只取 judge_verdict='' 的：影子不改 status，若按「最旧的 pending」取，
	// 每轮都会把同一批重新发给付费端点，积压永远轮不到后面。
	list, err := st.ListUnjudgedCandidates(*project, *limit)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("judge: 没有待判候选")
		return nil
	}

	// 优先级：flag > model.json > 环境变量 > auto|none，统一由 ResolveOptions 实现。
	// 这里不再手工补 cfg.Provider / cfg.Model / deepseek-flash：手工补出来的值
	// 会变成「已显式指定」的 flag，把 model.json 的 mode=cli 永久压掉。
	cfg, err := distill.LoadModelConfig(*cfgPath)
	if err != nil {
		// 报用户实际用的那个路径：--config 给过却报默认路径，会让人去改错文件。
		path := *cfgPath
		if strings.TrimSpace(path) == "" {
			path = distill.DefaultModelConfigPath()
		}
		return fmt.Errorf("读取模型配置失败（%s）: %w", path, err)
	}

	opt := distill.ResolveOptions(cfg, *provider, *model)
	opt.Timeout = *timeout
	complete, providerName, err := distill.PromptCompleter(opt)
	if err != nil {
		// 明确告诉用户「模型没起来」而不是「判定为 0 条」——后者会被当成「候选都干净」。
		return fmt.Errorf("判定器不可用（%w）。运行 mem setup（或 mem setup --status 查看当前来源）配置 provider/model/api_key，也可用 --provider deepseek 加 DEEPSEEK_API_KEY；候选正文会发往该端点", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	raw, err := complete(ctx, fmt.Sprintf(judgePrompt, judgeCandidatesJSON(list, func(id string) []string {
		ev, _ := st.CandidateEvidenceSummaries(id)
		return ev
	})))
	if err != nil {
		return fmt.Errorf("模型调用失败: %w", err)
	}
	verdicts, err := parseJudgeVerdicts(raw)
	if err != nil {
		// B3/B4 真实链路（选在 judge 解析路径）：影子期「只写痕迹不改状态」的前提
		// 是输出可解析；坏输出不能静默当成「判为 0 条」，否则下一轮又抓同一批、
		// 又烧一次调用。逐条留痕：failed（attempt+1、status_reason）→ 超上限
		// quarantined 终态（store.MarkJudgeInvalidOutput，单事务读当前 attempt）。
		if markErr := markJudgeInvalid(st, list, err.Error()); markErr != nil {
			return fmt.Errorf("模型输出无法解析: %w（原文前 200 字节：%s）；候选留痕失败: %v",
				err, truncateRunes(raw, 200), markErr)
		}
		return fmt.Errorf("模型输出无法解析: %w（原文前 200 字节：%s）", err, truncateRunes(raw, 200))
	}
	// 一输入一输出：少一条/多一条/未知 id 都按坏输出处理，不能静默丢候选。
	if err := validateJudgeVerdicts(list, verdicts); err != nil {
		if markErr := markJudgeInvalid(st, list, err.Error()); markErr != nil {
			return fmt.Errorf("判定输出不完整: %w；候选留痕失败: %v", err, markErr)
		}
		return fmt.Errorf("判定输出不完整: %w", err)
	}

	// 归因要能按模型分组：flag > 解析出的实际模型 > 只记 provider（cli 的版本分量在 providerName 里）。
	modelTag := judgeModelTag(providerName, *model, opt.Model)

	res, err := applyJudgments(st, list, verdicts, modelTag)
	if err != nil {
		return err
	}

	const mode = "影子（未改状态）"
	if *auditFile != "-" {
		if err := audit.Append(auditPath(*auditFile), audit.Record{
			TS: time.Now().Unix(), Event: "candidate-judge", Provider: providerName,
			Count: len(verdicts), Mode: mode, Source: "judge",
		}); err != nil {
			// 审计写不进去不影响判定本身，但不能静默：否则「跑过」这件事没有留痕。
			fmt.Fprintln(os.Stderr, "judge: 审计写入失败（判定结果已入库）:", err)
		}
	}
	if c.json {
		return printJSON(map[string]any{
			"provider": providerName, "model": modelTag, "mode": mode,
			"count": len(verdicts), "accept": res.Accept, "reject": res.Reject,
			"unsure": res.Unsure, "downgraded": res.Downgraded, "skipped": res.Skipped,
		})
	}
	fmt.Printf("judge: %d 条 → accept %d / reject %d / unsure %d；模式=%s\n",
		len(verdicts), res.Accept, res.Reject, res.Unsure, mode)
	if res.Skipped > 0 {
		fmt.Printf("  其中 %d 条 revision 已变化，跳过（不覆盖并发裁决）\n", res.Skipped)
	}
	if res.Downgraded > 0 {
		fmt.Printf("  其中 %d 条因 kind/scope 不允许自动采纳而降级 unsure\n", res.Downgraded)
	}
	return nil
}

// judgeModelTag 拼 judge_model：<provider>[:<model>]@<prompt 版本>。
//
// 为什么要分 flagModel 与 resolvedModel：flag 显式给了 --model 时它是权威；
// 否则要写 ResolveOptions 解析出的实际模型（model.json 的 model，或 resolve() 给
// deepseek 补的默认名）——只认 flag 会让 model.json 配的模型从归因里消失，
// 历史判定无法按模型分组。
//
// cli 的 providerName 形如 cli:<model_tag>：model_tag 里已经含模型（或模板）分量，
// 再拼 :<model> 会得到 cli:m1:m1@judge-v2，所以以 cli: 开头时直接返回。
func judgeModelTag(providerName, flagModel, resolvedModel string) string {
	if strings.HasPrefix(providerName, "cli:") {
		return providerName + "@" + judgePromptVersion
	}
	m := flagModel
	if m == "" {
		m = resolvedModel
	}
	if m == "" {
		return providerName + "@" + judgePromptVersion
	}
	return providerName + ":" + m + "@" + judgePromptVersion
}

// judgeResult 是落库循环的计数。抽出来是为了让「影子不改状态」这件事可测 ——
// 它不可能靠读代码相信，只能靠断言。
type judgeResult struct {
	Accept, Reject, Unsure, Downgraded, Skipped int
}

// applyJudgments 把判定写进候选：只写痕迹，不动 status。
func applyJudgments(st *store.Store, list []store.Candidate, verdicts []judgeVerdict, modelTag string) (judgeResult, error) {
	var r judgeResult
	for _, v := range verdicts {
		cand := findByPrefix(list, v.ID)
		if cand == nil {
			continue
		}
		verdict := v.Verdict
		// 模型的 accept 还要过结构判据：拿不准就不许授予，只能降级成 unsure。
		// 这不是防模型犯错，是防「模型权限比矩阵还大」——矩阵对这些类是 human_accept。
		if verdict == "accept" && !mayAutoAccept(cand) {
			verdict = "unsure"
			r.Downgraded++
		}
		switch verdict {
		case "accept":
			r.Accept++
		case "reject":
			r.Reject++
		default:
			verdict = "unsure"
			r.Unsure++
		}
		reason := v.Reason
		// unsure 的理由统一记成 unsure：模型对 unsure 常顺手带一个「像过程叙述」的猜测，
		// 存成 process_narrative 会让下一个人以为「判了过程叙述却是 unsure」，自相矛盾。
		if verdict == "unsure" || reason == "" || !judgeReasons[reason] {
			reason = "unsure"
		}
		if err := st.MarkJudgeVerdictCAS(cand.ID, verdict, modelTag, reason, cand.Revision); err != nil {
			if errors.Is(err, store.ErrJudgeVerdictStale) {
				// 并发自动裁决/人工动作已推进 revision：跳过，不覆盖别人的结果。
				r.Skipped++
				continue
			}
			return r, err
		}
	}
	return r, nil
}

// mayAutoAccept 是模型的权限边界，与 internal/commit/policy.go 的矩阵保持一致：
// 矩阵判 human_accept 的类，判定器不得越过。宁可漏（留给人看），不可越权。
func mayAutoAccept(cand *store.Candidate) bool {
	switch strings.ToLower(strings.TrimSpace(cand.Kind)) {
	case "fact", "task":
	default:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(cand.Scope)) {
	case "global", "cross_project":
		return false
	}
	if strings.TrimSpace(cand.Project) == "" {
		return false // 无工程归属 = 全局知识，矩阵同样判人工
	}
	return true
}

func judgeCandidatesJSON(list []store.Candidate, evidence func(string) []string) string {
	type item struct {
		ID        string   `json:"id"`
		Kind      string   `json:"kind"`
		Scope     string   `json:"scope,omitempty"`
		Project   string   `json:"project,omitempty"`
		Content   string   `json:"content"`
		Revision  int      `json:"revision"`
		TaskState string   `json:"task_state,omitempty"`
		Evidence  []string `json:"evidence,omitempty"`
	}
	out := make([]item, 0, len(list))
	for _, c := range list {
		it := item{c.ID[:8], c.Kind, c.Scope, c.Project, truncateRunes(c.Content, judgeContentRunes),
			c.Revision, c.TaskState, nil}
		if evidence != nil {
			it.Evidence = evidence(c.ID)
		}
		out = append(out, it)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// parseJudgeVerdicts 容忍模型包代码块或加解释：截取第一个 '[' 到最后一个 ']'。
func parseJudgeVerdicts(raw string) ([]judgeVerdict, error) {
	i, j := strings.Index(raw, "["), strings.LastIndex(raw, "]")
	if i < 0 || j <= i {
		return nil, errors.New("未找到 JSON 数组")
	}
	var out []judgeVerdict
	if err := json.Unmarshal([]byte(raw[i:j+1]), &out); err != nil {
		return nil, err
	}
	for _, v := range out {
		switch v.Verdict {
		case "accept", "reject", "unsure":
		default:
			return nil, fmt.Errorf("未知 verdict %q（id=%s）", v.Verdict, v.ID)
		}
	}
	return out, nil
}

// validateJudgeVerdicts 强制一输入一输出：每个送入的候选恰好一条判定，
// 不允许未知 id / 重复 / 缺失。坏输出按 markJudgeInvalid 统一留痕重试。
func validateJudgeVerdicts(list []store.Candidate, verdicts []judgeVerdict) error {
	count := make(map[string]int, len(verdicts))
	for _, v := range verdicts {
		cand := findByPrefix(list, v.ID)
		if cand == nil {
			return fmt.Errorf("未知候选 id %q", v.ID)
		}
		count[cand.ID]++
	}
	for _, c := range list {
		switch count[c.ID] {
		case 0:
			return fmt.Errorf("候选 %s 缺少判定", shortID(c.ID))
		case 1:
		default:
			return fmt.Errorf("候选 %s 重复判定", shortID(c.ID))
		}
	}
	return nil
}

// markJudgeInvalid 把一次非法输出落到本批全部候选：failed → 超上限 quarantined。
//
// attempt 的读取与状态迁移都在 store.MarkJudgeInvalidOutput 的同一事务里做，
// 避免「CLI 读到旧 attempt、写时已被别的进程推进」的竞态；这里只做批量与聚合。
// 留痕失败不吞：主错误已经说明输出坏了，留痕失败必须一起报出来，否则坏输出
// 又会变成「判为 0 条」的假象。
func markJudgeInvalid(st *store.Store, list []store.Candidate, reason string) error {
	var errs []error
	for i := range list {
		if _, err := st.MarkJudgeInvalidOutput(list[i].ID, "judge_invalid_output: "+reason); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func findByPrefix(list []store.Candidate, prefix string) *store.Candidate {
	prefix = strings.TrimSpace(prefix)
	if len(prefix) < 4 {
		return nil
	}
	for i := range list {
		if strings.HasPrefix(list[i].ID, prefix) {
			return &list[i]
		}
	}
	return nil
}
