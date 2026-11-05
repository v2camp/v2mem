package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// evidenceColumns 是 T1.0 的旧库补列清单。
// 新库由 schema.sql 直接创建完整列；旧库只能在 Open 时幂等 ALTER。
var evidenceColumns = []struct {
	table  string
	column string
	ddl    string
}{
	{"memories", "scope", "TEXT NOT NULL DEFAULT ''"},
	{"memories", "adopted_count", "INTEGER NOT NULL DEFAULT 0"},
	{"memories", "last_adopted_at", "INTEGER"},

	{"observations", "event_uid", "TEXT NOT NULL DEFAULT ''"},
	{"observations", "source_id", "TEXT NOT NULL DEFAULT ''"},
	{"observations", "source_generation", "INTEGER NOT NULL DEFAULT 0"},
	{"observations", "source_seq", "INTEGER NOT NULL DEFAULT 0"},
	{"observations", "occurred_at_ns", "INTEGER NOT NULL DEFAULT 0"},
	{"observations", "archive_hash", "TEXT NOT NULL DEFAULT ''"},
	{"observations", "raw_record_hash", "TEXT NOT NULL DEFAULT ''"},
	{"observations", "parser_version", "TEXT NOT NULL DEFAULT ''"},

	{"candidates", "run_id", "TEXT NOT NULL DEFAULT ''"},
	{"candidates", "bundle_id", "TEXT NOT NULL DEFAULT ''"},
	{"candidates", "proposal_index", "INTEGER NOT NULL DEFAULT 0"},
	{"candidates", "scope", "TEXT NOT NULL DEFAULT ''"},
	{"candidates", "subject_key", "TEXT NOT NULL DEFAULT ''"},
	{"candidates", "task_state", "TEXT NOT NULL DEFAULT ''"},
	{"candidates", "rationale", "TEXT NOT NULL DEFAULT ''"},
	{"candidates", "review_class", "TEXT NOT NULL DEFAULT ''"},
	{"candidates", "revision", "INTEGER NOT NULL DEFAULT 0"},
	{"candidates", "schema_version", "INTEGER NOT NULL DEFAULT 0"},

	{"distill_runs", "bundle_id", "TEXT NOT NULL DEFAULT ''"},
	{"distill_runs", "executor", "TEXT NOT NULL DEFAULT ''"},
	{"distill_runs", "model", "TEXT NOT NULL DEFAULT ''"},
	{"distill_runs", "model_version", "TEXT NOT NULL DEFAULT ''"},
	{"distill_runs", "prompt_version", "TEXT NOT NULL DEFAULT ''"},
	{"distill_runs", "schema_version", "INTEGER NOT NULL DEFAULT 0"},
	{"distill_runs", "generated_count", "INTEGER NOT NULL DEFAULT 0"},
	{"distill_runs", "valid_count", "INTEGER NOT NULL DEFAULT 0"},
	{"distill_runs", "invalid_count", "INTEGER NOT NULL DEFAULT 0"},
	{"distill_runs", "duplicate_count", "INTEGER NOT NULL DEFAULT 0"},
	{"distill_runs", "attempt", "INTEGER NOT NULL DEFAULT 0"},
	{"distill_runs", "source_fingerprint", "TEXT NOT NULL DEFAULT ''"},
}

// ensureEvidenceSchema 幂等完成 T1.0 Evidence schema 迁移。
//
// 旧库的 ux_obs_dedup 必须显式 DROP：schema.sql 里的 CREATE IF NOT EXISTS
// 不会删除已存在的对象。T1.0 先降级成只覆盖 legacy 行的部分索引，
// 保留旧 AppendObservation 的内容去重语义；T1.1 接管写入后再彻底移除。
func ensureEvidenceSchema(db *sql.DB) error {
	for _, c := range evidenceColumns {
		if err := ensureColumn(db, c.table, c.column, c.ddl); err != nil {
			return fmt.Errorf("补列 %s.%s: %w", c.table, c.column, err)
		}
	}
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS ux_obs_dedup`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_obs_dedup_legacy
			ON observations(session_id, content_hash)
			WHERE event_uid = '' AND source_id = ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_obs_event_uid
			ON observations(event_uid) WHERE event_uid <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_obs_source_seq
			ON observations(source_id, source_generation, source_seq)
			WHERE source_id <> '' AND source_seq > 0`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("建 Evidence 索引: %w", err)
		}
	}
	return markLegacyOnce(db)
}

// markLegacyOnce 给迁移前的行加“无原始证据”标记。
//
// 只写独立的 migration_marks，不给 observations/memories 伪造 source_seq、
// archive_hash 或 occurred_at_ns。PRAGMA user_version 作为一次性门控；
// 重复 Open 只会在 INSERT OR IGNORE 上命中已有标记，不会误标新行。
func markLegacyOnce(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 1 {
		return nil
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO migration_marks(entity_type, entity_id, mark, marked_at)
		SELECT 'memory', id, 'legacy/no_original_evidence', ? FROM memories`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO migration_marks(entity_type, entity_id, mark, marked_at)
		SELECT 'observation', id, 'legacy_unpositioned', ? FROM observations`, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		return err
	}
	return nil
}
