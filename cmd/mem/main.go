// Command mem 是 v2mem 的命令行入口（项目名 v2mem，命令 mem）。
//
// 设计要点：
//   - 只用标准库 flag，不加任何依赖，保证 go build 秒级、产物单文件
//   - 默认输出紧凑纯文本（省 token），--json 供 LLM 解析
//   - 工程名默认从当前 git 仓库根推断，让「项目级记忆」自然生效
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/recall"
	"github.com/v2camp/v2mem/internal/scrub"
	"github.com/v2camp/v2mem/internal/store"
)

const usageText = `v2mem (mem) — 个人 Agent 记忆系统

用法:
  mem add    [选项] "<原子事实>"    写入一条记忆（相同内容自动覆盖）
  mem search [选项] "<查询>"        全文检索（FTS5 / bm25）
  mem asset  <path> [选项]          按源文件反查关联记忆（provenance.file）
  mem ls     [选项]                列出库里的记忆（无需查询词；可按工程/类型/标记过滤）
  mem touch  <id|前缀>             记录一次采纳（adopted 兼容别名）
  mem show   <id|前缀>             深查：正文 + 溯源 + 最近读侧轨迹
  mem feedback <trace> <id|前缀> --adopted|--negative [--reason "..."]  给一次供给落反馈
  mem forget <id|前缀>             删除一条记忆
  mem notes  [选项]                先输出类码表，再输出硬规则小字条（幂等）
  mem lexicon init --project <X> [--terms "词A,词B"]   从该工程记忆提取同义词写入 synonyms.txt（--dry-run 预览；--auto 实验性 bigram 提取）
  mem gc     [选项]                回收：TTL 到期 + 久未命中且低重要性
  mem consolidate [选项]           相似知识归并（近重复聚簇，留 1 条）
  mem maintain [选项]               维护门面：恢复过期租约后最老优先连续追账（--catch-up / --force / --dry-run）
  mem export [路径.jsonl]          导出为 JSONL（省略路径则写标准输出）
  mem import <路径.jsonl>          按 (content_hash, project) 归并进本地库
  mem import-transcript [选项]     从 Claude 会话 JSONL 回填观测（默认不写 memories）
  mem distill [选项]                从证据 Bundle 抽出候选（--proposal 或 --bundle；不写 memories）
                                    --quarantined/--requeue/--gaps 为运维出口，需单独使用
  mem judge [选项]                  准入判定：只记 verdict，不改状态（影子期）
  mem accept <id|前缀>             采纳候选，写入 memories（source=distill-accept）
  mem reject <id|前缀>             拒绝候选
  mem review ls [选项]             复核队列（只读四态）：pending 待判定 / summary 未审核摘要 / quarantined 隔离 / failed 失败
  mem sync   <远端url> [分支]       跨设备 git 同步：pull → 三方合并 → push（冲突可 --abort 复原）
  mem hook   [选项]                 harness 钩子入口（读 stdin JSON，输出注入内容）
  mem harness [--json]              列出各工具的接入方式与检测结果
  mem setup  [选项]                配置判定器与蒸馏的模型来源：探测本机 CLI 并写入 ~/.v2mem/model.json（--status 看现状，--undo 回退）
  mem init   --harness <名字>       把钩子写入该工具的配置（幂等、合并、带备份）
  mem uninstall --harness <名字>    反向操作：移除该工具里的 v2mem 钩子（--all 全部）
  mem ingest <文件.md>              把既有 md 文件里的条目机械化搬进记忆库
  mem budget --file <文件>          检查 Level 1 文件是否超出注入预算
  mem audit  [--stats]             审计日志：钩子激活次数、空注入率、可评测样本数
  mem eval   recall|write|activity|flow  评测：检索准不准（recall）／记录准不准（write）／用量视图（activity）／逐跳验收（flow）
  mem mcp                           MCP server（stdio）：search/add/ls/touch/recall_context + review_ls/review_withdraw/maintain_wake
  mem stats  [选项]                 库概览
  mem help

通用选项:
  --db <path>   数据库路径（默认 ~/.v2mem/mem.db）
  --json        JSON 输出（供 LLM 解析）

add 选项:
  --kind <k>       preference|decision|pitfall|task|fact（默认 fact）
  --project <p>    工程标记（默认取当前 git 仓库名）
  --global         写入全局记忆（工程标记留空，对所有工程生效）
  --tag <k=v>      标记，可重复，如 --tag machine=mini
  --tool <t>       来源工具（默认 cli）
  --device <d>     来源设备（默认 hostname）
  --source <s>     来源标记: human|llm|ingest|harness-summary|bench|test（默认 human；bench/test 默认不计入用量视图）
  --channel <c>    写时通道: user|agent|tool|import（默认按 source：human→user，llm→agent，ingest→import）
  --no-scrub       关闭密钥打码（仅本地调试）
  --salience <f>   重要性 0..1（默认 0.5）
  --ttl <dur>      硬过期，如 720h（默认不过期）

search 选项:
  --limit <n>      返回条数（默认 10）
  --project <p>    限定工程（默认不限）
  --kind <k>       限定类型（默认不限）
  --tag <k=v>      限定标记，可重复，AND 语义（默认不限）
  --scope <s>      current=当前工程+全局；global=只要全局；all=跨工程（默认）
  --layer <l>      memory=已定记忆（默认）；observation=近期痕迹；both=两层交错。别名 recent、all
  --no-fuzzy       关闭模糊检索（MinHash 签名 + RRF 融合，默认开启；只作用于已定记忆）
  --no-scrub       审计 query 不打码（检索仍用原文；仅本地调试）

gc 选项:
  --max-idle <dur>      久未命中阈值（默认 720h，即 30 天）
  --min-salience <f>    低于此重要性才淘汰（默认 0.2）
  --no-backup           危险：跳过 gc 前的库备份，并连同归档文件级备份一起跳过（逃生口）
  --backup-dir <dir>    备份根（默认 <库目录>/backup）；归档备份落在其 evidence-gc-<时间戳>/
  --obs                 只回收 observations（不删 memories）
  --max-age <dur>       与 --obs 合用：观测保留时长（默认 720h）
  --candidates          只回收候选：pending 超 14 天标过期，accepted 超 90 天删除（不删 memories）
  --evidence            回收无观测引用的证据归档段（先移入备份根，再删账本；可与 --obs 合用，先观测后归档）
  --evidence-grace <dur> 与 --evidence 合用：写入保护期（默认 1h，刚写入的段不回收）
  --audit               回收审计账本：超期的 distill_runs / read_traces（不删 memories）
  --audit-age <dur>     与 --audit 合用：账本保留时长（默认 720h）
  --retention           终态保留回收：rejected 90 天候选 / 完成 Bundle 30 天 / 孤立决策账 / 文件轮转（默认关）
  --dry-run             与 --retention 合用：只预览计数、零写入（默认关）

maintain 选项:
  --catch-up            故障恢复模式：与默认行为等价（保留给 Harness / SOP 明示）
  --stage <s>           阶段：proposal|adjudicate|consolidate|gc|all（默认 proposal；逗号可多选，顺序固定）
  --model-auto          阶段含 proposal 时在之后追加自动裁决（默认关；adjudicate 需同时给）
  --adjudicate-scope <s> 裁决范围：this-run（只本轮 Bundle，默认）| all（全部 pending）
  --max-bundles <n>     本次最多处理几个 Bundle（默认 20）
  --force               清零退避中 fail/partial/backlog 的 not_before，本轮立即重试（不改 attempt）
  --dry-run             只读预演：不认领、不落候选、不写库（与 --force 互斥）

consolidate 选项:
  --threshold <f>       相似度阈值 0..1（默认 0.7）
                        准确性由护栏保证（数字/否定/长度/短文本），阈值只影响召回
  --no-backup           跳过归并前的库备份
  --backup-dir <dir>    备份目录（默认 <库目录>/backup）

show 选项:
  --traces <n>     附带最近多少条读侧轨迹（默认 5）

feedback 选项:
  --adopted        采纳（唯一会更新 adopted_count / last_adopted_at 的反馈）
  --negative       否定这次供给（只改轨迹，不动记忆）
  --reason <s>     反馈原因（可选，便于事后标注）

hook 选项:
  --event <e>           session-start|prompt-submit|pre-compact|post-tool-use|stop|session-end（默认从 stdin 推断）
  --harness <h>         指定工具，项目目录按其注入的环境变量取（默认依次尝试）
  --limit <n>           注入记忆条数上限（默认 3）
  --max-chars <n>       注入字符预算（默认 1200）

示例:
  mem add --kind decision "记忆库数据必须放在 ~/.v2mem，不放代码目录"
  mem search "记忆库 数据 路径"
  mem stats --json

兼容保留（下一轮随 P4/灰度删除）:
  accept / reject / judge / distill / gc / consolidate 仍可用，但只作人工运维与灾备兜底；
  常规维护主入口是 mem maintain --stage=proposal|adjudicate|consolidate|gc|all。
`

