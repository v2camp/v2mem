package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/commit"
	"github.com/v2camp/v2mem/internal/store"
)

// 批量准出（docs/design-admission.md「AI 出方案、人给授权」）。
//
// 分工刻意做成三段，缺一段都会退化成「提示词直接改库」：
//   - AI 只读候选、产出 plan.jsonl —— 它不调 mem accept，写库不是它的权限；
//   - 人 --dry-run 过目后执行一次 —— 授权来自这一下，不是来自模型的自评；
//   - 系统把 actor 记成 human-reviewed-llm —— 与 human 分开，才不会被算成人工标定样本。
//
// plan 同时是回滚凭证：执行时把 memory_id / created_memory 回填进归档，
// mem batch --rollback 照着归档原路退回。没有归档，批量落库就是不可逆写库。
//
// --reject-judged 是只覆盖否决的近路：不读方案文件，直接把影子判定 reject 的 pending
// 候选判掉。它不出 accept——采纳要人给的方案，这条边界不能拿「省事」换。
const (
	batchActor        = commit.ActorHumanReviewedLLM
	batchMaxItems     = 1000
	batchContentRunes = 60

	// 归档只记结果，审计要能分辨这批是「AI 方案」还是「照影子判定批量否决」。
	batchSourcePlan   = "batch-plan"
	batchSourceJudged = "batch-judged"
)

// batchPlanItem 是方案文件的一行。字段刻意只有三个：
// 让模型多写一个字段（confidence、note）就多一个没人校验的自由文本。
type batchPlanItem struct {
	ID     string `json:"id"`
	Action string `json:"action"` // accept|reject
	Reason string `json:"reason"` // 可空；非空必须是 judgeReasons 里的理由码
}

// batchPreview 是 --dry-run 的逐条视图：action、理由、正文摘要一眼可读。
type batchPreview struct {
	ID      string `json:"id"`
	Action  string `json:"action"`
	Reason  string `json:"reason,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Project string `json:"project,omitempty"`
	Status  string `json:"status"`
	Content string `json:"content,omitempty"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// batchRunRecord 是执行归档的一行，也是 --rollback 的唯一输入。
// Revision 记的是落库前那一刻的 revision，回滚时用它做 CAS：
// 归档之后又被人裁决过的候选，不再按旧 plan 撤。
type batchRunRecord struct {
	ID            string `json:"id"`
	Action        string `json:"action"`
	Reason        string `json:"reason,omitempty"`
	OK            bool   `json:"ok"`
	MemoryID      string `json:"memory_id,omitempty"`
	CreatedMemory bool   `json:"created_memory,omitempty"`
	Revision      int    `json:"revision,omitempty"`
	Error         string `json:"error,omitempty"`
	TS            int64  `json:"ts"`
}

func cmdBatch(args []string) error {
	var c common
	fs := flag.NewFlagSet("batch", flag.ContinueOnError)
	c.register(fs)
	planPath := fs.String("plan", "", "方案文件（JSONL，每行 {id,action,reason}）")
	rollback := fs.String("rollback", "", "按一次执行的归档撤回（退回 pending）")
	rejectJudged := fs.Bool("reject-judged", false, "批量否决影子判定为 reject 的 pending 候选（只否决、不采纳，无需方案文件）")
	project := fs.String("project", "", "只处理这个工程（配合 --reject-judged）")
	dryRun := fs.Bool("dry-run", false, "只校验并打印，不写库")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	modes := 0
	for _, on := range []bool{*planPath != "", *rollback != "", *rejectJudged} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		return errors.New("用法: mem batch --plan <file> [--dry-run] | --reject-judged [--project P] [--dry-run] | --rollback <archive>")
	}
	if *dryRun && *rollback != "" {
		return errors.New("--dry-run 只用于 --plan / --reject-judged，回滚不做预演")
	}
	if *project != "" && !*rejectJudged {
		return errors.New("--project 只配合 --reject-judged 使用")
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	if *rollback != "" {
		return rollbackBatch(st, *rollback, c.json)
	}

	var items []batchPlanItem
	source := batchSourcePlan
	if *rejectJudged {
		items, err = planFromJudged(st, *project)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			if c.json {
				return printJSON(map[string]any{"source": batchSourceJudged, "planned": 0})
			}
			fmt.Println("没有「影子判定 reject 且仍 pending」的候选，无需清退。")
			return nil
		}
		source = batchSourceJudged
	} else {
		items, err = readBatchPlan(*planPath)
		if err != nil {
			return err
		}
	}
	previews := previewBatch(st, items)
	if *dryRun {
		return printBatchPreview(previews, source, c.json)
	}
	// 预演里有不可执行的项就整体拒绝：批量落库最忌讳「一半执行一半跳过」，
	// 人看到的方案与实际写库的方案必须是同一份。
	for _, p := range previews {
		if !p.OK {
			return fmt.Errorf("方案未通过校验（%s: %s），先用 --dry-run 修好再执行", shortID(p.ID), p.Error)
		}
	}
	return runBatch(st, items, previews, dbPath(c.db), *auditFile, source, c.json)
}

