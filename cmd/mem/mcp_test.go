package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanghui/v2mem/internal/audit"
	"github.com/wanghui/v2mem/internal/store"
)

// ---------- 协议级（dispatch 是纯函数，直接喂 map 断言响应） ----------

func newMCPServer(t *testing.T) (*mcpServer, string) {
	t.Helper()
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &mcpServer{st: st, auditFile: filepath.Join(t.TempDir(), "audit.jsonl")}, db
}

func mcpReq(id any, method string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func mcpNotif(method string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	return map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
}

// mcpCall 执行 tools/call 并断言无错误，返回 result。
func mcpCall(t *testing.T, s *mcpServer, name string, args map[string]any) map[string]any {
	t.Helper()
	resp := s.dispatch(mcpReq(1, "tools/call", map[string]any{"name": name, "arguments": args}))
	if errObj, hasErr := resp["error"]; hasErr {
		t.Fatalf("tools/call %s 返回错误: %v", name, errObj)
	}
	return resp["result"].(map[string]any)
}

func mcpText(result map[string]any) string {
	// 直呼 dispatch 时 content 是 []map[string]any；经 JSON round-trip 后是 []any。
	switch c := result["content"].(type) {
	case []map[string]any:
		return c[0]["text"].(string)
	case []any:
		return c[0].(map[string]any)["text"].(string)
	}
	return ""
}

func TestMCPInitialize(t *testing.T) {
	s, _ := newMCPServer(t)
	resp := s.dispatch(mcpReq(1, "initialize", nil))
	if resp["error"] != nil {
		t.Fatalf("initialize 不应报错: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion 应为 %s，got %v", mcpProtocolVersion, result["protocolVersion"])
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "v2mem" {
		t.Errorf("serverInfo.name 应为 v2mem，got %v", info["name"])
	}
}

func TestMCPToolsList(t *testing.T) {
	s, _ := newMCPServer(t)
	resp := s.dispatch(mcpReq(1, "tools/list", nil))
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]map[string]any)
	if len(tools) != len(mcpTools) {
		t.Fatalf("应列出 %d 个工具，got %d", len(mcpTools), len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl["name"].(string)] = true
		if tl["inputSchema"] == nil {
			t.Errorf("工具 %v 缺 inputSchema", tl["name"])
		}
	}
	for _, want := range []string{"search", "add", "ls", "touch"} {
		if !names[want] {
			t.Errorf("工具清单缺 %s，got %v", want, names)
		}
	}
}