// knownSubcommands 是子命令的**唯一清单**。
//
// 单一出处：usage 文本、指引护栏、以及任何需要枚举子命令的地方都从这里取。
// 起因：新增 `mem report` 时漏了同步测试里的清单，护栏立刻报警 ——
// 清单有两个出处就一定会漂移，所以把它抽成一处。
var knownSubcommands = []string{
	"add", "search", "asset", "ls", "stats", "touch", "show", "feedback", "forget", "notes", "lexicon",
	"gc", "consolidate", "maintain", "export", "import", "import-transcript", "sync", "hook", "harness",
	"init", "uninstall", "ingest", "budget", "audit", "eval", "mcp", "help",
	"distill", "accept", "reject", "judge", "setup",
	"review",
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 是 main 的可测形态：吃掉 os.Args[1:]，返回进程退出码。
// 0 成功（含 --help）；2 用法错误（未知子命令、未定义或写错的 flag）；1 其余业务错误。
func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}
	err := dispatch(args)
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var usage usageError
	if errors.As(err, &usage) {
		fmt.Fprintf(os.Stderr, "%s\n\n%s", usage.msg, usageText)
		return 2
	}
	// flag 包的解析错误（未定义 flag / 坏语法 / 缺值 / 值非法）与未知子命令同级，
	// 都是「调用方式不对」：退出码 2。flag 包自己已把错误与用法打到 stderr。
	if isFlagUsageError(err) {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 2
	}
	fmt.Fprintln(os.Stderr, "错误:", err)
	return 1
}

