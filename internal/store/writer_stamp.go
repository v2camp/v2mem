package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
)

// CurrentSchemaVersion 是当前二进制理解的数据结构版本。任何改变「读出来的数据形状」的
// 迁移都必须 +1，否则降级守卫失效。
const CurrentSchemaVersion = 1

// ErrDBSchemaNewer 表示库由更新的二进制写过，当前二进制只理解更旧的 schema。
// 调用方应直接用 errors.Is 判定并拒绝打开，而不是尝试降级读。
var ErrDBSchemaNewer = errors.New("数据库由更新版本的 v2mem 写入")

// db_meta 的键名。用文本键而不是 PRAGMA user_version 的原因见 EnsureWriterStamp 的注释。
const (
	metaKeySchemaVersion = "schema_version"
	metaKeyWrittenBy     = "written_by"
	metaKeyFirstSeen     = "first_seen"
)

// WriterStamp 描述一次打戳后库里的版本台账。
type WriterStamp struct {
	SchemaVersion int
	WrittenBy     string
	FirstSeen     int64
	// Fresh=true 表示本次是首次打戳（新库或存量库），此前库里没有任何版本信息。
	Fresh bool
}

// EnsureWriterStamp 幂等地建表、打戳并校验。返回 ErrDBSchemaNewer 表示必须拒绝打开。
//
// 为什么不复用 PRAGMA user_version：
//   - 它已经被 markLegacyOnce 当成「一次性迁移门控」占用（跑完无条件写 1），
//     语义是「这段迁移跑过没有」，不是「这个库是什么版本」。
//   - 两套语义挤同一个整数位必然打架：门控先写 1，版本戳再想往上加就被门控误判成
//     「已迁移」而跳过；反过来门控也会把版本戳当成自己的进度直接覆盖掉。
//   - db_meta 是键值表，后续加 last_open_by / migrated_at 都不用再改 DDL，也不必
//     再抢一个 PRAGMA 整数位。
//
// 为什么 DDL 写在 Go 里而不是 schema.sql：schema.sql 只对新建库生效
// （`CREATE TABLE IF NOT EXISTS` 不会给存量库补对象），而存量库恰恰是最需要
// 认版本的那批。这里用和 ensureColumn 同一套路的幂等 DDL 兜。
//
// 本函数只维护版本台账，不跑迁移：真正改变表结构的迁移仍由 Open 里的
// ensureColumn / ensureEvidenceSchema 系列负责。降级守卫的价值在于「旧二进制打开
// 新库时立刻报错」，而不是在这里补齐所有历史迁移。
func EnsureWriterStamp(ctx context.Context, db *sql.DB, binVersion string, now int64) (WriterStamp, error) {
	// 空版本号会让库里的 written_by 变成空串，也会让「库比二进制新」的报错
	// 读起来像缺了一段。BinVersion() 本身不会返回空串，这里只兜调用方手搓参数的情况。
	if binVersion == "" {
		binVersion = "devel"
	}

	// 建表 + 读 + 写放在同一个事务里：hook 每次会话会多次打开库，
	// 并发时若分多次自动提交，两个进程会互相覆盖 first_seen。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return WriterStamp{}, fmt.Errorf("写入者版本戳: 开启事务: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS db_meta (
		k TEXT PRIMARY KEY,
		v TEXT NOT NULL
	)`); err != nil {
		return WriterStamp{}, fmt.Errorf("写入者版本戳: 建 db_meta: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT k, v FROM db_meta WHERE k IN (?, ?, ?)`,
		metaKeySchemaVersion, metaKeyWrittenBy, metaKeyFirstSeen)
	if err != nil {
		return WriterStamp{}, fmt.Errorf("写入者版本戳: 读 db_meta: %w", err)
	}
	var (
		rawVersion, rawFirstSeen string
		hasVersion, hasFirstSeen bool
	)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return WriterStamp{}, fmt.Errorf("写入者版本戳: 扫描 db_meta: %w", err)
		}
		switch k {
		case metaKeySchemaVersion:
			rawVersion, hasVersion = v, true
		case metaKeyWrittenBy:
			// 旧值不参与判断：每次打开都会用当前二进制覆盖它。
		case metaKeyFirstSeen:
			rawFirstSeen, hasFirstSeen = v, true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return WriterStamp{}, fmt.Errorf("写入者版本戳: 遍历 db_meta: %w", err)
	}
	rows.Close()

	// first_seen 一旦写下就不再改：它记录的是「这个库第一次被打了版本戳」，
	// 每次 Open 都刷新会让它失去时间锚点的意义。
	firstSeen := now
	if hasFirstSeen {
		if n, err := strconv.ParseInt(strings.TrimSpace(rawFirstSeen), 10, 64); err == nil {
			firstSeen = n
		}
	}

	stored, parseErr := strconv.Atoi(strings.TrimSpace(rawVersion))
	// 值解析失败（空串、非整数、键缺失）一律按「不存在」处理并覆盖，同时 Fresh=true。
	// 为什么不直接拒绝打开：这个库可能是用户唯一的记忆数据，认不出台账就拒绝
	// 会让用户彻底无法恢复。语义上退化成「以当前二进制为准，重新认领这个库」，
	// 代价是失去一次降级保护，收益是永远留一条可操作的路。
	if !hasVersion || parseErr != nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO db_meta(k, v) VALUES (?, ?), (?, ?), (?, ?)`,
			metaKeySchemaVersion, strconv.Itoa(CurrentSchemaVersion),
			metaKeyWrittenBy, binVersion,
			metaKeyFirstSeen, strconv.FormatInt(firstSeen, 10),
		); err != nil {
			return WriterStamp{}, fmt.Errorf("写入者版本戳: 首次打戳: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return WriterStamp{}, fmt.Errorf("写入者版本戳: 提交: %w", err)
		}
		return WriterStamp{SchemaVersion: CurrentSchemaVersion, WrittenBy: binVersion, FirstSeen: firstSeen, Fresh: true}, nil
	}

	if stored > CurrentSchemaVersion {
		// 不回滚之外什么都不做：库保持不变，错误直接冒泡给人看。
		// 文案必须可操作，用户看到的是「升级 mem」或「换回写库的那个版本」。
		return WriterStamp{}, fmt.Errorf("%w: 库的 schema 版本是 %d，当前二进制 %s 只理解 %d；"+
			"请升级 mem（brew/go install/install.sh），或用写入该库的版本打开。"+
			"不要用旧二进制继续读写，否则可能静默错读",
			ErrDBSchemaNewer, stored, binVersion, CurrentSchemaVersion)
	}

	// stored == Current：同版本重开，只更新 written_by（谁最后开过）。
	// stored < Current：就地升到 Current。真正的表结构迁移不在这里做，
	// 这里只把台账推平，避免下次打开继续按旧版本判断。
	// 两种情况都保留 first_seen。
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO db_meta(k, v) VALUES (?, ?), (?, ?), (?, ?)`,
		metaKeySchemaVersion, strconv.Itoa(CurrentSchemaVersion),
		metaKeyWrittenBy, binVersion,
		metaKeyFirstSeen, strconv.FormatInt(firstSeen, 10),
	); err != nil {
		return WriterStamp{}, fmt.Errorf("写入者版本戳: 更新台账: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WriterStamp{}, fmt.Errorf("写入者版本戳: 提交: %w", err)
	}
	return WriterStamp{SchemaVersion: CurrentSchemaVersion, WrittenBy: binVersion, FirstSeen: firstSeen}, nil
}