// planFromJudged 把「影子判定 reject」当场翻成方案，省掉一个中转文件。
//
// 只收 reject：采纳是授予权限，必须由人给的方案决定；否决只是把明显不该入库的
// 候选移出待裁决区，判错还能 --rollback 退回。这条不对称是 design-admission §3.2 的边界。
func planFromJudged(st *store.Store, project string) ([]batchPlanItem, error) {
	rows, err := st.ListJudgeRejects(project, batchMaxItems)
	if err != nil {
		return nil, err
	}
	out := make([]batchPlanItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, batchPlanItem{ID: r.ID, Action: "reject", Reason: derivedRejectReason(r.Rationale)})
	}
	return out, nil
}

// derivedRejectReason 沿用模型给的理由码；不是闭集里的码（老数据、模型自由发挥）
// 就统一记 judge_rejected —— 别把自由文本伪装成理由码写进裁决。
func derivedRejectReason(rationale string) string {
	if r := strings.TrimSpace(rationale); judgeReasons[r] {
		return r
	}
	return "judge_rejected"
}

// readBatchPlan 解析并做静态校验。逐行 Decode 而不是整体 Unmarshal：
// 方案文件被截断时，报错要能指到行号。
func readBatchPlan(path string) ([]batchPlanItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取方案失败: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out []batchPlanItem
	seen := make(map[string]bool)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var it batchPlanItem
		dec := json.NewDecoder(strings.NewReader(raw))
		// 未知字段直接报错：模型把 action 写成 verdict、或多写一个 id8 都该被挡住，
		// 而不是静默求出零值后按空动作处理。
		dec.DisallowUnknownFields()
		if err := dec.Decode(&it); err != nil {
			return nil, fmt.Errorf("%s:%d 不是合法方案行: %w", path, line, err)
		}
		it.ID = strings.TrimSpace(it.ID)
		it.Action = strings.ToLower(strings.TrimSpace(it.Action))
		it.Reason = strings.TrimSpace(it.Reason)
		switch {
		case it.ID == "":
			return nil, fmt.Errorf("%s:%d 缺少 id", path, line)
		case it.Action != "accept" && it.Action != "reject":
			return nil, fmt.Errorf("%s:%d action 只能是 accept|reject，got %q", path, line, it.Action)
		case it.Reason != "" && !judgeReasons[it.Reason]:
			return nil, fmt.Errorf("%s:%d 未知理由码 %q（可选：%s）", path, line, it.Reason, strings.Join(judgeReasonList(), "|"))
		case seen[it.ID]:
			return nil, fmt.Errorf("%s:%d 重复的 id %s", path, line, it.ID)
		}
		seen[it.ID] = true
		out = append(out, it)
		if len(out) > batchMaxItems {
			return nil, fmt.Errorf("方案超过 %d 条，分批执行", batchMaxItems)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("方案为空：至少给一行 {id,action}")
	}
	return out, nil
}

