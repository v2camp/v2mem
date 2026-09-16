package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanghui/v2mem/internal/audit"
	"github.com/wanghui/v2mem/internal/store"
)

// ---------- 审计埋点 ----------

func TestCmdHookWritesAuditRecord(t *testing.T) {
	db := testDB(t)
	proj, name := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--project", name, "--kind", "pitfall",
		"记忆库不能放进 iCloud 同步目录，会损坏 SQLite"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	ap := filepath.Join(t.TempDir(), "audit.jsonl")

	if _, err := withStdin(t, hookStdin("UserPromptSubmit", proj, "iCloud 同步目录能不能放库"), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--audit", ap})
	}); err != nil {
		t.Fatalf("cmdHook: %v", err)
	}

	rs, err := audit.Read(ap, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("应写入 1 条审计，got %d", len(rs))
	}
	r := rs[0]
	if r.Event != "prompt-submit" || r.Harness != "claude" || r.Project != name {
		t.Errorf("审计字段有误: %+v", r)
	}
	if !strings.Contains(r.Query, "iCloud") {
		t.Errorf("审计应记录真实 query（这是评测样本的来源），got %q", r.Query)
	}
	if len(r.Hashes) == 0 {
		t.Errorf("审计应记录实际注入的记忆 id，got %+v", r)
	}
}

func TestCmdHookAuditCanBeDisabled(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	ap := filepath.Join(t.TempDir(), "audit.jsonl")
	if _, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--audit", "-"})
	}); err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	if _, err := os.Stat(ap); !os.IsNotExist(err) {
		t.Error("--audit - 应完全不写审计")
	}
}

