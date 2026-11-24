package store

// 存量 summary / summary_idx 的一次性迁移。显式 CLI 调用，不在 Open 里批量写数据。

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type SummaryMigrationStats struct {
	Migrated    int   `json:"migrated"`
	BytesBefore int64 `json:"bytes_before"`
	BytesAfter  int64 `json:"bytes_after"`
	PagesBefore int64 `json:"pages_before"`
	PagesAfter  int64 `json:"pages_after"`
	Shards      int   `json:"shards"`
	AlreadyDone bool  `json:"already_done"`
}

type pendingSummary struct {
	id   string
	day  string
	line SummaryLine
}

// MigrateSummaryExternal 把两列正文写入 content/ 分片，DB 只留 ref/hash/payload_bytes。
// dryRun 只压缩到内存估算体积，不落盘、不改库。
func (s *Store) MigrateSummaryExternal(dryRun bool) (SummaryMigrationStats, error) {
	var stats SummaryMigrationStats
	stats.PagesBefore = dbPages(s.db)
	if summaryExternalMigrated(s.db) {
		stats.AlreadyDone = true
		stats.PagesAfter = stats.PagesBefore
		return stats, nil
	}

	rows, err := s.db.Query(`SELECT id, created_at, summary, summary_idx FROM observations
		WHERE summary_ref = '' AND (summary <> '' OR summary_idx <> '')
		ORDER BY created_at, id`)
	if err != nil {
		return stats, err
	}
	var pending []pendingSummary
	seqByDay := map[string]int{}
	for rows.Next() {
		var p pendingSummary
		var ts int64
		if err := rows.Scan(&p.id, &ts, &p.line.Summary, &p.line.SummaryIdx); err != nil {
			rows.Close()
			return stats, err
		}
		p.day = time.Unix(ts, 0).Format("2006/01/02")
		seqByDay[p.day]++
		p.line = SummaryLine{V: 1, Seq: seqByDay[p.day], ID: p.id,
			Summary: p.line.Summary, SummaryIdx: p.line.SummaryIdx}
		stats.BytesBefore += int64(len(p.line.Summary) + len(p.line.SummaryIdx))
		pending = append(pending, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, err
	}
	rows.Close()
	stats.Migrated = len(pending)
	stats.Shards = len(seqByDay)
	if len(pending) == 0 {
		stats.PagesAfter = stats.PagesBefore
		return stats, nil
	}

	byDay := map[string][]SummaryLine{}
	for _, p := range pending {
		byDay[p.day] = append(byDay[p.day], p.line)
	}

	if dryRun {
		for _, lines := range byDay {
			b, err := gzipLines(lines)
			if err != nil {
				return stats, err
			}
			stats.BytesAfter += int64(len(b))
		}
		stats.PagesAfter = stats.PagesBefore
		return stats, nil
	}

	root := SummaryContentRoot(s.path)
	// 目标分片必须不存在：避免把一次失败迁移的残留当成成功的新分片覆写。
	for day := range byDay {
		path := filepath.Join(root, filepath.FromSlash(dayRelPathForDay(day)))
		if _, err := os.Stat(path); err == nil {
			return stats, fmt.Errorf("外置分片已存在，先清理再重跑: %s", path)
		} else if !os.IsNotExist(err) {
			return stats, err
		}
	}

	type written struct {
		id    string
		ref   string
		hash  string
		bytes int
	}
	var all []written
	days := sortedDays(byDay)
	for _, day := range days {
		ts, err := dayStartUnix(day)
		if err != nil {
			return stats, err
		}
		refs, hashes, payloads, err := WriteSummaryDay(root, ts, byDay[day])
		if err != nil {
			return stats, err
		}
		stats.BytesAfter += int64(fileSize(filepath.Join(root, filepath.FromSlash(dayRelPathForDay(day)))))
		for i, line := range byDay[day] {
			all = append(all, written{id: line.ID, ref: refs[i], hash: hashes[i], bytes: payloads[i]})
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()
	for _, trigger := range []string{"observations_ai", "observations_ad", "observations_au"} {
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS ` + trigger); err != nil {
			return stats, err
		}
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS fts_obs`); err != nil {
		return stats, err
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS obs_fts_map`); err != nil {
		return stats, err
	}
	if _, err := tx.Exec(`CREATE TABLE obs_fts_map (rowid INTEGER PRIMARY KEY, obs_id TEXT NOT NULL UNIQUE)`); err != nil {
		return stats, err
	}
	if _, err := tx.Exec(obsFTSCreateStandalone); err != nil {
		return stats, err
	}
	if _, err := tx.Exec(`INSERT INTO obs_fts_map(obs_id)
		SELECT id FROM observations WHERE summary_idx <> ''`); err != nil {
		return stats, err
	}
	if _, err := tx.Exec(`INSERT INTO fts_obs(rowid, summary_idx)
		SELECT m.rowid, o.summary_idx FROM obs_fts_map m
		JOIN observations o ON o.id = m.obs_id`); err != nil {
		return stats, err
	}
	for _, w := range all {
		if _, err := tx.Exec(`UPDATE observations
			SET summary = '', summary_idx = '', summary_ref = ?, summary_hash = ?, payload_bytes = ?
			WHERE id = ?`, w.ref, w.hash, w.bytes, w.id); err != nil {
			return stats, err
		}
	}
	for _, day := range days {
		if err := setSummarySeqTx(tx, day, seqByDay[day]); err != nil {
			return stats, err
		}
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	for k, v := range map[string]string{
		"summary_external_version": "1",
		"summary_external_rows":    strconv.Itoa(len(all)),
		"summary_external_at":      now,
		metaKeySchemaVersion:       strconv.Itoa(CurrentSchemaVersion),
	} {
		if _, err := tx.Exec(`INSERT INTO db_meta(k, v) VALUES (?, ?)
			ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v); err != nil {
			return stats, err
		}
	}
	if err := tx.Commit(); err != nil {
		return stats, err
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return stats, err
	}
	s.summaryExternal = true
	stats.PagesAfter = dbPages(s.db)
	return stats, nil
}

func dbPages(db *sql.DB) int64 {
	var n int64
	if err := db.QueryRow(`SELECT SUM(pgsize) FROM dbstat`).Scan(&n); err != nil {
		return 0
	}
	return n
}

func sortedDays(byDay map[string][]SummaryLine) []string {
	days := make([]string, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	sort.Strings(days)
	return days
}

func setSummarySeqTx(tx *sql.Tx, day string, seq int) error {
	_, err := tx.Exec(`INSERT INTO summary_seq(day, seq) VALUES (?, ?)
		ON CONFLICT(day) DO UPDATE SET seq = excluded.seq`, day, seq)
	return err
}

func dayRelPathForDay(day string) string {
	return filepath.Join("content", day, "observations.jsonl.gz")
}

func dayStartUnix(day string) (int64, error) {
	t, err := time.ParseInLocation("2006/01/02", day, time.Local)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

func gzipLines(lines []SummaryLine) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	zw.Header.ModTime = time.Time{}
	zw.Header.OS = 255
	for _, l := range lines {
		b, err := EncodeSummaryLine(l)
		if err != nil {
			return nil, err
		}
		if _, err := zw.Write(b); err != nil {
			return nil, err
		}
		if _, err := zw.Write([]byte("\n")); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
