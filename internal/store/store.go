// Package store 是 v2mem 的本地记忆仓库：SQLite 单文件 + FTS5 全文检索。
//
// 设计约束：
//   - 零外部服务、零 CGO（驱动用 modernc.org/sqlite，纯 Go 转译）
//   - 写入路径必须便宜：只做 hash 去重 + FTS5 索引
//   - 检索必须快：<10ms，只走 FTS5，不依赖任何模型
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Store 持有一个到本地记忆库的连接。
type Store struct {
	db   *sql.DB
	path string
}

// DefaultPath 返回默认库路径：~/.v2mem/mem.db。
// 数据与代码分离——绝不放在代码目录里，避免被 git clean 或删库带走。
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mem.db"
	}
	return filepath.Join(home, ".v2mem", "mem.db")
}

// Open 打开（必要时创建）记忆库并建表。
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// WAL：单写者 + 多读者，支撑多工具并发；busy_timeout 避免瞬时锁冲突。
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("执行 %s 失败: %w", pragma, err)
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

// ---------- 归一化与哈希 ----------

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ") // 折叠所有空白
	s = strings.TrimRight(s, "。．.!！?？;；,，:：")
	return s
}

// HashOf 返回归一化内容的 sha256，用于「相同知识覆盖」。
func HashOf(content string) string {
	sum := sha256.Sum256([]byte(normalize(content)))
	return hex.EncodeToString(sum[:])
}

// ---------- 中文索引 ----------
//
// FTS5 自带分词器都不适用于中文：unicode61 把整串汉字当成单个 token；
// trigram 有 3 字符下限，二字词（目录/路径/配置）永远查不到。
// 这里自建索引：连续汉字切成一元组 + 二元组，拉丁/数字按词保留。
// 一元组支撑单字查询，二元组支撑词组的精确匹配。

func isHan(r rune) bool { return unicode.Is(unicode.Han, r) }

// scanTokens 把文本切成 (汉字串, 拉丁数字串) 交替的片段序列。
func scanTokens(s string, onHan func([]rune), onWord func([]rune)) {
	var han, word []rune
	flushHan := func() {
		if len(han) > 0 {
			onHan(han)
			han = han[:0]
		}
	}
	flushWord := func() {
		if len(word) > 0 {
			onWord(word)
			word = word[:0]
		}
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case isHan(r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word = append(word, r)
		default:
			flushHan()
			flushWord()
		}
	}
	flushHan()
	flushWord()
}

// indexText 生成 content_idx：汉字展开为一元组+二元组，拉丁/数字原样成词。
func indexText(s string) string {
	var toks []string
	scanTokens(s,
		func(han []rune) {
			for i := range han { // 一元组：支撑单字查询
				toks = append(toks, string(han[i]))
			}
			for i := 0; i+1 < len(han); i++ { // 二元组：支撑词组精确匹配
				toks = append(toks, string(han[i:i+2]))
			}
		},
		func(word []rune) { toks = append(toks, string(word)) },
	)
	return strings.Join(toks, " ")
}

