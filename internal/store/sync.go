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

// Ref 是跨设备可解析的身份引用。
//
// 取代关系不能直接用 id 表达：id 是本地随机值，照搬到另一端就是悬挂引用。
// 用「身份键」(content_hash, project) 表达，导入端再解析回本地 id。
type Ref struct {
	Hash    string `json:"hash"`
	Project string `json:"project"`
}

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
	SupersededBy string              `json:"superseded_by_id,omitempty"` // 本地 id，仅供排查
	Superseded   *Ref                `json:"superseded,omitempty"`       // 可跨设备重建的取代引用
	OriginDevice string              `json:"origin_device"`
	OriginTool   string              `json:"origin_tool"`
	Source       string              `json:"source,omitempty"`
	AccessCount  int                 `json:"access_count"`
	Tags         map[string][]string `json:"tags"`
}

// ImportStats 汇总一次归并的结果。
type ImportStats struct {
	Inserted   int `json:"inserted"`   // 本地没有，新插入
	Merged     int `json:"merged"`     // 本地已有，按字段规则归并
	Skipped    int `json:"skipped"`    // 内容为空等无法归并
	Unresolved int `json:"unresolved"` // 取代引用的目标不在本地也不在文件里
}

// Export 导出全部记忆，按 (content_hash, project) 排序以保证可复现——
// 否则每次导出的 JSONL 都会无谓 diff，跨设备比对失去意义。
func (s *Store) Export() ([]ExportRecord, error) {
	// LEFT JOIN 自身：把取代指针的本地 id 翻译成「身份键」，
	// 这样接收端才能把它接到自己那条同名记录上，而不是搬一个不存在的 id。
	rows, err := s.db.Query(
		`SELECT m.id, m.content, m.kind, m.content_hash, m.project, m.salience,
		        m.created_at, m.updated_at, m.last_seen_at, m.expires_at,
		        m.origin_device, m.origin_tool, m.source, m.access_count,
		        t.id, t.content_hash, t.project
		   FROM memories m
		   LEFT JOIN memories t ON t.id = m.superseded_by
		  ORDER BY m.content_hash, m.project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	recs := []ExportRecord{}
	idx := map[string]int{} // memory id → 在 recs 中的下标
	for rows.Next() {
		var r ExportRecord
		var tgtID, tgtHash, tgtProject *string
		if err := rows.Scan(&r.ID, &r.Content, &r.Kind, &r.ContentHash, &r.Project, &r.Salience,
			&r.CreatedAt, &r.UpdatedAt, &r.LastSeenAt, &r.ExpiresAt,
			&r.OriginDevice, &r.OriginTool, &r.Source, &r.AccessCount,
			&tgtID, &tgtHash, &tgtProject); err != nil {
			return nil, err
		}
		if tgtID != nil {
			r.SupersededBy = *tgtID
			r.Superseded = &Ref{Hash: *tgtHash, Project: *tgtProject}
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
//	origin_device / origin_tool / source  仅新插入时写入；已有记录保留本地值
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

	// identity 记录「本批记录的身份键 → 本地 id」，供第二遍解析取代引用
	identity := map[string]string{}

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
				                        THEN NULL ELSE MAX(expires_at, ?) END
				 WHERE id = ?`,
				kind, r.Salience, r.AccessCount,
				r.CreatedAt, r.UpdatedAt, r.LastSeenAt,
				r.ExpiresAt, r.ExpiresAt,
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
				    origin_device, origin_tool, source, access_count)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				localID, content, indexText(content), kind, h, r.Project, r.Salience,
				r.CreatedAt, r.UpdatedAt, r.LastSeenAt, r.ExpiresAt,
				r.OriginDevice, r.OriginTool, r.Source, r.AccessCount,
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

		// 记下本批记录的身份键 → 本地 id，供第二遍解析取代引用。
		// 键必须用归一化后的 h（文件缺 content_hash 时会由 HashOf 兜底），
		// 用原始 r.ContentHash 会在那种情况下建出对不上的键。
		identity[h+"\x00"+r.Project] = localID
	}

	// 第二遍：重建取代关系。
	//
	// 必须分两遍：被取代者可能排在存活者之前（导出按 content_hash 排序，
	// 顺序与取代关系无关），一遍扫描会解析不到目标。
	// 目标优先在本批记录里找，其次回落到库里已有的记录（存活者可能不在本文件内）。
	for _, r := range recs {
		if r.Superseded == nil || r.Superseded.Hash == "" {
			continue
		}
		selfID, ok := identity[r.ContentHash+"\x00"+r.Project]
		if !ok {
			continue // 该记录本身被跳过（内容为空），无需处理
		}
		key := r.Superseded.Hash + "\x00" + r.Superseded.Project

		target, ok := identity[key]
		if !ok {
			err := tx.QueryRow(
				`SELECT id FROM memories WHERE content_hash = ? AND project = ?`,
				r.Superseded.Hash, r.Superseded.Project,
			).Scan(&target)
			if errors.Is(err, sql.ErrNoRows) {
				// 目标既不在文件里也不在本地：宁可留空并计数上报，
				// 也不能写入一个不存在的 id（那就是悬挂引用）。
				stats.Unresolved++
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		if target == selfID {
			// 自引用是不可能的取代关系，跳过以免污染检索过滤
			stats.Unresolved++
			continue
		}
		if _, err := tx.Exec(
			`UPDATE memories SET superseded_by = ? WHERE id = ?`, target, selfID,
		); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stats, nil
}
