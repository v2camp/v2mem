package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wanghui/v2mem/internal/store"
)

// ---------- WS3: mem sync 跨设备 git 同步 ----------

// gitAvailable 检查 git 是否可用；不可用则跳过（CI 上通常有）。
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("本机无 git，跳过跨设备同步编排测试")
	}
}

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	var b strings.Builder
	cmd.Stdout = &b
	cmd.Stderr = &b
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v 失败: %v\n%s", args, err, b.String())
	}
	return b.String()
}

// runGitOK 允许 git 命令失败，返回输出；用于主动构造「合并冲突后停在冲突态」。
func runGitOK(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var b strings.Builder
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := cmd.Run()
	return b.String(), err
}

func gitIdentity(t *testing.T, dir string) {
	t.Helper()
	runGit(t, "-C", dir, "config", "user.email", "test@v2mem")
	runGit(t, "-C", dir, "config", "user.name", "v2mem-test")
}

func writeFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func bareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "init", "--bare", dir)
	return dir
}

// assertHasFact 校验回到本地库确已拥有 (content, project) 这条事实。
func assertHasFact(t *testing.T, db, content, proj string) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("打开库 %s: %v", db, err)
	}
	defer st.Close()
	recs, err := st.Export()
	if err != nil {
		t.Fatalf("导出 %s: %v", db, err)
	}
	for _, r := range recs {
		if r.Content == content && r.Project == proj {
			return
		}
	}
	t.Fatalf("库 %s 缺少事实 %q/%q，现有:\n", db, content, proj)
	for _, r := range recs {
		t.Logf("  %q/%q", r.Content, r.Project)
	}
}

func assertFactCount(t *testing.T, db string, want int) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("打开库 %s: %v", db, err)
	}
	defer st.Close()
	recs, err := st.Export()
	if err != nil {
		t.Fatalf("导出 %s: %v", db, err)
	}
	if len(recs) != want {
		t.Fatalf("库 %s 应有 %d 条事实，got %d", db, want, len(recs))
	}
}

// 端到端：A 首推 → B 拉并合并 → A 再同步归并 B → 两侧收敛。
func TestCmdSyncPullPushEndToEnd(t *testing.T) {
	gitAvailable(t)
	remote := bareRemote(t)

	// 设备 A：写一条事实并首次 sync（克隆空远端 → 建立分支 + 推送）。
	dbA := filepath.Join(t.TempDir(), "memA.db")
	if err := cmdAdd([]string{"--db", dbA, "--project", "projA", "--kind", "fact", "A端独有事实"}); err != nil {
		t.Fatalf("A add: %v", err)
	}
	wdA := filepath.Join(t.TempDir(), "syncA")
	if err := cmdSync([]string{"--db", dbA, "--workdir", wdA, "--no-backup", remote, "main"}); err != nil {
		t.Fatalf("A 首次 sync: %v", err)
	}
	assertHasFact(t, dbA, "A端独有事实", "projA")

	// 设备 B：全新库 + 自己一条事实，sync 拉 A 的导出并合并。
	dbB := filepath.Join(t.TempDir(), "memB.db")
	if err := cmdAdd([]string{"--db", dbB, "--project", "projB", "--kind", "fact", "B端独有事实"}); err != nil {
		t.Fatalf("B add: %v", err)
	}
	wdB := filepath.Join(t.TempDir(), "syncB")
	if err := cmdSync([]string{"--db", dbB, "--workdir", wdB, "--no-backup", remote, "main"}); err != nil {
		t.Fatalf("B sync: %v", err)
	}
	assertHasFact(t, dbB, "A端独有事实", "projA") // pull 后 A 的事实进了 B
	assertHasFact(t, dbB, "B端独有事实", "projB")

	// 设备 A 再次 sync：拉回 B 的改动，A 也应收敛到 B 的事实。
	if err := cmdSync([]string{"--db", dbA, "--workdir", wdA, "--no-backup", remote, "main"}); err != nil {
		t.Fatalf("A 再次 sync: %v", err)
	}
	assertHasFact(t, dbA, "B端独有事实", "projB")
}