// previewBatch 把每行解析成「能不能执行」：dry-run 直接打印它，
// 非 dry-run 也先跑一遍，保证看到的就是要执行的。
func previewBatch(st *store.Store, items []batchPlanItem) []batchPreview {
	out := make([]batchPreview, 0, len(items))
	for _, it := range items {
		p := batchPreview{ID: it.ID, Action: it.Action, Reason: it.Reason}
		cand, err := st.ResolveCandidate(it.ID)
		if err != nil {
			p.Error = err.Error()
			out = append(out, p)
			continue
		}
		p.ID = cand.ID
		p.Kind = cand.Kind
		p.Project = cand.Project
		p.Status = cand.Status
		p.Content = clipRunes(oneLine(cand.Content), batchContentRunes)
		if cand.Status != "pending" {
			p.Error = "状态为 " + cand.Status + "，不是 pending"
		} else {
			p.OK = true
		}
		out = append(out, p)
	}
	return out
}

func printBatchPreview(previews []batchPreview, source string, asJSON bool) error {
	accept, reject := countBatchActions(previews)
	bad := 0
	for _, p := range previews {
		if !p.OK {
			bad++
		}
	}
	var err error
	if asJSON {
		err = printJSON(map[string]any{
			"dry_run": true, "source": source, "planned": len(previews),
			"accept": accept, "reject": reject, "items": previews,
		})
	} else {
		if source == batchSourceJudged {
			fmt.Printf("来源：影子判定 reject 的 pending 候选 %d 条（只否决，不采纳）\n", len(previews))
		}
		fmt.Printf("方案 %d 条（accept %d / reject %d）：\n", len(previews), accept, reject)
		for _, p := range previews {
			mark := "ok  "
			if !p.OK {
				mark = "跳过"
			}
			fmt.Printf("  %s  %-6s %-10s %-6s %s\n", shortID(p.ID), p.Action, p.Kind, mark, p.Content)
			if p.Error != "" {
				fmt.Printf("        原因: %s\n", p.Error)
			}
		}
		fmt.Println("dry-run：未写库。确认无误后去掉 --dry-run 执行。")
	}
	if err != nil {
		return err
	}
	// 预演发现不可执行的项就以非零退出：让人在「还没写库」这一步就知道方案要改，
	// 而不是等执行时才被整体拒绝。
	if bad > 0 {
		return fmt.Errorf("dry-run：%d 条不可执行，先修方案再执行", bad)
	}
	return nil
}

