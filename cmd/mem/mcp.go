// MCP server（stdio）：让支持 MCP 的 AI 工具直接调用 mem 的检索与写入。
//
// 为什么自实现而不是引 SDK：项目纪律是「直接依赖只有 1 个、零外部服务」，
// MCP stdio 只是 newline-delimited JSON-RPC 2.0 的一个小子集（initialize /
// tools/list / tools/call / ping），自实现约 250 行且可纯函数单测；引 mcp-go
// 会破坏单文件轻量卖点，换来的流式/采样能力当前用不上。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/store"
)

const (
	// mcpProtocolVersion 取 MCP 规范 2025-06-18 版（stdio 传输与 tools 能力自该版稳定）。
	mcpProtocolVersion = "2025-06-18"
	mcpVersion         = "0.1.0"
)

// mcpTool 描述暴露给 MCP 客户端的工具。Schema 是 JSON Schema 的 properties 部分。
type mcpTool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// mcpTools 是 mem 暴露给 MCP 的工具清单 —— 只放「记忆本身的操作」，
// 评测/运维（audit、eval、gc、consolidate）不进工具面，保持工具语义最小。
var mcpTools = []mcpTool{
	{
		Name:        "search",
		Description: "全文检索。layer=memory（默认）只查已定记忆；layer=observation 查近期操作痕迹；layer=both 两层交错并在每条带 layer。开场注入不会自动带痕迹，要查「刚才做了什么 / 改过哪个文件」时自己把 layer 设为 observation。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":   map[string]any{"type": "string", "description": "检索词"},
				"limit":   map[string]any{"type": "number", "description": "返回条数，默认 10"},
				"scope":   map[string]any{"type": "string", "description": "current=当前工程+全局 / global=只要全局 / all=跨工程（默认）"},
				"project": map[string]any{"type": "string", "description": "限定工程（默认不限）"},
				"layer":   map[string]any{"type": "string", "description": "memory=已定记忆（默认）/ observation=近期痕迹 / both=两层。别名 recent、all"},
			},
			"required": []string{"query"},
		},
	},
	{
		Name:        "add",
		Description: "写入一条记忆（一条只写一个原子事实；相同内容自动覆盖，不会重复）。content 必填。记作来源 source=llm。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content":  map[string]any{"type": "string", "description": "原子事实正文（必填）"},
				"kind":     map[string]any{"type": "string", "description": "preference|decision|pitfall|task|fact，默认 fact"},
				"project":  map[string]any{"type": "string", "description": "工程标记，默认取当前 git 仓库名"},
				"global":   map[string]any{"type": "boolean", "description": "写入全局记忆（对所有工程生效），与 project 互斥"},
				"salience": map[string]any{"type": "number", "description": "重要性 0..1，默认 0.5"},
			},
			"required": []string{"content"},
		},
	},
	{
		Name:        "ls",
		Description: "列出库里的记忆（不需要查询词）。默认按工程分组列出全部。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"project": map[string]any{"type": "string", "description": "限定工程（默认不限）"},
				"kind":    map[string]any{"type": "string", "description": "限定类型"},
				"scope":   map[string]any{"type": "string", "description": "current / global / all"},
				"limit":   map[string]any{"type": "number", "description": "最多列出多少条，默认 50"},
			},
		},
	},
	{
		Name:        "touch",
		Description: "记录一次命中：刷新 last_seen_at、累加 access_count。用上某条记忆后调用它，防止该记忆被遗忘机制回收。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "记忆 id 或前缀"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "recall_context",
		Description: "只读。按 query 检索并把类码与命中拼成一段可注入文本。layer 与 search 相同，默认 memory。有 hook 注入时不必每轮调用。不写库。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":        map[string]any{"type": "string", "description": "检索词（必填）"},
				"scope":        map[string]any{"type": "string", "description": "current|global|all，默认 current"},
				"project":      map[string]any{"type": "string", "description": "限定工程"},
				"layer":        map[string]any{"type": "string", "description": "memory|observation|both，默认 memory"},
				"budget_chars": map[string]any{"type": "number", "description": "注入文本的字符预算，默认 1200"},
			},
			"required": []string{"query"},
		},
	},
}

