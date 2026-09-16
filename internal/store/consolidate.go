// 相似知识归并：把「不同 hash 但说的是同一件事」的记忆聚成簇，留 1 条、其余置 superseded_by。
//
// 与 M1 的「相同内容覆盖」分工：
//   - 完全相同的归一化文本（同 content_hash）→ M1 覆盖，不走这里
//   - 措辞略有差异的近重复 → 这里归并
//
// 准确性来自 similarity.Judge 的四条护栏，不是单靠阈值（见 DESIGN.md §11.10）。
package store

import (
	"sort"
	"time"

	"github.com/wanghui/v2mem/internal/similarity"
)

// DefaultConsolidateThreshold 是相似度阈值默认值。
//
// 定 0.7 而非 0.8：实测负对照最高只到 0.03（分离度极大），阈值主要影响召回；
// 精度交给护栏（数字/否定/长度/短文本），所以可以放心压低阈值多抓近重复。
const DefaultConsolidateThreshold = 0.7

// ConsolidatePair 记录一次取代。
type ConsolidatePair struct {
	Survivor   string  `json:"survivor"`
	Superseded string  `json:"superseded"`
	Similarity float64 `json:"similarity"`
}

// ConsolidateResult 汇总一次归并。
type ConsolidateResult struct {
	Scanned    int               `json:"scanned"`    // 参与比较的存活记忆数
	Groups     int               `json:"groups"`     // 形成的近重复簇数
	Superseded int               `json:"superseded"` // 被取代的条数
	Pairs      []ConsolidatePair `json:"pairs"`
}

// candidate 是参与归并比较的最小列集。
type candidate struct {
	id          string
	content     string
	project     string
	salience    float64
	accessCount int
	createdAt   int64
}

// Consolidate 对存活记忆做相似归并。
//
// 只在同一 project 内比较（跨工程是独立记忆），比较范围限于未过期且未被取代的条目。
// 幂等：被取代者不再进入下一轮的比较范围，故重复运行不产生新的取代。
func (s *Store) Consolidate(threshold float64) (*ConsolidateResult, error) {
	if threshold <= 0 {
		threshold = DefaultConsolidateThreshold
	}
	res := &ConsolidateResult{Pairs: []ConsolidatePair{}}

	now := time.Now().Unix()
	rows, err := s.db.Query(
		`SELECT id, content, project, salience, access_count, created_at
		   FROM memories
		  WHERE superseded_by IS NULL
		    AND (expires_at IS NULL OR expires_at > ?)
		  ORDER BY project, content_hash`,
		now)
	if err != nil {
		return nil, err
	}
	var all []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.content, &c.project, &c.salience, &c.accessCount, &c.createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.Scanned = len(all)

	// 按工程分组：跨工程不得互相归并
	byProject := map[string][]candidate{}
	for _, c := range all {
		byProject[c.project] = append(byProject[c.project], c)
	}

	// 工程遍历顺序固定，保证结果可复现
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, proj := range projects {
		group := byProject[proj]
		if len(group) < 2 {
			continue
		}
		// 预计算签名，避免 O(n²) 次重复签名
		sigs := make([]similarity.Signature, len(group))
		for i := range group {
			sigs[i] = similarity.Sign(group[i].content)
		}

		// O(n²) 两两比较：个人库规模（千级）下即几十毫秒。
		// 超过约 5000 条再上 LSH banding，当前不做是刻意的简单性取舍。
		uf := newUnionFind(len(group))
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				if similarity.Estimate(sigs[i], sigs[j]) < threshold {
					continue // 先按签名快速筛掉，省去护栏与字符串开销
				}
				v := similarity.Judge(group[i].content, group[j].content, threshold)
				if v.Allowed {
					uf.union(i, j)
				}
			}
		}

		// 按簇归并
		clusters := map[int][]int{}
		for i := range group {
			root := uf.find(i)
			clusters[root] = append(clusters[root], i)
		}
		for _, members := range clusters {
			if len(members) < 2 {
				continue
			}
			survivor := pickSurvivor(group, members)
			for _, m := range members {
				if group[m].id == survivor.id {
					continue
				}
				if _, err := tx.Exec(
					`UPDATE memories SET superseded_by = ?, updated_at = ? WHERE id = ?`,
					survivor.id, now, group[m].id,
				); err != nil {
					return nil, err
				}
				// 标记转移：不然被取代者承载的机器/工具信息随归并消失
				if _, err := tx.Exec(
					`UPDATE OR IGNORE tags SET memory_id = ? WHERE memory_id = ?`,
					survivor.id, group[m].id,
				); err != nil {
					return nil, err
				}
				// 冲突的 (key,value) 已在存活者身上时，上一条 UPDATE OR IGNORE 会跳过，
				// 这里补一次删除，避免留下悬挂标记。
				if _, err := tx.Exec(
					`DELETE FROM tags WHERE memory_id = ?`, group[m].id,
				); err != nil {
					return nil, err
				}
				res.Superseded++
				res.Pairs = append(res.Pairs, ConsolidatePair{
					Survivor:   survivor.id,
					Superseded: group[m].id,
					Similarity: similarity.Estimate(sigs[survivorIdx(group, members, survivor.id)], sigs[m]),
				})
			}
			res.Groups++
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// pickSurvivor 选定存活者。规则必须完全确定（跨设备独立运行也要得到同一结论）：
// salience 降序 → access_count 降序 → created_at 升序 → id 升序。
func pickSurvivor(group []candidate, members []int) candidate {
	best := group[members[0]]
	for _, m := range members[1:] {
		c := group[m]
		switch {
		case c.salience != best.salience:
			if c.salience > best.salience {
				best = c
			}
		case c.accessCount != best.accessCount:
			if c.accessCount > best.accessCount {
				best = c
			}
		case c.createdAt != best.createdAt:
			if c.createdAt < best.createdAt {
				best = c
			}
		case c.id < best.id:
			best = c
		}
	}
	return best
}

func survivorIdx(group []candidate, members []int, id string) int {
	for _, m := range members {
		if group[m].id == id {
			return m
		}
	}
	return members[0]
}

// unionFind 是并查集，用于把近重复关系聚成连通分量。
type unionFind struct{ parent []int }

func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n)}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

func (u *unionFind) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]] // 路径压缩
		i = u.parent[i]
	}
	return i
}

func (u *unionFind) union(i, j int) {
	ri, rj := u.find(i), u.find(j)
	if ri != rj {
		u.parent[ri] = rj
	}
}
