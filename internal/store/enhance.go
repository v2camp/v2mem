package store

// 正文提升（mem repair content）的 store 层：把「只有原始内容、没有来源」的已入库记忆
// 交给模型提炼成可复用正文。选择器在调用方；这里只负责两件事：
//  1. 列出可参与提升的存活记忆（撤回 / 被取代 / 过期不参与）；
//  2. 改写与回滚的写路径——与 review correct 同源（重算 content_hash / mem_sigs，
//     original_hash 留档），但状态回到未审核：模型改写不算人工确认。
//
// 红线（docs/VALUE.md §4）：模型改写不新增记忆、不改身份键之外的事实；
// 每次改写都可在归档后按 content_hash 精确回滚。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/v2camp/v2mem/internal/similarity"
)

// EnhanceCandidate 是正文提升的只读投影；选择器在调用方。
type EnhanceCandidate struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	ContentHash string `json:"content_hash"`
	ReviewState string `json:"review_state"`
}

// ListEnhanceCandidates 列出可参与提升的存活记忆：
// 排除 review_state='withdrawn'、superseded_by 非空、已过期（expires_at 非空且 <= now）。
// 按 created_at ASC, id ASC 排序；limit<=0 时默认 500。
func (s *Store) ListEnhanceCandidates(limit int) ([]EnhanceCandidate, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT id, content, kind, project, content_hash, review_state
		FROM memories
		WHERE review_state <> ?
		  AND COALESCE(superseded_by, '') = ''
		  AND (expires_at IS NULL OR expires_at > ?)
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, ReviewStateWithdrawn, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnhanceCandidate{}
	for rows.Next() {
		var c EnhanceCandidate
		if err := rows.Scan(&c.ID, &c.Content, &c.Kind, &c.Project, &c.ContentHash, &c.ReviewState); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RewriteMemoryResult 记录一次模型改写。
type RewriteMemoryResult struct {
	MemoryID   string `json:"memory_id"`
	OldContent string `json:"old_content"`
	OldHash    string `json:"old_hash"`
	NewHash    string `json:"new_hash"`
	Changed    bool   `json:"changed"`
}

// ErrMemoryContentChanged 表示选择候选后正文已被改动（人工修正、再次改写等）。
// CLI 据此跳过该条，而不是用模型稿覆盖期间发生的决定。
var ErrMemoryContentChanged = errors.New("记忆正文在选择后被修改")

// RewriteMemoryContent 用模型产出替换正文。
//
// expectHash 是选择候选时读到的 content_hash（CAS 守卫）：非空且与当前值不等时
// 返回 ErrMemoryContentChanged，不写库；传空串表示调用方不做选择时校验。
// 模型调用耗时数秒，期间人工修正/再次改写必须优先于模型稿。
//
// 与 ApplyMemoryReview(correct) 共用同一套写路径：content/content_idx/content_hash
// 同步重算、mem_sigs 重签名、provenance 只在第一次改写时留 original_hash（跨设备
// 身份追溯）。差别只在状态：模型改写必须回到未审核，reviewed_* 清零。
func (s *Store) RewriteMemoryContent(idOrPrefix, content, enhanceTag, expectHash string, now int64) (*RewriteMemoryResult, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("改写后的正文为空")
	}
	if now <= 0 {
		now = time.Now().Unix()
	}
	id, err := s.resolveID(idOrPrefix)
	if err != nil {
		return nil, err
	}
	res := &RewriteMemoryResult{MemoryID: id}
	err = s.withImmediateTx(func(ctx context.Context, q sqlQuerier) error {
		var curContent, curHash, state, prov, project string
		if err := q.QueryRowContext(ctx, `SELECT content, content_hash, review_state,
				COALESCE(provenance, ''), project FROM memories WHERE id = ?`, id).
			Scan(&curContent, &curHash, &state, &prov, &project); err != nil {
			return err
		}
		res.OldContent = curContent
		res.OldHash = curHash
		res.NewHash = curHash
		if expectHash != "" && curHash != expectHash {
			return ErrMemoryContentChanged
		}
		if state == ReviewStateWithdrawn {
			return fmt.Errorf("记忆 %s 已撤回，不能改写正文", id)
		}
		if content == curContent {
			return nil // 仅空白差异：Changed=false，不写库
		}
		newHash := HashOf(content)
		var conflict string
		switch err := q.QueryRowContext(ctx, `SELECT id FROM memories
			WHERE content_hash = ? AND project = ? AND id <> ?`,
			newHash, project, id).Scan(&conflict); {
		case err == nil:
			return fmt.Errorf("改写后的正文与记忆 %s 内容相同，应先撤回本条再合并到该条", conflict)
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		if _, err := q.ExecContext(ctx, `UPDATE memories
			SET content = ?, content_idx = ?, content_hash = ?, provenance = ?,
			    review_state = ?, reviewed_at = 0, reviewed_by = '', updated_at = ?
			WHERE id = ?`,
			content, indexText(content), newHash,
			provenanceWithEnhance(prov, curHash, enhanceTag, now),
			ReviewStateNone, now, id); err != nil {
			return err
		}
		// 签名与正文同事务更新：否则改写后的记忆仍拿旧文本的 MinHash 参与模糊检索。
		if _, err := q.ExecContext(ctx,
			`INSERT OR REPLACE INTO mem_sigs(memory_id, sig) VALUES (?, ?)`,
			id, encodeSig(similarity.Sign(content))); err != nil {
			return err
		}
		res.NewHash = newHash
		res.Changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// RestoreMemoryContent 回滚一次改写。
//
// 只在「改写后没人再动过」时生效：当前 content_hash 必须等于 expectHash（改写时
// 拿到的 NewHash）。否则报 content_changed；还原后的旧 hash 若已被同工程另一条
// 记忆占用，报 conflict:<id>，两条都不是错误而是可预期的拒绝。
func (s *Store) RestoreMemoryContent(idOrPrefix, content, expectHash string, now int64) (bool, string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return false, "", errors.New("还原的正文为空")
	}
	if now <= 0 {
		now = time.Now().Unix()
	}
	id, err := s.resolveID(idOrPrefix)
	if err != nil {
		return false, "", err
	}
	restored := false
	reason := ""
	err = s.withImmediateTx(func(ctx context.Context, q sqlQuerier) error {
		var curHash, prov, project string
		if err := q.QueryRowContext(ctx, `SELECT content_hash, COALESCE(provenance, ''), project
				FROM memories WHERE id = ?`, id).Scan(&curHash, &prov, &project); err != nil {
			return err
		}
		// 改写后又被修正/再改写：还原会覆盖别人后来的决定，拒绝而不是猜。
		if curHash != expectHash {
			reason = "content_changed"
			return nil
		}
		newHash := HashOf(content)
		if newHash != curHash {
			var conflict string
			switch err := q.QueryRowContext(ctx, `SELECT id FROM memories
				WHERE content_hash = ? AND project = ? AND id <> ?`,
				newHash, project, id).Scan(&conflict); {
			case err == nil:
				reason = "conflict:" + conflict
				return nil
			case errors.Is(err, sql.ErrNoRows):
			default:
				return err
			}
		}
		if _, err := q.ExecContext(ctx, `UPDATE memories
			SET content = ?, content_idx = ?, content_hash = ?, provenance = ?,
			    review_state = ?, reviewed_at = 0, reviewed_by = '', updated_at = ?
			WHERE id = ?`,
			content, indexText(content), newHash,
			provenanceWithRestoredAt(prov, now), ReviewStateNone, now, id); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			`INSERT OR REPLACE INTO mem_sigs(memory_id, sig) VALUES (?, ?)`,
			id, encodeSig(similarity.Sign(content))); err != nil {
			return err
		}
		restored = true
		return nil
	})
	if err != nil {
		return false, "", err
	}
	return restored, reason, nil
}

// provenanceWithEnhance 记录一次模型改写：original_hash 只在首次写入（由
// provenanceWithOriginalHash 保证），enhance_tag/enhanced_at 每次覆盖为最新一次。
func provenanceWithEnhance(raw, originalHash, enhanceTag string, now int64) string {
	out := provenanceWithOriginalHash(raw, originalHash)
	kv := map[string]any{"enhanced_at": now}
	if tag := strings.TrimSpace(enhanceTag); tag != "" {
		kv["enhance_tag"] = tag
	}
	return provenanceFields(out, kv)
}

// provenanceWithRestoredAt 在 provenance 上追加回滚时间，原有键（含 original_hash）保留。
func provenanceWithRestoredAt(raw string, now int64) string {
	return provenanceFields(raw, map[string]any{"enhance_restored_at": now})
}

// provenanceFields 在现有 provenance 上写入若干键并返回紧凑 JSON。
// 旧行的自由文本 provenance 不丢，包在 legacy 键下保留（与 provenanceWithOriginalHash 同口径）。
func provenanceFields(raw string, kv map[string]any) string {
	m := map[string]any{}
	if s := strings.TrimSpace(raw); s != "" {
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			m = map[string]any{"legacy": s}
		}
	}
	for k, v := range kv {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return string(b)
}
