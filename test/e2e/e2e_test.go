//go:build e2e

// 端到端测试：黑盒跑真实二进制，覆盖 安装→初始化→使用→维护→优化→评测→卸载
// 全生命周期核心动线。不 import 任何内部包，只断言命令输出与副作用。
// 运行：make test-e2e（= go test -tags e2e -count=1 -v ./test/e2e）
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var repoRoot string

// go 构建缓存路径：HOME 重定向后 Go 工具的默认 GOMODCACHE/GOPATH/GOCACHE
// 会跟着落到空的临时 HOME 下，导致 install 阶段联网重新下载依赖 —— 这里显式保留原值。
var goModCache, goPath, goCache string

func TestMain(m *testing.M) {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Getwd:", err)
		os.Exit(1)
	}
	repoRoot = filepath.Dir(filepath.Dir(cwd)) // test/e2e → 仓库根
	if out, err := exec.Command("go", "env", "GOMODCACHE", "GOPATH", "GOCACHE").Output(); err == nil {
		fields := strings.Fields(string(out))
		if len(fields) >= 3 {
			goModCache, goPath, goCache = fields[0], fields[1], fields[2]
		}
	}
	os.Exit(m.Run())
}

// env 是隔离环境：HOME 指向临时目录，库/词库/工具配置全部落内。
type env struct {
	home string
	db   string
	bin  string // mem 二进制（install 阶段后设置）
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	return &env{home: home, db: filepath.Join(home, ".v2mem", "mem.db")}
}

// run 执行命令并返回合并输出；HOME 重定向到隔离目录。
func (e *env) run(t *testing.T, name, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = repoRoot
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOME=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+e.home)
	if goModCache != "" {
		cmd.Env = append(cmd.Env, "GOMODCACHE="+goModCache, "GOPATH="+goPath, "GOCACHE="+goCache)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return string(out)
}

// runMem 跑 mem 二进制；--db 是子命令级选项，插在子命令之后。
func (e *env) runMem(t *testing.T, name string, args ...string) string {
	t.Helper()
	if e.bin == "" {
		t.Fatal("mem 二进制未就绪（install 阶段未运行）")
	}
	full := append([]string{args[0], "--db", e.db}, args[1:]...)
	return e.run(t, name, e.bin, full...)
}

type hitRow struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

type exportRow struct {
	ContentHash string `json:"content_hash"`
	Content     string `json:"content"`
}

func parseJSONL[T any](t *testing.T, data string) []T {
	t.Helper()
	var out []T
	for _, ln := range strings.Split(strings.TrimSpace(data), "\n") {
		if ln == "" {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("jsonl 解析失败: %v\n%s", err, ln)
		}
		out = append(out, v)
	}
	return out
}

