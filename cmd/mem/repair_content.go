package main

// mem repair content —— 已入库记忆的正文提炼（docs/design-content-enhance.md）。
//
// 与 repair evidence 的分工：evidence 修结构（确定性补证据链、不动正文），
// content 修语义（模型把过程叙述改写成「触发场景 → 判断依据 → 处置」）。
// 模型产出必须可归因、可回滚：每次改写立即归档到 <db 目录>/enhance/，
// 回滚逐条校验当前 content_hash 仍是当次改写结果，绝不覆盖人工后来的修改。

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/proposal"
	"github.com/v2camp/v2mem/internal/store"
)

const (
	// enhancePromptVersion 是归因的一部分：同 tag 的归档能回答「哪版提示词」。
	enhancePromptVersion = "enhance-v1"
	enhanceDefaultLimit  = 20
	enhanceDefaultBatch  = 10
	// overlong 阈值：与候选正文的 240 rune 预算同口径，超过即视为需要压缩。
	enhanceOverlongRunes = 240
	// enhancePromptMarker 是提示词正文与输入 JSON 的分界，测试据此取本批 id。
	enhancePromptMarker = "输入："
)

// 模型接入的两个接缝：测试只替换它们注入 fake，不碰网络。
var (
	enhanceLoadConfig      = distill.LoadModelConfig
	enhancePromptCompleter = distill.PromptCompleter
)

const enhancePrompt = `你是记忆正文提炼器。把已入库的过程叙述改写成可复用规则。

改写目标：每条正文回答三件事——触发场景（遇到什么情况）→ 判断依据（凭什么这么判断）→ 处置（该怎么做 / 不该怎么做）。

硬约束：
1. 不新增事实：只能压缩、重排、去指代化；库外知识与推测一律不得引入。
2. 禁止第一人称（我 / 我们）、会话指代（这个链接 / 那个报错）、时点词（刚才 / 本次 / 目前）。
3. 专名原样保留：命令、路径、函数名、错误码、数字、机器名不改写、不翻译。
4. 不改变语义类别：kind 由代码保留，不要改写。
5. 长度尽量不超过原正文的 1.2 倍。

只输出 JSON，不要解释，不要代码块，输出格式：
{"items":[{"id":"输入里的 id","content":"改写后的正文"}]}

` + enhancePromptMarker

// enhanceCandidate 是一条进入处理流程的候选。Reason 记录它为什么被选中
// （narrative / overlong / all / explicit），dry-run 直接展示给操作者。
type enhanceCandidate struct {
	ID          string
	Content     string
	Kind        string
	Project     string
	ContentHash string
	ReviewState string
	Reason      string
}

// enhanceItem 是报告里的逐条结果。dry-run 只填投影与原因；
// apply/preview/rollback 再填状态与正文对照。
type enhanceItem struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	ReviewState string `json:"review_state"`
	Reason      string `json:"reason,omitempty"`
	Status      string `json:"status,omitempty"`
	Error       string `json:"error,omitempty"`
	OldContent  string `json:"old_content,omitempty"`
	NewContent  string `json:"new_content,omitempty"`
}

// enhanceReport 是文本与 --json 共用的报告。回滚字段始终输出 0，
// 让脚本可以只认一套键而不必区分模式。
type enhanceReport struct {
	Mode             string        `json:"mode"`
	Filter           string        `json:"filter"`
	ModelTag         string        `json:"model_tag"`
	DryRun           bool          `json:"dry_run"`
	Scanned          int           `json:"scanned"`
	Eligible         int           `json:"eligible"`
	Processed        int           `json:"processed"`
	Rewritten        int           `json:"rewritten"`
	Unchanged        int           `json:"unchanged"`
	SkippedConfirmed int           `json:"skipped_confirmed"`
	SkippedOther     int           `json:"skipped_other"`
	Conflict         int           `json:"conflict"`
	Failed           int           `json:"failed"`
	Archive          string        `json:"archive"`
	Restored         int           `json:"restored"`
	Skipped          int           `json:"skipped"`
	SkippedConflict  int           `json:"skipped_conflict"`
	Items            []enhanceItem `json:"items"`
}

