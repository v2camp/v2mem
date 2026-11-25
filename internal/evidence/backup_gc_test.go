package evidence

// 破坏性 GC 前的文件级备份（design-data-lifecycle §8.1 顺序铁律第 3 条）：
// 删除归档段之前，先把它移进 <备份根>/evidence-gc-<时间戳>/ 并写清单。
// 备份失败 → 一个文件都不删；账本只在全部文件落位后才删。

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedOrphans 造 n 段归档，再把观测与游标都推进到「已发窗且无观测」的库况：
// 这 n 段全部成为待回收的孤儿。
func seedOrphans(t *testing.T, e *Engine, db string, uids ...string) {
	t.Helper()
	for _, uid := range uids {
		if _, err := e.Capture(rawEvent(uid)); err != nil {
			t.Fatalf("Capture(%s): %v", uid, err)
		}
	}
	gcExecSQL(t, db, `UPDATE evidence_sources SET cursor_seq = last_seq`)
	gcExecSQL(t, db, `DELETE FROM observations`)
}

func ledgerRows(t *testing.T, db string) int {
	t.Helper()
	c, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer c.Close()
	var n int
	if err := c.QueryRow(`SELECT COUNT(*) FROM evidence_archives`).Scan(&n); err != nil {
		t.Fatalf("数账本行: %v", err)
	}
	return n
}

func archiveFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ent := range entries {
		if ent.IsDir() || !isArchiveFileName(ent.Name()) {
			continue
		}
		out = append(out, filepath.Join(dir, ent.Name()))
	}
	return out
}

// backupRoot 下应恰好有一个 evidence-gc-* 目录。
func onlyBackupDir(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读备份根: %v", err)
	}
	var dirs []string
	for _, ent := range entries {
		if ent.IsDir() {
			dirs = append(dirs, filepath.Join(root, ent.Name()))
		}
	}
	if len(dirs) != 1 {
		t.Fatalf("备份根下应有 1 个 evidence-gc 目录，got %v", dirs)
	}
	return dirs[0]
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// C1：2 段孤儿 + 1 段保留。孤儿段以「文件名 = 内容 sha256」落进备份目录，
// 清单记全量；归档目录只剩保留段，账本只剩 1 行。
func TestGCEvidenceBackupMovesFilesAndManifest(t *testing.T) {
	e, db := newTestEngine(t)
	for _, uid := range []string{"u1", "u2", "u3"} {
		if _, err := e.Capture(rawEvent(uid)); err != nil {
			t.Fatalf("Capture(%s): %v", uid, err)
		}
	}
	if n := archiveFileCount(t, e.ArchiveDir()); n != 3 {
		t.Fatalf("初始归档文件 = %d，期望 3", n)
	}
	// 先推到已发窗，再只留一条观测：GCObservations 保最新一条，另两条的归档成为孤儿。
	gcExecSQL(t, db, `UPDATE evidence_sources SET cursor_seq = last_seq`)
	if _, err := e.st.GCObservations(0, 1); err != nil {
		t.Fatalf("GCObservations: %v", err)
	}

	backupRoot := filepath.Join(t.TempDir(), "backup")
	res, err := e.GCEvidenceBackup(time.Now().Unix()+60, backupRoot)
	if err != nil {
		t.Fatalf("GCEvidenceBackup: %v", err)
	}
	if res.Segments != 2 || res.BackedUp != 2 {
		t.Fatalf("回收/备份段数 = %d/%d，期望 2/2: %+v", res.Segments, res.BackedUp, res)
	}
	if res.Bytes <= 0 {
		t.Fatalf("移出字节应为正，got %d", res.Bytes)
	}
	if res.BackupDir == "" {
		t.Fatal("应有备份目录路径")
	}

	dir := onlyBackupDir(t, backupRoot)
	if dir != res.BackupDir {
		t.Fatalf("结果里的备份目录 = %q，实际 %q", res.BackupDir, dir)
	}
	backed := archiveFiles(t, dir)
	if len(backed) != 2 {
		t.Fatalf("备份目录应有 2 个归档段，got %d: %v", len(backed), backed)
	}
	var wantBytes int64
	for _, p := range backed {
		// 文件名就是 archive_hash：内容 sha256 必须与之一致，否则「备份了但读不回」。
		if got, want := fileSHA256(t, p), filepath.Base(p[:len(p)-len(archiveFileSuffix)]); got != want {
			t.Errorf("%s 的 sha256 = %s，与文件名不符", filepath.Base(p), got)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		wantBytes += fi.Size()
	}
	if wantBytes != res.Bytes {
		t.Errorf("清单前字节合计 = %d，结果报 %d", wantBytes, res.Bytes)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("应有 manifest.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest 不是合法 JSON: %v", err)
	}
	if got := int(m["total_files"].(float64)); got != 2 {
		t.Errorf("manifest total_files = %d，期望 2", got)
	}
	if got := int64(m["total_bytes"].(float64)); got != res.Bytes {
		t.Errorf("manifest total_bytes = %d，期望 %d", got, res.Bytes)
	}
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("manifest items = %d 条，期望 2", len(items))
	}

	if n := archiveFileCount(t, e.ArchiveDir()); n != 1 {
		t.Fatalf("归档目录应只剩 1 段，got %d", n)
	}
	if n := ledgerRows(t, db); n != 1 {
		t.Fatalf("账本应只剩 1 行，got %d", n)
	}
}

