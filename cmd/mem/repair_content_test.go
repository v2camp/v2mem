package main

// mem repair content 的命令面测试：选择器三档、三种执行形态、一进一出校验、
// 批量调用次数、缺归因标签、归档字段与按 tag/文件回滚。全部用 t.TempDir 临时库，
// 模型一律注入 fake，不碰网络与 ~/.v2mem。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/store"
)

const testEnhanceTag = "local:fake@enhance-v1"

func newEnhanceDB(t *testing.T) (string, *store.Store) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "mem.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return db, st
}

func addEnhanceMemory(t *testing.T, st *store.Store, content, project, kind string) string {
	t.Helper()
	// 用非直写来源落库，review_state 保持未审核——直写来源默认 confirmed，
	// 会让默认「跳过已确认」把所有候选都挡掉。
	res, err := st.Add(store.AddInput{
		Content: content, Project: project, Kind: kind, Source: "distill-accept",
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return res.ID
}

func confirmEnhanceMemory(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.ApplyMemoryReview(store.MemoryReviewInput{
		MemoryID: id, Action: store.ReviewActionConfirm, Actor: "human",
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
}

func correctEnhanceMemory(t *testing.T, st *store.Store, id, content string) {
	t.Helper()
	if _, err := st.ApplyMemoryReview(store.MemoryReviewInput{
		MemoryID: id, Action: store.ReviewActionCorrect, Content: content, Actor: "human",
	}); err != nil {
		t.Fatalf("correct: %v", err)
	}
}

func enhanceMemoryRow(t *testing.T, st *store.Store, id string) (content, hash, state, by string) {
	t.Helper()
	if err := st.DB().QueryRow(
		`SELECT content, content_hash, review_state, reviewed_by FROM memories WHERE id = ?`, id).
		Scan(&content, &hash, &state, &by); err != nil {
		t.Fatalf("query %s: %v", id, err)
	}
	return
}

// fakeEnhanceCompleter 记录调用次数并返回 fn 的结果；fn 从提示词尾部取本批 id。
type fakeEnhanceCompleter struct {
	calls int
	fn    func(prompt string) (string, error)
}

func installFakeEnhance(t *testing.T, fn func(prompt string) (string, error)) *fakeEnhanceCompleter {
	t.Helper()
	f := &fakeEnhanceCompleter{fn: fn}
	old := enhancePromptCompleter
	enhancePromptCompleter = func(opt distill.Options) (func(context.Context, string) (string, error), string, error) {
		return func(ctx context.Context, prompt string) (string, error) {
			f.calls++
			return f.fn(prompt)
		}, "fake", nil
	}
	t.Cleanup(func() { enhancePromptCompleter = old })
	return f
}

func enhanceInputIDs(t *testing.T, prompt string) []string {
	t.Helper()
	const marker = "输入："
	i := strings.LastIndex(prompt, marker)
	if i < 0 {
		t.Fatalf("提示词缺少输入段:\n%s", prompt)
	}
	raw := strings.TrimSpace(prompt[i+len(marker):])
	var items []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("解析提示词输入失败: %v\n%s", err, raw)
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

func enhanceOutputFor(ids []string, content func(id string) string) string {
	type outItem struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	items := make([]outItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, outItem{ID: id, Content: content(id)})
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return string(b)
}

func runRepairContentJSON(t *testing.T, args ...string) (enhanceReport, string, error) {
	t.Helper()
	var out string
	var err error
	out, err = captureStdout(t, func() error { return cmdRepairContent(args) })
	var rep enhanceReport
	if out != "" {
		if uerr := json.Unmarshal([]byte(out), &rep); uerr != nil {
			t.Fatalf("输出应是纯 JSON: %v\n%s", uerr, out)
		}
	}
	return rep, out, err
}

// TestRepairContentSelectorsAndIncludeConfirmed 覆盖三档选择器、显式 id 优先、
// 默认跳过 confirmed/corrected 与 --include-confirmed 开关。
func TestRepairContentSelectorsAndIncludeConfirmed(t *testing.T) {
	db, st := newEnhanceDB(t)
	const proj = "proj"
	nar1 := addEnhanceMemory(t, st, "我刚才尝试了那个命令，报错了", proj, "fact")
	nar2 := addEnhanceMemory(t, st, "本次修复已完成，提交 abc1234", proj, "pitfall")
	long := addEnhanceMemory(t, st, strings.Repeat("构建规则", 130), proj, "fact")
	plain := addEnhanceMemory(t, st, "CGO_ENABLED=0 构建 v2mem 前必须显式设置，否则 sqlite 驱动编译失败", proj, "decision")
	conf := addEnhanceMemory(t, st, "我刚才确认了这条规则要保留", proj, "fact")
	corr := addEnhanceMemory(t, st, "我刚才修正了这条规则", proj, "fact")
	confirmEnhanceMemory(t, st, conf)
	correctEnhanceMemory(t, st, corr, "修正后的正文只描述规则")

	// dry-run 绝不能调模型：被调用即 panic。
	oldLoad := enhanceLoadConfig
	enhanceLoadConfig = func(string) (distill.ModelConfig, error) { return distill.ModelConfig{}, nil }
	oldCompleter := enhancePromptCompleter
	enhancePromptCompleter = func(distill.Options) (func(context.Context, string) (string, error), string, error) {
		panic("--dry-run 不得调用模型")
	}
	t.Cleanup(func() {
		enhanceLoadConfig = oldLoad
		enhancePromptCompleter = oldCompleter
	})

	cases := []struct {
		name         string
		args         []string
		wantEligible int
		wantSkipped  int
		wantIDs      []string
	}{
		{"narrative", []string{"--filter=narrative"}, 2, 2, []string{nar1, nar2}},
		{"overlong", []string{"--filter=overlong"}, 1, 2, []string{long}},
		{"all", []string{"--filter=all"}, 4, 2, []string{nar1, nar2, long, plain}},
		{"all-include-confirmed", []string{"--filter=all", "--include-confirmed"}, 6, 0,
			[]string{nar1, nar2, long, plain, conf, corr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--db", db, "--dry-run", "--json"}, tc.args...)
			rep, _, err := runRepairContentJSON(t, args...)
			if err != nil {
				t.Fatalf("dry-run: %v", err)
			}
			if rep.Mode != "dry-run" {
				t.Errorf("mode=%q，期望 dry-run", rep.Mode)
			}
			if rep.Scanned != 6 || rep.Eligible != tc.wantEligible || rep.Processed != 0 {
				t.Errorf("计数不对: scanned=%d eligible=%d processed=%d，期望 6/%d/0",
					rep.Scanned, rep.Eligible, rep.Processed, tc.wantEligible)
			}
			if rep.SkippedConfirmed != tc.wantSkipped || rep.Rewritten != 0 || rep.Failed != 0 {
				t.Errorf("跳过/改写/失败计数不对: %+v", rep)
			}
			got := make(map[string]string, len(rep.Items))
			for _, it := range rep.Items {
				got[it.ID] = it.Reason
				if it.Kind == "" || it.Project != proj {
					t.Errorf("候选投影缺字段: %+v", it)
				}
			}
			for _, id := range tc.wantIDs {
				if _, ok := got[id]; !ok {
					t.Errorf("候选 %s 应入选，got %v", shortID(id), got)
				}
			}
			if len(got) != tc.wantEligible {
				t.Errorf("入选条数=%d，期望 %d", len(got), tc.wantEligible)
			}
		})
	}

	// 显式 id 优先且忽略 --filter；confirmed 默认跳过，--include-confirmed 才处理。
	rep, _, err := runRepairContentJSON(t, "--db", db, "--dry-run", "--json",
		"--filter=overlong", nar1[:8])
	if err != nil {
		t.Fatalf("显式 id: %v", err)
	}
	if rep.Scanned != 1 || rep.Eligible != 1 || rep.Items[0].Reason != "explicit" {
		t.Fatalf("显式 id 应按 explicit 处理且忽略 filter，got %+v", rep)
	}

	rep, _, err = runRepairContentJSON(t, "--db", db, "--dry-run", "--json", conf)
	if err != nil {
		t.Fatalf("显式 confirmed: %v", err)
	}
	if rep.Eligible != 0 || rep.SkippedConfirmed != 1 {
		t.Fatalf("显式 confirmed 默认应跳过，got %+v", rep)
	}
	rep, _, err = runRepairContentJSON(t, "--db", db, "--dry-run", "--json",
		"--include-confirmed", conf)
	if err != nil {
		t.Fatalf("显式 confirmed + include: %v", err)
	}
	if rep.Eligible != 1 || rep.SkippedConfirmed != 0 {
		t.Fatalf("--include-confirmed 应处理 confirmed，got %+v", rep)
	}

	// withdrawn 永远跳过（即使显式指定）。
	wd := addEnhanceMemory(t, st, "我刚才试了撤回前的规则", proj, "fact")
	if _, err := st.ApplyMemoryReview(store.MemoryReviewInput{
		MemoryID: wd, Action: store.ReviewActionWithdraw, Actor: "human",
	}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	rep, _, err = runRepairContentJSON(t, "--db", db, "--dry-run", "--json", wd)
	if err != nil {
		t.Fatalf("显式 withdrawn: %v", err)
	}
	if rep.Eligible != 0 || rep.SkippedOther != 1 {
		t.Fatalf("withdrawn 应计入 skipped_other，got %+v", rep)
	}
}

// TestRepairContentPreviewCallsModelWithoutWriting 验证 preview 调模型但零写入。
func TestRepairContentPreviewCallsModelWithoutWriting(t *testing.T) {
	db, st := newEnhanceDB(t)
	a := addEnhanceMemory(t, st, "我刚才尝试了命令 A，报错了", "p", "fact")
	b := addEnhanceMemory(t, st, "本次修复 B 已完成", "p", "fact")
	f := installFakeEnhance(t, func(prompt string) (string, error) {
		return enhanceOutputFor(enhanceInputIDs(t, prompt), func(id string) string {
			return "遇到构建报错时，先确认 CGO_ENABLED 设置（" + shortID(id) + "）"
		}), nil
	})

	rep, _, err := runRepairContentJSON(t, "--db", db, "--preview", "--json",
		"--model-tag", testEnhanceTag)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("preview 应调用模型 1 次，got %d", f.calls)
	}
	if rep.Mode != "preview" || rep.Processed != 2 || rep.Rewritten != 0 {
		t.Fatalf("preview 报告不对: %+v", rep)
	}
	if len(rep.Items) != 2 || rep.Items[0].OldContent == "" || rep.Items[0].NewContent == "" {
		t.Fatalf("preview 应给出 before/after: %+v", rep.Items)
	}
	for id, want := range map[string]string{a: "我刚才尝试了命令 A，报错了", b: "本次修复 B 已完成"} {
		if got, _, _, _ := enhanceMemoryRow(t, st, id); got != want {
			t.Errorf("preview 不得写库，%s got %q want %q", shortID(id), got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(db), "enhance")); !os.IsNotExist(err) {
		t.Errorf("preview 不得产生归档目录，got err=%v", err)
	}
}

// TestRepairContentApplyConflictAndArchive 覆盖 apply 的成功/冲突两条路径与归档字段。
func TestRepairContentApplyConflictAndArchive(t *testing.T) {
	db, st := newEnhanceDB(t)
	proj := "proj"
	a := addEnhanceMemory(t, st, "我刚才尝试了命令 A，报错了", proj, "fact")
	b := addEnhanceMemory(t, st, "本次修复 B 已完成", proj, "pitfall")
	c := addEnhanceMemory(t, st, "那个报错已经处理过了", proj, "fact")
	// 同工程已有正文：模型的改写稿撞上它，应记 conflict 而不是写库。
	collide := "CGO_ENABLED=0 构建前必须显式设置，否则 sqlite 驱动报错"
	addEnhanceMemory(t, st, collide, proj, "decision")

	newContent := map[string]string{
		a: "遇到命令报错时，先检查环境变量是否缺失",
		b: "构建失败时先确认 CGO_ENABLED 设置",
		c: collide,
	}
	installFakeEnhance(t, func(prompt string) (string, error) {
		return enhanceOutputFor(enhanceInputIDs(t, prompt), func(id string) string {
			return newContent[id]
		}), nil
	})

	rep, _, err := runRepairContentJSON(t, "--db", db, "--json", "--model-tag", testEnhanceTag)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Mode != "apply" || rep.Rewritten != 2 || rep.Conflict != 1 || rep.Failed != 0 {
		t.Fatalf("apply 报告不对: %+v", rep)
	}
	for _, id := range []string{a, b} {
		content, hash, state, by := enhanceMemoryRow(t, st, id)
		if content != newContent[id] {
			t.Errorf("%s 正文未改写: %q", shortID(id), content)
		}
		if hash != store.HashOf(content) {
			t.Errorf("%s content_hash 未重算", shortID(id))
		}
		if state != "" || by != "" {
			t.Errorf("%s 应回到未审核且清空 reviewed_by，got state=%q by=%q", shortID(id), state, by)
		}
	}
	if got, _, _, _ := enhanceMemoryRow(t, st, c); got != "那个报错已经处理过了" {
		t.Errorf("冲突条目不得写库，got %q", got)
	}
	if rep.Archive == "" {
		t.Fatal("成功改写应给出归档路径")
	}
	data, err := os.ReadFile(rep.Archive)
	if err != nil {
		t.Fatalf("读归档: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("归档应有 2 行，got %d:\n%s", len(lines), data)
	}
	required := []string{"id", "kind", "project", "old_content", "old_hash",
		"new_content", "new_hash", "enhance_tag", "at"}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("归档行不是 JSON: %v\n%s", err, line)
		}
		for _, k := range required {
			if _, ok := m[k]; !ok {
				t.Errorf("归档行缺少字段 %q: %s", k, line)
			}
		}
		if m["enhance_tag"] != testEnhanceTag {
			t.Errorf("enhance_tag=%v，期望 %s", m["enhance_tag"], testEnhanceTag)
		}
		if m["old_hash"] == m["new_hash"] {
			t.Errorf("old_hash 与 new_hash 不应相同: %s", line)
		}
		if at, ok := m["at"].(float64); !ok || at <= 0 {
			t.Errorf("at 缺失或非法: %v", m["at"])
		}
	}
}

// TestRepairContentOneToOneViolations 验证多一个/少一个 id 都让整批失败且零写。
func TestRepairContentOneToOneViolations(t *testing.T) {
	cases := []struct {
		name  string
		build func(ids []string) string
	}{
		{"多一个 id", func(ids []string) string {
			return enhanceOutputFor(append(ids, "deadbeefdeadbeefdeadbeefdeadbeef"),
				func(id string) string { return "规则化正文 " + shortID(id) })
		}},
		{"少一个 id", func(ids []string) string {
			return enhanceOutputFor(ids[:1], func(id string) string { return "规则化正文 " + shortID(id) })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, st := newEnhanceDB(t)
			a := addEnhanceMemory(t, st, "我刚才尝试了命令 A，报错了", "p", "fact")
			b := addEnhanceMemory(t, st, "本次修复 B 已完成", "p", "fact")
			installFakeEnhance(t, func(prompt string) (string, error) {
				return tc.build(enhanceInputIDs(t, prompt)), nil
			})

			rep, _, err := runRepairContentJSON(t, "--db", db, "--json",
				"--model-tag", testEnhanceTag)
			if err == nil {
				t.Fatal("一进一出违例应返回错误（failed>0）")
			}
			if rep.Rewritten != 0 || rep.Failed != 2 {
				t.Fatalf("整批应失败且零写，got %+v", rep)
			}
			for id, want := range map[string]string{a: "我刚才尝试了命令 A，报错了", b: "本次修复 B 已完成"} {
				if got, _, _, _ := enhanceMemoryRow(t, st, id); got != want {
					t.Errorf("%s 不得写库，got %q", shortID(id), got)
				}
			}
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(db), "enhance")); !os.IsNotExist(statErr) {
				t.Errorf("零写时不得产生归档目录，got err=%v", statErr)
			}
		})
	}
}

// TestRepairContentBatchCallCount 验证 12 条 candidate、batch=10 恰好 2 次模型调用。
func TestRepairContentBatchCallCount(t *testing.T) {
	db, st := newEnhanceDB(t)
	for i := 0; i < 12; i++ {
		addEnhanceMemory(t, st, fmt.Sprintf("我刚才尝试了第 %d 个命令", i), "p", "fact")
	}
	f := installFakeEnhance(t, func(prompt string) (string, error) {
		return enhanceOutputFor(enhanceInputIDs(t, prompt), func(id string) string {
			return "规则化正文 " + id
		}), nil
	})

	rep, _, err := runRepairContentJSON(t, "--db", db, "--json", "--model-tag", testEnhanceTag)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f.calls != 2 {
		t.Fatalf("12 条 / batch 10 应调用 2 次，got %d", f.calls)
	}
	if rep.Rewritten != 12 || rep.Failed != 0 {
		t.Fatalf("应全部改写，got %+v", rep)
	}
}

// TestRepairContentMissingModelTag 验证归因标签缺失即报错且不写库。
func TestRepairContentMissingModelTag(t *testing.T) {
	db, st := newEnhanceDB(t)
	id := addEnhanceMemory(t, st, "我刚才尝试了命令 A，报错了", "p", "fact")
	f := installFakeEnhance(t, func(prompt string) (string, error) {
		return "", fmt.Errorf("不应被调用")
	})
	oldLoad := enhanceLoadConfig
	enhanceLoadConfig = func(string) (distill.ModelConfig, error) { return distill.ModelConfig{}, nil }
	t.Cleanup(func() { enhanceLoadConfig = oldLoad })

	_, _, err := runRepairContentJSON(t, "--db", db, "--json")
	if err == nil || !strings.Contains(err.Error(), "model-tag") {
		t.Fatalf("缺 --model-tag 应报错，got %v", err)
	}
	if f.calls != 0 {
		t.Errorf("标签缺失时不得调用模型，got %d", f.calls)
	}
	if got, _, _, _ := enhanceMemoryRow(t, st, id); got != "我刚才尝试了命令 A，报错了" {
		t.Errorf("不得写库，got %q", got)
	}
}

// TestRepairContentRollback 覆盖按文件/按 tag 回滚、hash 守卫与 dry-run 零写。
func TestRepairContentRollback(t *testing.T) {
	db, st := newEnhanceDB(t)
	a := addEnhanceMemory(t, st, "我刚才尝试了命令 A，报错了", "p", "fact")
	b := addEnhanceMemory(t, st, "本次修复 B 已完成", "p", "fact")
	newContent := map[string]string{
		a: "遇到命令 A 报错时，先确认环境变量",
		b: "修复 B 完成后，先跑回归再提交",
	}
	installFakeEnhance(t, func(prompt string) (string, error) {
		return enhanceOutputFor(enhanceInputIDs(t, prompt), func(id string) string {
			return newContent[id]
		}), nil
	})
	applyRep, _, err := runRepairContentJSON(t, "--db", db, "--json", "--model-tag", testEnhanceTag)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applyRep.Archive == "" {
		t.Fatal("apply 应产生归档")
	}
	if _, err := os.Stat(applyRep.Archive); err != nil {
		t.Fatalf("归档文件不存在: %v", err)
	}

	// dry-run 回滚：只报告，零写。
	dryRep, _, err := runRepairContentJSON(t, "--db", db, "--json",
		"--rollback", applyRep.Archive, "--dry-run")
	if err != nil {
		t.Fatalf("rollback dry-run: %v", err)
	}
	if dryRep.Mode != "rollback" || dryRep.Restored != 2 || dryRep.Failed != 0 {
		t.Fatalf("dry-run 回滚应报 2 条可还原，got %+v", dryRep)
	}
	for id := range newContent {
		if got, _, _, _ := enhanceMemoryRow(t, st, id); got != newContent[id] {
			t.Errorf("dry-run 不得写库，%s got %q", shortID(id), got)
		}
	}

	// 人工修正 a 后，回滚应跳过它（content_changed），只还原 b。
	correctEnhanceMemory(t, st, a, "人工修正 A：以后遇到此报错先看日志")
	rep, _, err := runRepairContentJSON(t, "--db", db, "--json", "--rollback", applyRep.Archive)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rep.Restored != 1 || rep.Skipped != 1 || rep.SkippedConflict != 0 || rep.Failed != 0 {
		t.Fatalf("回滚计数不对: %+v", rep)
	}
	if got, _, state, _ := enhanceMemoryRow(t, st, a); got != "人工修正 A：以后遇到此报错先看日志" || state != store.ReviewStateCorrected {
		t.Errorf("人工修正不得被回滚覆盖，got %q state=%q", got, state)
	}
	if got, _, _, _ := enhanceMemoryRow(t, st, b); got != "本次修复 B 已完成" {
		t.Errorf("b 应还原为原正文，got %q", got)
	}

	// 幂等：第二次回滚（按 tag 找文件）全部 skipped。
	rep, _, err = runRepairContentJSON(t, "--db", db, "--json", "--rollback", testEnhanceTag)
	if err != nil {
		t.Fatalf("rollback by tag: %v", err)
	}
	if rep.Restored != 0 || rep.Skipped != 2 || rep.Failed != 0 {
		t.Fatalf("二次回滚应全 skipped，got %+v", rep)
	}
}

// TestRepairContentJSONFields 校验 dry-run 的 JSON 报告字段齐全。
func TestRepairContentJSONFields(t *testing.T) {
	db, st := newEnhanceDB(t)
	addEnhanceMemory(t, st, "我刚才尝试了命令 A，报错了", "p", "fact")
	out, err := captureStdout(t, func() error {
		return cmdRepairContent([]string{"--db", db, "--dry-run", "--json"})
	})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("JSON: %v\n%s", err, out)
	}
	for _, k := range []string{"mode", "filter", "model_tag", "scanned", "eligible",
		"processed", "rewritten", "unchanged", "skipped_confirmed", "skipped_other",
		"conflict", "failed", "archive"} {
		if _, ok := m[k]; !ok {
			t.Errorf("报告缺少字段 %q: %s", k, out)
		}
	}
}

// TestRepairHelpMentionsContent 验证 help 与无参提示都带上了新子命令。
func TestRepairHelpMentionsContent(t *testing.T) {
	out, err := captureStdout(t, func() error { return cmdRepair([]string{"help"}) })
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(out, "content") {
		t.Errorf("help 应包含 content：\n%s", out)
	}
	if err := cmdRepair(nil); err == nil || !strings.Contains(err.Error(), "content") {
		t.Errorf("无参提示应包含 content，got %v", err)
	}
}

// TestRunEnhanceBatchesIsolatesFailures 验证批失败不影响后续批。
func TestRunEnhanceBatchesIsolatesFailures(t *testing.T) {
	items := make([]enhanceCandidate, 0, 3)
	for i := 0; i < 3; i++ {
		items = append(items, enhanceCandidate{ID: fmt.Sprintf("id-%d", i), Content: fmt.Sprintf("正文 %d", i)})
	}
	calls := 0
	out := runEnhanceBatches(context.Background(), func(_ context.Context, prompt string) (string, error) {
		calls++
		ids := enhanceInputIDs(t, prompt)
		if calls == 1 {
			return "not json", nil
		}
		return enhanceOutputFor(ids, func(id string) string { return "改写 " + id }), nil
	}, items, 2)
	if calls != 2 || len(out) != 2 {
		t.Fatalf("应 2 批 2 次调用，got calls=%d batches=%d", calls, len(out))
	}
	if out[0].Err == nil || out[1].Err != nil {
		t.Fatalf("第一批应失败、第二批应成功，got %+v", out)
	}
	if len(out[1].Rewrites) != 1 {
		t.Fatalf("第二批应只有 1 条改写，got %+v", out[1])
	}
}
