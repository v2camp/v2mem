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

	"github.com/wanghui/v2mem/internal/similarity"
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
	Provenance   *string             `json:"provenance,omitempty"` // 原始 JSON；NULL 序列化为缺省（无溯源）
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
		        m.origin_device, m.origin_tool, m.source, m.provenance, m.access_count,
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
			&r.OriginDevice, &r.OriginTool, &r.Source, &r.Provenance, &r.AccessCount,
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
				                        THEN NULL ELSE MAX(expires_at, ?) END,
				   provenance    = COALESCE(provenance, ?)
				 WHERE id = ?`,
				kind, r.Salience, r.AccessCount,
				r.CreatedAt, r.UpdatedAt, r.LastSeenAt,
				r.ExpiresAt, r.ExpiresAt,
				r.Provenance,
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
				    origin_device, origin_tool, source, provenance, access_count)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				localID, content, indexText(content), kind, h, r.Project, r.Salience,
				r.CreatedAt, r.UpdatedAt, r.LastSeenAt, r.ExpiresAt,
				r.OriginDevice, r.OriginTool, r.Source, r.Provenance, r.AccessCount,
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

// ---------- WS3: 三方合并 ----------
//
// Import 是无脑字段归并（updated_at 一律取 MAX），无法表达「最近者胜 /
// 冲突并存」的决策：两台设备分别写下同一条记忆时，谁新谁说了算；时间戳
// 撞车时则不能静默吞掉某一端的写入。
//
// SyncMerge 就是 `mem sync` 的合并判定：以 (content_hash, project) 为键，
// 把远端一批记录并进本地库，规则如下：
//
//	本地没有此键         → 新插入
//	远端 updated_at 明显更新 → 最近者胜：远端内容写法为准，时间戳推进到远端
//	本地 updated_at 明显更新 → 本地已更新，远端较老，保持不动
//	两端 updated_at 撞车   → 冲突并存：内容归一化后本就相同，故保留本地写法，
//	                        但给该条打 sync 标记（不静默丢失任一端的来源信息），
//	                        交由 mem merge 人工收敛
//
// 「并入本地库的方向」固定是远端 → 本地，不做双向（每端都跑一次 sync 即收敛）。
// 唯一索引 ux_mem_hash 不允许 (content_hash, project) 出现两行，故冲突时以
// 「同一条上打来源标记」表达并存，而不是复制成两行。
//
// 内容归一化相等 → content_hash 相同，因此「换写法」只动 content 原串与索引，
// 身份键不变。凡是改 content 的分支都会同步 mem_sigs 签名与 content_idx，
// 保证模糊检索与 FTS 与正文一致。

// SyncTolerance 是时间戳冲突判定阈值（秒）。
// 两端 updated_at 之差不超过该值的记录视为「无法自动决断」，走冲突并存。
const SyncTolerance = 2

// SyncConflict 描述一次无法自动决断的归并冲突。
type SyncConflict struct {
	Hash         string `json:"hash"`
	Project      string `json:"project"`
	LocalAt      int64  `json:"local_updated_at"`
	RemoteAt     int64  `json:"remote_updated_at"`
	RemoteDevice string `json:"remote_device,omitempty"`
}

// SyncStats 汇总一次三方合并。
type SyncStats struct {
	Inserted   int            `json:"inserted"`
	Merged     int            `json:"merged"`      // 远端更新（最近者胜）已并入
	NewerLocal int            `json:"newer_local"` // 本地已更新，远端较老，未改动
	Conflicts  []SyncConflict `json:"conflicts,omitempty"`
	Skipped    int            `json:"skipped"`
	Unresolved int            `json:"unresolved"` // 取代引用目标既不在文件也不在本地
}

// SyncMerge 把远端导出的记录按三路规则并进本地库。见上方文档注释。
func (s *Store) SyncMerge(remote []ExportRecord) (*SyncStats, error) {
	stats := &SyncStats{}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 本地 (hash, project) → (id, updated_at)，供判决使用。
	type localMeta struct {
		id   string
		at   int64
		proj string
	}
	localIdx := map[string]localMeta{}
	rows, err := tx.Query(`SELECT content_hash, project, updated_at, id FROM memories`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var h, proj, id string
		var at int64
		if err := rows.Scan(&h, &proj, &at, &id); err != nil {
			rows.Close()
			return nil, err
		}
		localIdx[h+"\x00"+proj] = localMeta{id: id, at: at, proj: proj}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// identity 记录「本批记录的身份键 → 本地 id」，供第二遍解析取代引用。
	identity := map[string]string{}

	for _, r := range remote {
		content := strings.TrimSpace(r.Content)
		if content == "" {
			stats.Skipped++
			continue
		}
		h := r.ContentHash
		if h == "" {
			h = HashOf(content)
		}
		kind := r.Kind
		if kind == "" {
			kind = "fact"
		}
		idx := indexText(content)
		key := h + "\x00" + r.Project

		local, present := localIdx[key]
		if !present {
			// 新记录：直插（与 Import 相同，含签名、tags）。
			id := newID()
			if _, err := tx.Exec(
				`INSERT INTO memories
				   (id, content, content_idx, kind, content_hash, project, salience,
				    created_at, updated_at, last_seen_at, expires_at,
				    origin_device, origin_tool, source, access_count)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				id, content, idx, kind, h, r.Project, r.Salience,
				r.CreatedAt, r.UpdatedAt, r.LastSeenAt, r.ExpiresAt,
				r.OriginDevice, r.OriginTool, r.Source, r.AccessCount,
			); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(
				`INSERT OR REPLACE INTO mem_sigs(memory_id, sig) VALUES (?, ?)`,
				id, encodeSig(similarity.Sign(content)),
			); err != nil {
				return nil, err
			}
			identity[key] = id
			stats.Inserted++
			syncMergeTags(tx, id, r.Tags)
			continue
		}

		switch {
		case r.UpdatedAt > local.at+SyncTolerance:
			// 最近者胜：以远端写法为准，时间戳推进到远端，元数据取「更值得」项。
			if _, err := tx.Exec(
				`UPDATE memories SET
				   content = ?, content_idx = ?,
				   kind = MIN(kind, ?),
				   salience = MAX(salience, ?),
				   access_count = MAX(access_count, ?),
				   created_at = MIN(created_at, ?),
				   updated_at = ?,
				   last_seen_at = MAX(last_seen_at, ?),
				   expires_at = CASE WHEN expires_at IS NULL OR ? IS NULL
				                    THEN NULL ELSE MAX(expires_at, ?) END
				 WHERE id = ?`,
				content, idx, kind, r.Salience, r.AccessCount,
				r.CreatedAt, r.UpdatedAt, r.LastSeenAt,
				r.ExpiresAt, r.ExpiresAt, local.id,
			); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(
				`INSERT OR REPLACE INTO mem_sigs(memory_id, sig) VALUES (?, ?)`,
				local.id, encodeSig(similarity.Sign(content)),
			); err != nil {
				return nil, err
			}
			stats.Merged++

		case local.at > r.UpdatedAt+SyncTolerance:
			// 本地已更新，远端较老：保持本地不动。
			stats.NewerLocal++

		default:
			// 时间戳撞车：冲突并存。内容归一化后本就相同，保留本地写法，
			// 但打上 sync 冲突标记，把远端设备的来源噪音显式留下，不静默吞掉。
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO tags(memory_id, key, value) VALUES (?, ?, ?)`,
				local.id, "sync", "conflict:"+r.OriginDevice,
			); err != nil {
				return nil, err
			}
			stats.Conflicts = append(stats.Conflicts, SyncConflict{
				Hash: h, Project: r.Project,
				LocalAt: local.at, RemoteAt: r.UpdatedAt,
				RemoteDevice: r.OriginDevice,
			})
		}

		// 无论是并入还是保持，都取远端 tags 并集（幂等 INSERT OR IGNORE，
		// 不引入重复），保证远端追加的机器/工具标记不因归并丢失。
		syncMergeTags(tx, local.id, r.Tags)
		identity[key] = local.id
	}

	// 第二遍：重建取代引用（与 Import 相同的两遍逻辑，见 Import 中注释）。
	for _, r := range remote {
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
				stats.Unresolved++
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		if target == selfID {
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

// syncMergeTags 把一键多值的标记表并进目标记忆（幂等并集）。
func syncMergeTags(tx *sql.Tx, memoryID string, tags map[string][]string) {
	if len(tags) == 0 {
		return
	}
	for k, vals := range tags {
		for _, v := range vals {
			_, _ = tx.Exec(
				`INSERT OR IGNORE INTO tags(memory_id, key, value) VALUES (?,?,?)`,
				memoryID, k, v,
			)
		}
	}
}
