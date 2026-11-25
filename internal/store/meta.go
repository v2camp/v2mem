package store

import "database/sql"

// MetaMaintainLastOK 记录最后一次成功维护的 unix 秒（cmdGC / cmdConsolidate 成功返回后写）。
// 从未维护时键不存在 → mem stats 显示 null，「从未维护」与「刚维护过」必须可区分。
const MetaMaintainLastOK = "maintain:last_ok"

// ProjectMemoryCount 返回某个工程已入库的记忆条数。
// 冷启动判定只关心「这个工程还没有任何已采纳记忆」，不需要扫描全库。
func (s *Store) ProjectMemoryCount(project string) (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE project = ?`, project).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MetaGet 读取 db_meta 的键值。键不存在返回 ("", false, nil)，便于调用方
// 区分「没标记」与「读失败」——冷启动靠这个区分决定是否重试。
func (s *Store) MetaGet(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM db_meta WHERE k = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// MetaSet 幂等写入 db_meta。db_meta 由 EnsureWriterStamp 在 Open 时建好。
func (s *Store) MetaSet(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO db_meta(k, v) VALUES(?, ?)
		 ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		key, value,
	)
	return err
}
