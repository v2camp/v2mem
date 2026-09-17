package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// Count 返回库中记忆条数，供「是否值得备份」判断。
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n)
	return n, err
}

// Backup 用 `VACUUM INTO` 生成一个独立的库快照。
//
// 为什么不用文件复制：库处于 WAL 模式，直接 copy .db 会漏掉 -wal 里未落盘的
// 提交，拿到的是损坏/过期快照。VACUUM INTO 由 SQLite 在事务内产出单文件、
// 一致且可移植的快照，因此是跨驱动下最稳的方案。
func (s *Store) Backup(dest string) error {
	if dest == "" {
		return fmt.Errorf("备份路径为空")
	}
	if dir := filepath.Dir(dest); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建备份目录失败: %w", err)
		}
	}
	if _, err := s.db.Exec("VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("生成备份失败: %w", err)
	}
	return nil
}