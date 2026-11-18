package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/v2camp/v2mem/internal/store"
)

// cmdAsset 按源文件反查关联记忆：回答「关于这个文件，我记住过什么」。
//
// 为什么需要它：provenance 当初就是为资产联动存的（见 store.Provenance 注释与
// docs/design-provenance.md），但一直没有反查入口。语义检索对「这个文件」这种
// 结构化条件无能为力——改文件前想知道相关结论/坑，按文件路径精确查比按关键词猜
// 要可靠得多。
//
// 范围限制（刻意）：不支持目录输入。目录前缀匹配本质上是隐藏的全表 LIKE，
// 既慢又容易召回一堆弱相关内容；「这个目录下有哪些记忆」是 mem ls --project 的活。
func cmdAsset(args []string) error {
	var c common
	fs := flag.NewFlagSet("asset", flag.ContinueOnError)
	c.register(fs)
	project := fs.String("project", "", "限定工程（默认按路径所在 git 仓库推断）")
	limit := fs.Int("limit", 50, "返回条数（默认 50，最多 200）")
	// 与 show/feedback 一致：允许 `mem asset <path> --json`，选项写在位置参数之后。
	flags, pos := reorderFlags(args, map[string]bool{"--db": true, "--project": true, "--limit": true})
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(pos) == 0 {
		fmt.Fprint(os.Stderr, assetUsage)
		return errors.New("缺少路径，用法: mem asset <path>")
	}
	if len(pos) > 1 {
		fmt.Fprint(os.Stderr, assetUsage)
		return fmt.Errorf("只接受一个路径，收到 %d 个: %s", len(pos), strings.Join(pos, " "))
	}
	input := strings.TrimSpace(pos[0])
	if input == "" {
		fmt.Fprint(os.Stderr, assetUsage)
		return errors.New("路径为空")
	}

	// 展开 ~ 后取绝对路径：库里存的是相对工程根的路径，绝对路径只用于
	// 求相对投影与命中历史绝对路径数据。
	abs, err := assetAbsPath(input)
	if err != nil {
		return err
	}
	if fi, statErr := os.Stat(abs); statErr == nil && fi.IsDir() {
		return fmt.Errorf("%s 是目录：mem asset 只按单个文件反查。看目录下的记忆用 mem ls --project，语义检索用 mem search", input)
	}

	root, proj := assetScope(abs, *project)
	files := normalizeAssetPaths(input, root)

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	hits, err := st.AssetsByFile(files, proj, *limit)
	if err != nil {
		return err
	}

	if c.json {
		return printJSON(assetJSON{Path: input, Files: files, Count: len(hits), Items: hits})
	}
	if len(hits) == 0 {
		fmt.Printf("未找到与 %s 关联的记忆\n", input)
		fmt.Printf("（可试 mem search \"%s\" 做语义检索，或确认是否在 %s 工程下写入）\n",
			filepath.Base(input), proj)
		return nil
	}
	fmt.Printf("共 %d 条（%s）\n", len(hits), strings.Join(files, ", "))
	for _, h := range hits {
		fmt.Printf("%s  %s  %s  [%s]\n",
			assetLineTag(h.Line), h.Kind, truncateRunes(oneLine(h.Content), 80), shortID(h.ID))
	}
	return nil
}

// assetJSON 是 --json 的稳定契约：files 回显最终匹配用的字面量，
// 排查「为什么没查到」时第一眼看的就是它。
type assetJSON struct {
	Path  string           `json:"path"`
	Files []string         `json:"files"`
	Count int              `json:"count"`
	Items []store.AssetHit `json:"items"`
}

// assetUsage 只在本命令的参数错误时打到 stderr：主 usage 太长，
// 缺参数的人需要的是「这一条怎么用」，不是整份帮助。
const assetUsage = `用法: mem asset <path> [--db ...] [--json] [--project P] [--limit N]

按源文件反查关联记忆（provenance.file）。path 可写相对路径、绝对路径或 ~/ 开头。
仅支持单个文件；目录请用 mem ls --project 或 mem search。
`

// assetLineTag 把行号渲染成输出前缀。0 表示 provenance 没记行号，
// 打成 L0 会让人误以为「第 0 行」，故用 L- 表示未知。
func assetLineTag(line int) string {
	if line <= 0 {
		return "L-"
	}
	return fmt.Sprintf("L%d", line)
}