func TestMCPCallSearch(t *testing.T) {
	s, db := newMCPServer(t)
	if err := cmdAdd([]string{"--db", db, "--project", "demo", "--kind", "pitfall",
		"活库 mem.db 不能放进 iCloud 同步目录"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	result := mcpCall(t, s, "search", map[string]any{"query": "iCloud", "project": "demo"})
	text := mcpText(result)
	if !strings.Contains(text, "iCloud") {
		t.Errorf("search 应命中记忆，got: %s", text)
	}
	if result["isError"] == true {
		t.Errorf("正常检索不应标 isError")
	}
}

func TestMCPCallAddAndDedup(t *testing.T) {
	s, _ := newMCPServer(t)
	r1 := mcpCall(t, s, "add", map[string]any{"content": "MCP 写入的原子事实", "project": "demo"})
	if !strings.Contains(mcpText(r1), `"created":true`) {
		t.Errorf("首次写入应 created=true，got: %s", mcpText(r1))
	}
	r2 := mcpCall(t, s, "add", map[string]any{"content": "MCP 写入的原子事实", "project": "demo"})
	if !strings.Contains(mcpText(r2), `"created":false`) {
		t.Errorf("相同内容应覆盖（created=false），got: %s", mcpText(r2))
	}
	// 库里只有 1 条
	hits, err := s.st.List(store.ListQuery{Scope: "all", Limit: 100})
	if err != nil || len(hits) != 1 {
		t.Errorf("去重后应只有 1 条，got %d（err=%v）", len(hits), err)
	}
}

func TestMCPCallLS(t *testing.T) {
	s, db := newMCPServer(t)
	for _, c := range []string{"甲项目的事实", "乙项目的事实"} {
		if err := cmdAdd([]string{"--db", db, "--project", "demo", c}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	}
	result := mcpCall(t, s, "ls", map[string]any{"project": "demo"})
	if !strings.Contains(mcpText(result), "甲项目的事实") {
		t.Errorf("ls 应列出记忆，got: %s", mcpText(result))
	}
}

func TestMCPCallTouch(t *testing.T) {
	s, db := newMCPServer(t)
	if err := cmdAdd([]string{"--db", db, "--project", "demo", "要被 touch 的记忆"}); err != nil {
		t.Fatalf("cmdAdd: %v", err)
	}
	hits, _ := s.st.List(store.ListQuery{Scope: "all", Limit: 10})
	prefix := hits[0].ID[:8]
	result := mcpCall(t, s, "touch", map[string]any{"id": prefix})
	if !strings.Contains(mcpText(result), `"touched":"true"`) {
		t.Errorf("touch 应返回 touched=true，got: %s", mcpText(result))
	}
	// 命中计数应累加
	full, err := s.st.Touch(prefix)
	if err != nil || full == "" {
		t.Errorf("库内记录应可继续命中，err=%v", err)
	}
}

// 审计埋点：MCP 的读/写必须与 CLI 同一条埋点，否则 MCP 通道不可评测。
func TestMCPWritesAuditRecords(t *testing.T) {
	s, _ := newMCPServer(t)
	mcpCall(t, s, "add", map[string]any{"content": "审计样本", "project": "demo"})
	mcpCall(t, s, "search", map[string]any{"query": "审计", "project": "demo"})

	rs, err := audit.Read(s.auditFile, 0)
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("MCP 读写应各留 1 条审计，got %d", len(rs))
	}
	var hasRead, hasWrite bool
	for _, r := range rs {
		hasRead = hasRead || (r.Event == "manual-search" && strings.Contains(r.Query, "审计"))
		hasWrite = hasWrite || (r.Event == "manual-add" && r.Mode == "create")
	}
	if !hasRead || !hasWrite {
		t.Errorf("审计应同时有读/写记录，got %+v", rs)
	}
}

func TestMCPCallRejectsBadArgs(t *testing.T) {
	s, _ := newMCPServer(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"search", map[string]any{}},                        // 缺 query
		{"search", map[string]any{"query": "q", "scope": "bogus"}}, // 非法 scope
		{"search", map[string]any{"query": "q", "limit": 0}}, // limit 非正
		{"add", map[string]any{"content": ""}},              // content 为空
		{"add", map[string]any{"content": "x", "global": true, "project": "p"}}, // 互斥
		{"add", map[string]any{"content": "x", "salience": 2}}, // salience 越界
		{"touch", map[string]any{}},                         // 缺 id
	}
	for _, c := range cases {
		resp := s.dispatch(mcpReq(1, "tools/call", map[string]any{"name": c.name, "arguments": c.args}))
		if resp["error"] == nil {
			t.Errorf("tools/call %s %v 应报参数错误，got 成功", c.name, c.args)
			continue
		}
		code := resp["error"].(map[string]any)["code"]
		if code != -32602 {
			t.Errorf("tools/call %s 应返回 -32602，got %v", c.name, code)
		}
	}
}

func TestMCPUnknownMethodAndTool(t *testing.T) {
	s, _ := newMCPServer(t)
	resp := s.dispatch(mcpReq(1, "frobnicate", nil))
	if resp["error"] == nil || resp["error"].(map[string]any)["code"] != -32601 {
		t.Errorf("未知方法应返回 -32601，got %v", resp)
	}
	resp = s.dispatch(mcpReq(1, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}}))
	if resp["error"] == nil || resp["error"].(map[string]any)["code"] != -32602 {
		t.Errorf("未知工具应返回 -32602，got %v", resp)
	}
}

func TestMCPPing(t *testing.T) {
	s, _ := newMCPServer(t)
	resp := s.dispatch(mcpReq(1, "ping", nil))
	if resp["error"] != nil {
		t.Fatalf("ping 不应报错: %v", resp["error"])
	}
	if resp["result"] == nil {
		t.Error("ping 应返回空 result")
	}
}

// 通知（无 id）不得产生响应 —— 这是 MCP stdio 半双工的关键，否则客户端读挂。
func TestMCPNotificationsSilent(t *testing.T) {
	s, _ := newMCPServer(t)
	for _, method := range []string{"notifications/initialized", "ping", "tools/list"} {
		if resp := s.dispatch(mcpNotif(method, nil)); resp != nil {
			t.Errorf("通知 %s 不应有响应，got %v", method, resp)
		}
	}
}

// ---------- serve 传输层（stdin 逐行 / stdout 逐行） ----------

