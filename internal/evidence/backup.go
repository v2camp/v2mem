package evidence

// 归档段的文件级备份（S3）。
//
// 为什么不是「删了就删了」：归档是 memory_evidence 的溯源目标，删错不可逆。
// 破坏性 GC 前把要删的段整体移进 `<备份根>/evidence-gc-<时间戳>/` 并落一份清单，
// 清单就是捞回的索引（文件名 + sha256 + 字节数）。
//
// 不变量：
//   - 清单落盘前一个字节都不算「已备份」；调用方只在清单写入成功后才删账本行；
//   - 任一步失败，已移出的文件按逆序搬回原位（含跨盘场景），归档目录看起来像没动过；
//   - 备份目录名带时间戳，同一秒重复跑加 -2/-3 后缀，绝不覆盖既有备份。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const manifestName = "manifest.json"

// backupItem 是清单里的一条：文件名（= 归档 hash）、内容 sha256、字节数。
type backupItem struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// backupManifest 是备份目录的捞回索引。
type backupManifest struct {
	CreatedAt  int64        `json:"created_at"`
	TotalFiles int          `json:"total_files"`
	TotalBytes int64        `json:"total_bytes"`
	Items      []backupItem `json:"items"`
}

// checkBackupRoot 拒绝把备份目录放进归档目录。
//
// 归档目录每轮都会被扫一遍找「账本外残段」；备份若落在里面，下一轮就会把备份
// 当残段清掉——备份等于没做，而且失效是静默的。
func checkBackupRoot(archiveDir, root string) error {
	a, err := filepath.Abs(archiveDir)
	if err != nil {
		return fmt.Errorf("解析归档目录失败: %w", err)
	}
	r, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("解析备份目录失败: %w", err)
	}
	if r == a || strings.HasPrefix(r, a+string(os.PathSeparator)) {
		return fmt.Errorf("备份目录不能落在归档目录内（%s）", r)
	}
	return nil
}

// newBackupDir 在 root 下建 evidence-gc-<时间戳>；同秒撞名加 -2/-3 后缀。
func newBackupDir(root string, now time.Time) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("创建备份根失败: %w", err)
	}
	base := "evidence-gc-" + now.Format("20060102-150405")
	for i := 1; ; i++ {
		dir := filepath.Join(root, base)
		if i > 1 {
			dir = filepath.Join(root, fmt.Sprintf("%s-%d", base, i))
		}
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("创建备份目录失败: %w", err)
		}
	}
}

// moveToBackupDir 把 files 逐个移进 backupDir，返回落位清单。
// 任一文件移不进去 → 把已移出的按逆序搬回原位（失败回滚、备份目录不留半份）。
func moveToBackupDir(files []string, backupDir string) ([]backupItem, error) {
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建备份目录失败: %w", err)
	}
	type placed struct{ src, dst string }
	var done []placed
	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			_ = moveFile(done[i].dst, done[i].src)
		}
	}

	items := make([]backupItem, 0, len(files))
	for _, src := range files {
		dst := filepath.Join(backupDir, filepath.Base(src))
		if err := moveFile(src, dst); err != nil {
			rollback()
			return nil, fmt.Errorf("移入备份目录失败（%s）: %w", filepath.Base(src), err)
		}
		done = append(done, placed{src: src, dst: dst})
		item, err := describeFile(dst)
		if err != nil {
			done = done[:len(done)-1]
			_ = moveFile(dst, src)
			rollback()
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// moveFile 先试 rename（同盘原子、零拷贝）；跨盘（EXDEV）退化为
// copy + fsync + sha256 回读校验 + 删源。复制校验不过就删掉半份目标、保留源。
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyFileSync(src, dst); err != nil {
		_ = os.Remove(dst)
		return err
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("备份已复制但删源失败: %w", err)
	}
	return nil
}

// copyFileSync 复制并 fsync，再回读比对 sha256 与字节数：跨盘备份宁可多一次读，
// 也不能出现「源删了、目标是坏的」。
func copyFileSync(src, dst string) error {
	want, size, err := hashFile(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("备份复制字节数不符（%d != %d）", n, size)
	}
	got, _, err := hashFile(dst)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("备份复制校验失败（sha256 %s != %s）", got, want)
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func describeFile(path string) (backupItem, error) {
	sum, n, err := hashFile(path)
	if err != nil {
		return backupItem{}, fmt.Errorf("计算备份校验和失败（%s）: %w", filepath.Base(path), err)
	}
	return backupItem{File: filepath.Base(path), SHA256: sum, Bytes: n}, nil
}

// writeManifest 用 tmp + rename 原子落盘：清单要么完整存在，要么不存在。
func writeManifest(dir string, items []backupItem, now time.Time) error {
	m := backupManifest{CreatedAt: now.Unix(), TotalFiles: len(items), Items: items}
	for _, it := range items {
		m.TotalBytes += it.Bytes
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化备份清单失败: %w", err)
	}
	tmp := filepath.Join(dir, manifestName+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("写备份清单失败: %w", err)
	}
	_, werr := f.Write(append(body, '\n'))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("写备份清单失败: %w", werr)
	}
	if err := os.Rename(tmp, filepath.Join(dir, manifestName)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("落盘备份清单失败: %w", err)
	}
	return nil
}