func cmdMCP(args []string) error {
	var c common
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	c.register(fs)
	auditFile := fs.String("audit-file", "", "审计日志路径（默认 ~/.v2mem/audit.jsonl；- 关闭）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	// MCP server 是长驻进程、且每个工具调用都埋点，是异步审计的天然场景：
	// 写入交给后台协程，工具调用不摊磁盘 IO。队列满时退化同步写（见 auditAppend）。
	var ap *audit.AsyncAppender
	if *auditFile != "-" {
		ap = audit.NewAsyncAppender(auditPath(*auditFile), 128)
	}

	s := &mcpServer{st: st, auditFile: *auditFile, audit: ap}
	err = s.serve(os.Stdin, os.Stdout)
	if ap != nil {
		ap.Close() // 返回前排空未落盘的审计，避免进程退出丢记录
	}
	return err
}

type mcpServer struct {
	st        *store.Store
	auditFile string
	audit     *audit.AsyncAppender
}

// auditAppend 优先投递到异步通道；通道满或未启用时退回同步写。
// 审计是交叉验证的事实记录，宁可用同步写补一次也不丢弃。
func (s *mcpServer) auditAppend(rec audit.Record) {
	if s.audit != nil && s.audit.Append(rec) {
		return
	}
	_ = audit.Append(auditPath(s.auditFile), rec)
}

// serve 跑 stdio 主循环：逐行读请求，逐行写响应（newline-delimited JSON-RPC 2.0）。
func (s *mcpServer) serve(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			if err := enc.Encode(mcpError(nil, -32700, "parse error: "+err.Error())); err != nil {
				return err
			}
			continue
		}
		resp := s.dispatch(req)
		if resp == nil {
			continue // 通知：无需响应
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// dispatch 是协议的核心纯函数：入请求 map，出响应 map（nil = 通知不响应）。
func (s *mcpServer) dispatch(req map[string]any) map[string]any {
	method, _ := req["method"].(string)
	id, hasID := req["id"]
	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	switch method {
	case "initialize":
		if !hasID {
			return nil
		}
		return mcpResult(id, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "v2mem", "version": mcpVersion},
		})
	case "notifications/initialized":
		return nil
	case "ping":
		if !hasID {
			return nil
		}
		return mcpResult(id, map[string]any{})
	case "tools/list":
		if !hasID {
			return nil
		}
		tools := make([]map[string]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.Schema,
			})
		}
		return mcpResult(id, map[string]any{"tools": tools})
	case "tools/call":
		if !hasID {
			return nil
		}
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		return s.callTool(id, name, args)
	default:
		if !hasID {
			return nil
		}
		return mcpError(id, -32601, "method not found: "+method)
	}
}

func (s *mcpServer) callTool(id any, name string, args map[string]any) map[string]any {
	switch name {
	case "search":
		return s.toolSearch(id, args)
	case "add":
		return s.toolAdd(id, args)
	case "ls":
		return s.toolLS(id, args)
	case "touch":
		return s.toolTouch(id, args)
	case "recall_context":
		return s.toolRecall(id, args)
	default:
		return mcpError(id, -32602, "unknown tool: "+name)
	}
}

// ---------- 工具实现 ----------

func (s *mcpServer) toolSearch(id any, args map[string]any) map[string]any {
	q := argString(args, "query")
	if q == "" {
		return mcpError(id, -32602, "search 需要非空的 query")
	}
	limit := argInt(args, "limit", 10)
	if limit <= 0 {
		return mcpError(id, -32602, "limit 应为正数")
	}
	scope := argString(args, "scope")
	switch scope {
	case "", "all", "current", "global":
	default:
		return mcpError(id, -32602, fmt.Sprintf("scope 可选 current|global|all，got %q", scope))
	}
	project := argString(args, "project")
	if scope == "current" && project == "" {
		project = detectProject()
	}
	layer := argString(args, "layer")
	if _, err := store.CanonicalLayer(layer); err != nil {
		return mcpError(id, -32602, err.Error())
	}

	started := time.Now()
	hits, err := s.st.Search(store.SearchQuery{Query: q, Project: project, Scope: scope, Limit: limit, Layer: layer})
	if err != nil {
		return mcpError(id, -32603, "search 失败: "+err.Error())
	}
	// 审计：与 CLI 手工检索同一埋点，MCP 使用也进评测样本（不写则 MCP 通道不可观测）
	if s.auditFile != "-" {
		rec := audit.Record{
			TS: started.Unix(), Event: "manual-search", Project: project,
			Query: q, Empty: len(hits) == 0, MS: time.Since(started).Milliseconds(),
		}
		for _, h := range hits {
			rec.Hashes = append(rec.Hashes, h.ID)
			rec.Kinds = append(rec.Kinds, h.Kind)
		}
		s.auditAppend(rec)
	}
	return toolResult(id, hits, false)
}

func (s *mcpServer) toolAdd(id any, args map[string]any) map[string]any {
	content := argString(args, "content")
	if content == "" {
		return mcpError(id, -32602, "add 需要非空的 content")
	}
	kind := argString(args, "kind")
	if kind == "" {
		kind = "fact"
	}
	project := argString(args, "project")
	global := argBool(args, "global", false)
	if global && project != "" {
		return mcpError(id, -32602, "global 与 project 互斥")
	}
	if global {
		project = ""
	} else if project == "" {
		project = detectProject()
	}
	salience := argFloat(args, "salience", 0.5)
	if salience < 0 || salience > 1 {
		return mcpError(id, -32602, "salience 应落在 [0,1]")
	}

	res, err := s.st.Add(store.AddInput{
		Content: content, Kind: kind, Project: project,
		Tool: "mcp", Source: "llm", Salience: salience,
	})
	if err != nil {
		return mcpError(id, -32603, "add 失败: "+err.Error())
	}
	if s.auditFile != "-" {
		mode := "create"
		if !res.Created {
			mode = "overwrite"
		}
		s.auditAppend(audit.Record{
			TS: time.Now().Unix(), Event: "manual-add", Project: project,
			Hashes: []string{res.Hash}, Kinds: []string{kind}, Mode: mode,
			Source: "llm",
		})
	}
	return toolResult(id, res, false)
}

