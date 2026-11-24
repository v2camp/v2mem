package store

// summary/summary_idx 的外置读写原语。放在 store 包是为了让写侧与迁移都能用，
// 又不让 store 反向依赖 evidence（evidence 已经依赖 store）。文件 IO 只发生在这里。

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// SummaryLine 是一个外置内容行。字段顺序固定，hash 只对这串规范 JSON 字节计算，
// 不含结尾换行；分片 append 不会让已写行的 hash 失效。
type SummaryLine struct {
	V          int    `json:"v"`
	Seq        int    `json:"seq"`
	ID         string `json:"id"`
	Summary    string `json:"summary"`
	SummaryIdx string `json:"summary_idx"`
}

func EncodeSummaryLine(l SummaryLine) ([]byte, error) {
	return json.Marshal(l)
}

func ParseSummaryLine(b []byte) (SummaryLine, error) {
	var l SummaryLine
	if err := json.Unmarshal(b, &l); err != nil {
		return SummaryLine{}, err
	}
	return l, nil
}

func SummaryLineHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SummaryContentRoot 返回解析外置 ref 的数据根；ref 自身已含 content/ 前缀。
func SummaryContentRoot(dbPath string) string {
	return filepath.Dir(dbPath)
}

func dayRelPath(ts int64) string {
	return filepath.Join("content", time.Unix(ts, 0).Format("2006/01/02"), "observations.jsonl.gz")
}

func summaryRefOf(ts int64, seq int) string {
	return filepath.ToSlash(dayRelPath(ts)) + "#" + strconv.Itoa(seq)
}

// AppendSummaryLine 把一行作为独立 gzip member 追加到当日分片。
// 每个 member 独立压缩，gzip.Reader 默认跨 member 读取，历史字节永不被重写。
func AppendSummaryLine(root string, ts int64, l SummaryLine) (ref, hash string, payloadBytes int, err error) {
	if l.Seq <= 0 {
		return "", "", 0, errors.New("外置内容 seq 必须为正数")
	}
	b, err := EncodeSummaryLine(l)
	if err != nil {
		return "", "", 0, err
	}
	ref = summaryRefOf(ts, l.Seq)
	path := resolveSummaryPath(root, filepath.ToSlash(dayRelPath(ts)))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", "", 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return "", "", 0, err
	}
	cleanup := func() {
		f.Close()
	}
	zw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		cleanup()
		return "", "", 0, err
	}
	zw.Header.ModTime = time.Time{}
	zw.Header.OS = 255
	if _, err := zw.Write(b); err != nil {
		cleanup()
		return "", "", 0, err
	}
	if _, err := zw.Write([]byte("\n")); err != nil {
		cleanup()
		return "", "", 0, err
	}
	if err := zw.Close(); err != nil {
		cleanup()
		return "", "", 0, err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", "", 0, err
	}
	if err := f.Close(); err != nil {
		return "", "", 0, err
	}
	return ref, SummaryLineHash(b), len(b), nil
}

// WriteSummaryDay 把当日全部行写成一个 gzip member（迁移用），返回与 lines 等长的 ref/hash。
func WriteSummaryDay(root string, ts int64, lines []SummaryLine) (refs, hashes []string, payloads []int, err error) {
	if len(lines) == 0 {
		return nil, nil, nil, nil
	}
	path := resolveSummaryPath(root, dayRelPath(ts))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, nil, nil, err
	}
	f, err := os.Create(path) // 迁移只在空目录下运行；已存在视为重跑前的残留，由 CLI 拒绝
	if err != nil {
		return nil, nil, nil, err
	}
	zw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	zw.Header.ModTime = time.Time{}
	zw.Header.OS = 255
	for _, l := range lines {
		b, e := EncodeSummaryLine(l)
		if e != nil {
			zw.Close()
			f.Close()
			return nil, nil, nil, e
		}
		if _, e = zw.Write(b); e != nil {
			zw.Close()
			f.Close()
			return nil, nil, nil, e
		}
		if _, e = zw.Write([]byte("\n")); e != nil {
			zw.Close()
			f.Close()
			return nil, nil, nil, e
		}
		refs = append(refs, summaryRefOf(ts, l.Seq))
		hashes = append(hashes, SummaryLineHash(b))
		payloads = append(payloads, len(b))
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	return refs, hashes, payloads, f.Close()
}