func TestMCPServeRoundTrip(t *testing.T) {
	s, _ := newMCPServer(t)
	var sb strings.Builder
	reqs := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n" +
		"\n" // 空行应跳过
	if err := s.serve(strings.NewReader(reqs), &sb); err != nil {
		t.Fatalf("serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("应只响应 2 个带 id 的请求（通知与空行不响应），got %d 行:\n%s", len(lines), sb.String())
	}
	for _, l := range lines {
		var resp map[string]any
		if err := json.Unmarshal([]byte(l), &resp); err != nil {
			t.Fatalf("响应应为合法 JSON: %v", err)
		}
	}
}

func TestMCPServeParseError(t *testing.T) {
	s, _ := newMCPServer(t)
	var sb strings.Builder
	if err := s.serve(strings.NewReader("这不是 JSON\n"), &sb); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !strings.Contains(sb.String(), "-32700") {
		t.Errorf("非法输入应回 -32700 解析错误，got: %s", sb.String())
	}
}

// ---------- cmdMCP 端到端（经真实 stdin/stdout） ----------

func TestCmdMCPEndToEnd(t *testing.T) {
	db := testDB(t)
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add","arguments":{"content":"经 MCP 写入","project":"demo"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"MCP","project":"demo"}}}` + "\n"
	out, err := withStdin(t, input, func() error {
		return cmdMCP([]string{"--db", db, "--audit-file", filepath.Join(t.TempDir(), "audit.jsonl")})
	})
	if err != nil {
		t.Fatalf("cmdMCP: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("3 个请求应得 3 行响应，got %d:\n%s", len(lines), out)
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatalf("响应应为合法 JSON: %v", err)
	}
	var addResp map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &addResp); err != nil {
		t.Fatalf("add 响应应为合法 JSON: %v", err)
	}
	text := mcpText(addResp["result"].(map[string]any))
	if !strings.Contains(text, `"created":true`) {
		t.Errorf("add 应回写 created=true，got: %s", text)
	}
}

// ---------- 工具错误路径与辅助函数补覆盖 ----------

func TestMCPToolLSErrors(t *testing.T) {
	s, _ := newMCPServer(t)
	if resp := s.dispatch(mcpReq(1, "tools/call", map[string]any{"name": "ls", "arguments": map[string]any{"limit": 0}})); resp["error"] == nil {
		t.Error("ls limit=0 应报参数错误")
	}
	if resp := s.dispatch(mcpReq(1, "tools/call", map[string]any{"name": "ls", "arguments": map[string]any{"scope": "bogus"}})); resp["error"] == nil {
		t.Error("ls scope=bogus 应报参数错误")
	}
	// current 作用域：无 project 时按 cwd 推断工程，应正常返回
	resp := s.dispatch(mcpReq(1, "tools/call", map[string]any{"name": "ls", "arguments": map[string]any{"scope": "current"}}))
	if resp["error"] != nil {
		t.Errorf("ls --scope current 不应报错: %v", resp["error"])
	}
}

func TestMCPToolResultMarshalError(t *testing.T) {
	// chan 无法 JSON 序列化，应回 -32603 而非崩溃
	resp := toolResult(1, make(chan int), false)
	if resp["error"] == nil || resp["error"].(map[string]any)["code"] != -32603 {
		t.Errorf("不可序列化结果应回 -32603，got %v", resp)
	}
}

func TestMCAuditPathDefault(t *testing.T) {
	if auditPath("") == "" {
		t.Error("空值应返回默认审计路径")
	}
	if got := auditPath("/x/audit.jsonl"); got != "/x/audit.jsonl" {
		t.Errorf("显式路径应原样返回，got %q", got)
	}
}

func TestMCPArgNumberCoercion(t *testing.T) {
	if got := argInt(map[string]any{"n": int64(7)}, "n", 0); got != 7 {
		t.Errorf("int64 应解析为 7，got %d", got)
	}
	if got := argInt(map[string]any{}, "n", 3); got != 3 {
		t.Errorf("缺省应回默认值 3，got %d", got)
	}
	if got := argFloat(map[string]any{"f": int64(7)}, "f", 0); got != 7 {
		t.Errorf("int64 应解析为 7，got %f", got)
	}
	if got := argFloat(map[string]any{"f": "bad"}, "f", 0.5); got != 0.5 {
		t.Errorf("非法类型应回默认值 0.5，got %f", got)
	}
}
