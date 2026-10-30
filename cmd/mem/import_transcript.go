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

	imported := 0
	skipped := 0
	sessions := map[string]bool{}
	for _, path := range files {
		n, sk, sess, err := importClaudeFile(st, path, importOpts{
			Project: *project, Since: sinceUnix, Dry: *dry, Limit: *limit,
			NoScrub: *noScrub, Already: imported,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "v2mem import-transcript: %s: %v（已跳过该文件）\n", path, err)
			continue
		}
		imported += n
		skipped += sk
		for s := range sess {
			sessions[s] = true
		}
		if *limit > 0 && imported >= *limit {
			break
		}
		fmt.Fprintf(os.Stderr, "v2mem import-transcript: %s 导入 %d 行\n", path, n)
	}

	candN := 0
	if (*into == "both" || *into == "candidates") && !*dry {
		for sid := range sessions {
			_, n, err := distillOne(st, distillJob{
				Session: sid, Provider: "none", Audit: "-", DB: c.db,
				Model: "rules", Timeout: time.Second,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "v2mem import-transcript: 会话 %s 规则蒸馏失败，已放过: %v\n", sid, err)
				continue
			}
			candN += n
		}
	}
	if c.json {
		return printJSON(map[string]any{
			"observations": imported, "skipped": skipped, "candidates": candN, "into": *into, "dry_run": *dry,
		})
	}
	fmt.Printf("观测 %d 行，跳过 %d，候选 %d（未写 memories）\n", imported, skipped, candN)
	return nil
}

type importOpts struct {
	Project string
	Since   int64
	Dry     bool
	Limit   int
	NoScrub bool
	Already int
}

func importClaudeFile(st *store.Store, path string, opt importOpts) (imported, skipped int, sessions map[string]bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, nil, err
	}
	defer f.Close()
	lines, err := transcript.ParseClaudeJSONL(f)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, nil, err
	}
	sessions = map[string]bool{}
	for _, line := range lines {
		if opt.Limit > 0 && opt.Already+imported >= opt.Limit {
			break
		}
		if opt.Since > 0 && line.At > 0 && line.At < opt.Since {
			skipped++
			continue
		}
		text := strings.TrimSpace(line.Text)
		if text == "" {
			skipped++
			continue
		}
		if !opt.NoScrub {
			text = scrubSecrets(text)
		}
		text = truncateRunes(text, observeSummaryMaxRunes)
		project := opt.Project
		if project == "" {
			project = line.Project
		}
		sessions[line.SessionID] = true
		if opt.Dry {
			imported++
			continue
		}
		res, err := st.AppendObservation(store.ObsInput{
			SessionID: line.SessionID, Project: project, Harness: "claude",
			Event: "import-transcript", ToolName: line.Role, Summary: text, RawRef: path,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "v2mem import-transcript: 写观测失败，已跳过该行: %v\n", err)
			skipped++
			continue
		}
		if res.Created {
			imported++
		} else {
			skipped++
		}
	}
	return imported, skipped, sessions, nil
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