func (s *mcpServer) toolLS(id any, args map[string]any) map[string]any {
	limit := argInt(args, "limit", 50)
	if limit <= 0 {
		return mcpError(id, -32602, "limit 应为正数")
	}
	scope := argString(args, "scope")
	switch scope {
	case "", "all", "current", "global":
	default:
		return mcpError(id, -32602, fmt.Sprintf("scope 可选 current|global|all，got %q", scope))
	}
	project := argString(args, "project")
	if scope == "current" && project == "" {
		project = detectProject()
	}
	hits, err := s.st.List(store.ListQuery{
		Scope: scope, Project: project, Kind: argString(args, "kind"), Limit: limit,
	})
	if err != nil {
		return mcpError(id, -32603, "ls 失败: "+err.Error())
	}
	if hits == nil {
		hits = []store.Hit{}
	}
	return toolResult(id, hits, false)
}

func (s *mcpServer) toolTouch(id any, args map[string]any) map[string]any {
	tid := argString(args, "id")
	if tid == "" {
		return mcpError(id, -32602, "touch 需要 id（记忆 id 或前缀）")
	}
	full, err := s.st.Touch(tid)
	if err != nil {
		return mcpError(id, -32603, "touch 失败: "+err.Error())
	}
	return toolResult(id, map[string]string{"id": full, "touched": "true"}, false)
}

// toolRecall 是 search 的编排糖：返回一段可注入文本，不写库。
func (s *mcpServer) toolRecall(id any, args map[string]any) map[string]any {
	q := argString(args, "query")
	if q == "" {
		return mcpError(id, -32602, "recall_context 需要非空的 query")
	}
	scope := argString(args, "scope")
	if scope == "" {
		scope = "current"
	}
	switch scope {
	case "all", "current", "global":
	default:
		return mcpError(id, -32602, fmt.Sprintf("scope 可选 current|global|all，got %q", scope))
	}
	project := argString(args, "project")
	if scope == "current" && project == "" {
		project = detectProject()
	}
	budget := argInt(args, "budget_chars", defaultHookBudget)
	if budget <= 0 {
		return mcpError(id, -32602, "budget_chars 应为正数")
	}
	layer := argString(args, "layer")
	if _, err := store.CanonicalLayer(layer); err != nil {
		return mcpError(id, -32602, err.Error())
	}
	started := time.Now()
	hits, err := s.st.Search(store.SearchQuery{Query: q, Project: project, Scope: scope, Limit: defaultHookLimit, Layer: layer})
	if err != nil {
		return mcpError(id, -32603, "recall_context 失败: "+err.Error())
	}
	if s.auditFile != "-" {
		rec := audit.Record{
			TS: started.Unix(), Event: "manual-search", Project: project,
			Query: q, Empty: len(hits) == 0, MS: time.Since(started).Milliseconds(),
		}
		for _, h := range hits {
			rec.Hashes = append(rec.Hashes, h.ID)
			rec.Kinds = append(rec.Kinds, h.Kind)
		}
		s.auditAppend(rec)
	}
	text := recallText(hits, budget)
	return toolResult(id, map[string]string{"text": text}, false)
}

func recallText(hits []store.Hit, budget int) string {
	var b strings.Builder
	b.WriteString(renderKindCodes())
	if len(hits) == 0 {
		line := "（无命中）\n"
		if runeLen(b.String())+runeLen(line) <= budget {
			b.WriteString(line)
		}
		return b.String()
	}
	block, _ := formatHits("相关记忆", hits, budget-runeLen(b.String()))
	b.WriteString(block)
	return b.String()
}

// ---------- 协议辅助 ----------

func mcpResult(id any, result map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpError(id any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	}
}

// toolResult 把执行结果包成 MCP 工具响应：text 内容放紧凑 JSON（LLM 友好）。
func toolResult(id any, data any, isError bool) map[string]any {
	b, err := json.Marshal(data)
	if err != nil {
		return mcpError(id, -32603, "marshal result: "+err.Error())
	}
	return mcpResult(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
		"isError": isError,
	})
}

// auditPath 与 cmdAudit/cmdEvalActivity 保持一致：空值取默认审计日志位置。
func auditPath(p string) string {
	if p == "" {
		return audit.Path()
	}
	return p
}

func argString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func argInt(args map[string]any, key string, def int) int {
	// JSON number 解到 any 是 float64；测试/直呼时 Go 字面量是 int，两者都要认。
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}

func argBool(args map[string]any, key string, def bool) bool {
	v, ok := args[key].(bool)
	if !ok {
		return def
	}
	return v
}

func argFloat(args map[string]any, key string, def float64) float64 {
	switch v := args[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return def
}
