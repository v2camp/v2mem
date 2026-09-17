package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wanghui/v2mem/internal/audit"
	"github.com/wanghui/v2mem/internal/store"
)

// 跨设备同步：`mem sync <remote>`。
//
// 编排链路（git pull → 三方合并 → push）：
//
//	1. 确保本地同步工作区 <workdir> 是 <remote> 的克隆（首次数 clone，之后复用）
//	2. 写本机库前先 VACUUM INTO 备份（沿用 gc/consolidate 的 backupBeforeMutating）
//	3. git pull：把远端历史与最近导出拉下来。若 git 层在 mem.jsonl 上冲突
//	   （两端都改了同一段导出行）→ 不写库、不推送，git merge --abort 复原到 pull 前。
//	4. 读远端导出，用 store.SyncMerge 做三方合并回本地库
//	5. 重新导出（现在含远端数据）写回 mem.jsonl，连同 audit 一起 commit
//	6. push。失败则停在合并后状态、不推送、交由下次或 --abort 处理。
//
// 只把记忆数据（mem.jsonl 与 audit.jsonl）入仓，不带 hooks/配置等杂项。
// 本机库始终只写本机那份 db，git 只在拉/推的瞬间接触导出副本——绝不同库并发两写。

// syncSub branches in on the subcommand dispatch (registered in main.go).
// 说明："sync" 已加入 knownSubcommands，main() 分发到 cmdSync。

func cmdSync(args []string) error {
	var c common
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	c.register(fs)
	workdir := fs.String("workdir", "", "同步工作区目录（默认 ~/.v2mem/sync）")
	branch := fs.String("branch", "main", "远端分支名")
	noBackup := fs.Bool("no-backup", false, "跳过写库前备份")
	backupDir := fs.String("backup-dir", "", "备份目录（默认 <库目录>/backup）")
	abort := fs.Bool("abort", false, "中止进行中的冲突合并并复原到 pull 前（不做同步）")
	noPush := fs.Bool("no-push", false, "只合并不推送（调试/测试用）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()

	if *abort {
		rem := ""
		if len(pos) > 0 {
			rem = pos[0]
		}
		return cmdSyncAbort(c.db, *workdir, rem, c.json)
	}
	if len(pos) == 0 {
		return errors.New("缺少远端仓库，用法: mem sync <remote-url> [分支]  (-workdir 指定同步工作区)")
	}
	remote := pos[0]
	if len(pos) > 1 {
		*branch = pos[1]
	}

	wd := *workdir
	if wd == "" {
		wd = defaultSyncWorkdir()
	}
	if err := syncEnsureRepo(wd, remote); err != nil {
		return err
	}

	// 写前备份：VACUUM INTO 库快照（本命令唯一的破坏性动作是向本地库归并远端记录）。
	bak, err := backupBeforeMutating(c.db, *noBackup, *backupDir)
	if err != nil {
		return err
	}
	if bak != "" && !c.json {
		fmt.Printf("已备份 → %s\n", bak)
	}

	// git pull：冲突则 abort 复原、不写库不推送。
	noRemote, conflict, err := syncGitPull(wd, *branch)
	if err != nil {
		return err
	}
	if conflict {
		_ = syncGitAbort(wd)
		return fmt.Errorf("git 拉取在记忆导出上冲突，已中止并复原到 pull 前（未写库、未推送）；请先在 %s 手工化解 mem.jsonl 冲突后重试", wd)
	}

	// 读远端导出作为合输入。首次（远端还没有分支/导出）时为空。
	var remoteRecs []store.ExportRecord
	if !noRemote {
		remoteRecs, err = readSyncJSONL(filepath.Join(wd, "mem.jsonl"))
		if err != nil {
			return err
		}
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	stats, err := st.SyncMerge(remoteRecs)
	if err != nil {
		return err
	}

	// 重新导出（现已含远端数据）写回工作区，作为下次同步的可复现快照。
	merged, err := st.Export()
	if err != nil {
		return err
	}
	if err := writeSyncJSONL(filepath.Join(wd, "mem.jsonl"), merged); err != nil {
		return err
	}
	// audit 随同步一并入仓（按行去重并集），只挪记忆数据不带杂项。
	syncMergeAudit(wd, audit.Path())

	if err := syncGitCommitAll(wd, "sync: 归并记忆快照"); err != nil {
		return err
	}
	if !*noPush {
		if err := syncGitPush(wd, *branch); err != nil {
			return err
		}
	}

	if c.json {
		out := map[string]any{
			"remote": remote, "branch": *branch,
			"inserted": stats.Inserted, "merged": stats.Merged,
			"newer_local": stats.NewerLocal,
		}
		if len(stats.Conflicts) > 0 {
			out["conflicts"] = stats.Conflicts
		}
		if bak != "" {
			out["backup"] = bak
		}
		return printJSON(out)
	}

	fmt.Printf("同步 %s/%s：新增=%d 并入=%d 本地更新=%d\n",
		remote, *branch, stats.Inserted, stats.Merged, stats.NewerLocal)
	if len(stats.Conflicts) > 0 {
		fmt.Printf("  冲突 %d 条已并存并打 sync 冲突标记，需人工收敛（mem ls --tag sync=conflict:…）\n", len(stats.Conflicts))
	}
	if bak != "" {
		fmt.Printf("  已备份 → %s\n", bak)
	}
	return nil
}

// cmdSyncAbort 复原同步工作区里中断的合并，不做任何同步动作。
func cmdSyncAbort(db, workdir, remote string, jsonOut bool) error {
	wd := workdir
	if wd == "" {
		wd = defaultSyncWorkdir()
	}
	if !dirIsGitRepo(wd) {
		return fmt.Errorf("同步工作区 %s 不是 git 仓库，无合并可中止", wd)
	}
	_ = remote
	if err := syncGitAbort(wd); err != nil {
		return err
	}
	if jsonOut {
		return printJSON(map[string]string{"aborted": wd})
	}
	fmt.Printf("已中止冲突合并并复原 %s\n", wd)
	return nil
}

// ---------- git 编排辅助 ----------

// defaultSyncWorkdir 返回默认同步工作区：~/.v2mem/sync。
// 与库同父目录，便于统一备份/清理；数据与代码目录分离。
func defaultSyncWorkdir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".v2mem", "sync")
	}
	return filepath.Join(home, ".v2mem", "sync")
}