// usageError 是用法级错误的哨兵：退出码 2，并附带完整 usage。
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// isFlagUsageError 按文案前缀识别 flag 包的解析错误。ContinueOnError 下
// flag/flag.go 产出的这些字符串是稳定的，比给每个子命令包一层错误类型更省。
func isFlagUsageError(err error) bool {
	msg := err.Error()
	for _, p := range []string{
		"flag provided but not defined",
		"bad flag syntax",
		"flag needs an argument",
		"invalid value",
	} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

func dispatch(args []string) error {
	var err error
	switch args[0] {
	case "add":
		err = cmdAdd(args[1:])
	case "search":
		err = cmdSearch(args[1:])
	case "asset":
		err = cmdAsset(args[1:])
	case "ls":
		err = cmdLs(args[1:])
	case "stats":
		err = cmdStats(args[1:])
	case "touch":
		err = cmdTouch(args[1:])
	case "show":
		err = cmdShow(args[1:])
	case "feedback":
		err = cmdFeedback(args[1:])
	case "forget":
		err = cmdForget(args[1:])
	case "notes":
		err = cmdNotes(args[1:])
	case "lexicon":
		err = cmdLexicon(args[1:])
	case "gc":
		err = cmdGC(args[1:])
	case "consolidate":
		err = cmdConsolidate(args[1:])
	case "maintain":
		err = cmdMaintain(args[1:])
	case "export":
		err = cmdExport(args[1:])
	case "import":
		err = cmdImport(args[1:])
	case "import-transcript":
		err = cmdImportTranscript(args[1:])
	case "distill":
		err = cmdDistill(args[1:])
	case "judge":
		err = cmdJudge(args[1:])
	case "accept":
		err = cmdAccept(args[1:])
	case "reject":
		err = cmdReject(args[1:])
	case "batch":
		// 已下线：批量准出的归档与回滚由 review rollback 承接。
		return usageError{msg: "命令 batch 已下线：回滚用 mem review rollback；复核入口见 mem review ls --state all"}
	case "migrate":
		// 已下线：summary / summary_idx 外置迁移已随旧库迁移完成。
		return usageError{msg: "命令 migrate 已下线：summary/summary_idx 外置迁移已完成，无需再执行"}
	case "candidates":
		// 已下线：候选查看并入 review 四态视图。
		return usageError{msg: "命令 candidates 已下线：用 mem review ls --state pending|all"}
	case "review":
		err = cmdReview(args[1:])
	case "sync":
		err = cmdSync(args[1:])
	case "hook":
		err = cmdHook(args[1:])
	case "harness":
		err = cmdHarness(args[1:])
	case "setup":
		err = cmdSetup(args[1:])
	case "init":
		err = cmdInit(args[1:])
	case "uninstall":
		err = cmdUninstall(args[1:])
	case "ingest":
		err = cmdIngest(args[1:])
	case "budget":
		err = cmdBudget(args[1:])
	case "audit":
		err = cmdAudit(args[1:])
	case "eval":
		err = cmdEval(args[1:])
	case "mcp":
		err = cmdMCP(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return nil
	default:
		return usageError{msg: "未知子命令: " + args[0]}
	}
	return err
}

type common struct {
	db   string
	json bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.db, "db", "", "数据库路径（默认 ~/.v2mem/mem.db）")
	fs.BoolVar(&c.json, "json", false, "JSON 输出（供 LLM 解析）")
}

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// misplacedFlag 检查查询文本里是否混入了本命令的选项。
// 只认已知选项名，避免把正文里出现的 "--xxx"（如一条讲 CLI 用法的记忆）误判。
func misplacedFlag(query string) string {
	known := map[string]bool{
		"--json": true, "--db": true, "--limit": true,
		"--project": true, "--kind": true, "--tag": true, "--scope": true, "--layer": true,
		"--no-fuzzy": true, "--no-audit": true, "--audit-file": true,
	}
	for _, tok := range strings.Fields(query) {
		if known[tok] {
			return tok
		}
	}
	return ""
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// detectProject 从当前目录向上找 .git，用仓库根目录名作工程标记。
func detectProject() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return detectProjectAt(wd)
}

// detectProjectAt 是 detectProject 的显式目录版本。
// 钩子场景必须用它：harness 通过环境变量或 stdin 给出的项目目录
// 未必等于进程的工作目录。
func detectProjectAt(wd string) string {
	dir := wd
	for i := 0; i < 32; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Base(wd)
}

func parseTags(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("标记格式应为 k=v，收到: %q", kv)
		}
		out[k] = v
	}
	return out, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func cmdAdd(args []string) error {
	var c common
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	c.register(fs)
	kind := fs.String("kind", "fact", "记忆类型")
	project := fs.String("project", "", "工程标记")
	global := fs.Bool("global", false, "全局记忆：project 留空，对所有工程生效")
	tool := fs.String("tool", "cli", "来源工具")
	device := fs.String("device", "", "来源设备")
	source := fs.String("source", "human", "来源标记: human|llm|ingest|harness-summary|bench|test")
	channel := fs.String("channel", "", "写时通道: user|agent|tool|import（空则按 source 取默认）")
	noScrub := fs.Bool("no-scrub", false, "关闭密钥打码（仅本地调试）")
	salience := fs.Float64("salience", 0.5, "重要性 0..1")
	ttl := fs.Duration("ttl", 0, "硬过期时长")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl）")
	noAudit := fs.Bool("no-audit", false, "不写审计日志")
	var tags stringSlice
	fs.Var(&tags, "tag", "标记 k=v，可重复")
	if err := fs.Parse(args); err != nil {
		return err
	}

	content := scrub.Apply(strings.TrimSpace(strings.Join(fs.Args(), " ")), *noScrub)
	if content == "" {
		return errors.New("缺少内容，用法: mem add \"<原子事实>\"")
	}
	if *global && *project != "" {
		return errors.New("--global 与 --project 不可同时使用（全局记忆的工程标记必须为空）")
	}
	if *global {
		*project = ""
	} else if *project == "" {
		// 未显式指定工程时按当前 git 仓库推断，让「项目级记忆」自然生效
		*project = detectProject()
	}
	tagMap, err := parseTags(tags)
	if err != nil {
		return err
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := st.Add(store.AddInput{
		Content:  content,
		Kind:     *kind,
		Project:  *project,
		Tool:     *tool,
		Device:   *device,
		Source:   *source,
		Channel:  *channel,
		Tags:     tagMap,
		Salience: *salience,
		TTL:      *ttl,
	})
	if err != nil {
		return err
	}

	// 审计：**写侧也留痕**。
	//
	// 此前只记读侧，导致「模型到底记了什么」完全不可观测 —— 而
	// 「用户会话 → mem 记录 → 准确记录」这条评测方向正需要它。
	// 写失败静默：审计只是观测，不是功能。
	if !*noAudit {
		mode := "create"
		if !res.Created {
			mode = "overwrite"
		}
		_ = audit.Append(*auditFile, audit.Record{
			TS: time.Now().Unix(), Event: "manual-add", Project: *project,
			Hashes: []string{res.Hash}, Kinds: []string{*kind}, Mode: mode,
			Source: *source,
		})
	}

	if c.json {
		return printJSON(res)
	}
	if res.Created {
		fmt.Printf("已写入 %s [%s/%s]\n", shortID(res.ID), *kind, *project)
	} else {
		fmt.Printf("已覆盖 %s（相同知识，[%s/%s]）\n", shortID(res.ID), *kind, *project)
	}
	return nil
}

func cmdSearch(args []string) error {
	var c common
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	c.register(fs)
	limit := fs.Int("limit", 10, "返回条数")
	project := fs.String("project", "", "限定工程")
	kind := fs.String("kind", "", "限定类型")
	scope := fs.String("scope", "", "作用域: current=当前工程+全局，global=只要全局，all=跨工程（默认）")
	layer := fs.String("layer", "", "层: memory=已定记忆（默认），observation=近期痕迹，both=两层")
	noFuzzy := fs.Bool("no-fuzzy", false, "关闭模糊检索（MinHash + RRF 融合）")
	explain := fs.Bool("explain", false, "打印同义词展开诊断后退出")
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl）")
	noAudit := fs.Bool("no-audit", false, "不写审计日志")
	noScrub := fs.Bool("no-scrub", false, "审计 query 不打码（检索仍用原文；仅本地调试）")
	var tags stringSlice
	fs.Var(&tags, "tag", "标记 k=v，可重复")
	if err := fs.Parse(args); err != nil {
		return err
	}

	q := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if q == "" {
		return errors.New("缺少查询，用法: mem search \"<query>\"")
	}
	// Go 的 flag 解析在首个位置参数处停止，因此 `mem search "查询" --json` 会把
	// --json 当成查询的一部分 —— 既不报错也搜不到东西，是个静默陷阱（实测踩过）。
	if bad := misplacedFlag(q); bad != "" {
		return fmt.Errorf("查询里出现了选项 %q：选项必须写在查询之前（mem search --json \"<查询>\"）", bad)
	}
	// --scope all 与 --project 语义矛盾：all 就是「忽略工程」。
	// 此前只在 store 的注释里声明要拦，实际没拦 —— 结果是静默忽略工程过滤，
	// 用户以为过滤生效了。注释比代码超前，这里补上实现。
	if *scope == "all" && strings.TrimSpace(*project) != "" {
		return errors.New("--scope all 与 --project 矛盾：all 表示跨工程、忽略工程过滤；要按工程过滤请去掉 --scope all")
	}
	switch *scope {
	case "", "all", "current", "global":
	default:
		return fmt.Errorf("未知 --scope: %q（可选 current|global|all）", *scope)
	}
	if _, err := store.CanonicalLayer(*layer); err != nil {
		return err
	}
	tagMap, err := parseTags(tags)
	if err != nil {
		return err
	}
	proj := *project
	if *scope == "current" && proj == "" {
		proj = detectProject()
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	if *explain {
		expanded := st.ExpandQuery(q, proj)
		if expanded == q {
			fmt.Println("同义词展开: （无命中词库，原样检索）")
		} else {
			fmt.Printf("同义词展开: %q  →  %q\n", q, expanded)
		}
		return nil
	}

	started := time.Now()
	hits, err := st.Search(store.SearchQuery{
		Query:   q,
		Project: proj,
		Kind:    *kind,
		Tags:    tagMap,
		Scope:   *scope,
		NoFuzzy: *noFuzzy,
		Limit:   *limit,
		Layer:   *layer,
	})
	if err != nil {
		return err
	}
	if err := hydrateHits(st.Path(), hits); err != nil {
		return err
	}

	// 审计：手工检索也要留样本。
	//
	// 🔴 这条路径对 WorkBuddy / TraeWork 是**唯一**的样本来源 —— 它们没有原生钩子，
	// 模型只能靠手工调 `mem search`。若只有 mem hook 写审计，那两个工具里用再久
	// 也不会有可标注的样本。写失败静默：审计只是观测，不是功能。
	if !*noAudit {
		rec := audit.Record{
			TS: started.Unix(), Event: "manual-search", Project: proj,
			Query: scrub.Apply(q, *noScrub), Empty: len(hits) == 0, MS: time.Since(started).Milliseconds(),
		}
		for _, h := range hits {
			rec.Hashes = append(rec.Hashes, h.ID)
			rec.Kinds = append(rec.Kinds, h.Kind)
		}
		_ = audit.Append(*auditFile, rec)
	}

	if c.json {
		if hits == nil {
			hits = []store.Hit{}
		}
		return printJSON(hits)
	}
	if len(hits) == 0 {
		fmt.Println("（无结果）")
		return nil
	}
	for _, h := range hits {
		// 单行输出，省 token；格式: <id8>  <内容>  [<kind>/<project>]
		proj := h.Project
		if proj == "" {
			proj = "global"
		}
		kind := h.Kind
		if h.Layer == "observation" {
			kind = "observation:" + h.Kind
		} else if h.Layer == "memory" {
			kind = "memory:" + h.Kind
		}
		fmt.Printf("%s  %s  [%s/%s]\n", shortID(h.ID), oneLine(h.Content), kind, proj)
	}
	return nil
}

// cmdTouch 是 adopted 的兼容别名：刷新 last_seen_at/access_count，
// 并累加 adopted_count / last_adopted_at。读侧检索、列举、注入都不会自动调用它。
func cmdTouch(args []string) error {
	var c common
	fs := flag.NewFlagSet("touch", flag.ContinueOnError)
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		return errors.New("缺少 ID，用法: mem touch <id|前缀>")
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	full, adopted, err := st.Adopt(id)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(map[string]any{"id": full, "touched": "true", "adopted_count": adopted})
	}
	fmt.Printf("已记录采纳 %s（累计 %d 次）\n", shortID(full), adopted)
	return nil
}

// cmdShow 深查一条记忆：正文、溯源与最近的读侧供给轨迹。
func cmdShow(args []string) error {
	var c common
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	c.register(fs)
	traces := fs.Int("traces", 5, "附带最近多少条读侧轨迹")
	flags, pos := reorderFlags(args, map[string]bool{"--db": true, "--traces": true})
	if err := fs.Parse(flags); err != nil {
		return err
	}
	id := strings.TrimSpace(strings.Join(pos, " "))
	if id == "" {
		return errors.New("缺少 ID，用法: mem show <id|前缀>")
	}
	if *traces < 0 {
		return errors.New("--traces 不能为负数")
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	d, err := st.MemoryDetailByID(id)
	if err != nil {
		return err
	}
	prov := recall.ProvenanceFor(st, []store.Hit{{ID: d.ID, Project: d.Project}})[d.ID]
	if prov == "" && d.Provenance != nil {
		prov = d.Provenance.Preview
	}
	ts, err := st.TracesForMemory(d.ID, *traces)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(struct {
			Memory     *store.MemoryDetail `json:"memory"`
			Provenance string              `json:"provenance,omitempty"`
			Traces     []store.MemoryTrace `json:"traces"`
		}{Memory: d, Provenance: prov, Traces: ts})
	}

	fmt.Printf("id          %s\n", d.ID)
	fmt.Printf("kind        %s\n", d.Kind)
	proj := d.Project
	if proj == "" {
		proj = "(global)"
	}
	fmt.Printf("project     %s\n", proj)
	if d.Scope != "" {
		fmt.Printf("scope       %s\n", d.Scope)
	}
	fmt.Printf("salience    %.2f\n", d.Salience)
	fmt.Printf("content_hash %s\n", d.ContentHash)
	fmt.Printf("created     %s\n", formatUnix(d.CreatedAt))
	fmt.Printf("updated     %s\n", formatUnix(d.UpdatedAt))
	fmt.Printf("last_seen   %s\n", formatUnix(d.LastSeenAt))
	if d.ExpiresAt != nil {
		fmt.Printf("expires     %s\n", formatUnix(*d.ExpiresAt))
	}
	if d.SupersededBy != "" {
		fmt.Printf("superseded  %s\n", shortID(d.SupersededBy))
	}
	fmt.Printf("source      %s/%s\n", d.Source, d.Channel)
	if d.Tool != "" {
		fmt.Printf("tool        %s\n", d.Tool)
	}
	fmt.Printf("access      %d\n", d.AccessCount)
	lastAdopted := "—"
	if d.LastAdopted != nil {
		lastAdopted = formatUnix(*d.LastAdopted)
	}
	fmt.Printf("adopted     %d（最近 %s）\n", d.AdoptedCount, lastAdopted)
	if prov != "" {
		fmt.Printf("provenance  %s\n", prov)
	}
	if d.Provenance != nil {
		fmt.Printf("origin      %s", d.Provenance.File)
		if d.Provenance.Line > 0 {
			fmt.Printf(":%d", d.Provenance.Line)
		}
		if d.Provenance.Preview != "" {
			fmt.Printf("  %s", oneLine(d.Provenance.Preview))
		}
		fmt.Println()
	}
	fmt.Printf("content\n  %s\n", oneLine(d.Content))
	if len(ts) == 0 {
		fmt.Println("读侧轨迹     （暂无）")
		return nil
	}
	fmt.Println("读侧轨迹")
	for _, t := range ts {
		fb := t.Feedback
		if fb == "" {
			fb = "—"
		}
		fmt.Printf("  %s  %-14s rank=%d  feedback=%s", shortID(t.TraceID), t.Outcome, t.InjectionRank, fb)
		if t.QueryGate == "skip" {
			fmt.Printf("  gate=skip(%s)", t.GateReason)
		}
		if t.FeedbackReason != "" {
			fmt.Printf("  reason=%s", oneLine(t.FeedbackReason))
		}
		fmt.Println()
	}
	return nil
}

// cmdFeedback 给一条读侧供给里的某条记忆落反馈。
// 只有 adopted 会更新 memories.adopted_count / last_adopted_at；negative 只改轨迹。
func cmdFeedback(args []string) error {
	var c common
	fs := flag.NewFlagSet("feedback", flag.ContinueOnError)
	c.register(fs)
	adopted := fs.Bool("adopted", false, "采纳这条供给")
	negative := fs.Bool("negative", false, "否定这条供给")
	reason := fs.String("reason", "", "反馈原因")
	// Go 的 flag 在首个位置参数处停止解析，而 feedback 的自然写法是
	// `mem feedback <trace> <memory> --adopted --reason ...`。先把选项挑出来，
	// 使选项写在位置参数前后都能用。
	flags, rest := reorderFlags(args, map[string]bool{"--db": true, "--reason": true})
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(rest) < 2 {
		return errors.New("用法: mem feedback <trace> <memory> --adopted|--negative [--reason \"...\"]")
	}
	if *adopted == *negative {
		return errors.New("必须且只能指定 --adopted 或 --negative 之一")
	}
	feedback := "adopted"
	if *negative {
		feedback = "negative"
	}
	traceID := strings.TrimSpace(rest[0])
	memoryID := strings.TrimSpace(rest[1])
	if traceID == "" || memoryID == "" {
		return errors.New("trace 与 memory 都不能为空")
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := st.ApplyReadFeedback(traceID, memoryID, feedback, strings.TrimSpace(*reason))
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(res)
	}
	fmt.Printf("已记录反馈 %s → %s（%s）", shortID(res.TraceID), shortID(res.MemoryID), res.Feedback)
	if res.Reason != "" {
		fmt.Printf("  原因：%s", oneLine(res.Reason))
	}
	fmt.Printf("  累计采纳=%d\n", res.AdoptedCount)
	return nil
}

// reorderFlags 把选项与位置参数分开，允许 `cmd <arg> --flag` 的写法。
// valueFlags 是「后面必须跟一个值」的选项名。
func reorderFlags(args []string, valueFlags map[string]bool) (flags, pos []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name, _, hasValue := strings.Cut(a, "=")
			if valueFlags[name] && !hasValue && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return flags, pos
}

func formatUnix(ts int64) string {
	if ts <= 0 {
		return "—"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

// cmdForget 显式删除一条记忆。
func cmdForget(args []string) error {
	var c common
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		return errors.New("缺少 ID，用法: mem forget <id|前缀>")
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	full, err := st.Forget(id)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(map[string]string{"id": full, "forgotten": "true"})
	}
	fmt.Printf("已删除 %s\n", shortID(full))
	return nil
}

// cmdGC 回收过期与衰减的记忆。破坏性操作：先备份库快照。
func cmdGC(args []string) error {
	var c common
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	c.register(fs)
	maxIdle := fs.Duration("max-idle", 720*time.Hour, "久未命中阈值")
	minSalience := fs.Float64("min-salience", 0.2, "低于此重要性才淘汰")
	noBackup := fs.Bool("no-backup", false, "跳过 gc 前的库备份")
	backupDir := fs.String("backup-dir", "", "备份目录（默认 <库目录>/backup）")
	obsOnly := fs.Bool("obs", false, "只回收 observations，不删除 memories")
	candOnly := fs.Bool("candidates", false, "只回收候选，不删除 memories")
	evOnly := fs.Bool("evidence", false, "回收无观测引用的证据归档（可与 --obs 合用）")
	evGrace := fs.Duration("evidence-grace", time.Hour, "证据归档写入保护期（配合 --evidence）")
	auditOnly := fs.Bool("audit", false, "回收超期的审计账本 distill_runs / read_traces（不删 memories）")
	auditAge := fs.Duration("audit-age", 720*time.Hour, "审计账本保留时长（配合 --audit）")
	maxAge := fs.Duration("max-age", 720*time.Hour, "observations 保留时长（配合 --obs）")
	// S13：终态保留回收默认关；--dry-run 只预览不删（零写）。
	retention := fs.Bool("retention", false, "终态保留回收：rejected 候选 / 完成 Bundle / 决策账 / 文件轮转（默认关）")
	dryRun := fs.Bool("dry-run", false, "只预览不删除（配合 --retention）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *obsOnly && *candOnly {
		return errors.New("--obs 与 --candidates 不能同时使用")
	}
	if *dryRun && !*retention {
		return errors.New("--dry-run 只配合 --retention 使用")
	}
	if *retention && (*obsOnly || *candOnly || *evOnly || *auditOnly) {
		return errors.New("--retention 不与 --obs / --candidates / --evidence / --audit 同用")
	}

	// 备份前置到分支派发**之前**：一次快照覆盖默认分支、四个子分支与合跑。
	// 放在分支之后会让 --obs / --candidates / --evidence / --audit 完全不备份——
	// 它们删的字节同样不可逆，正是 S3 要堵的口子。备份/校验失败直接返回，业务零执行。
	// --retention --dry-run 是唯一例外：它不物理删，故跳过备份以保证「零写」；
	// 其余路径（含 --retention 实删）一律先过这道前置，S9a 故障注入守卫不绕过。
	backupRoot := ""
	if !*dryRun {
		bak, err := backupBeforeMutating(c.db, *noBackup, *backupDir)
		if err != nil {
			return err
		}
		if bak != "" && !c.json {
			fmt.Printf("已备份 → %s\n", bak)
		}
		// 归档段的文件级备份与库快照共用备份根；--no-backup 是逃生口，连归档备份一起跳过。
		if !*noBackup {
			backupRoot = backupDirFor(c.db, *backupDir)
		}
	}

	if *retention {
		return cmdGCRetention(c, *dryRun, backupRoot)
	}

	// --obs / --evidence / --audit 允许合并且顺序固定：先回收观测，被删观测对应的
	// 归档段这一轮才成为孤儿；账本排在最后，因为「会话是否还在写观测」正是它判断
	// 「这条轨迹能不能删」的依据——观测没先清掉，活跃会话的轨迹就不会被回收。
	if *obsOnly || *candOnly || *evOnly || *auditOnly {
		if *obsOnly {
			if err := cmdGCObservations(c, *maxAge); err != nil {
				return err
			}
		}
		if *candOnly {
			if err := cmdGCCandidates(c); err != nil {
				return err
			}
		}
		if *evOnly {
			if err := cmdGCEvidence(c, *evGrace, backupRoot); err != nil {
				return err
			}
		}
		if *auditOnly {
			if err := cmdGCAudit(c, *auditAge); err != nil {
				return err
			}
		}
		// 只有命令成功返回才写水位；任何分支出错都已提前 return，不会打点。
		markMaintainOKPath(c.db)
		return nil
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := st.GC(*maxIdle, *minSalience)
	if err != nil {
		return err
	}
	if c.json {
		if err := printJSON(res); err != nil {
			return err
		}
		markMaintainOK(st)
		return nil
	}
	fmt.Printf("已回收 过期=%d 衰减=%d，保留 %d\n", res.Expired, res.Decayed, res.Kept)
	markMaintainOK(st)
	return nil
}

// cmdGCObservations 只删 observations。memories 不在这条路径上。
func cmdGCObservations(c common, maxAge time.Duration) error {
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.GCObservations(maxAge, 50000)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(res)
	}
	fmt.Printf("已回收观测 %d 条，保留 %d（未改动 memories）\n", res.Deleted, res.Kept)
	return nil
}

// cmdGCEvidence 回收「已经没有任何观测指向」的证据归档：账本行 + 磁盘文件。
//
// 破坏性（删磁盘字节）且不进 --obs 的默认路径：必须显式 --evidence。
// 写入保护期默认 1h，避免把「已落文件、还没写账」的段当成孤儿删掉。
// backupRoot 非空时先做文件级备份（移入 evidence-gc-<时间戳>/ + manifest），
// 全部落位后才删账本；只有 --no-backup 才传空串（逃生口，直接删）。
func cmdGCEvidence(c common, grace time.Duration, backupRoot string) error {
	es, err := openEvidenceSession(c.db)
	if err != nil {
		return err
	}
	defer es.Close()

	res, err := es.engine.GCEvidenceBackup(time.Now().Add(-grace).Unix(), backupRoot)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(res)
	}
	if res.BackupDir != "" {
		fmt.Printf("归档备份 → %s（%d 个文件）\n", res.BackupDir, res.BackedUp)
	}
	fmt.Printf("已回收归档段 %d（%d 条原始事件，释放 %.1f MB），账本外残文件 %d，临时文件 %d，空转证据源 %d\n",
		res.Segments, res.RawEvents, float64(res.Bytes)/1024/1024,
		res.Untracked, res.TempFiles, res.Sources)
	if res.Failed > 0 {
		fmt.Printf("%d 段删文件失败，已留在账本里等下轮重试\n", res.Failed)
	}
	return nil
}

// cmdGCAudit 回收审计账本里「已结束会话」的超期记录。
//
// 与 --obs 一样是只删账本、不碰 memories。两条保留规则写在 store.GCAudit 里：
// 每个会话留一条蒸馏游标，活跃会话的轨迹不动。默认保留 720h，远大于巡检窗口
// （12h），所以不会影响 R3/R12 的窗口统计。
func cmdGCAudit(c common, age time.Duration) error {
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.GCAudit(time.Now().Add(-age).Unix())
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(res)
	}
	fmt.Printf("已回收审计账本：distill_runs %d 条，read_traces %d 条（明细 %d 条）；未改动 memories\n",
		res.DistillRuns, res.ReadTraces, res.ReadTraceItems)
	return nil
}