// enhanceArchiveRecord 是回滚凭证的冻结字段，不允许增删改名。
type enhanceArchiveRecord struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Project    string `json:"project"`
	OldContent string `json:"old_content"`
	OldHash    string `json:"old_hash"`
	NewContent string `json:"new_content"`
	NewHash    string `json:"new_hash"`
	EnhanceTag string `json:"enhance_tag"`
	At         int64  `json:"at"`
}

func cmdRepairContent(args []string) error {
	var c common
	fs := flag.NewFlagSet("repair content", flag.ContinueOnError)
	c.register(fs)
	filter := fs.String("filter", "narrative", "选择器: narrative|overlong|all")
	all := fs.Bool("all", false, "等价 --filter=all")
	dryRun := fs.Bool("dry-run", false, "只列候选，不调模型、不写库")
	preview := fs.Bool("preview", false, "调模型出 before/after，不写库")
	limit := fs.Int("limit", enhanceDefaultLimit, "本次最多处理条数")
	batch := fs.Int("batch", enhanceDefaultBatch, "单次模型调用条数")
	modelTag := fs.String("model-tag", "", "归因标签（默认取模型配置拼出的 <provider>:<model>@enhance-v1）")
	includeConfirmed := fs.Bool("include-confirmed", false, "连人工已确认/修正的条目一起处理")
	rollback := fs.String("rollback", "", "按归档回滚：<tag|file>")
	positional, flagArgs := splitLeadingArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	ids := make([]string, 0, len(positional)+len(fs.Args()))
	for _, id := range append(positional, fs.Args()...) {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}

	switch *filter {
	case "narrative", "overlong", "all":
	default:
		return usageError{msg: fmt.Sprintf("未知 --filter: %q（可选 narrative|overlong|all）", *filter)}
	}
	if *all {
		*filter = "all"
	}
	if *dryRun && *preview {
		return usageError{msg: "--dry-run 与 --preview 不能同时使用"}
	}
	if *limit <= 0 {
		return usageError{msg: "--limit 应为正数"}
	}
	if *batch <= 0 {
		return usageError{msg: "--batch 应为正数"}
	}
	if strings.TrimSpace(*rollback) != "" && len(ids) > 0 {
		return usageError{msg: "--rollback 只做还原，不接收具体 id"}
	}
	if strings.TrimSpace(*rollback) != "" && *preview {
		return usageError{msg: "--rollback 与 --preview 互斥"}
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	if strings.TrimSpace(*rollback) != "" {
		return runEnhanceRollback(st, c.json, *rollback, *dryRun)
	}

	// 归因标签先于选候选确定：缺标签要在「模型没起来」之前报，且不写库。
	tag, complete, err := resolveEnhanceModel(strings.TrimSpace(*modelTag), *dryRun)
	if err != nil {
		return err
	}

	picks, items, scanned, skippedConfirmed, skippedOther, failed, err :=
		collectEnhanceCandidates(st, ids, *filter, *includeConfirmed)
	if err != nil {
		return err
	}
	eligible := len(picks)
	if len(picks) > *limit {
		picks = picks[:*limit]
	}

	rep := &enhanceReport{
		Filter: *filter, ModelTag: tag, DryRun: *dryRun,
		Scanned: scanned, Eligible: eligible,
		SkippedConfirmed: skippedConfirmed, SkippedOther: skippedOther,
		Failed: failed, Items: items,
	}

	if *dryRun {
		rep.Mode = "dry-run"
		for _, p := range picks {
			rep.Items = append(rep.Items, enhanceItem{
				ID: p.ID, Kind: p.Kind, Project: p.Project,
				ReviewState: p.ReviewState, Reason: p.Reason, Status: "candidate",
			})
		}
		return finishEnhance(rep, c.json, "正文提炼")
	}

	batches := runEnhanceBatches(context.Background(), complete, picks, *batch)
	if *preview {
		rep.Mode = "preview"
		applyEnhancePreview(rep, batches)
		return finishEnhance(rep, c.json, "正文预览")
	}

	rep.Mode = "apply"
	applyEnhanceRewrites(st, rep, batches, tag)
	return finishEnhance(rep, c.json, "正文提炼")
}

// resolveEnhanceModel 解析归因标签与模型调用函数。
//
// dry-run 不调模型，也不因缺模型配置失败：它只列候选。preview/apply 必须
// 有非空 tag——归因缺失时归档失去意义，不能静默兜底。
func resolveEnhanceModel(flagTag string, dryRun bool) (string, func(context.Context, string) (string, error), error) {
	tag := flagTag
	cfg, cfgErr := enhanceLoadConfig("")
	if cfgErr != nil && !dryRun {
		return "", nil, fmt.Errorf("读取模型配置失败: %w", cfgErr)
	}
	if tag == "" && cfgErr == nil {
		tag = defaultEnhanceTag(cfg)
	}
	// dry-run 只列候选：配置坏了也不该阻断零副作用的预演。
	if dryRun {
		return tag, nil, nil
	}
	if tag == "" {
		return "", nil, errors.New("缺少 --model-tag，且模型配置无法推导归因标签（<provider>:<model>@" +
			enhancePromptVersion + "）；请传 --model-tag 或先跑 mem setup")
	}
	opt := distill.ResolveOptions(cfg, "", "")
	complete, _, err := enhancePromptCompleter(opt)
	if err != nil {
		return "", nil, fmt.Errorf("模型不可用（%w）。请先 mem setup 配置 provider/model/api_key，或显式传 --model-tag；候选正文会发往该端点", err)
	}
	return tag, complete, nil
}

// enhanceTagFor 拼归因标签：<provider>[:<model>]@<prompt 版本>。
// cli 来源的 providerName 形如 cli:<model_tag>，已含模型分量，不再重复拼。
func enhanceTagFor(providerName, model string) string {
	if strings.HasPrefix(providerName, "cli:") {
		return providerName + "@" + enhancePromptVersion
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return providerName + "@" + enhancePromptVersion
	}
	return providerName + ":" + model + "@" + enhancePromptVersion
}

// defaultEnhanceTag 从 model.json 推导归因标签。推导不出来返回空串，
// 由调用方决定「报错」而不是静默使用空 tag。
func defaultEnhanceTag(cfg distill.ModelConfig) string {
	if cfg.Mode == "cli" {
		if tag := strings.TrimSpace(cfg.ModelTag); tag != "" {
			return enhanceTagFor("cli:"+tag, "")
		}
		return ""
	}
	opt := distill.ResolveOptions(cfg, "", "")
	provider := strings.TrimSpace(opt.Provider)
	model := strings.TrimSpace(opt.Model)
	if model == "" {
		model = strings.TrimSpace(cfg.ModelTag)
	}
	if provider == "" && model == "" {
		return ""
	}
	if provider == "" {
		return model + "@" + enhancePromptVersion
	}
	return enhanceTagFor(provider, model)
}

// collectEnhanceCandidates 是选择器：显式 id 优先；否则按 filter 过滤存活记忆，
// 并默认跳过人工已确认/修正的条目。返回的 items 是选择阶段就跳过的明细，
// 让 dry-run 能解释「为什么这条没被处理」。
func collectEnhanceCandidates(st *store.Store, ids []string, filter string, includeConfirmed bool) (
	picks []enhanceCandidate, items []enhanceItem, scanned, skippedConfirmed, skippedOther, failed int, err error,
) {
	items = []enhanceItem{}
	if len(ids) > 0 {
		return collectEnhanceByIDs(st, ids, includeConfirmed)
	}
	list, err := st.ListEnhanceCandidates(0)
	if err != nil {
		return nil, nil, 0, 0, 0, 0, err
	}
	for _, c := range list {
		if isEnhanceConfirmed(c.ReviewState) && !includeConfirmed {
			skippedConfirmed++
			continue
		}
		reason, ok := enhanceMatchReason(c.Content, filter)
		if !ok {
			continue
		}
		picks = append(picks, enhanceCandidate{
			ID: c.ID, Content: c.Content, Kind: c.Kind, Project: c.Project,
			ContentHash: c.ContentHash, ReviewState: c.ReviewState, Reason: reason,
		})
	}
	return picks, items, len(list), skippedConfirmed, 0, 0, nil
}

func collectEnhanceByIDs(st *store.Store, ids []string, includeConfirmed bool) (
	picks []enhanceCandidate, items []enhanceItem, scanned, skippedConfirmed, skippedOther, failed int, err error,
) {
	items = []enhanceItem{}
	seen := map[string]bool{}
	for _, raw := range ids {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row, lerr := lookupEnhanceMemory(st, raw)
		if lerr != nil {
			if errors.Is(lerr, errEnhanceNotFound) {
				failed++
				scanned++
				items = append(items, enhanceItem{ID: raw, Status: "failed", Error: lerr.Error()})
				continue
			}
			return nil, nil, 0, 0, 0, 0, lerr
		}
		if seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		scanned++
		item := enhanceItem{ID: row.ID, Kind: row.Kind, Project: row.Project, ReviewState: row.ReviewState}
		switch {
		case row.ReviewState == store.ReviewStateWithdrawn:
			skippedOther++
			item.Status, item.Reason = "skipped", "withdrawn"
		case row.SupersededBy != "":
			skippedOther++
			item.Status, item.Reason = "skipped", "superseded"
		case row.ExpiresAt > 0 && row.ExpiresAt <= time.Now().Unix():
			skippedOther++
			item.Status, item.Reason = "skipped", "expired"
		case isEnhanceConfirmed(row.ReviewState) && !includeConfirmed:
			skippedConfirmed++
			item.Status, item.Reason = "skipped", row.ReviewState
		default:
			picks = append(picks, enhanceCandidate{
				ID: row.ID, Content: row.Content, Kind: row.Kind, Project: row.Project,
				ContentHash: row.ContentHash, ReviewState: row.ReviewState, Reason: "explicit",
			})
			continue
		}
		items = append(items, item)
	}
	return picks, items, scanned, skippedConfirmed, skippedOther, failed, nil
}

func isEnhanceConfirmed(state string) bool {
	return state == store.ReviewStateConfirmed || state == store.ReviewStateCorrected
}

// enhanceMatchReason 返回命中原因；all 不做过滤，原因按实际形态标注。
func enhanceMatchReason(content, filter string) (string, bool) {
	narrative := proposal.IsProcessNarrative(content)
	overlong := utf8.RuneCountInString(content) > enhanceOverlongRunes
	switch filter {
	case "narrative":
		return "narrative", narrative
	case "overlong":
		return "overlong", overlong
	case "all":
		switch {
		case narrative:
			return "narrative", true
		case overlong:
			return "overlong", true
		default:
			return "all", true
		}
	}
	return filter, false
}

var errEnhanceNotFound = errors.New("未找到记忆")

type enhanceMemoryRecord struct {
	ID           string
	Content      string
	Kind         string
	Project      string
	ContentHash  string
	ReviewState  string
	SupersededBy string
	ExpiresAt    int64
}

const enhanceMemoryColumns = `id, content, kind, project, content_hash,
	COALESCE(review_state, ''), COALESCE(superseded_by, ''), COALESCE(expires_at, 0)`

func lookupEnhanceMemory(st *store.Store, idOrPrefix string) (enhanceMemoryRecord, error) {
	row, err := scanEnhanceMemory(st.DB().QueryRow(
		`SELECT `+enhanceMemoryColumns+` FROM memories WHERE id = ?`, idOrPrefix))
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, errEnhanceNotFound) {
		return enhanceMemoryRecord{}, err
	}
	rows, err := st.DB().Query(
		`SELECT `+enhanceMemoryColumns+` FROM memories WHERE id LIKE ? ORDER BY id`, idOrPrefix+"%")
	if err != nil {
		return enhanceMemoryRecord{}, err
	}
	defer rows.Close()
	var matches []enhanceMemoryRecord
	for rows.Next() {
		r, serr := scanEnhanceMemory(rows)
		if serr != nil {
			return enhanceMemoryRecord{}, serr
		}
		matches = append(matches, r)
	}
	if err := rows.Err(); err != nil {
		return enhanceMemoryRecord{}, err
	}
	switch len(matches) {
	case 0:
		return enhanceMemoryRecord{}, fmt.Errorf("%w: %s", errEnhanceNotFound, idOrPrefix)
	case 1:
		return matches[0], nil
	default:
		return enhanceMemoryRecord{}, fmt.Errorf("id 前缀 %q 匹配到 %d 条记忆，请给更长的前缀", idOrPrefix, len(matches))
	}
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEnhanceMemory(row rowScanner) (enhanceMemoryRecord, error) {
	var r enhanceMemoryRecord
	err := row.Scan(&r.ID, &r.Content, &r.Kind, &r.Project, &r.ContentHash,
		&r.ReviewState, &r.SupersededBy, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, errEnhanceNotFound
	}
	return r, err
}

// enhanceBatchOutcome 是一批模型调用的结果。Err 非空表示整批失败：
// 缺项、多项、未知 id、空正文都不降级成部分写入。
type enhanceBatchOutcome struct {
	Candidates []enhanceCandidate
	Rewrites   map[string]string
	Err        error
}

func runEnhanceBatches(ctx context.Context, complete func(context.Context, string) (string, error),
	candidates []enhanceCandidate, batchSize int) []enhanceBatchOutcome {
	if batchSize <= 0 {
		batchSize = enhanceDefaultBatch
	}
	var out []enhanceBatchOutcome
	for start := 0; start < len(candidates); start += batchSize {
		end := start + batchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		chunk := candidates[start:end]
		b := enhanceBatchOutcome{Candidates: chunk}
		raw, err := complete(ctx, buildEnhancePrompt(chunk))
		if err == nil {
			b.Rewrites, err = parseEnhanceRewrites(raw, chunk)
		}
		b.Err = err
		out = append(out, b)
	}
	return out
}

func buildEnhancePrompt(candidates []enhanceCandidate) string {
	type inputItem struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Project string `json:"project"`
		Content string `json:"content"`
	}
	items := make([]inputItem, 0, len(candidates))
	for _, c := range candidates {
		items = append(items, inputItem{ID: c.ID, Kind: c.Kind, Project: c.Project, Content: c.Content})
	}
	b, _ := json.Marshal(items)
	return enhancePrompt + string(b)
}