// queryExpr 把用户输入编译成 FTS5 MATCH 表达式。
// 语义：词组内部 AND（精确），词组之间 OR（记忆检索以召回优先，靠 bm25 排序）。
// 所有词都用 strconv.Quote 包成字面量，避免特殊字符破坏 FTS5 语法。
func queryExpr(q string) string {
	var groups []string
	scanTokens(q,
		func(han []rune) {
			var terms []string
			if len(han) == 1 {
				terms = append(terms, strconv.Quote(string(han)))
			} else {
				for i := 0; i+1 < len(han); i++ {
					terms = append(terms, strconv.Quote(string(han[i:i+2])))
				}
			}
			groups = append(groups, "("+strings.Join(terms, " AND ")+")")
		},
		func(word []rune) {
			groups = append(groups, strconv.Quote(string(word))+"*") // 前缀匹配
		},
	)
	return strings.Join(groups, " OR ")
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ---------- 写入 ----------

// AddInput 是写入一条记忆的参数。
type AddInput struct {
	Content  string            // 原子事实，不是文档
	Kind     string            // preference|decision|pitfall|task|fact
	Project  string            // 工程标记
	Tool     string            // claude-code|workbuddy|cursor|cli
	Device   string            // 来源设备，默认取 hostname
	Tags     map[string]string // 多机/多工具/多工程标记
	Salience float64           // 重要性 0..1
	TTL      time.Duration     // 硬过期，0 表示不过期
}

// AddResult 描述写入结果。Created=false 表示命中了「相同知识覆盖」。
type AddResult struct {
	ID      string `json:"id"`
	Created bool   `json:"created"`
	Hash    string `json:"hash"`
}

// Add 写入一条记忆。已存在相同（hash, project）时执行覆盖而非新增。
func (s *Store) Add(in AddInput) (*AddResult, error) {
	in.Content = strings.TrimSpace(in.Content)
	if in.Content == "" {
		return nil, errors.New("内容为空")
	}
	if in.Kind == "" {
		in.Kind = "fact"
	}
	if in.Salience <= 0 {
		in.Salience = 0.5
	}
	if in.Device == "" {
		in.Device, _ = os.Hostname()
	}

	h := HashOf(in.Content)
	idx := indexText(in.Content)
	now := time.Now().Unix()
	var expires any // nil → 不过期
	if in.TTL > 0 {
		expires = time.Now().Add(in.TTL).Unix()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRow(
		`SELECT id FROM memories WHERE content_hash = ? AND project = ?`, h, in.Project,
	).Scan(&id)

	created := false
	switch {
	case err == nil:
		// 相同知识覆盖：刷新时间戳、累加命中、salience 取较大值
		if _, err = tx.Exec(
			`UPDATE memories SET content = ?, content_idx = ?, updated_at = ?, last_seen_at = ?,
			        access_count = access_count + 1, salience = MAX(salience, ?),
			        expires_at = ?, origin_tool = ?
			 WHERE id = ?`,
			in.Content, idx, now, now, in.Salience, expires, in.Tool, id,
		); err != nil {
			return nil, err
		}
	case errors.Is(err, sql.ErrNoRows):
		created = true
		id = newID()
		if _, err = tx.Exec(
			`INSERT INTO memories
			   (id, content, content_idx, kind, content_hash, project, salience,
			    created_at, updated_at, last_seen_at, expires_at,
			    origin_device, origin_tool, access_count)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			id, in.Content, idx, in.Kind, h, in.Project, in.Salience,
			now, now, now, expires, in.Device, in.Tool,
		); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	for k, v := range in.Tags {
		if _, err = tx.Exec(
			`INSERT OR IGNORE INTO tags(memory_id, key, value) VALUES (?,?,?)`, id, k, v,
		); err != nil {
			return nil, err
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &AddResult{ID: id, Created: created, Hash: h}, nil
}

// ---------- 检索 ----------

// SearchQuery 是检索参数。
type SearchQuery struct {
	Query   string            // FTS5 MATCH 表达式
	Project string            // 空表示不限
	Kind    string            // 空表示不限
	Tags    map[string]string // 标记过滤：AND 语义，需全部满足
	Scope   string            // ""|"all" 不限工程；"current" = 当前工程 + 全局（project 为空）；"global" = 只要全局
	Limit   int
}

// Hit 是一条检索结果。
type Hit struct {
	ID        string  `json:"id"`
	Content   string  `json:"content"`
	Kind      string  `json:"kind"`
	Project   string  `json:"project"`
	Salience  float64 `json:"salience"`
	UpdatedAt int64   `json:"updated_at"`
	Rank      float64 `json:"rank"` // bm25，越小越相关
}

// Search 走 FTS5 全文检索。已过期的记忆自动排除。
// bm25 返回负值，ORDER BY rank ASC 即「最相关在前」。
func (s *Store) Search(q SearchQuery) ([]Hit, error) {
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" {
		return nil, errors.New("查询为空")
	}
	expr := queryExpr(q.Query)
	if expr == "" {
		return nil, errors.New("查询不含可检索内容（需至少含一个汉字或字母数字）")
	}
	if q.Limit <= 0 {
		q.Limit = 10
	}

	now := time.Now().Unix()
	// 可见性判据与 List 共用同一函数，避免两处各写一遍后漂移
	where := append([]string{"fts_mem MATCH ?"}, visibilityWhere()...)
	args := []any{expr, now}

	// 作用域用显式 switch：写成一串布尔判断时「all + 非空 Project」
	// 会落进 Project 分支，使 --scope all 静默失效。
	switch q.Scope {
	case "current":
		// project 为空的记忆视为全局，对所有工程生效
		where = append(where, "(m.project = ? OR m.project = '')")
		args = append(args, q.Project)
	case "global":
		// 只看全局记忆：跨工程硬规则需要能被单独列出，不被工程噪声淹没
		where = append(where, "m.project = ''")
	case "all":
		// 显式跨工程：忽略 Project。CLI 侧会把「--scope all + --project」判为矛盾用法，
		// 以免用户以为过滤生效了而实际没有。
	default:
		if q.Project != "" {
			where = append(where, "m.project = ?")
			args = append(args, q.Project)
		}
	}
	if q.Kind != "" {
		where = append(where, "m.kind = ?")
		args = append(args, q.Kind)
	}
	for k, v := range q.Tags {
		where = append(where, "EXISTS (SELECT 1 FROM tags WHERE memory_id = m.id AND key = ? AND value = ?)")
		args = append(args, k, v)
	}
	args = append(args, q.Limit)

	rows, err := s.db.Query(
		`SELECT m.id, m.content, m.kind, m.project, m.salience, m.updated_at,
		        bm25(fts_mem) AS rank
		   FROM fts_mem
		   JOIN memories m ON m.rowid = fts_mem.rowid
		  WHERE `+strings.Join(where, " AND ")+`
		  ORDER BY rank ASC, m.salience DESC, m.last_seen_at DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ID, &h.Content, &h.Kind, &h.Project, &h.Salience, &h.UpdatedAt, &h.Rank); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---------- 生命周期：命中反馈 / 淘汰 / 删除 ----------

// resolveID 支持完整 ID 或前缀（CLI 输出的是 8 位短 ID）。
func (s *Store) resolveID(idOrPrefix string) (string, error) {
	idOrPrefix = strings.TrimSpace(idOrPrefix)
	if idOrPrefix == "" {
		return "", errors.New("ID 为空")
	}
	var id string
	err := s.db.QueryRow(`SELECT id FROM memories WHERE id = ?`, idOrPrefix).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	rows, err := s.db.Query(`SELECT id FROM memories WHERE id LIKE ?`, idOrPrefix+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return "", err
		}
		ids = append(ids, x)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("未找到记忆: %s", idOrPrefix)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("前缀 %q 歧义，匹配到 %d 条，请使用更长的 ID", idOrPrefix, len(ids))
	}
}

// Touch 记录一次命中：刷新 last_seen_at 并累加 access_count（衰减与排序的依据）。
func (s *Store) Touch(idOrPrefix string) (string, error) {
	id, err := s.resolveID(idOrPrefix)
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(
		`UPDATE memories SET last_seen_at = ?, access_count = access_count + 1 WHERE id = ?`,
		time.Now().Unix(), id,
	); err != nil {
		return "", err
	}
	return id, nil
}

// Forget 显式删除一条记忆（tags 经外键级联删除）。
func (s *Store) Forget(idOrPrefix string) (string, error) {
	id, err := s.resolveID(idOrPrefix)
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`DELETE FROM memories WHERE id = ?`, id); err != nil {
		return "", err
	}
	return id, nil
}

// GCResult 描述一次回收的结果。
type GCResult struct {
	Expired int `json:"expired"` // 硬过期（TTL 到期）
	Decayed int `json:"decayed"` // 久未命中且重要性低
	Kept    int `json:"kept"`
}

// GC 回收两类记忆：TTL 到期的，以及久未命中且 salience 低于阈值的。
// 高 salience 的记忆即使久未命中也保留——重要性应当能抵御遗忘。
func (s *Store) GC(maxIdle time.Duration, minSalience float64) (*GCResult, error) {
	if maxIdle <= 0 {
		maxIdle = 720 * time.Hour // 默认 30 天
	}
	if minSalience <= 0 {
		minSalience = 0.2
	}
	now := time.Now().Unix()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res := &GCResult{}

	del := func(where string, args ...any) (int, error) {
		r, err := tx.Exec("DELETE FROM memories WHERE "+where, args...)
		if err != nil {
			return 0, err
		}
		n, err := r.RowsAffected()
		return int(n), err
	}

	expired, err := del(`expires_at IS NOT NULL AND expires_at <= ?`, now)
	if err != nil {
		return nil, err
	}
	decayed, err := del(`last_seen_at < ? AND salience < ?`, now-int64(maxIdle.Seconds()), minSalience)
	if err != nil {
		return nil, err
	}
	res.Expired, res.Decayed = expired, decayed

	if err := tx.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&res.Kept); err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

// ---------- 统计 ----------

// Stats 是库的概览信息。
type Stats struct {
	Path        string         `json:"path"`
	SizeBytes   int64          `json:"size_bytes"`
	Total       int            `json:"total"`
	Live        int            `json:"live"`
	Superseded  int            `json:"superseded"`
	ByKind      map[string]int `json:"by_kind"`
	ByProject   map[string]int `json:"by_project"`
	ExpiredLive int            `json:"expired_pending_gc"`
}

// visibilityWhere 返回「一条记忆是否对检索可见」的条件。
//
// Search 与 List 必须共用同一份判据：两处各写一遍迟早会漂移，
// 而漂移的表现是「搜得到但列不出」或反之，极难察觉。
func visibilityWhere() []string {
	return []string{
		"(m.expires_at IS NULL OR m.expires_at > ?)",
		// 已被相似归并取代的记忆不再出现：否则用户会同时看到两条互相矛盾的答案。
		// 它们仍留在库里（可由 export 带走、可追溯），只是不参与检索。
		"m.superseded_by IS NULL",
	}
}

// ListQuery 是「不依赖全文检索」的列举条件。
//
// 与 SearchQuery 的分工：Search 要匹配词，List 只要「最重要 / 最近的」。
// 会话起始注入需要的是后者 —— 那时还没有用户提问，没有关键词可用。
type ListQuery struct {
	Scope   string // "current" = 本工程 + 全局；"project" = 只本工程；"global" = 只要全局；其他 → 不限
	Project string
	Limit   int
}

// List 按重要性降序列出可见记忆（同 salience 时取最近命中的在前）。
func (s *Store) List(q ListQuery) ([]Hit, error) {
	if q.Limit <= 0 {
		q.Limit = 10
	}
	args := []any{time.Now().Unix()}
	where := visibilityWhere()

	switch q.Scope {
	case "current":
		where = append(where, "(m.project = ? OR m.project = '')")
		args = append(args, q.Project)
	case "project":
		// 只取本工程，不含全局 —— 供「硬规则」与「本工程记忆」分段注入时去重
		where = append(where, "m.project = ?")
		args = append(args, q.Project)
	case "global":
		where = append(where, "m.project = ''")
	}
	args = append(args, q.Limit)

	rows, err := s.db.Query(
		`SELECT m.id, m.content, m.kind, m.project, m.salience, m.updated_at, 0.0 AS rank
		   FROM memories m
		  WHERE `+strings.Join(where, " AND ")+`
		  ORDER BY m.salience DESC, m.last_seen_at DESC, m.updated_at DESC, m.id ASC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ID, &h.Content, &h.Kind, &h.Project, &h.Salience, &h.UpdatedAt, &h.Rank); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) Stats() (*Stats, error) {
	st := &Stats{Path: s.path, ByKind: map[string]int{}, ByProject: map[string]int{}}

	if fi, err := os.Stat(s.path); err == nil {
		st.SizeBytes = fi.Size()
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&st.Total); err != nil {
		return nil, err
	}
	// Live 与 Superseded 是检索可见性的口径：只有 superseded_by IS NULL
	// 且未过期的条目会出现在 search 结果里。
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE superseded_by IS NULL`,
	).Scan(&st.Live); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE superseded_by IS NOT NULL`,
	).Scan(&st.Superseded); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		time.Now().Unix(),
	).Scan(&st.ExpiredLive); err != nil {
		return nil, err
	}

	if rows, err := s.db.Query(`SELECT kind, COUNT(*) FROM memories GROUP BY kind`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err == nil {
				st.ByKind[k] = n
			}
		}
	}
	if rows, err := s.db.Query(`SELECT project, COUNT(*) FROM memories GROUP BY project`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var p string
			var n int
			if err := rows.Scan(&p, &n); err == nil {
				if p == "" {
					p = "(global)"
				}
				st.ByProject[p] = n
			}
		}
	}
	return st, nil
}
