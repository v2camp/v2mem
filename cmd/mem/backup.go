package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wanghui/v2mem/internal/store"
)

// backupBeforeMutating 在破坏性操作（gc / consolidate）前生成库快照。
// 返回备份路径；跳过场景（--no-backup / 库不存在 / 库为空）返回空串。
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

	dir := backupDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(path), "backup")
	}
	dest := filepath.Join(dir, fmt.Sprintf("mem-%s.db", time.Now().Format("20060102-150405")))
	if err := st.Backup(dest); err != nil {
		return "", err
	}
	return dest, nil
}