func cmdGCCandidates(c common) error {
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.GCCandidates(14*24*time.Hour, 90*24*time.Hour)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(res)
	}
	fmt.Printf("候选过期 %d，删除已采纳行 %d、过期行 %d、已拒行 %d（未改动 memories）\n",
		res.Expired, res.Deleted, res.DeletedExpired, res.DeletedRejected)
	return nil
}

// cmdExport 把全部记忆写成 JSONL：每行一条，供跨设备归集。
// 给路径则写文件并把摘要打到标准输出；不给路径则整份 JSONL 走标准输出。
//
// 绝不导出活的 mem.db——写中途的同步会损坏 SQLite 库。
func cmdExport(args []string) error {
	var c common
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := strings.TrimSpace(strings.Join(fs.Args(), " "))

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	recs, err := st.Export()
	if err != nil {
		return err
	}

	var w io.Writer = os.Stdout
	if path != "" {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}

	if path == "" {
		return nil
	}
	if c.json {
		return printJSON(map[string]any{"exported": len(recs), "path": path})
	}
	fmt.Printf("已导出 %d 条 → %s\n", len(recs), path)
	return nil
}

// cmdImport 读取 JSONL 并按 (content_hash, project) 归并进本地库。
func cmdImport(args []string) error {
	var c common
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if path == "" {
		return errors.New("缺少文件路径，用法: mem import <in.jsonl>")
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	var recs []store.ExportRecord
	for {
		var r store.ExportRecord
		if err := dec.Decode(&r); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("解析 JSONL 失败（第 %d 条起）: %w", len(recs)+1, err)
		}
		recs = append(recs, r)
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := st.Import(recs)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(res)
	}
	fmt.Printf("已归并 新增=%d 合并=%d 跳过=%d", res.Inserted, res.Merged, res.Skipped)
	if res.Unresolved > 0 {
		// 取代引用的目标既不在文件里也不在本地：留空而非写悬挂 id，必须让人看见
		fmt.Printf(" 未解析=%d（取代引用的目标不在文件与本地库中）", res.Unresolved)
	}
	fmt.Printf("（读入 %d 行）\n", len(recs))
	return nil
}

