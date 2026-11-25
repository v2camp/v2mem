package testkit

import (
	"fmt"
	"os"
)

// ReadOnlyDir 把目录改成 0555（不可写、可读可执行），模拟磁盘只读/权限故障；
// 返回的 restore 恢复原权限。调用方必须 defer restore，否则 t.TempDir 清理
// 也会失败。
//
// 用 chmod 而不是伪造错误路径，是为了让被测代码走真实的 open/write 失败分支，
// 与生产里「磁盘满/挂载只读」的错误来源一致。
func ReadOnlyDir(dir string) (restore func() error, err error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("testkit.ReadOnlyDir: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("testkit.ReadOnlyDir: %q 不是目录", dir)
	}
	old := fi.Mode().Perm()
	if err := os.Chmod(dir, 0o555); err != nil {
		return nil, fmt.Errorf("testkit.ReadOnlyDir: chmod 失败: %w", err)
	}
	return func() error { return os.Chmod(dir, old) }, nil
}