// parseJSON 解析整段 JSON 数组（ls --json 的输出形态，与 export 的 JSONL 不同）。
func parseJSON[T any](t *testing.T, data string) []T {
	t.Helper()
	var out []T
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatalf("json 解析失败: %v\n%s", err, data)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

func TestE2EFullLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh 是 bash 脚本，E2E 暂覆盖 unix")
	}
	e := newEnv(t)

	t.Run("01 安装（install.sh 源码构建）", func(t *testing.T) {
		binDir := filepath.Join(e.home, "install-bin")
		e.run(t, "install.sh", "bash",
			filepath.Join(repoRoot, "scripts/install.sh"), "--from-source", "--dir", binDir)
		e.bin = filepath.Join(binDir, "mem")
		if _, err := os.Stat(e.bin); err != nil {
			t.Fatalf("安装产物缺失: %v", err)
		}
		if out := e.runMem(t, "安装产物冒烟", "stats"); out == "" {
			t.Fatal("安装产物 stats 无输出")
		}
	})

	t.Run("02 初始化（mem init 幂等 + 备份）", func(t *testing.T) {
		cfgPath := filepath.Join(e.home, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
			t.Fatal(err)
		}
		userCfg := `{"permissions":{"allow":[]}}`
		if err := os.WriteFile(cfgPath, []byte(userCfg), 0o644); err != nil {
			t.Fatal(err)
		}
		e.runMem(t, "init", "init", "--harness", "claude")
		cfg := readFile(t, cfgPath)
		if !strings.Contains(cfg, "# v2mem") {
			t.Fatal("init 未写入 v2mem 钩子")
		}
		if !strings.Contains(cfg, "permissions") {
			t.Fatal("init 不应覆盖用户已有配置")
		}
		e.runMem(t, "init 幂等", "init", "--harness", "claude")
		before := strings.Count(cfg, "# v2mem")
		after := strings.Count(readFile(t, cfgPath), "# v2mem")
		if after != before {
			t.Fatalf("init 应幂等（v2mem 标记数 %d→%d）", before, after)
		}
		if _, err := os.Stat(cfgPath + ".v2mem.bak"); err != nil {
			t.Fatalf("init 应生成备份 %s.v2mem.bak", cfgPath)
		}
	})

	t.Run("03 使用（add 去重 / search / touch / stats）", func(t *testing.T) {
		const content = "记忆库数据必须放在 ~/.v2mem，不放代码目录"
		e.runMem(t, "add", "add", "--project", "demo", "--kind", "decision", content)
		e.runMem(t, "add 重复", "add", "--project", "demo", "--kind", "decision", content)
		if out := e.runMem(t, "search", "search", "记忆库"); !strings.Contains(out, content) {
			t.Fatalf("search 未命中: %s", out)
		}
		hits := parseJSON[hitRow](t, e.runMem(t, "ls", "ls", "--json"))
		if len(hits) != 1 {
			t.Fatalf("相同内容 add 两次应去重为 1 条，got %d", len(hits))
		}
		e.runMem(t, "touch", "touch", hits[0].ID)
		if out := e.runMem(t, "stats", "stats"); out == "" {
			t.Fatal("stats 无输出")
		}
	})

	t.Run("04 维护（gc TTL 回收 + consolidate 归并）", func(t *testing.T) {
		e.runMem(t, "add ttl", "add", "--project", "demo", "--ttl", "1s", "这条记忆 1 秒后过期")
		time.Sleep(1500 * time.Millisecond)
		e.runMem(t, "gc", "gc")
		if out := e.runMem(t, "search 过期", "search", "这条记忆"); strings.Contains(out, "1 秒后过期") {
			t.Fatalf("TTL 过期记忆未被 gc 回收: %s", out)
		}
		backupDir := filepath.Join(e.home, ".v2mem", "backup")
		entries, err := os.ReadDir(backupDir)
		if err != nil || len(entries) == 0 {
			t.Fatalf("gc 应生成库备份（%s）: %v", backupDir, err)
		}
		e.runMem(t, "add 近重复1", "add", "--project", "demo", "命令 mem consolidate 会归并相似记忆")
		e.runMem(t, "add 近重复2", "add", "--project", "demo", "命令 mem consolidate 会归并相似记忆，真的")
		e.runMem(t, "consolidate", "consolidate")
		n := 0
		for _, r := range parseJSON[hitRow](t, e.runMem(t, "ls 归并后", "ls", "--json")) {
			if strings.Contains(r.Content, "consolidate") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("consolidate 后应剩 1 条近重复，got %d", n)
		}
	})

	t.Run("05 优化（lexicon 提取 + 同义词展开生效）", func(t *testing.T) {
		e.runMem(t, "add s1", "add", "--project", "demo", "--kind", "pitfall", "ingest 命令把 md 文件机械化搬进记忆库")
		e.runMem(t, "add s2", "add", "--project", "demo", "--kind", "pitfall", "用 mem ingest 把文档搬进库里")
		e.runMem(t, "add s3", "add", "--project", "demo", "--kind", "pitfall", "ingest 搬运完成后内容就在库内")
		e.run(t, "lexicon init", e.bin, "lexicon", "init", "--db", e.db, "--project", "demo", "--terms", "搬进,ingest")
		syn := readFile(t, filepath.Join(e.home, ".v2mem", "synonyms.txt"))
		if !strings.Contains(syn, "[demo]") {
			t.Fatalf("synonyms.txt 缺 [demo] 段:\n%s", syn)
		}
		if !strings.Contains(syn, "ingest") || !strings.Contains(syn, "搬进") {
			t.Fatalf("词对未收录:\n%s", syn)
		}
		out := e.runMem(t, "search 别名词", "search", "--project", "demo", "搬进")
		if !strings.Contains(out, "ingest") {
			t.Fatalf("别名词「搬进」应经展开命中 ingest 记忆: %s", out)
		}
	})

	t.Run("06 评测（gold 判据 + 用量视图）", func(t *testing.T) {
		target := ""
		for _, r := range parseJSONL[exportRow](t, e.runMem(t, "export", "export")) {
			if strings.Contains(r.Content, "ingest 命令把 md 文件机械化搬进记忆库") {
				target = r.ContentHash
			}
		}
		if target == "" {
			t.Fatal("export 中未找到目标记忆")
		}
		prefix := target
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		goldPath := filepath.Join(e.home, "recall.gold.jsonl")
		gold := fmt.Sprintf("{\"query\":\"搬进\",\"gold\":[\"%s\"]}\n", prefix)
		if err := os.WriteFile(goldPath, []byte(gold), 0o644); err != nil {
			t.Fatal(err)
		}
		out := e.run(t, "eval recall", e.bin, "eval", "recall", "--db", e.db, "--gold", goldPath)
		if !strings.Contains(out, "Recall@") {
			t.Fatalf("eval recall 无 Recall 输出: %s", out)
		}
		if act := e.run(t, "eval activity", e.bin, "eval", "activity", "--db", e.db); act == "" {
			t.Fatal("eval activity 无输出")
		}
	})

	t.Run("07 卸载（uninstall 摘除且不碰用户配置）", func(t *testing.T) {
		cfgPath := filepath.Join(e.home, ".claude", "settings.json")
		e.runMem(t, "uninstall", "uninstall", "--harness", "claude")
		cfg := readFile(t, cfgPath)
		if strings.Contains(cfg, "# v2mem") {
			t.Fatalf("uninstall 后仍有 v2mem 标记: %s", cfg)
		}
		if !strings.Contains(cfg, "permissions") {
			t.Fatal("uninstall 不应动用户自己的配置")
		}
		e.runMem(t, "uninstall all", "uninstall", "--all")
		if got := readFile(t, cfgPath); strings.Contains(got, "# v2mem") {
			t.Fatal("uninstall --all 后仍有 v2mem 标记")
		}
	})
}