func gitRunDir(dir string, args ...string) (string, string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var outB, errB strings.Builder
	cmd.Stdout = &outB
	cmd.Stderr = &errB
	err := cmd.Run()
	return outB.String(), errB.String(), err
}

func dirIsGitRepo(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	return true
}

// syncEnsureRepo 确保工作区是 remote 的克隆，并把本地 git 身份钉在仓库上
// （测试环境 HOME 无全局配置，不设则 commit 必失败）。
func syncEnsureRepo(wd, remote string) error {
	if dirIsGitRepo(wd) {
		if _, _, err := gitRunDir(wd, "remote", "set-url", "origin", remote); err != nil {
			return fmt.Errorf("设置 origin 失败: %w", err)
		}
	} else {
		if fi, err := os.Stat(wd); err == nil && fi.IsDir() && !isEmptyDir(wd) {
			return fmt.Errorf("同步工作区 %s 已存在且非空但非 git 仓库——为避免误删请换个 --workdir", wd)
		}
		if err := os.MkdirAll(filepath.Dir(wd), 0o755); err != nil {
			return err
		}
		if _, errB, err := gitRunDir(filepath.Dir(wd), "clone", remote, wd); err != nil {
			return fmt.Errorf("克隆远端 %s 失败: %s", remote, strings.TrimSpace(errB))
		}
	}
	_, _, _ = gitRunDir(wd, "config", "user.email", "sync@v2mem.local")
	_, _, _ = gitRunDir(wd, "config", "user.name", "v2mem-sync")
	return nil
}

func isEmptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	return len(entries) == 0
}