// BinVersion 返回当前二进制的版本标识。构建期信息缺失时返回 "devel" 而不是空串。
//
// 它和 scripts/derive-version.sh 不是一回事：那边按 conventional commits 推导
// 下一个发布 tag，是「即将发什么版本」；这里读的是「正在跑的这坨二进制自述是什么」。
//
// 选 runtime/debug.ReadBuildInfo 而不是 ldflags 注入：
//   - 不需要改 Makefile，也不依赖发布流程是否记得注入；
//   - `go run` / worktree 里调试时同样成立（此时通常是 devel + vcs.revision）；
//   - 两者都不完美：没注入 tag 的 go build 只会给出 devel，但至少不会撒谎成
//     一个它其实不是的发布版本。降级守卫只需要「能区分新旧」这个粗粒度事实。
func BinVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return "devel"
	}
	v := strings.TrimSpace(info.Main.Version)
	if v != "" && v != "(devel)" {
		return v
	}
	// 无发布版本信息时退回 devel，并尽量带上 git 修订号前 7 位，
	// 便于在「同一台机器上两个 devel 谁是旧的」这类场景里区分。
	rev := ""
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			rev = strings.TrimSpace(s.Value)
			break
		}
	}
	if len(rev) >= 7 {
		return "devel+" + rev[:7]
	}
	return "devel"
}