// parseEnhanceRewrites 强制一输入一输出：输出 id 集合必须与输入完全一致，
// 不允许未知 id、重复、缺失或空正文。坏输出让整批失败，不做部分写入。
func parseEnhanceRewrites(raw string, chunk []enhanceCandidate) (map[string]string, error) {
	i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if i < 0 || j <= i {
		return nil, fmt.Errorf("未找到 JSON 对象（原文前 200 字：%s）", truncateRunes(raw, 200))
	}
	var parsed struct {
		Items []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw[i:j+1]), &parsed); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %w", err)
	}
	expected := make(map[string]bool, len(chunk))
	for _, c := range chunk {
		expected[c.ID] = true
	}
	out := make(map[string]string, len(chunk))
	for _, it := range parsed.Items {
		id := strings.TrimSpace(it.ID)
		content := strings.TrimSpace(it.Content)
		if id == "" {
			return nil, errors.New("输出存在空 id")
		}
		if !expected[id] {
			return nil, fmt.Errorf("输出包含未知 id %q", id)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("输出重复 id %q", id)
		}
		if content == "" {
			return nil, fmt.Errorf("输出 id %q 正文为空", id)
		}
		out[id] = content
	}
	for _, c := range chunk {
		if _, ok := out[c.ID]; !ok {
			return nil, fmt.Errorf("输出缺少 id %q", c.ID)
		}
	}
	return out, nil
}

