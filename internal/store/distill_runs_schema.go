package store

// schema v4：distill_runs 去掉旧窗口时代的表级 UNIQUE(session_id, started_at)。
//
// 背景（U5 真实走查发现，生产库可复现）：该约束保护的是「按会话窗口推进蒸馏水位」
// 的旧语义，已随单轨切换消亡；真正还生效的窗口守卫是部分索引
// ux_distill_window(session_id, window_key) WHERE window_key <> ''。
// proposal runner 的 maintain --catch-up 会在同一秒连续处理多个同源窗口，
// 第 4 条运行账撞满 3 次 started_at 退避后直接报错退出。
//
// 迁移形态：SQLite 不支持 DROP CONSTRAINT，只能重建表。必须在
// ensureEvidenceSchema 之后执行——旧库的 distill_runs 缺 bundle_id 等后加列，
// 先由 ensureColumn 补齐，重建时才能按统一列集拷贝。

import (
	"database/sql"
	"fmt"
	"strings"
)

// distillRunsV4Columns 是重建表与 INSERT SELECT 共用的列序（与 schema.sql 一致）。
var distillRunsV4Columns = []string{
	"id", "session_id", "provider", "status", "error", "cand_count",
	"started_at", "finished_at", "window_key", "bundle_id", "executor",
	"model", "model_version", "prompt_version", "schema_version",
	"generated_count", "valid_count", "invalid_count", "duplicate_count",
	"attempt", "source_fingerprint",
}

const distillRunsV4DDL = `CREATE TABLE distill_runs_v4 (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL,
  provider      TEXT NOT NULL,
  status        TEXT NOT NULL,
  error         TEXT NOT NULL DEFAULT '',
  cand_count    INTEGER NOT NULL DEFAULT 0,
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER NOT NULL,
  window_key    TEXT NOT NULL DEFAULT '',
  bundle_id     TEXT NOT NULL DEFAULT '',
  executor      TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  model_version TEXT NOT NULL DEFAULT '',
  prompt_version TEXT NOT NULL DEFAULT '',
  schema_version INTEGER NOT NULL DEFAULT 0,
  generated_count INTEGER NOT NULL DEFAULT 0,
  valid_count   INTEGER NOT NULL DEFAULT 0,
  invalid_count INTEGER NOT NULL DEFAULT 0,
  duplicate_count INTEGER NOT NULL DEFAULT 0,
  attempt       INTEGER NOT NULL DEFAULT 0,
  source_fingerprint TEXT NOT NULL DEFAULT ''
)`

// ensureDistillRunsSchema 幂等移除 distill_runs 上的旧窗口唯一键。
// 新库来自 schema.sql，本来就没有该约束，这里只处理存量库。
func ensureDistillRunsSchema(db *sql.DB) error {
	legacy, err := hasLegacyDistillRunUnique(db)
	if err != nil {
		return err
	}
	if !legacy {
		return nil
	}

	cols := strings.Join(distillRunsV4Columns, ", ")
	// DROP TABLE 会带走旧表上的索引；这里按 v4 的已知索引集重建
	// （ux_distill_window 是窗口幂等守卫，ix_distill_bundle 是 GC 反查热点）。
	stmts := []string{
		distillRunsV4DDL,
		`INSERT INTO distill_runs_v4 (` + cols + `) SELECT ` + cols + ` FROM distill_runs`,
		`DROP TABLE distill_runs`,
		`ALTER TABLE distill_runs_v4 RENAME TO distill_runs`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_distill_window
			ON distill_runs(session_id, window_key) WHERE window_key <> ''`,
		`CREATE INDEX IF NOT EXISTS ix_distill_bundle ON distill_runs(bundle_id)`,
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("迁移 distill_runs 唯一键: 开启事务: %w", err)
	}
	defer tx.Rollback()
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("迁移 distill_runs 唯一键: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("迁移 distill_runs 唯一键: 提交: %w", err)
	}
	return nil
}

// hasLegacyDistillRunUnique 用索引元数据判断是否存在 UNIQUE(session_id, started_at)。
// 看索引而不是解析 DDL 文本：表级 UNIQUE 生成的 sqlite_autoindex 在不同版本里
// 的 DDL 排版可能变，但列组合不会。
func hasLegacyDistillRunUnique(db *sql.DB) (bool, error) {
	rows, err := db.Query(`SELECT name, "unique" FROM pragma_index_list('distill_runs')`)
	if err != nil {
		return false, fmt.Errorf("读 distill_runs 索引: %w", err)
	}
	var uniqueIndexes []string
	for rows.Next() {
		var name string
		var unique int
		if err := rows.Scan(&name, &unique); err != nil {
			rows.Close()
			return false, fmt.Errorf("扫描 distill_runs 索引: %w", err)
		}
		if unique == 1 {
			uniqueIndexes = append(uniqueIndexes, name)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("遍历 distill_runs 索引: %w", err)
	}
	rows.Close()

	for _, name := range uniqueIndexes {
		info, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name)
		if err != nil {
			return false, fmt.Errorf("读索引 %s 列: %w", name, err)
		}
		var cols []string
		for info.Next() {
			var col string
			if err := info.Scan(&col); err != nil {
				info.Close()
				return false, fmt.Errorf("扫描索引 %s 列: %w", name, err)
			}
			cols = append(cols, col)
		}
		if err := info.Err(); err != nil {
			info.Close()
			return false, fmt.Errorf("遍历索引 %s 列: %w", name, err)
		}
		info.Close()
		if len(cols) == 2 && cols[0] == "session_id" && cols[1] == "started_at" {
			return true, nil
		}
	}
	return false, nil
}
