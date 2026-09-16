// 跨设备归集：JSONL 导出 / 导入。
//
// 为什么不是同步数据库文件：写中途的同步会把 SQLite 库传坏（见 DESIGN.md §9）。
// 只同步「导出的事实日志」，由每一端各自归并入库。
//
// 身份键是 (content_hash, project)，不是 id —— id 由 newID() 随机生成，
// 两台设备独立写下同一事实必然得到不同 id，而唯一索引 ux_mem_hash 只允许一条。
package store

import (
	"database/sql"
	"errors"
	"strings"
)

// ExportRecord 是归集的传输单元：JSONL 一行一条。
// 不含 content_idx（派生列，导入端按自身规则重算）。
type ExportRecord struct {
	ID           string              `json:"id"` // 仅供参考，不作为归并身份
	Content      string              `json:"content"`
	Kind         string              `json:"kind"`
	ContentHash  string              `json:"content_hash"`
	Project      string              `json:"project"`
	Salience     float64             `json:"salience"`
	CreatedAt    int64               `json:"created_at"`
	UpdatedAt    int64               `json:"updated_at"`
	LastSeenAt   int64               `json:"last_seen_at"`
	ExpiresAt    *int64              `json:"expires_at"`
	SupersededBy *string             `json:"superseded_by"`
	OriginDevice string              `json:"origin_device"`
	OriginTool   string              `json:"origin_tool"`
	AccessCount  int                 `json:"access_count"`
	Tags         map[string][]string `json:"tags"`
}

// ImportStats 汇总一次归并的结果。
type ImportStats struct {
	Inserted int `json:"inserted"` // 本地没有，新插入
	Merged   int `json:"merged"`   // 本地已有，按字段规则归并
	Skipped  int `json:"skipped"`  // 内容为空等无法归并
}

// Export 导出全部记忆，按 (content_hash, project) 排序以保证可复现——
// 否则每次导出的 JSONL 都会无谓 diff，跨设备比对失去意义。
func (s *Store) Export() ([]ExportRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, content, kind, content_hash, project, salience,
		        created_at, updated_at, last_seen_at, expires_at,
		        superseded_by, origin_device, origin_tool, access_count
		   FROM memories
		  ORDER BY content_hash, project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	recs := []ExportRecord{}
	idx := map[string]int{} // memory id → 在 recs 中的下标
	for rows.Next() {
		var r ExportRecord
		if err := rows.Scan(&r.ID, &r.Content, &r.Kind, &r.ContentHash, &r.Project, &r.Salience,
			&r.CreatedAt, &r.UpdatedAt, &r.LastSeenAt, &r.ExpiresAt,
			&r.SupersededBy, &r.OriginDevice, &r.OriginTool, &r.AccessCount); err != nil {
			return nil, err
		}
		idx[r.ID] = len(recs)
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 标记：一键可多值，一个数组装得下，不会丢信息
	tagRows, err := s.db.Query(
		`SELECT memory_id, key, value FROM tags ORDER BY key, value`)
	if err != nil {
		return nil, err
	}
	defer tagRows.Close()
	for tagRows.Next() {
		var mid, k, v string
		if err := tagRows.Scan(&mid, &k, &v); err != nil {
			return nil, err
		}
		i, ok := idx[mid]
		if !ok {
			continue
		}
		if recs[i].Tags == nil {
			recs[i].Tags = map[string][]string{}
		}
		recs[i].Tags[k] = append(recs[i].Tags[k], v)
	}
	return recs, tagRows.Err()
}

// Import 按 (content_hash, project) 归并一批记录。
//
// 字段级归并规则（全部选定为幂等且可交换，故重复导入 / 双向导入都收敛）：
//
//	kind         取字典序较小者（双方都非空时的确定性让步）
//	salience     取较大者（更重要的判断占优）
//	access_count 取较大者 —— 不是求和，求和会随重复导入无限增长
//	created_at   取最早者（最早的创建时间最接近真相）
//	updated_at   取较晚者
//	last_seen_at 取较晚者
//	expires_at   任一边为 NULL（永不过期）则结果为 NULL，否则取较晚者
//	tags         并集（一键多值全部保留）
//	origin_device / origin_tool  仅新插入时写入；已有记录保留本地值
//
// 两条刻意的例外（本地书写形式优先，非可交换）：
//   - content：hash 相同即视为同一事实（归一化已折叠大小写与空白），保留本地写法
//   - superseded_by：已有非空值不被覆盖
func (s *Store) Import(recs []ExportRecord) (*ImportStats, error) {
	stats := &ImportStats{}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, r := range recs {
		content := strings.TrimSpace(r.Content)
		if content == "" {
			stats.Skipped++
			continue
		}
		h := r.ContentHash
		if h == "" {
			// 容错：外部手工编辑过的 JSONL 可能漏了 hash
			h = HashOf(content)
		}
		kind := r.Kind
		if kind == "" {
			kind = "fact"
		}

		var localID string
		err := tx.QueryRow(
			`SELECT id FROM memories WHERE content_hash = ? AND project = ?`, h, r.Project,
		).Scan(&localID)

		switch {
		case err == nil:
			if _, err := tx.Exec(
				`UPDATE memories SET
				   kind          = MIN(kind, ?),
				   salience      = MAX(salience, ?),
				   access_count  = MAX(access_count, ?),
				   created_at    = MIN(created_at, ?),
				   updated_at    = MAX(updated_at, ?),
				   last_seen_at  = MAX(last_seen_at, ?),
				   expires_at    = CASE WHEN expires_at IS NULL OR ? IS NULL
				                        THEN NULL ELSE MAX(expires_at, ?) END,
				   superseded_by = COALESCE(superseded_by, ?)
				 WHERE id = ?`,
				kind, r.Salience, r.AccessCount,
				r.CreatedAt, r.UpdatedAt, r.LastSeenAt,
				r.ExpiresAt, r.ExpiresAt, r.SupersededBy,
				localID,
			); err != nil {
				return nil, err
			}
			stats.Merged++

		case errors.Is(err, sql.ErrNoRows):
			localID = newID()
			if _, err := tx.Exec(
				`INSERT INTO memories
				   (id, content, content_idx, kind, content_hash, project, salience,
				    created_at, updated_at, last_seen_at, expires_at,
				    origin_device, origin_tool, access_count)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				localID, content, indexText(content), kind, h, r.Project, r.Salience,
				r.CreatedAt, r.UpdatedAt, r.LastSeenAt, r.ExpiresAt,
				r.OriginDevice, r.OriginTool, r.AccessCount,
			); err != nil {
				return nil, err
			}
			stats.Inserted++

		default:
			return nil, err
		}

		for k, vals := range r.Tags {
			for _, v := range vals {
				if _, err := tx.Exec(
					`INSERT OR IGNORE INTO tags(memory_id, key, value) VALUES (?,?,?)`,
					localID, k, v,
				); err != nil {
					return nil, err
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stats, nil
}