// C2：备份根不可建（普通文件下的子目录）→ 报错且一个文件都不删、账本不动。
func TestGCEvidenceBackupFailureDeletesNothing(t *testing.T) {
	e, db := newTestEngine(t)
	seedOrphans(t, e, db, "u1", "u2", "u3")
	// 无需保留集：全部三段都是孤儿（seedOrphans 已清观测）。

	blocker := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(blocker, []byte("不是目录"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.GCEvidenceBackup(time.Now().Unix()+60, filepath.Join(blocker, "sub"))
	if err == nil {
		t.Fatal("备份目录建不出来时必须报错")
	}
	if res.Segments != 0 || res.BackedUp != 0 || res.Bytes != 0 || res.Untracked != 0 || res.TempFiles != 0 {
		t.Fatalf("失败时计数必须为 0: %+v", res)
	}
	if n := archiveFileCount(t, e.ArchiveDir()); n != 3 {
		t.Fatalf("失败时归档文件必须原样 3 个，got %d", n)
	}
	if n := ledgerRows(t, db); n != 3 {
		t.Fatalf("失败时账本必须原样 3 行，got %d", n)
	}
}

// C3：备份根指进归档目录 → 拒绝。否则下一轮扫目录会把备份目录里的段当成
// 账本外残段清掉，备份等于没做。
func TestGCEvidenceBackupRejectsDirInsideArchive(t *testing.T) {
	e, db := newTestEngine(t)
	seedOrphans(t, e, db, "u1")

	if _, err := e.GCEvidenceBackup(time.Now().Unix()+60, e.ArchiveDir()); err == nil {
		t.Fatal("备份根等于归档目录时必须拒绝")
	}
	if _, err := e.GCEvidenceBackup(time.Now().Unix()+60, filepath.Join(e.ArchiveDir(), "backup")); err == nil {
		t.Fatal("备份根落在归档目录内时必须拒绝")
	}
	if n := archiveFileCount(t, e.ArchiveDir()); n != 1 {
		t.Fatalf("拒绝时归档文件必须原样，got %d", n)
	}
	if n := ledgerRows(t, db); n != 1 {
		t.Fatalf("拒绝时账本必须原样，got %d", n)
	}
}

// C4：第 2 个 move 失败（目标位置被同名目录占住）→ 已移出的第 1 个文件
// 必须被搬回原位，备份目录不留半份。
func TestMoveToBackupRollsBack(t *testing.T) {
	srcDir := t.TempDir()
	a := filepath.Join(srcDir, "a.jsonl.gz")
	b := filepath.Join(srcDir, "b.jsonl.gz")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("body "+filepath.Base(p)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backupDir := filepath.Join(t.TempDir(), "bk")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 用同名目录占住第二个目标位：rename 到已存在的非空/目录名会失败。
	if err := os.MkdirAll(filepath.Join(backupDir, filepath.Base(b)), 0o700); err != nil {
		t.Fatal(err)
	}

	items, err := moveToBackupDir([]string{a, b}, backupDir)
	if err == nil {
		t.Fatal("第二个文件移不进备份时必须报错")
	}
	if len(items) != 0 {
		t.Fatalf("失败时不该返回落位清单，got %+v", items)
	}
	if _, statErr := os.Stat(a); statErr != nil {
		t.Fatalf("已移出的文件必须回滚到原位: %v", statErr)
	}
	if _, statErr := os.Stat(b); statErr != nil {
		t.Fatalf("源文件 b 不该被动过: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(backupDir, filepath.Base(a))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("回滚后备份目录不该留 a: %v", statErr)
	}
}

// 跨盘（EXDEV）分支在单盘测试机上无法用 rename 触发，直接验它的原语：
// 复制后内容、字节数、sha256 都必须与源一致——否则「源删了、目标是坏的」。
func TestCopyFileSyncCopiesAndVerifies(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	body := []byte("归档字节 跨盘备份原语\n")
	for i := 0; i < 100; i++ {
		body = append(body, byte(i%251))
	}
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.bin")
	if err := copyFileSync(src, dst); err != nil {
		t.Fatalf("copyFileSync: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("复制内容不一致：%d 字节 vs %d 字节", len(got), len(body))
	}
	wantSum, wantN, err := hashFile(src)
	if err != nil {
		t.Fatal(err)
	}
	gotSum, gotN, err := hashFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if wantN != gotN || wantSum != gotSum {
		t.Fatalf("复制校验不一致：%d/%s vs %d/%s", gotN, gotSum, wantN, wantSum)
	}
}