// consolidateOptions 是归并核心的输入：CLI 与 maintain 共用。
type consolidateOptions struct {
	Threshold float64
	NoBackup  bool
	BackupDir string
	DryRun    bool
}

// consolidateReport 是一次归并的备份凭证与结果。
//
// Skipped 只出现在 dry-run：store.Consolidate 没有预览模式，全阶段零写时
// 宁可不跑，也不为了「报个数」写库。
type consolidateReport struct {
	Backup  string
	DryRun  bool
	Skipped bool
	Result  *store.ConsolidateResult
}

// runConsolidate 是归并的执行核心（供 cmdConsolidate 与 maintain --stage=consolidate 复用）。
//
// 备份前置与 CLI 原行为一致：先 backupBeforeMutating（备份校验失败直接拒绝执行），
// 再调 store.Consolidate。dry-run 全阶段零写，不做备份、不调归并。
func runConsolidate(st *store.Store, opts consolidateOptions) (*consolidateReport, error) {
	rep := &consolidateReport{
		DryRun: opts.DryRun,
		Result: &store.ConsolidateResult{Pairs: []store.ConsolidatePair{}},
	}
	if opts.DryRun {
		rep.Skipped = true
		return rep, nil
	}
	bak, err := backupBeforeMutating(st.Path(), opts.NoBackup, opts.BackupDir)
	if err != nil {
		return nil, err
	}
	rep.Backup = bak
	res, err := st.Consolidate(opts.Threshold)
	if err != nil {
		return nil, err
	}
	rep.Result = res
	return rep, nil
}