func runBatch(st *store.Store, items []batchPlanItem, previews []batchPreview, dbPath, auditFile, source string, asJSON bool) error {
	recs := make([]batchRunRecord, 0, len(items))
	accepted, rejected, failed := 0, 0, 0
	for i, it := range items {
		rec := batchRunRecord{ID: previews[i].ID, Action: it.Action, Reason: it.Reason, TS: time.Now().Unix()}
		view, err := st.CommitCandidate(rec.ID)
		if err != nil {
			rec.Error = err.Error()
		} else {
			rec.Revision = view.Revision
			var res *store.CommitResult
			switch it.Action {
			case "accept":
				res, err = st.CommitAccept(rec.ID, view.Revision, batchActor)
			case "reject":
				res, err = st.CommitReject(rec.ID, view.Revision, batchActor, it.Reason)
			}
			if err != nil {
				rec.Error = err.Error()
			} else {
				rec.OK = true
				rec.MemoryID = res.MemoryID
				rec.CreatedMemory = res.CreatedMemory
			}
		}
		switch {
		case !rec.OK:
			failed++
			fmt.Fprintf(os.Stderr, "batch: %s %s 失败: %s\n", shortID(rec.ID), rec.Action, rec.Error)
		case rec.Action == "accept":
			accepted++
			appendBatchAudit(auditFile, "accept", previews[i].Project, previews[i].ID, rec.MemoryID, source)
		default:
			rejected++
			appendBatchAudit(auditFile, "reject", previews[i].Project, previews[i].ID, "", source)
		}
		recs = append(recs, rec)
	}

	archive, err := writeBatchArchive(dbPath, recs)
	if err != nil {
		// 归档失败必须让用户知道：没有归档就没有回滚凭证，这批落库已经不可撤了。
		fmt.Fprintf(os.Stderr, "batch: 归档写入失败（本批无法 --rollback）: %v\n", err)
	}

	if asJSON {
		out := map[string]any{
			"source": source, "planned": len(items),
			"accepted": accepted, "rejected": rejected,
			"failed": failed, "records": recs,
		}
		if archive != "" {
			out["archive"] = archive
		}
		if err := printJSON(out); err != nil {
			return err
		}
	} else {
		fmt.Printf("批量准出：已采纳 %d 条 / 已拒绝 %d 条 / 失败 %d 条\n", accepted, rejected, failed)
		if archive != "" {
			fmt.Printf("方案归档：%s\n回滚：mem batch --rollback %s\n", archive, archive)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d 条未能落库，详见上面的 stderr", failed)
	}
	return nil
}

func rollbackBatch(st *store.Store, archive string, asJSON bool) error {
	recs, err := readBatchArchive(archive)
	if err != nil {
		return err
	}
	var done, failed int
	results := make([]batchRunRecord, 0, len(recs))
	for _, rec := range recs {
		if !rec.OK {
			continue
		}
		_, err := st.RollbackCandidate(rec.ID, batchActor, rec.CreatedMemory, rec.Revision+1)
		r := batchRunRecord{ID: rec.ID, Action: "rollback", Reason: rec.Reason, TS: time.Now().Unix()}
		if err != nil {
			r.Error = err.Error()
			failed++
			fmt.Fprintf(os.Stderr, "batch: 回滚 %s 失败: %v\n", shortID(rec.ID), err)
		} else {
			r.OK = true
			done++
		}
		results = append(results, r)
	}
	if asJSON {
		return printJSON(map[string]any{
			"rollback": true, "reopened": done, "failed": failed, "records": results,
		})
	}
	fmt.Printf("回滚：退回 pending %d 条 / 失败 %d 条\n", done, failed)
	if done > 0 {
		fmt.Println("注意：退回的候选仍带原 judge_verdict，不会被自动判定再处理一遍。")
	}
	if failed > 0 {
		return fmt.Errorf("%d 条回滚失败，见上面的 stderr", failed)
	}
	return nil
}

func readBatchArchive(path string) ([]batchRunRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取归档失败: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out []batchRunRecord
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var rec batchRunRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("%s:%d 不是归档行: %w", path, line, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("归档为空，没有可回滚的记录")
	}
	return out, nil
}

// writeBatchArchive 把执行结果落到 <db 目录>/plans/ 下。
// 放在 DB 旁边而不是固定 ~/.v2mem：--db 指向临时库时（测试、排障），
// 归档跟着走，不会在真实数据目录里留垃圾。
func writeBatchArchive(dbPath string, recs []batchRunRecord) (string, error) {
	dir := filepath.Join(filepath.Dir(dbPath), "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, time.Now().Format("20060102-150405")+"-*.jsonl")
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, rec := range recs {
		raw, err := json.Marshal(rec)
		if err != nil {
			return "", err
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// appendBatchAudit 逐条留痕，事件名与人工 accept/reject 保持一致：
// 另起一套事件名会让 audit --stats 与看板漏掉批量这条路。
func appendBatchAudit(path, event, project, candidateID, memoryID, source string) {
	if path == "-" {
		return
	}
	_ = audit.Append(auditPath(path), audit.Record{
		TS: time.Now().Unix(), Event: event, Project: project,
		Source: source, Mode: "batch", Count: 1,
		Hashes: []string{memoryID}, Kinds: nil,
	})
}

func countBatchActions(previews []batchPreview) (accept, reject int) {
	for _, p := range previews {
		if p.Action == "accept" {
			accept++
		} else {
			reject++
		}
	}
	return accept, reject
}

func judgeReasonList() []string {
	out := make([]string, 0, len(judgeReasons))
	for r := range judgeReasons {
		out = append(out, r)
	}
	return out
}

// clipRunes 按 rune 截断，避免把多字节字符切成半个。
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// dbPath 给出实际使用的库路径，归档目录跟着它走。
func dbPath(p string) string {
	if p == "" {
		return store.DefaultPath()
	}
	return p
}