// assetAbsPath 展开 ~、求绝对路径并做目录形式的前置检查。
func assetAbsPath(input string) (string, error) {
	if strings.HasSuffix(input, "/") || strings.HasSuffix(input, `\`) {
		return "", fmt.Errorf("%s 以目录分隔符结尾：mem asset 只按单个文件反查，目录请用 mem ls --project", input)
	}
	home, _ := os.UserHomeDir()
	expanded := expandTilde(input, home)
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded), nil
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}
	return filepath.Clean(abs), nil
}

// expandTilde 只展开开头的 ~ 与 ~/。
//
// 不做完整 shell 展开：路径通常是带引号从命令行传进来的，~ 原样到达程序，
// 而 `~user/...` 这类写法在本地多用户场景几乎没有意义。支持不了的就原样返回，
// 让它按相对路径继续走——不会静默换到别的文件上。
func expandTilde(p, home string) string {
	if home == "" || p == "" || p[0] != '~' {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// assetScope 决定「工程根」与「工程名」。
//
// 工程名沿用 CLI 侧既有口径（detectProject 取含 .git 的目录名），只是起点从
// 「进程 cwd」换成「源文件所在目录」——查别的工程的文件时，用 cwd 的工程名会把
// 结果滤空。根用 `git rev-parse --show-toplevel` 而不是向上找 .git 目录：
// worktree / submodule 的 .git 是文件而非目录，向上找目录会漏（本仓库自身就是 worktree）。
//
// 兜底顺序：git 根 → （文件在 cwd 之下时）cwd → 文件所在目录。
// 中间那步是为了「机器没装 git 但 cwd 就是仓库根」这种常见情形仍能算出
// 正确的相对路径，否则相对路径会退化成文件名本身而匹配不到。
//
// 这一层是**唯一**允许碰文件系统的地方（要判断哪个目录存在、git 是否可用）；
// 拿到 root 之后，路径投影就是纯字符串运算。注意 git 返回的是 realpath，
// 输入可能带未解析的 symlink 前缀——我们**不**去对齐二者（见 normalizeAssetPaths），
// 代价是这类工作区下相对候选可能算不出来，绝对路径候选仍然可用。
func assetScope(absPath, explicitProject string) (root, project string) {
	// 起点是「离目标文件最近的已存在目录」：`mem asset new/file.go` 这种
	// 文件还没创建（或刚被删）的场景很常见，直接拿不存在的目录去跑 git 会失败，
	// 从而丢掉整条相对路径投影——而那恰恰是最需要命中的形态。
	dir := nearestExistingDir(filepath.Dir(absPath))
	if dir == "" {
		dir = filepath.Dir(absPath)
	}
	if root = gitRootAt(dir); root == "" {
		if wd, err := os.Getwd(); err == nil && isUnder(wd, absPath) {
			root = wd
		} else {
			root = dir
		}
	}
	project = explicitProject
	if project == "" {
		project = filepath.Base(root)
	}
	return root, project
}

// nearestExistingDir 向上找到第一个存在的目录；到根都没有则返回 ""。
func nearestExistingDir(dir string) string {
	for i := 0; i < 64; i++ {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// gitRootAt 返回 dir 所在 git 仓库的根；不是仓库或没装 git 时返回 ""。
// 复用 sync.go 的 gitRunDir，避免第二套 exec 封装。
func gitRootAt(dir string) string {
	out, _, err := gitRunDir(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(out)
	if root == "" {
		return ""
	}
	return filepath.Clean(root)
}

// isUnder 判断 path 是否落在 dir 之内（含 dir 本身）。
func isUnder(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// normalizeAssetPaths 把用户输入投影成库里可能存过的字面量集合。
//
// 这是本命令最容易出错的地方：库里存的是**相对工程根**的路径（跨设备才有意义，
// 见 Provenance.File 注释），而用户手里通常是绝对路径或相对 cwd 的路径。
// 于是投影出三个候选：
//  1. 相对工程根（斜杠分隔）—— 今天写入的主流形态；
//  2. 输入本身（Clean 后）—— 覆盖「cwd 正好是工程根」的写法；
//  3. 绝对路径 —— 覆盖历史/外部工具写入的绝对路径数据。
//
// 全部转成斜杠分隔：Windows 下 `\` 与 `/` 会分裂成两份互不相交的数据。
// 去重后交给 store，命中的是哪个字面量由输出里的 files 回显。
//
// **刻意不解析符号链接**：投影链路（本函数与 assetPathCandidates/assetAbsOf）只做
// 字符串运算，不调 filepath.EvalSymlinks，也不 stat 目标文件。取舍：
//   - 解析会把路径移出工程根——工程本身经 symlink 暴露时，「相对工程根」这个
//     唯一真正有用的候选会直接失效；
//   - 解析结果依赖本机文件系统布局（不存在的路径解析失败、autofs 的 /home 指向
//     /System/Volumes/Data 之类），投影就不再是纯函数，也无法稳定测试；
//   - 代价是「库里的路径经过 symlink、用户给的是另一条等价路径」时查不到。
//     这是可接受的：溯源存的就是写入当时的字面量，投影只为对齐常见写法。
func normalizeAssetPaths(input, root string) []string {
	home, _ := os.UserHomeDir()
	wd, _ := os.Getwd()
	return assetPathCandidates(input, root, wd, home)
}

// assetPathCandidates 是 normalizeAssetPaths 的纯函数形态：cwd 与 home 显式传入，
// 让路径投影的三种输入形态都能被直接断言，不必依赖进程状态。
func assetPathCandidates(input, root, cwd, home string) []string {
	expanded := expandTilde(strings.TrimSpace(input), home)
	cleaned := filepath.Clean(expanded)
	abs := assetAbsOf(input, home, cwd)

	var out []string
	add := func(s string) {
		s = filepath.ToSlash(s)
		if s == "" || s == "." {
			return
		}
		for _, x := range out {
			if x == s {
				return
			}
		}
		out = append(out, s)
	}
	// 1. 相对工程根。越出工程根（../…）说明文件不在该工程里，投影出来只会误命中
	// 同名的无关文件，直接丢弃。
	if rel, ok := assetRel(root, abs); ok {
		add(rel)
	}
	add(cleaned)
	add(abs)
	return out
}

// assetRel 返回 abs 相对 root 的路径字面量；abs 不在 root 之下时 ok=false。
func assetRel(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return rel, true
}

// assetAbsOf 把输入变成绝对路径（~ 展开 + 相对 cwd 合并 + Clean）。
// 只做字符串运算，不碰文件系统，见 normalizeAssetPaths 的取舍说明。
func assetAbsOf(input, home, cwd string) string {
	cleaned := filepath.Clean(expandTilde(strings.TrimSpace(input), home))
	if filepath.IsAbs(cleaned) {
		return cleaned
	}
	if cwd == "" {
		return cleaned
	}
	return filepath.Clean(filepath.Join(cwd, cleaned))
}