// ReadSummaryLine 按 "<path>#<seq>" 从分片中取回一行，并核对行级 hash。
func ReadSummaryLine(root, ref, wantHash string) (SummaryLine, error) {
	path, seq, err := parseSummaryRef(ref)
	if err != nil {
		return SummaryLine{}, err
	}
	if wantHash == "" {
		return SummaryLine{}, errors.New("外置内容引用缺少 hash")
	}
	f, err := os.Open(resolveSummaryPath(root, path))
	if err != nil {
		return SummaryLine{}, fmt.Errorf("打开外置内容失败: %w", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return SummaryLine{}, fmt.Errorf("打开 gzip 分片失败: %w", err)
	}
	defer zr.Close()
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for i := 1; sc.Scan(); i++ {
		line := sc.Bytes()
		if i != seq {
			continue
		}
		if got := SummaryLineHash(line); got != wantHash {
			return SummaryLine{}, fmt.Errorf("外置内容 hash 不匹配: %s#%d", path, seq)
		}
		return ParseSummaryLine(line)
	}
	if err := sc.Err(); err != nil {
		return SummaryLine{}, fmt.Errorf("读外置内容失败: %w", err)
	}
	return SummaryLine{}, fmt.Errorf("外置内容找不到 seq=%d: %s", seq, path)
}

func parseSummaryRef(ref string) (string, int, error) {
	s := bytes.TrimSpace([]byte(ref))
	i := bytes.LastIndexByte(s, '#')
	if i < 0 {
		return "", 0, errors.New("外置内容 locator 缺少 #seq")
	}
	seq, err := strconv.Atoi(string(bytes.TrimSpace(s[i+1:])))
	if err != nil || seq <= 0 {
		return "", 0, fmt.Errorf("外置内容 locator 的 seq 无效: %q", ref)
	}
	path := string(bytes.TrimSpace(s[:i]))
	if path == "" {
		return "", 0, errors.New("外置内容 locator 缺少路径")
	}
	return path, seq, nil
}

func resolveSummaryPath(root, ref string) string {
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}
	return filepath.Join(root, filepath.FromSlash(ref))
}

// NextSummarySeq 取当日下一个序号。独立事务：写侧调用后若 DB 插入失败会留下序号空洞，
// 空洞无害；关键是同一分片里不会有两个行共用 seq。
func NextSummarySeq(db *sql.DB, day string) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var seq int
	if err := tx.QueryRow(`SELECT seq FROM summary_seq WHERE day = ?`, day).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	seq++
	if _, err := tx.Exec(`INSERT INTO summary_seq(day, seq) VALUES (?, ?)
		ON CONFLICT(day) DO UPDATE SET seq = excluded.seq`, day, seq); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// nextSummarySeqTx 与 captureEvidenceTx 共用外部事务，避免嵌套 Begin 抢写锁。
func nextSummarySeqTx(tx *sql.Tx, day string) (int, error) {
	var seq int
	if err := tx.QueryRow(`SELECT seq FROM summary_seq WHERE day = ?`, day).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	seq++
	if _, err := tx.Exec(`INSERT INTO summary_seq(day, seq) VALUES (?, ?)
		ON CONFLICT(day) DO UPDATE SET seq = excluded.seq`, day, seq); err != nil {
		return 0, err
	}
	return seq, nil
}

func SetSummarySeq(db *sql.DB, day string, seq int) error {
	_, err := db.Exec(`INSERT INTO summary_seq(day, seq) VALUES (?, ?)
		ON CONFLICT(day) DO UPDATE SET seq = excluded.seq`, day, seq)
	return err
}

// ensureSummaryExternalSchema 只做 DDL：新库由 schema.sql 建好，旧库幂等补列。
func ensureSummaryExternalSchema(db *sql.DB) error {
	for _, c := range []struct{ col, typ string }{
		{"summary_ref", "TEXT NOT NULL DEFAULT ''"},
		{"summary_hash", "TEXT NOT NULL DEFAULT ''"},
		{"payload_bytes", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := ensureColumn(db, "observations", c.col, c.typ); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS summary_seq (
		day TEXT PRIMARY KEY, seq INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS obs_fts_map (
		rowid INTEGER PRIMARY KEY, obs_id TEXT NOT NULL UNIQUE
	)`)
	return err
}

// summaryExternalMigrated 看迁移完成标记；未迁移时 Open 保留旧 FTS 路径，不改数据。
func summaryExternalMigrated(db *sql.DB) bool {
	var v string
	return db.QueryRow(`SELECT v FROM db_meta WHERE k = 'summary_external_version'`).Scan(&v) == nil
}
