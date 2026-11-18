package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// 路径投影是本命令最容易做错的地方，三种输入形态各断言一次具体值。
func TestAssetPathCandidatesProjectsThreeForms(t *testing.T) {
	const (
		root = "/proj"
		cwd  = "/proj/sub"
		home = "/home/u"
	)
	cases := []struct {
		name  string
		input string
		root  string // 空则用默认 /proj
		want  []string
	}{
		{
			name:  "相对 cwd",
			input: "./cmd/mem/hook.go",
			want:  []string{"sub/cmd/mem/hook.go", "cmd/mem/hook.go", "/proj/sub/cmd/mem/hook.go"},
		},
		{
			name:  "绝对路径",
			input: "/proj/cmd/mem/hook.go",
			want:  []string{"cmd/mem/hook.go", "/proj/cmd/mem/hook.go"},
		},
		{
			name:  "~/ 开头",
			input: "~/proj/cmd/mem/hook.go",
			root:  "/home/u/proj",
			want:  []string{"cmd/mem/hook.go", "/home/u/proj/cmd/mem/hook.go"},
		},
		{
			name:  "文件不在工程根之内：丢掉越界的相对候选",
			input: "/elsewhere/x.go",
			want:  []string{"/elsewhere/x.go"},
		},
		{
			name:  "~user 不做展开，按原样继续",
			input: "~bob/x.go",
			want:  []string{"sub/~bob/x.go", "~bob/x.go", "/proj/sub/~bob/x.go"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.root
			if r == "" {
				r = root
			}
			got := assetPathCandidates(c.input, r, cwd, home)
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("assetPathCandidates(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

// normalizeAssetPaths 从进程环境取 HOME，这里把 HOME 钉死，验证 ~ 展开真的接上了。
func TestNormalizeAssetPathsExpandsHome(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	got := normalizeAssetPaths("~/proj/cmd/mem/hook.go", "/home/u/proj")
	want := []string{"cmd/mem/hook.go", "/home/u/proj/cmd/mem/hook.go"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("normalizeAssetPaths = %v, want %v", got, want)
	}
}

func TestExpandTildeOnlyExpandsLeadingHome(t *testing.T) {
	cases := []struct{ in, home, want string }{
		{"~/a/b.go", "/home/u", "/home/u/a/b.go"},
		{"~", "/home/u", "/home/u"},
		{"~bob/a.go", "/home/u", "~bob/a.go"}, // ~user 不支持：原样返回，不猜
		{"a/~/b.go", "/home/u", "a/~/b.go"},   // 只认开头
		{"~/a/b.go", "", "~/a/b.go"},          // 拿不到 HOME 就不展开
	}
	for _, c := range cases {
		if got := expandTilde(c.in, c.home); got != c.want {
			t.Errorf("expandTilde(%q, home=%q) = %q, want %q", c.in, c.home, got, c.want)
		}
	}
}

func TestIsUnder(t *testing.T) {
	if !isUnder("/a", "/a/b/c.go") || !isUnder("/a", "/a") {
		t.Error("/a 应包含 /a/b/c.go 与自身")
	}
	if isUnder("/a", "/b/c.go") {
		t.Error("/a 不应包含 /b/c.go")
	}
}

// addProvenanceMemory 直接往库里写一条带 provenance 的记忆（绕过 cmd 层，
// 因为 mem add 没有 --provenance 参数，溯源由 ingest 等路径写入）。
func addProvenanceMemory(t *testing.T, db, content, project, file string, line int) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	res, err := st.Add(store.AddInput{
		Content: content, Project: project, Device: "test",
		Provenance: &store.Provenance{File: file, Line: line, Source: "asset"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return res.ID
}

// gitRepoDir 建一个真实 git 仓库当工程根：工程根是「相对路径投影」的基准，
// 用假的目录结构测不出 git rev-parse 那一段。macOS 的 /var → /private/var 是
// 符号链接，git 返回真实路径，这里先取真实路径再拼文件路径——生产代码**不做**
// 这种解析，测试侧对齐是为了让断言与 git 的返回值同源。
func gitRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if _, stderr, err := gitRunDir(dir, "init", "-q"); err != nil {
		t.Skipf("git 不可用，跳过（工程根推断依赖 git）: %v %s", err, stderr)
	}
	return dir
}

func TestCmdAssetFindsByRelativeAndAbsolutePath(t *testing.T) {
	db := testDB(t)
	dir := gitRepoDir(t)
	abs := filepath.Join(dir, "cmd", "mem", "hook.go")
	relID := addProvenanceMemory(t, db, "hook 注入预算默认 1200 字符", "demo", "cmd/mem/hook.go", 12)
	absID := addProvenanceMemory(t, db, "hook 去重窗口默认 10 秒", "demo", abs, 40)

	// 绝对路径输入 → 投影出相对工程根的字面量，命中相对存法；
	// 同时绝对字面量命中绝对存法。工程根按文件所在目录推断（dir 不是 git 仓库）。
	out, err := captureStdout(t, func() error {
		return cmdAsset([]string{"--db", db, "--project", "demo", abs})
	})
	if err != nil {
		t.Fatalf("cmdAsset: %v", err)
	}
	if !strings.Contains(out, "共 2 条") {
		t.Fatalf("应命中 2 条，got:\n%s", out)
	}
	// 同一文件按行号升序：L12 在 L40 之前。
	if i, j := strings.Index(out, "L12"), strings.Index(out, "L40"); i < 0 || j < 0 || i > j {
		t.Errorf("应按行号升序输出，got:\n%s", out)
	}
	for _, want := range []string{"hook 注入预算默认 1200 字符", "[" + relID[:8] + "]", "[" + absID[:8] + "]"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出应含 %q，got:\n%s", want, out)
		}
	}
}

func TestCmdAssetJSONContract(t *testing.T) {
	db := testDB(t)
	dir := gitRepoDir(t)
	abs := filepath.Join(dir, "deep", "x.go")
	// 超过 80 字符：用来证明 --json 不截断内容。
	long := strings.Repeat("长内容", 40)
	addProvenanceMemory(t, db, long, "demo", "deep/x.go", 7)

	out, err := captureStdout(t, func() error {
		// 选项写在位置参数之后也应生效（reorderFlags）。
		return cmdAsset([]string{"--db", db, "--project", "demo", abs, "--json"})
	})
	if err != nil {
		t.Fatalf("cmdAsset: %v", err)
	}
	var got struct {
		Path  string           `json:"path"`
		Files []string         `json:"files"`
		Count int              `json:"count"`
		Items []store.AssetHit `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("JSON 应可解析: %v\n%s", err, out)
	}
	if got.Path != abs {
		t.Errorf("path 应回显原始输入，got %q", got.Path)
	}
	if got.Count != 1 || len(got.Items) != 1 {
		t.Fatalf("应返回 1 条，got %+v", got)
	}
	if got.Items[0].Content != long {
		t.Errorf("--json 不得截断 content，got %d 字符", len([]rune(got.Items[0].Content)))
	}
	if got.Items[0].Line != 7 || got.Items[0].Project != "demo" {
		t.Errorf("items 字段应完整，got %+v", got.Items[0])
	}
	if len(got.Files) != 2 || got.Files[0] != "deep/x.go" {
		t.Errorf("files 应回显候选字面量且相对路径在前，got %v", got.Files)
	}
}

// 空结果：打印提示、退出码 0（与 mem search 无命中的既有语义一致）。
func TestCmdAssetEmptyResultIsNotAnError(t *testing.T) {
	db := testDB(t)
	dir := t.TempDir()
	out, err := captureStdout(t, func() error {
		return cmdAsset([]string{"--db", db, "--project", "demo", filepath.Join(dir, "none.go")})
	})
	if err != nil {
		t.Fatalf("空结果不应返回错误: %v", err)
	}
	if !strings.Contains(out, "未找到") || !strings.Contains(out, "mem search") {
		t.Errorf("空结果应给出「未找到」与后续建议，got:\n%s", out)
	}
}

func TestCmdAssetRejectsDirectoryInput(t *testing.T) {
	db := testDB(t)
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	// 已存在的目录
	if _, err := captureStdout(t, func() error {
		return cmdAsset([]string{"--db", db, sub})
	}); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Errorf("目录输入应报错并点名「目录」，got %v", err)
	}
	// 带尾部分隔符（即使路径不存在也拒绝，避免误以为做了前缀匹配）
	if _, err := captureStdout(t, func() error {
		return cmdAsset([]string{"--db", db, sub + "/"})
	}); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Errorf("尾分隔符输入应报错，got %v", err)
	}
}

func TestCmdAssetArgumentValidation(t *testing.T) {
	db := testDB(t)
	if _, err := captureStdout(t, func() error {
		return cmdAsset([]string{"--db", db})
	}); err == nil || !strings.Contains(err.Error(), "缺少路径") {
		t.Errorf("缺路径应报错，got %v", err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdAsset([]string{"--db", db, "a.go", "b.go"})
	}); err == nil || !strings.Contains(err.Error(), "只接受一个路径") {
		t.Errorf("多余位置参数应报错，got %v", err)
	}
}

// 刻意不解析 symlink：即使目标真实存在，投影也只用输入的字面拼写。
// 若有人重新引入 EvalSymlinks，这个用例会以「多出 real/... 候选」失败。
func TestNormalizeAssetPathsDoesNotResolveSymlinks(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	linkDir := filepath.Join(base, "link")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("当前文件系统不支持 symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(linkDir, "a.go"), []byte("package a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	abs := filepath.Join(linkDir, "a.go")
	got := normalizeAssetPaths(abs, base)
	want := []string{"link/a.go", filepath.ToSlash(abs)}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("不应解析 symlink，got %v, want %v", got, want)
	}
}

// 无行号的溯源渲染成 L-：打成 L0 会让人以为「第 0 行」。
func TestCmdAssetRendersUnknownLineAsDash(t *testing.T) {
	db := testDB(t)
	dir := t.TempDir()
	abs := filepath.Join(dir, "f.go")
	addProvenanceMemory(t, db, "整份文件级结论", "demo", "f.go", 0)

	out, err := captureStdout(t, func() error {
		return cmdAsset([]string{"--db", db, "--project", "demo", abs})
	})
	if err != nil {
		t.Fatalf("cmdAsset: %v", err)
	}
	if !strings.Contains(out, "L-  fact  整份文件级结论") {
		t.Errorf("无行号应渲染成 L-，got:\n%s", out)
	}
	if strings.Contains(out, "L0") {
		t.Errorf("不应出现 L0，got:\n%s", out)
	}
}

func TestCmdAssetHonoursLimit(t *testing.T) {
	db := testDB(t)
	dir := t.TempDir()
	abs := filepath.Join(dir, "f.go")
	for i := 1; i <= 3; i++ {
		addProvenanceMemory(t, db, "第 "+string(rune('0'+i))+" 条", "demo", "f.go", i)
	}
	out, err := captureStdout(t, func() error {
		return cmdAsset([]string{"--db", db, "--project", "demo", "--limit", "1", abs})
	})
	if err != nil {
		t.Fatalf("cmdAsset: %v", err)
	}
	if !strings.Contains(out, "共 1 条") {
		t.Errorf("--limit 1 应只返回 1 条，got:\n%s", out)
	}
	// 头部仍是 1 条、正文只有一行：用 L 前缀行数反证没有多打。
	if n := strings.Count(out, "\nL"); n != 1 {
		t.Errorf("正文应只有 1 行，got %d 行:\n%s", n, out)
	}
}
