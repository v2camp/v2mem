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

	"github.com/wanghui/v2mem/internal/store"
)

const usageText = `v2mem (mem) — 个人 Agent 记忆系统

用法:
  mem add    [选项] "<原子事实>"    写入一条记忆（相同内容自动覆盖）
  mem search [选项] "<查询>"        全文检索（FTS5 / bm25）
  mem touch  <id|前缀>             记录命中，刷新 last_seen_at 并累加 access_count
  mem forget <id|前缀>             删除一条记忆
  mem gc     [选项]                回收：TTL 到期 + 久未命中且低重要性
  mem consolidate [选项]           相似知识归并（近重复聚簇，留 1 条）
  mem export [路径.jsonl]          导出为 JSONL（省略路径则写标准输出）
  mem import <路径.jsonl>          按 (content_hash, project) 归并进本地库
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
  --salience <f>   重要性 0..1（默认 0.5）
  --ttl <dur>      硬过期，如 720h（默认不过期）

search 选项:
  --limit <n>      返回条数（默认 10）
  --project <p>    限定工程（默认不限）
  --kind <k>       限定类型（默认不限）
  --tag <k=v>      限定标记，可重复，AND 语义（默认不限）
  --scope <s>      current=当前工程+全局；global=只要全局；all=跨工程（默认）

gc 选项:
  --max-idle <dur>      久未命中阈值（默认 720h，即 30 天）
  --min-salience <f>    低于此重要性才淘汰（默认 0.2）

consolidate 选项:
  --threshold <f>       相似度阈值 0..1（默认 0.7）
                        准确性由护栏保证（数字/否定/长度/短文本），阈值只影响召回

示例:
  mem add --kind decision "记忆库数据必须放在 ~/.v2mem，不放代码目录"
  mem search "记忆库 数据 路径"
  mem stats --json
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "add":
		err = cmdAdd(os.Args[2:])
	case "search":
		err = cmdSearch(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "touch":
		err = cmdTouch(os.Args[2:])
	case "forget":
		err = cmdForget(os.Args[2:])
	case "gc":
		err = cmdGC(os.Args[2:])
	case "consolidate":
		err = cmdConsolidate(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
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
	salience := fs.Float64("salience", 0.5, "重要性 0..1")
	ttl := fs.Duration("ttl", 0, "硬过期时长")
	var tags stringSlice
	fs.Var(&tags, "tag", "标记 k=v，可重复")
	if err := fs.Parse(args); err != nil {
		return err
	}

	content := strings.TrimSpace(strings.Join(fs.Args(), " "))
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
		Tags:     tagMap,
		Salience: *salience,
		TTL:      *ttl,
	})
	if err != nil {
		return err
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
	var tags stringSlice
	fs.Var(&tags, "tag", "标记 k=v，可重复")
	if err := fs.Parse(args); err != nil {
		return err
	}

	q := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if q == "" {
		return errors.New("缺少查询，用法: mem search \"<query>\"")
	}
	switch *scope {
	case "", "all", "current", "global":
	default:
		return fmt.Errorf("未知 --scope: %q（可选 current|global|all）", *scope)
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

	hits, err := st.Search(store.SearchQuery{
		Query:   q,
		Project: proj,
		Kind:    *kind,
		Tags:    tagMap,
		Scope:   *scope,
		Limit:   *limit,
	})
	if err != nil {
		return err
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
		fmt.Printf("%s  %s  [%s/%s]\n", shortID(h.ID), h.Content, h.Kind, proj)
	}
	return nil
}

// cmdTouch 记录一次命中：刷新 last_seen_at、累加 access_count。
// 这是衰减机制的输入——常被命中的记忆不会被淘汰。
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

	full, err := st.Touch(id)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(map[string]string{"id": full, "touched": "true"})
	}
	fmt.Printf("已记录命中 %s\n", shortID(full))
	return nil
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

// cmdGC 回收过期与衰减的记忆。
func cmdGC(args []string) error {
	var c common
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	c.register(fs)
	maxIdle := fs.Duration("max-idle", 720*time.Hour, "久未命中阈值")
	minSalience := fs.Float64("min-salience", 0.2, "低于此重要性才淘汰")
	if err := fs.Parse(args); err != nil {
		return err
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
		return printJSON(res)
	}
	fmt.Printf("已回收 过期=%d 衰减=%d，保留 %d\n", res.Expired, res.Decayed, res.Kept)
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
	fmt.Printf("已归并 新增=%d 合并=%d 跳过=%d（读入 %d 行）\n",
		res.Inserted, res.Merged, res.Skipped, len(recs))
	return nil
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

	res, err := st.Consolidate(*threshold)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(res)
	}
	fmt.Printf("已归并 扫描=%d 簇=%d 取代=%d\n", res.Scanned, res.Groups, res.Superseded)
	for _, p := range res.Pairs {
		fmt.Printf("  %s ← %s  (相似度 %.3f)\n",
			shortID(p.Survivor), shortID(p.Superseded), p.Similarity)
	}
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
	if c.json {
		return printJSON(s)
	}

	fmt.Printf("库路径   %s\n", s.Path)
	fmt.Printf("大小     %.1f KB\n", float64(s.SizeBytes)/1024)
	fmt.Printf("总条数   %d\n", s.Total)
	fmt.Printf("检索可见 %d（未被取代；已过期的另计）\n", s.Live)
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
	return nil
}