// hasRemoteBranch 判断远端是否已经有该分支（还没有则首次 pull 无可拉取）。
func hasRemoteBranch(wd, branch string) bool {
	out, _, err := gitRunDir(wd, "rev-parse", "--verify", "refs/remotes/origin/"+branch)
	return err == nil && strings.TrimSpace(out) != ""
}

// syncGitPull 拉取并合并远端分支。返回：
//
//	noRemote — 远端还没有该分支（首次），调用方应视作无远端数据
//	conflict — git 层在导出文件上冲突，需要 abort
func syncGitPull(wd, branch string) (noRemote, conflict bool, err error) {
	if !hasRemoteBranch(wd, branch) {
		return true, false, nil
	}
	out, errB, err := gitRunDir(wd, "pull", "origin", branch, "--no-rebase", "--allow-unrelated-histories")
	if err != nil {
		// git 的合并告警可能落 stdout 也可能落 stderr（版本相关），两处都查。
		s := out + errB
		if strings.Contains(s, "CONFLICT") || strings.Contains(s, "Automatic merge failed") ||
			strings.Contains(s, "Merge conflict") || strings.Contains(s, "unmerged files") {
			return false, true, nil
		}
		return false, false, fmt.Errorf("git pull 失败: %s", strings.TrimSpace(s))
	}
	return false, false, nil
}

// syncGitAbort 复原进行中的合并到 pull 前状态；无合并进行时是幂等成功。
func syncGitAbort(wd string) error {
	_, errB, err := gitRunDir(wd, "merge", "--abort")
	if err != nil {
		if strings.Contains(errB, "no merge to abort") {
			return nil // 已复原/本来就没有进行中的合并
		}
		return fmt.Errorf("中止合并失败: %s", strings.TrimSpace(errB))
	}
	return nil
}

// syncGitCommitAll 提交工作区全部改动；无改动则静默成功。
func syncGitCommitAll(wd, msg string) error {
	if _, _, err := gitRunDir(wd, "add", "-A"); err != nil {
		return err
	}
	_, errB, err := gitRunDir(wd, "commit", "-m", msg)
	if err != nil {
		if strings.Contains(errB, "nothing to commit") {
			return nil
		}
		return fmt.Errorf("提交失败: %s", strings.TrimSpace(errB))
	}
	return nil
}

// syncGitPush 推送本地分支；已是最新则静默成功。
func syncGitPush(wd, branch string) error {
	_, errB, err := gitRunDir(wd, "push", "-u", "origin", branch)
	if err != nil {
		if strings.Contains(errB, "up-to-date") {
			return nil
		}
		return fmt.Errorf("推送失败: %s", strings.TrimSpace(errB))
	}
	return nil
}

// ---------- 导出读写 ----------

// readSyncJSONL 读 JSONL 导出为记录集（不存在则视为空）。
func readSyncJSONL(path string) ([]store.ExportRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return decodeRecords(f)
}

func decodeRecords(r io.Reader) ([]store.ExportRecord, error) {
	dec := json.NewDecoder(r)
	var recs []store.ExportRecord
	for {
		var rec store.ExportRecord
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("解析同步导出失败: %w", err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

func writeSyncJSONL(path string, recs []store.ExportRecord) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return f.Sync()
}

// syncMergeAudit 把本地审计日志按行去重并入同步工作区的 audit.jsonl（尽力而为）。
func syncMergeAudit(wd, localPath string) {
	dest := filepath.Join(wd, "audit.jsonl")
	seen := map[string]bool{}
	if f, err := os.Open(dest); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				seen[line] = true
			}
		}
		_ = f.Close()
	}
	if f, err := os.Open(localPath); err == nil {
		defer f.Close()
		var sb strings.Builder
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || seen[line] {
				continue
			}
			seen[line] = true
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		if sb.Len() > 0 {
			f, err2 := os.OpenFile(dest, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err2 == nil {
				_, _ = f.WriteString(sb.String())
				_ = f.Close()
			}
		}
	}
}