// 冲突：两端各自在 mem.jsonl 上改了同一行 → pull 冲突 → abort 复原、未推送、
// DB 未写入，且写前已有 VACUUM INTO 备份。
func TestCmdSyncConflictAbortsAndBacksUp(t *testing.T) {
	gitAvailable(t)
	remote := bareRemote(t)

	// 用一组手工 git 提交搭出「远端已前进 + 本地另改同段」的发散状态。
	baseDir := filepath.Join(t.TempDir(), "base")
	runGit(t, "clone", remote, baseDir)
	gitIdentity(t, baseDir)
	writeFileAt(t, filepath.Join(baseDir, "mem.jsonl"), "1\n2\n3\n")
	runGit(t, "-C", baseDir, "add", "-A")
	runGit(t, "-C", baseDir, "commit", "-m", "base")
	runGit(t, "-C", baseDir, "push", "-u", "origin", "main") // bare → C1

	// 同步工作区（模拟设备 B 已有的 clone，基于 C1）。
	wd := filepath.Join(t.TempDir(), "sync")
	runGit(t, "clone", remote, wd)
	gitIdentity(t, wd)

	// 远端推进会改 mem.jsonl 第 2 行 → bare → C2(AA)。
	writeFileAt(t, filepath.Join(baseDir, "mem.jsonl"), "1\nAA\n3\n")
	runGit(t, "-C", baseDir, "add", "-A")
	runGit(t, "-C", baseDir, "commit", "-m", "remote-advance")
	runGit(t, "-C", baseDir, "push", "origin", "main")

	// 本地自己的提交也在第 2 行改 → 与远端 C2 在同一处冲突。
	writeFileAt(t, filepath.Join(wd, "mem.jsonl"), "1\nBB\n3\n")
	runGit(t, "-C", wd, "add", "-A")
	runGit(t, "-C", wd, "commit", "-m", "local-edit") // wd HEAD = C3(基于 C1)

	// 本机库只有一条本地事实。
	db := filepath.Join(t.TempDir(), "mem.db")
	if err := cmdAdd([]string{"--db", db, "--project", "projX", "--kind", "fact", "本地事实"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	backupDir := filepath.Join(t.TempDir(), "backs")

	err := cmdSync([]string{"--db", db, "--workdir", wd, "--backup-dir", backupDir, remote, "main"})
	if err == nil {
		t.Fatal("远端与本地都改了同一行，sync 应因 git 冲突而失败")
	}

	// abort 复原：HEAD 回到本地提交 C3，mem.jsonl 恢复成本地版本，未被远端覆盖。
	if got := strings.TrimSpace(runGit(t, "-C", wd, "log", "-1", "--pretty=%s")); got != "local-edit" {
		t.Errorf("abort 后 HEAD 应变回本地提交，got %q", got)
	}
	mj, err := os.ReadFile(filepath.Join(wd, "mem.jsonl"))
	if err != nil {
		t.Fatalf("读 mem.jsonl: %v", err)
	}
	if string(mj) != "1\nBB\n3\n" {
		t.Errorf("abort 后 mem.jsonl 应复原为本地版本，got %q", string(mj))
	}

	// 未推送：远端仍停在 C2(AA)。
	if got := strings.TrimSpace(runGit(t, "-C", wd, "log", "-1", "origin/main", "--pretty=%s")); got != "remote-advance" {
		t.Errorf("失败后不应推送，origin/main 应停在远端提交，got %q", got)
	}

	// DB 未被写入（SyncMerge 未到达），仍是 1 条。
	assertFactCount(t, db, 1)
	assertHasFact(t, db, "本地事实", "projX")

	// 写前已有 VACUUM INTO 备份。
	matches, err := filepath.Glob(filepath.Join(backupDir, "mem-*.db"))
	if err != nil || len(matches) == 0 {
		t.Errorf("写库前应有备份文件，got %v (err=%v)", matches, err)
	}
}

// --abort 单独调用：在「停在冲突态」中显式复原合并。
func TestCmdSyncAbortFlag(t *testing.T) {
	gitAvailable(t)
	remote := bareRemote(t)

	baseDir := filepath.Join(t.TempDir(), "base")
	runGit(t, "clone", remote, baseDir)
	gitIdentity(t, baseDir)
	writeFileAt(t, filepath.Join(baseDir, "mem.jsonl"), "1\n2\n")
	runGit(t, "-C", baseDir, "add", "-A")
	runGit(t, "-C", baseDir, "commit", "-m", "base")
	runGit(t, "-C", baseDir, "push", "-u", "origin", "main")

	// 设备同步工作区克隆到 base；随后远端与本地各自改同一行 → 制造发散。
	wd := filepath.Join(t.TempDir(), "sync")
	runGit(t, "clone", remote, wd)
	gitIdentity(t, wd)

	writeFileAt(t, filepath.Join(baseDir, "mem.jsonl"), "1\nAA\n")
	runGit(t, "-C", baseDir, "add", "-A")
	runGit(t, "-C", baseDir, "commit", "-m", "remote-advance")
	runGit(t, "-C", baseDir, "push", "origin", "main")

	writeFileAt(t, filepath.Join(wd, "mem.jsonl"), "1\nBB\n")
	runGit(t, "-C", wd, "add", "-A")
	runGit(t, "-C", wd, "commit", "-m", "local-edit")

	// 手工触发一次 merge，刻意让其留在「合并冲突中」状态（MERGE_HEAD 在、正文带标记）。
	runGit(t, "-C", wd, "fetch", "origin") // 先拉取远端推进，origin/main 才指向 C2
	if _, err := runGitOK("-C", wd, "merge", "origin/main"); err == nil {
		t.Fatal("前置：合并不应干净成功（应在同一行冲突）")
	}
	if _, err := os.Stat(filepath.Join(wd, ".git", "MERGE_HEAD")); err != nil {
		t.Fatal("前置：应处于合并冲突中（有 MERGE_HEAD）")
	}

	// mem sync --abort 复原到 pull 前：HEAD 回到本地提交、正文标记清除。
	if err := cmdSync([]string{"--abort", "--workdir", wd}); err != nil {
		t.Fatalf("--abort 不应失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wd, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Error("--abort 后不应再有进行中的合并")
	}
	if got := strings.TrimSpace(runGit(t, "-C", wd, "log", "-1", "--pretty=%s")); got != "local-edit" {
		t.Errorf("abort 后 HEAD 应变回本地提交，got %q", got)
	}
	mj, err := os.ReadFile(filepath.Join(wd, "mem.jsonl"))
	if err != nil {
		t.Fatalf("读 mem.jsonl: %v", err)
	}
	if string(mj) != "1\nBB\n" {
		t.Errorf("abort 后 mem.jsonl 应复原为本地版本，got %q", string(mj))
	}

	// 无冲突可中止时 --abort 应幂等成功。
	if err := cmdSync([]string{"--abort", "--workdir", wd}); err != nil {
		t.Fatalf("干净的 --abort 应幂等成功: %v", err)
	}
}