// 审计写失败绝不能影响注入 —— 这条在宿主会话的关键链上。
func TestCmdHookStillInjectsWhenAuditPathUnwritable(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	if err := cmdAdd([]string{"--db", db, "--global", "全局硬规则样本"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	// 指向一个不可写的路径（把目录当成文件用）
	bad := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(bad, []byte("x"), 0o444); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	out, err := withStdin(t, hookStdin("SessionStart", proj, ""), func() error {
		return cmdHook([]string{"--db", db, "--harness", "claude", "--audit", filepath.Join(bad, "a.jsonl")})
	})
	if err != nil {
		t.Errorf("审计失败不应让钩子报错: %v", err)
	}
	if !strings.Contains(out, "全局硬规则样本") {
		t.Errorf("审计失败时仍必须正常注入，got:\n%s", out)
	}
}

func TestCmdAuditSummarizesRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	for i := 0; i < 3; i++ {
		if err := audit.Append(p, audit.Record{Event: "prompt-submit", Harness: "claude", Query: "q", Empty: i == 0}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	out, err := captureStdout(t, func() error { return cmdAudit([]string{"--audit-file", p, "--stats"}) })
	if err != nil {
		t.Fatalf("cmdAudit: %v", err)
	}
	for _, want := range []string{"总激活次数: 3", "可评测样本: 3", "按事件"} {
		if !strings.Contains(out, want) {
			t.Errorf("汇总应含 %q，got:\n%s", want, out)
		}
	}
}

// ---------- 检索评测 ----------

func writeGold(t *testing.T, dir string, cases []map[string]any) string {
	t.Helper()
	p := filepath.Join(dir, "gold.jsonl")
	var sb strings.Builder
	for _, c := range cases {
		b, _ := json.Marshal(c)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// 取库内某条记忆的 hash 前缀 —— 金标准用哈希而不是文本匹配。
func hashPrefixOf(t *testing.T, db, contentSub string) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	hits, err := st.List(store.ListQuery{Scope: "all", Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, h := range hits {
		if strings.Contains(h.Content, contentSub) {
			hh, err := st.HashByID(h.ID)
			if err != nil {
				t.Fatalf("HashByID: %v", err)
			}
			return hh[:8]
		}
	}
	t.Fatalf("库中找不到含 %q 的记忆", contentSub)
	return ""
}

func TestCmdEvalRecallWithGoldScoresPerfectMatch(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "记忆库不能放进 iCloud 同步目录，会损坏 SQLite"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	gold := writeGold(t, t.TempDir(), []map[string]any{
		{"query": "iCloud 同步目录能不能放数据库", "gold": []string{hashPrefixOf(t, db, "iCloud")}},
	})
	out, err := captureStdout(t, func() error {
		return cmdEvalRecall([]string{"--db", db, "--gold", gold, "--k", "5"})
	})
	if err != nil {
		t.Fatalf("cmdEvalRecall: %v", err)
	}
	if !strings.Contains(out, "Recall@5: 100.0%") {
		t.Errorf("应命中并报 100%%，got:\n%s", out)
	}
}

// gold 写错必须报出来 —— 否则「gold 不在库中」会表现成「Recall 低」，误导结论。
func TestCmdEvalRecallReportsInvalidGold(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "一条真实存在的记忆内容"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	gold := writeGold(t, t.TempDir(), []map[string]any{
		{"query": "随便问", "gold": []string{"deadbeef"}},
	})
	out, err := captureStdout(t, func() error {
		return cmdEvalRecall([]string{"--db", db, "--gold", gold})
	})
	if err != nil {
		t.Fatalf("cmdEvalRecall: %v", err)
	}
	if !strings.Contains(out, "已剔除 1 条无效样本") || !strings.Contains(out, "deadbeef") {
		t.Errorf("应显式上报无效样本，got:\n%s", out)
	}
	if !strings.Contains(out, "有效样本: 0") {
		t.Errorf("分母应为 0（不可静默计入），got:\n%s", out)
	}
}

// --auto 的输出必须自带「这是下限、不代表真实质量」的声明，防止数字被误引用。
func TestCmdEvalRecallAutoIsLabelledAsLowerBound(t *testing.T) {
	db := testDB(t)
	for _, c := range []string{"记忆库数据固定放 ~/.v2mem 目录", "活库不能放进 iCloud 同步目录"} {
		if err := cmdAdd([]string{"--db", db, c}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}
	out, err := captureStdout(t, func() error {
		return cmdEvalRecall([]string{"--db", db, "--auto", "2"})
	})
	if err != nil {
		t.Fatalf("cmdEvalRecall --auto: %v", err)
	}
	for _, want := range []string{"下限", "不代表真实检索质量", "全文复述", "截断复述"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出应含 %q（防止数字被误引用），got:\n%s", want, out)
		}
	}
}

func TestCmdEvalRecallRejectsBothModesAndNeither(t *testing.T) {
	db := testDB(t)
	if err := cmdEvalRecall([]string{"--db", db}); err == nil {
		t.Error("既无 --gold 也无 --auto 应报错")
	}
	gold := writeGold(t, t.TempDir(), []map[string]any{{"query": "q", "gold": []string{"x"}}})
	if err := cmdEvalRecall([]string{"--db", db, "--gold", gold, "--auto", "3"}); err == nil {
		t.Error("--gold 与 --auto 同时给出应报错")
	}
}

// ---------- 记录评测 ----------

func TestCmdEvalWriteWithoutGoldRefusesToClaimAccuracy(t *testing.T) {
	src := writeFile(t, t.TempDir(), "session.md",
		"- 记忆库数据固定放 ~/.v2mem，代码与数据分离\n- 活库不能放进 iCloud 同步目录\n")
	out, err := captureStdout(t, func() error {
		return cmdEvalWrite([]string{"--session", src})
	})
	if err != nil {
		t.Fatalf("cmdEvalWrite: %v", err)
	}
	if !strings.Contains(out, "不能当作准确率") {
		t.Errorf("无 gold 时必须声明不是准确率，got:\n%s", out)
	}
	if !strings.Contains(out, "候选 2 条") {
		t.Errorf("应报告候选数，got:\n%s", out)
	}
}

func TestCmdEvalWriteWithGoldCountsMissed(t *testing.T) {
	src := writeFile(t, t.TempDir(), "session.md",
		"- 记忆库数据固定放 ~/.v2mem，代码与数据分离\n")
	goldFile := filepath.Join(t.TempDir(), "write-gold.txt")
	// 第一条能被抽到；第二条根本没写进会话 → 应判为漏记
	if err := os.WriteFile(goldFile, []byte("记忆库数据固定放\n这条事实在会话里根本没写\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	out, err := captureStdout(t, func() error {
		return cmdEvalWrite([]string{"--session", src, "--gold", goldFile})
	})
	if err != nil {
		t.Fatalf("cmdEvalWrite: %v", err)
	}
	if !strings.Contains(out, "漏记      : 1 条") {
		t.Errorf("应判出 1 条漏记，got:\n%s", out)
	}
	if !strings.Contains(out, "这条事实在会话里根本没写") {
		t.Errorf("漏记清单应列出具体条目，got:\n%s", out)
	}
}

func TestCmdEvalWriteCountsSuspectedFragments(t *testing.T) {
	// 以逗号结尾的候选 = 疑似被硬换行切断
	src := writeFile(t, t.TempDir(), "session.md",
		"- 一条正常的完整事实内容在这里\n- 一条被硬换行切断的事实前半句，\n")
	out, err := captureStdout(t, func() error {
		return cmdEvalWrite([]string{"--session", src})
	})
	if err != nil {
		t.Fatalf("cmdEvalWrite: %v", err)
	}
	if !strings.Contains(out, "疑似碎片  : 1 条") {
		t.Errorf("应数出 1 条疑似碎片，got:\n%s", out)
	}
}

// 选项写在查询之后是个静默陷阱：Go 的 flag 解析在首个位置参数处停止，
// 于是 `mem search "查询" --json` 会把 --json 搜进去 —— 不报错、也无结果。
func TestCmdSearchRejectsFlagAfterQuery(t *testing.T) {
	db := testDB(t)
	err := cmdSearch([]string{"--db", db, "记忆库", "--json"})
	if err == nil {
		t.Fatal("选项写在查询之后应报错，而不是静默搜进去")
	}
	if !strings.Contains(err.Error(), "--json") {
		t.Errorf("错误应指出是哪个选项，got: %v", err)
	}
	// 正文里出现 --xxx 不应被误判（只认已知选项名）
	if err := cmdSearch([]string{"--db", db, "写入时用 --global 表示全局记忆"}); err != nil {
		t.Errorf("正文含 --global 是合法查询，不该被拦：%v", err)
	}
}

// 🔴 关键缺口：`mem search` 必须也写审计。
//
// 为什么这是关键：WorkBuddy / TraeWork 是**指令级**接入（无原生钩子），
// 模型只能靠手工调 `mem search` 来用记忆库。如果只有 `mem hook` 写审计，
// 那么在这两个工具里用再久，审计日志都不增长 —— 回来也无样本可标注。
func TestCmdSearchWritesAuditRecord(t *testing.T) {
	db := testDB(t)
	ap := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := cmdAdd([]string{"--db", db, "记忆库不能放进 iCloud 同步目录"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--audit-file", ap, "iCloud 同步目录"})
	}); err != nil {
		t.Fatalf("cmdSearch: %v", err)
	}

	rs, err := audit.Read(ap, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("应写入 1 条审计，got %d", len(rs))
	}
	r := rs[0]
	if r.Event != "manual-search" {
		t.Errorf("事件应标为 manual-search 以便与钩子区分，got %q", r.Event)
	}
	if r.Query == "" || len(r.Hashes) == 0 {
		t.Errorf("应记录 query 与命中的记忆，got %+v", r)
	}
}

func TestCmdSearchAuditCanBeDisabled(t *testing.T) {
	db := testDB(t)
	ap := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := cmdAdd([]string{"--db", db, "一条用于测试的记忆内容"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--audit-file", ap, "--no-audit", "测试记忆"})
	}); err != nil {
		t.Fatalf("cmdSearch: %v", err)
	}
	if _, err := os.Stat(ap); !os.IsNotExist(err) {
		t.Error("--no-audit 不应写审计")
	}
}

// 审计失败不得影响检索结果 —— 它只是观测，不是功能。
func TestCmdSearchStillReturnsHitsWhenAuditFails(t *testing.T) {
	db := testDB(t)
	if err := cmdAdd([]string{"--db", db, "审计失败也要能搜到的记忆内容"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(bad, []byte("x"), 0o444); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	out, err := captureStdout(t, func() error {
		return cmdSearch([]string{"--db", db, "--audit-file", filepath.Join(bad, "a.jsonl"), "审计失败"})
	})
	if err != nil {
		t.Errorf("审计失败不应让检索报错: %v", err)
	}
	if !strings.Contains(out, "审计失败也要能搜到") {
		t.Errorf("审计失败时仍必须返回命中，got:\n%s", out)
	}
}

// ---------- 护栏：生成给模型的指引里，命令必须可直接执行 ----------

// 逐行抽取文本里的 mem 命令。同时容忍反引号包裹（指令块）与裸行（用法提示），
// 并剥掉行尾的 `# 注释`。
func extractMemCommands(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		l = strings.TrimPrefix(l, "- ")
		l = strings.TrimPrefix(l, "`")
		l = strings.TrimSuffix(l, "`")
		l = strings.TrimPrefix(l, "\"")
		l = strings.TrimSuffix(l, "\"")
		if !strings.HasPrefix(l, "mem ") {
			continue
		}
		if i := strings.Index(l, "  #"); i > 0 {
			l = l[:i]
		}
		out = append(out, strings.TrimSpace(l))
	}
	return out
}

// valueFlags 是「后面跟一个值」的选项。校验时必须跳过它们的值，
// 否则会把 `--scope current` 里的 current 误当成位置参数（这是第一版的错）。
var valueFlags = map[string]bool{
	"--db": true, "--limit": true, "--project": true, "--kind": true, "--tag": true,
	"--scope": true, "--salience": true, "--ttl": true, "--tool": true, "--device": true,
	"--harness": true, "--event": true, "--max-chars": true, "--min-runes": true,
	"--k": true, "--auto": true, "--gold": true, "--session": true, "--file": true,
	"--threshold": true, "--max-idle": true, "--min-salience": true,
	"--audit-file": true, "--audit": true, "--tail": true,
}

// flagsAfterPositional 返回第一个「写在位置参数之后」的选项（无则空串）。
//
// 注意要跳过 `mem <子命令>` 这两段 —— 子命令名本身不是位置参数（第一版就是
// 把 `search` 当成了位置参数，于是所有命令都被误判为违规）。eval 还多一层子命令。
func flagsAfterPositional(cmd string) string {
	fields := strings.Fields(cmd)
	skip := 2
	if len(fields) > 1 && fields[1] == "eval" {
		skip = 3
	}
	if len(fields) <= skip {
		return ""
	}
	expectValue := false
	seenPositional := false
	for _, tok := range fields[skip:] {
		if expectValue {
			expectValue = false
			continue
		}
		if strings.HasPrefix(tok, "-") {
			if seenPositional {
				return tok
			}
			if valueFlags[tok] {
				expectValue = true
			}
			continue
		}
		seenPositional = true
	}
	return ""
}

// 🔴 本项目生成的所有「给模型看的命令」都必须真的能跑。
//
// 起因：hintBlock / instructionBlock 里曾写成 `mem search "<关键词>" --scope current --json`
// 与 `mem ingest <文件> --project <工程>` —— Go 的 flag 在**首个位置参数处停止解析**，
// 于是选项被当成位置参数：前者被守卫拦下报错，后者直接 `open --project: no such file`。
// 这两段文本会被注入到每一次会话里，模型照着执行就会失败 —— 属于「指引本身是坏的」。
//
// 这个用例把「选项必须在位置参数之前」固化成不变量。
func TestGuidanceCommandsAreExecutable(t *testing.T) {
	blocks := map[string]string{
		"hook 注入的用法提示":      hintBlock(),
		"指令级接入块(workbuddy)": instructionBlock("workbuddy"),
		"指令级接入块(traework)":  instructionBlock("traework"),
	}
	total := 0
	for name, block := range blocks {
		cmds := extractMemCommands(block)
		if len(cmds) == 0 {
			t.Errorf("%s 里没抽到任何 mem 命令 —— 抽取器失效，这个用例就成了空转", name)
			continue
		}
		for _, c := range cmds {
			total++
			if bad := flagsAfterPositional(c); bad != "" {
				t.Errorf("%s 的命令把选项写在了位置参数之后，实际执行会失败：\n  %s\n  违规选项: %s",
					name, c, bad)
			}
		}
	}
	if total < 6 {
		t.Errorf("至少应校验 6 条命令，实际 %d 条（抽取器可能漏了）", total)
	}
}

// 指引里出现的每个子命令名都必须是真实的子命令。
// 防的是「文档写了、命令不存在」这类漂移。
func TestGuidanceNamesRealSubcommands(t *testing.T) {
	known := map[string]bool{
		"add": true, "search": true, "touch": true, "forget": true, "gc": true,
		"consolidate": true, "export": true, "import": true, "hook": true,
		"harness": true, "init": true, "ingest": true, "budget": true,
		"audit": true, "eval": true, "stats": true, "help": true,
	}
	for name, block := range map[string]string{
		"hook 用法提示": hintBlock(),
		"指令块":       instructionBlock("x"),
	} {
		for _, c := range extractMemCommands(block) {
			fields := strings.Fields(c)
			if len(fields) < 2 {
				continue
			}
			sub := fields[1]
			if strings.HasPrefix(sub, "-") {
				continue
			}
			if !known[sub] {
				t.Errorf("%s 引用了不存在的子命令 %q：%s", name, sub, c)
			}
		}
	}
}
