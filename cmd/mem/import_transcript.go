package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/scrub"
	"github.com/v2camp/v2mem/internal/store"
	"github.com/v2camp/v2mem/internal/transcript"
)

// import-transcript 只写 observations，以及（--into both|candidates 时）pending 候选。
// 默认不写 memories。

func cmdImportTranscript(args []string) error {
	var c common
	fs := flag.NewFlagSet("import-transcript", flag.ContinueOnError)
	c.register(fs)
	claudeDir := fs.String("claude-dir", "", "Claude 项目目录（递归读 *.jsonl）")
	cursorDir := fs.String("cursor-dir", "", "Cursor transcript 目录（解析尚未落地，传入则跳过）")
	file := fs.String("file", "", "单个 JSONL 文件，可重复使用时请多次调用")
	project := fs.String("project", "", "工程标记（默认取 transcript 里的目录名）")
	since := fs.Duration("since", 0, "只导入这段时间之内的行，如 720h")
	into := fs.String("into", "obs", "obs|candidates|both")
	dry := fs.Bool("dry-run", false, "只统计，不写库")
	limit := fs.Int("limit", 0, "最多导入多少行，0 表示不限")
	noScrub := fs.Bool("no-scrub", false, "关闭密钥打码（默认打码）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *into {
	case "obs", "candidates", "both":
	default:
		return fmt.Errorf("--into 可选 obs|candidates|both，got %q", *into)
	}
	if strings.TrimSpace(*cursorDir) != "" {
		fmt.Fprintln(os.Stderr, "v2mem import-transcript: Cursor transcript 解析尚未落地，已跳过该目录")
	}
	var files []string
	if p := strings.TrimSpace(*file); p != "" {
		files = append(files, p)
	}
	if dir := strings.TrimSpace(*claudeDir); dir != "" {
		found, err := jsonlFiles(dir)
		if err != nil {
			return err
		}
		files = append(files, found...)
	}
	if len(files) == 0 {
		return errors.New("需要 --file 或 --claude-dir")
	}

	var sinceUnix int64
	if *since > 0 {
		sinceUnix = time.Now().Add(-*since).Unix()
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	var stats importStats
	stats.Sessions = map[string]bool{}
	var fileErrs []error
	for _, path := range files {
		got, err := importClaudeFile(st, path, importOpts{
			Project: *project, Since: sinceUnix, Dry: *dry, Limit: *limit,
			NoScrub: *noScrub, Already: stats.Written,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "v2mem import-transcript: %s: %v（已跳过该文件）\n", path, err)
			// 文件级失败必须影响退出码：整份 transcript 没进来是数据丢失，
			// 不能靠 stderr 一句话就让调用方以为成功。
			fileErrs = append(fileErrs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		stats.add(got)
		if *limit > 0 && stats.Written >= *limit {
			break
		}
		fmt.Fprintf(os.Stderr, "v2mem import-transcript: %s 导入 %d 行\n", path, got.Written)
	}

	candN := 0
	if (*into == "both" || *into == "candidates") && !*dry {
		for sid := range stats.Sessions {
			rep, err := distillOne(st, distillJob{
				Session: sid, Provider: "none", Audit: "-", DB: c.db,
				Model: "rules", Timeout: time.Second,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "v2mem import-transcript: 会话 %s 规则蒸馏失败，已放过: %v\n", sid, err)
				continue
			}
			candN += rep.Inserted
		}
	}
	if c.json {
		if err := printJSON(map[string]any{
			// observations 保留旧字段名（= written），新字段把四类结局拆开。
			"observations": stats.Written,
			"written":      stats.Written,
			"duplicate":    stats.Duplicate,
			"empty":        stats.Empty,
			"filtered":     stats.Filtered,
			"failed":       stats.Failed,
			"skipped":      stats.Duplicate + stats.Empty + stats.Filtered,
			"candidates":   candN,
			"files_failed": len(fileErrs),
			"into":         *into,
			"dry_run":      *dry,
		}); err != nil {
			return err
		}
	} else {
		fmt.Printf("观测 %d 行，重复 %d，空行 %d，超出时间窗 %d，写失败 %d，候选 %d（未写 memories）\n",
			stats.Written, stats.Duplicate, stats.Empty, stats.Filtered, stats.Failed, candN)
	}
	return errors.Join(fileErrs...)
}

// importStats 是导入的账目。
//
// 为什么必须拆开：旧实现只有一个 skipped 计数器，把「重复」「空内容」「since 过滤」
// 「写库失败」全塞在一起。于是「写库失败 100 条」和「100 条重复」在输出上完全一样，
// 数据丢失被伪装成正常的幂等跳过。
type importStats struct {
	Written   int // 真正新建的观测行数（dry-run 时为「本应写入」）
	Duplicate int // 内容已存在，命中幂等去重
	Empty     int // 正文为空
	Filtered  int // 早于 --since
	Failed    int // 写库失败（真实的错误，不是跳过）
	Sessions  map[string]bool
}

func (s *importStats) add(o importStats) {
	s.Written += o.Written
	s.Duplicate += o.Duplicate
	s.Empty += o.Empty
	s.Filtered += o.Filtered
	s.Failed += o.Failed
	for sid := range o.Sessions {
		s.Sessions[sid] = true
	}
}

type importOpts struct {
	Project string
	Since   int64
	Dry     bool
	Limit   int
	NoScrub bool
	Already int
}

func importClaudeFile(st *store.Store, path string, opt importOpts) (importStats, error) {
	stats := importStats{Sessions: map[string]bool{}}
	f, err := os.Open(path)
	if err != nil {
		return stats, err
	}
	defer f.Close()
	lines, err := transcript.ParseClaudeJSONL(f)
	if err != nil && !errors.Is(err, io.EOF) {
		return stats, err
	}
	for _, line := range lines {
		if opt.Limit > 0 && opt.Already+stats.Written >= opt.Limit {
			break
		}
		if opt.Since > 0 && line.At > 0 && line.At < opt.Since {
			stats.Filtered++
			continue
		}
		text := strings.TrimSpace(line.Text)
		if text == "" {
			stats.Empty++
			continue
		}
		text = scrub.Apply(text, opt.NoScrub)
		text = truncateRunes(text, observeSummaryMaxRunes)
		project := opt.Project
		if project == "" {
			project = line.Project
		}
		if opt.Dry {
			stats.Written++
			stats.Sessions[line.SessionID] = true
			continue
		}
		res, err := st.AppendObservation(store.ObsInput{
			SessionID: line.SessionID, Project: project, Harness: "claude",
			Event: "import-transcript", ToolName: line.Role, Summary: text, RawRef: path,
			Channel: store.ChannelImport,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "v2mem import-transcript: 写观测失败，已跳过该行: %v\n", err)
			// 写失败单独计数：这不是「跳过」，是丢数据。
			stats.Failed++
			continue
		}
		// 会话标记必须放在写库成功之后。原来放在写之前，写失败的行也会把会话
		// 推进到规则蒸馏，蒸馏读到的是不完整的历史，候选与观测对不上。
		stats.Sessions[line.SessionID] = true
		if res.Created {
			stats.Written++
		} else {
			stats.Duplicate++
		}
	}
	return stats, nil
}

func jsonlFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".jsonl") {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}