func applyEnhancePreview(rep *enhanceReport, batches []enhanceBatchOutcome) {
	for _, b := range batches {
		rep.Processed += len(b.Candidates)
		if b.Err != nil {
			for _, c := range b.Candidates {
				rep.Failed++
				rep.Items = append(rep.Items, enhanceItem{
					ID: c.ID, Kind: c.Kind, Project: c.Project,
					ReviewState: c.ReviewState, Status: "failed", Error: b.Err.Error(),
				})
			}
			continue
		}
		for _, c := range b.Candidates {
			rep.Items = append(rep.Items, enhanceItem{
				ID: c.ID, Kind: c.Kind, Project: c.Project, ReviewState: c.ReviewState,
				Status: "preview", OldContent: c.Content, NewContent: b.Rewrites[c.ID],
			})
		}
	}
}

// applyEnhanceRewrites 逐条落库并立即归档。
//
// 归档失败必须记 failed 并明确报错：正文已经改了，回滚凭证不能静默丢。
func applyEnhanceRewrites(st *store.Store, rep *enhanceReport, batches []enhanceBatchOutcome, tag string) {
	started := time.Now().Unix()
	archivePath := ""
	for _, b := range batches {
		rep.Processed += len(b.Candidates)
		if b.Err != nil {
			for _, c := range b.Candidates {
				rep.Failed++
				rep.Items = append(rep.Items, enhanceItem{
					ID: c.ID, Kind: c.Kind, Project: c.Project,
					ReviewState: c.ReviewState, Status: "failed", Error: b.Err.Error(),
				})
			}
			continue
		}
		for _, c := range b.Candidates {
			newContent := b.Rewrites[c.ID]
			item := enhanceItem{ID: c.ID, Kind: c.Kind, Project: c.Project, ReviewState: c.ReviewState}
			res, err := st.RewriteMemoryContent(c.ID, newContent, tag, time.Now().Unix())
			switch {
			case err != nil:
				item.Error = err.Error()
				if strings.Contains(err.Error(), "内容相同") {
					item.Status = "conflict"
					rep.Conflict++
				} else {
					item.Status = "failed"
					rep.Failed++
				}
			case !res.Changed:
				item.Status = "unchanged"
				rep.Unchanged++
			default:
				if archivePath == "" {
					archivePath = enhanceArchivePath(st.Path(), started, tag)
					rep.Archive = archivePath
				}
				rec := enhanceArchiveRecord{
					ID: res.MemoryID, Kind: c.Kind, Project: c.Project,
					OldContent: res.OldContent, OldHash: res.OldHash,
					NewContent: newContent, NewHash: res.NewHash,
					EnhanceTag: tag, At: time.Now().Unix(),
				}
				if aerr := appendEnhanceArchive(archivePath, rec); aerr != nil {
					item.Status = "failed"
					item.Error = fmt.Sprintf("改写已落库但归档失败（回滚凭证缺失）: %v", aerr)
					rep.Failed++
				} else {
					item.Status = "rewritten"
					item.OldContent, item.NewContent = res.OldContent, newContent
					rep.Rewritten++
				}
			}
			rep.Items = append(rep.Items, item)
		}
	}
}

