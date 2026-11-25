package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

// verifyBackupFn 是备份校验的测试缝：默认走 store.VerifyBackup（只读打开 +
// quick_check + memories 表可查）。测试用它模拟「快照生成了但读不回来」。
var verifyBackupFn = store.VerifyBackup

// backupDirFor 返回备份根：显式 --backup-dir 优先，否则 <库目录>/backup。
// 库快照（mem-*.db）与归档备份（evidence-gc-*）共用这个根。
func backupDirFor(dbPath, backupDir string) string {
	if backupDir != "" {
		return backupDir
	}
	path := dbPath
	if path == "" {
		path = store.DefaultPath()
	}
	return filepath.Join(filepath.Dir(path), "backup")
}

// backupBeforeMutating 在破坏性操作（gc / consolidate / sync / migrate）前生成库快照，
// 并校验快照真的能读回来。返回备份路径；跳过场景（--no-backup / 库不存在 / 库为空）返回空串。
//
// 校验失败按「备份不成立」处理：调用方必须拒绝执行——否则删掉的字节只剩一个
// 打不开的文件当「备份」。
func backupBeforeMutating(dbPath string, noBackup bool, backupDir string) (string, error) {
	if noBackup {
		return "", nil
	}
	path := dbPath
	if path == "" {
		path = store.DefaultPath()
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", nil // 库尚不存在，无备份可言
	}

	st, err := store.Open(path)
	if err != nil {
		return "", err
	}
	defer st.Close()

	n, err := st.Count()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil // 空库无需备份
	}

	dir := backupDirFor(path, backupDir)
	ts := time.Now().Format("20060102-150405")
	dest := filepath.Join(dir, fmt.Sprintf("mem-%s.db", ts))
	for i := 2; ; i++ { // 同秒内多次破坏性操作（gc→consolidate）会撞名；VACUUM INTO 拒绝覆盖
		// 只有「文件确实存在」才换名。Stat 的其它错误（父路径是普通文件会返回
		// ENOTDIR，不是 ErrNotExist）交给 Backup 去报，不能在这里空转重试。
		if _, err := os.Stat(dest); err != nil {
			break
		}
		dest = filepath.Join(dir, fmt.Sprintf("mem-%s-%d.db", ts, i))
	}
	if err := st.Backup(dest); err != nil {
		return "", err
	}
	if err := verifyBackupFn(dest); err != nil {
		return "", fmt.Errorf("备份校验失败，拒绝执行（%s）: %w", dest, err)
	}
	return dest, nil
}
