package store

import (
	"database/sql"
	"fmt"
	"net/url"
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

// VerifyBackup 校验一个备份文件真的能用：文件非空 → `PRAGMA quick_check` 返回 ok →
// `memories` 表可查（schema 在位）。design-data-lifecycle §8.1 的「删除前确认新备份有效」
// 就落在这里：只生成了文件、读不回来，等于没备份。
//
// 必须**只读**打开（`mode=ro`）。不能用 Open：Open 会建表、跑迁移、写 db_meta 写者戳，
// 那会把「快照当时的样子」改成「这次校验的样子」——备份就不再是备份。
func VerifyBackup(path string) error {
	if path == "" {
		return fmt.Errorf("备份路径为空")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("备份文件不可读: %w", err)
	}
	if fi.IsDir() {
		return fmt.Errorf("备份路径是目录: %s", path)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("备份文件为空: %s", path)
	}

	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fmt.Errorf("只读打开备份失败: %w", err)
	}
	defer db.Close()

	var check string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
		return fmt.Errorf("备份完整性校验失败: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("备份完整性校验未通过: %s", check)
	}

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
		return fmt.Errorf("备份 schema 校验失败: %w", err)
	}
	return nil
}