// cmdConsolidate 做相似知识归并：把措辞略有差异的近重复聚成簇，
// 留 1 条（salience / 命中次数 / 更早创建 依次优先），其余置 superseded_by。
//
// 准确性靠 similarity.Judge 的四条护栏（数字、否定、长度、短文本），不单靠阈值。
func cmdConsolidate(args []string) error {
	var c common
	fs := flag.NewFlagSet("consolidate", flag.ContinueOnError)
	c.register(fs)
	threshold := fs.Float64("threshold", store.DefaultConsolidateThreshold, "相似度阈值 0..1")
	noBackup := fs.Bool("no-backup", false, "跳过归并前的库备份")
	backupDir := fs.String("backup-dir", "", "备份目录（默认 <库目录>/backup）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *threshold < 0 || *threshold > 1 {
		return fmt.Errorf("--threshold 应落在 [0,1]，got %v", *threshold)
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	rep, err := runConsolidate(st, consolidateOptions{
		Threshold: *threshold, NoBackup: *noBackup, BackupDir: *backupDir,
	})
	if err != nil {
		return err
	}
	if rep.Backup != "" && !c.json {
		fmt.Printf("已备份 → %s\n", rep.Backup)
	}
	res := rep.Result
	if c.json {
		if err := printJSON(res); err != nil {
			return err
		}
		markMaintainOK(st)
		return nil
	}
	fmt.Printf("已归并 扫描=%d 簇=%d 取代=%d\n", res.Scanned, res.Groups, res.Superseded)
	for _, p := range res.Pairs {
		fmt.Printf("  %s ← %s  (相似度 %.3f)\n",
			shortID(p.Survivor), shortID(p.Superseded), p.Similarity)
	}
	markMaintainOK(st)
	return nil
}

func cmdStats(args []string) error {
	var c common
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	s, err := st.Stats()
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	bl, err := st.Backlog(now)
	if err != nil {
		return err
	}
	spoolRoot := filepath.Join(filepath.Dir(s.Path), "spool")
	spoolFiles, spoolOldest, err := scanSpoolBacklog(spoolRoot)
	if err != nil {
		return err
	}
	var spoolAge *int64
	if !spoolOldest.IsZero() {
		age := now - spoolOldest.Unix()
		if age < 0 {
			age = 0
		}
		spoolAge = &age
	}
	if c.json {
		return printJSON(&statsView{
			Stats: s,
			Backlog: &backlogView{
				Backlog:               bl,
				SpoolBacklogFiles:     spoolFiles,
				OldestSpoolBacklogAge: spoolAge,
			},
		})
	}

	fmt.Printf("库路径   %s\n", s.Path)
	fmt.Printf("大小     %.1f KB\n", float64(s.SizeBytes)/1024)
	fmt.Printf("总条数   %d（全库各数据表之和）\n", s.Total)
	if len(s.ByTable) > 0 {
		keys := make([]string, 0, len(s.ByTable))
		for k := range s.ByTable {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, s.ByTable[k]))
		}
		fmt.Printf("按表     %s\n", strings.Join(parts, " "))
	}
	fmt.Printf("检索可见 %d（记忆层；未被取代；已过期的另计）\n", s.Live)
	if s.Superseded > 0 {
		fmt.Printf("已归并   %d（被相似归并取代，保留可追溯）\n", s.Superseded)
	}
	fmt.Printf("待回收   %d\n", s.ExpiredLive)
	if len(s.ByKind) > 0 {
		keys := make([]string, 0, len(s.ByKind))
		for k := range s.ByKind {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, s.ByKind[k]))
		}
		fmt.Printf("按类型   %s\n", strings.Join(parts, " "))
	}
	if len(s.ByProject) > 0 {
		keys := make([]string, 0, len(s.ByProject))
		for k := range s.ByProject {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, s.ByProject[k]))
		}
		fmt.Printf("按工程   %s\n", strings.Join(parts, " "))
	}
	printBacklog(bl, spoolFiles, spoolAge)
	return nil
}