// runEnhanceRollback 只还原正文，不选候选、不调模型。
func runEnhanceRollback(st *store.Store, asJSON bool, target string, dryRun bool) error {
	files, err := resolveEnhanceRollbackFiles(st.Path(), target)
	if err != nil {
		return err
	}
	records, err := readEnhanceArchive(files[0])
	if err != nil {
		return err
	}
	rep := &enhanceReport{
		Mode: "rollback", Filter: "-", DryRun: dryRun,
		Scanned: len(records), Eligible: len(records), Processed: len(records),
		Items: []enhanceItem{},
	}
	if len(records) > 0 {
		rep.ModelTag = records[0].EnhanceTag
	}
	now := time.Now().Unix()

	// 后写的先还原：同一 tag 多次归档时，先撤销最后一次改写。
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		item := enhanceItem{ID: rec.ID, Kind: rec.Kind, Project: rec.Project}
		if dryRun {
			var curHash string
			lerr := st.DB().QueryRow(`SELECT content_hash FROM memories WHERE id = ?`, rec.ID).Scan(&curHash)
			switch {
			case errors.Is(lerr, sql.ErrNoRows):
				item.Status, item.Error = "failed", "未找到记忆: "+rec.ID
				rep.Failed++
			case lerr != nil:
				item.Status, item.Error = "failed", lerr.Error()
				rep.Failed++
			case curHash != rec.NewHash:
				item.Status, item.Reason = "skipped", "content_changed"
				rep.Skipped++
			default:
				item.Status = "restored"
				rep.Restored++
			}
			rep.Items = append(rep.Items, item)
			continue
		}
		restored, reason, rerr := st.RestoreMemoryContent(rec.ID, rec.OldContent, rec.NewHash, now)
		switch {
		case rerr != nil:
			item.Status, item.Error = "failed", rerr.Error()
			rep.Failed++
		case restored:
			item.Status = "restored"
			rep.Restored++
		case reason == "content_changed":
			item.Status, item.Reason = "skipped", reason
			rep.Skipped++
		case strings.HasPrefix(reason, "conflict:"):
			item.Status, item.Reason = "skipped_conflict", reason
			rep.SkippedConflict++
		default:
			item.Status = "skipped"
			rep.Skipped++
		}
		rep.Items = append(rep.Items, item)
	}

	if err := emitEnhanceReport(rep, asJSON); err != nil {
		return err
	}
	if rep.Failed > 0 {
		return fmt.Errorf("%d 条回滚失败", rep.Failed)
	}
	return nil
}

func resolveEnhanceRollbackFiles(dbPath, target string) ([]string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, errors.New("缺少回滚目标（tag 或归档文件）")
	}
	if strings.ContainsAny(target, `/\`) || strings.HasSuffix(target, ".jsonl") {
		if _, err := os.Stat(target); err != nil {
			return nil, fmt.Errorf("归档文件不可用（%s）: %w", target, err)
		}
		return []string{target}, nil
	}
	pattern := filepath.Join(filepath.Dir(dbPath), "enhance", "*-"+sanitizeEnhanceTag(target)+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("未找到 tag %q 的归档（%s）", target, pattern)
	case 1:
		return matches, nil
	default:
		return nil, fmt.Errorf("tag %q 匹配到 %d 个归档，请改用具体文件：%s",
			target, len(matches), strings.Join(matches, ", "))
	}
}

func readEnhanceArchive(path string) ([]enhanceArchiveRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []enhanceArchiveRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" {
			continue
		}
		var rec enhanceArchiveRecord
		if err := json.Unmarshal([]byte(s), &rec); err != nil {
			return nil, fmt.Errorf("归档第 %d 行解析失败: %w", line, err)
		}
		if rec.ID == "" {
			return nil, fmt.Errorf("归档第 %d 行缺少 id", line)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("归档为空: %s", path)
	}
	return out, nil
}

func enhanceArchivePath(dbPath string, unix int64, tag string) string {
	return filepath.Join(filepath.Dir(dbPath), "enhance",
		fmt.Sprintf("%d-%s.jsonl", unix, sanitizeEnhanceTag(tag)))
}

// enhanceTagUnsafe 把 tag 里可能逃出 db 目录或破坏 glob 的字符统一替换掉。
var enhanceTagUnsafe = regexp.MustCompile(`[^A-Za-z0-9._@+:-]+`)

func sanitizeEnhanceTag(tag string) string {
	s := enhanceTagUnsafe.ReplaceAllString(strings.TrimSpace(tag), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "unknown"
	}
	return s
}

func appendEnhanceArchive(path string, rec enhanceArchiveRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		f.Close()
		return err
	}
	b = append(b, '\n')
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func finishEnhance(rep *enhanceReport, asJSON bool, action string) error {
	if err := emitEnhanceReport(rep, asJSON); err != nil {
		return err
	}
	if rep.Failed > 0 {
		return fmt.Errorf("%d 条%s失败", rep.Failed, action)
	}
	return nil
}

func emitEnhanceReport(rep *enhanceReport, asJSON bool) error {
	if asJSON {
		return printJSON(rep)
	}
	renderEnhanceReport(rep)
	return nil
}

func renderEnhanceReport(rep *enhanceReport) {
	if rep.Mode == "rollback" {
		dry := ""
		if rep.DryRun {
			dry = "（dry-run，未写库）"
		}
		fmt.Printf("回滚 %s%s：共 %d 条，还原 %d / 跳过 %d / 冲突跳过 %d / 失败 %d\n",
			rep.ModelTag, dry, rep.Scanned, rep.Restored, rep.Skipped, rep.SkippedConflict, rep.Failed)
		for _, it := range rep.Items {
			if it.Status == "restored" {
				continue
			}
			detail := it.Reason
			if it.Error != "" {
				detail = it.Error
			}
			fmt.Printf("  %s  %s  %s\n", shortID(it.ID), it.Status, detail)
		}
		return
	}

	switch rep.Mode {
	case "dry-run":
		fmt.Printf("预演（不写库）：扫描 %d 条，符合 %d 条（已确认/修正跳过 %d，其它跳过 %d，失败 %d）\n",
			rep.Scanned, rep.Eligible, rep.SkippedConfirmed, rep.SkippedOther, rep.Failed)
		for _, it := range rep.Items {
			if it.Status != "candidate" {
				continue
			}
			fmt.Printf("  %s  [%s/%s]  %s  原因=%s\n",
				shortID(it.ID), it.Kind, enhanceProjectText(it.Project),
				reviewStateText(it.ReviewState), it.Reason)
		}
	case "preview":
		fmt.Printf("预览（不写库）：扫描 %d 条，符合 %d 条，处理 %d 条\n",
			rep.Scanned, rep.Eligible, rep.Processed)
		for _, it := range rep.Items {
			switch it.Status {
			case "preview":
				fmt.Printf("  %s\n    改写前: %s\n    改写后: %s\n",
					shortID(it.ID), oneLine(it.OldContent), oneLine(it.NewContent))
			case "failed":
				fmt.Printf("  %s  失败: %s\n", shortID(it.ID), it.Error)
			}
		}
	default:
		fmt.Printf("正文提炼：扫描 %d 条，符合 %d 条，处理 %d 条\n",
			rep.Scanned, rep.Eligible, rep.Processed)
		fmt.Printf("  改写 %d / 未变 %d / 冲突 %d / 失败 %d；跳过（已确认 %d，其它 %d）\n",
			rep.Rewritten, rep.Unchanged, rep.Conflict, rep.Failed, rep.SkippedConfirmed, rep.SkippedOther)
		if rep.Archive != "" {
			fmt.Printf("  归档 → %s\n", rep.Archive)
		}
		for _, it := range rep.Items {
			switch it.Status {
			case "conflict", "failed":
				fmt.Printf("  %s  %s: %s\n", shortID(it.ID), it.Status, it.Error)
			}
		}
	}
}

func enhanceProjectText(project string) string {
	if strings.TrimSpace(project) == "" {
		return "global"
	}
	return project
}
