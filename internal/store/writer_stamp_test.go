package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// openStampDB 用包内既有测试的同一驱动名开一个文件库。
// 用文件而不是 :memory:，因为要跨调用/跨连接观察落盘结果。
func openStampDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "stamp.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// stampMetaKV 读出 db_meta 全部键值。
func stampMetaKV(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT k, v FROM db_meta`)
	if err != nil {
		t.Fatalf("读 db_meta: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatalf("扫描 db_meta: %v", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 db_meta: %v", err)
	}
	return out
}

func stampSetVersion(t *testing.T, db *sql.DB, v string) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS db_meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatalf("建 db_meta: %v", err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO db_meta(k, v) VALUES ('schema_version', ?)`, v); err != nil {
		t.Fatalf("写 schema_version: %v", err)
	}
}

// 分支 1：空库首次打戳，建表 + 三键齐全 + Fresh=true。
func TestEnsureWriterStampFreshDB(t *testing.T) {
	db := openStampDB(t)
	ctx := context.Background()

	st, err := EnsureWriterStamp(ctx, db, "v0.9.3", 1700000000)
	if err != nil {
		t.Fatalf("空库首次打戳应成功: %v", err)
	}
	if !st.Fresh {
		t.Fatal("空库首次打戳 Fresh 应为 true")
	}
	if st.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion 应为 %d，got %d", CurrentSchemaVersion, st.SchemaVersion)
	}
	if st.WrittenBy != "v0.9.3" {
		t.Fatalf("WrittenBy 应为 v0.9.3，got %q", st.WrittenBy)
	}
	if st.FirstSeen != 1700000000 {
		t.Fatalf("FirstSeen 应为 now，got %d", st.FirstSeen)
	}
	kv := stampMetaKV(t, db)
	if len(kv) != 3 {
		t.Fatalf("db_meta 应有 3 个键，got %d: %v", len(kv), kv)
	}
	for _, k := range []string{"schema_version", "written_by", "first_seen"} {
		if _, ok := kv[k]; !ok {
			t.Fatalf("缺少键 %q: %v", k, kv)
		}
	}
}

// 分支 2：同版本重开，Fresh=false、first_seen 不变、written_by 被更新。
func TestEnsureWriterStampSameVersion(t *testing.T) {
	db := openStampDB(t)
	ctx := context.Background()

	if _, err := EnsureWriterStamp(ctx, db, "v0.9.3", 1700000000); err != nil {
		t.Fatalf("首次打戳: %v", err)
	}
	st, err := EnsureWriterStamp(ctx, db, "v0.9.4", 1700009999)
	if err != nil {
		t.Fatalf("同版本重开应成功: %v", err)
	}
	if st.Fresh {
		t.Fatal("同版本重开 Fresh 应为 false")
	}
	if st.FirstSeen != 1700000000 {
		t.Fatalf("first_seen 必须保留首次值，got %d", st.FirstSeen)
	}
	if st.WrittenBy != "v0.9.4" {
		t.Fatalf("written_by 应被更新为 v0.9.4，got %q", st.WrittenBy)
	}
	// 落库的 written_by 也要跟上，否则下次开库看到的仍是老二进制。
	if kv := stampMetaKV(t, db); kv["written_by"] != "v0.9.4" {
		t.Fatalf("written_by 应落库为 v0.9.4，got %q", kv["written_by"])
	}
}

// 分支 3：库版本更高 → ErrDBSchemaNewer，且错误文本含具体版本数字。
func TestEnsureWriterStampNewerDB(t *testing.T) {
	db := openStampDB(t)
	// 用相对版本而不是字面量：CurrentSchemaVersion 每升一版都要 +1，
	// 写死「3」会在下一次升版时让这条守卫静默失效（3 变成「同版本」）。
	newer := strconv.Itoa(CurrentSchemaVersion + 1)
	stampSetVersion(t, db, newer)

	_, err := EnsureWriterStamp(context.Background(), db, "v0.8.0", 1700000000)
	if err == nil {
		t.Fatal("库版本更高时必须报错")
	}
	if !errors.Is(err, ErrDBSchemaNewer) {
		t.Fatalf("应可用 errors.Is 判定为 ErrDBSchemaNewer，got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{newer, "v0.8.0", strconv.Itoa(CurrentSchemaVersion)} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误文案应含 %q，实际: %s", want, msg)
		}
	}
	// 拒绝打开时不得改写库里的版本。
	if kv := stampMetaKV(t, db); kv["schema_version"] != newer {
		t.Fatalf("拒绝打开时不应改写 schema_version，got %q", kv["schema_version"])
	}
}

// 分支 4：库版本更低 → 升到 Current、written_by 更新、不报错。
func TestEnsureWriterStampOlderDB(t *testing.T) {
	db := openStampDB(t)
	stampSetVersion(t, db, "0")

	st, err := EnsureWriterStamp(context.Background(), db, "v0.9.3", 1700000000)
	if err != nil {
		t.Fatalf("库版本更低不应报错: %v", err)
	}
	if st.Fresh {
		t.Fatal("库已有版本信息时 Fresh 应为 false")
	}
	if st.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("应升到 %d，got %d", CurrentSchemaVersion, st.SchemaVersion)
	}
	kv := stampMetaKV(t, db)
	if kv["schema_version"] != strconv.Itoa(CurrentSchemaVersion) {
		t.Fatalf("落库的 schema_version 应为 %d，got %q", CurrentSchemaVersion, kv["schema_version"])
	}
	if kv["written_by"] != "v0.9.3" {
		t.Fatalf("written_by 应更新为 v0.9.3，got %q", kv["written_by"])
	}
	if kv["first_seen"] != "1700000000" {
		t.Fatalf("first_seen 应补为 now，got %q", kv["first_seen"])
	}
}

// 分支（容错）：schema_version 存了非数字 → 覆盖、Fresh=true、不报错。
func TestEnsureWriterStampUnparsableVersion(t *testing.T) {
	for _, bad := range []string{"not-a-number", ""} {
		t.Run("bad="+bad, func(t *testing.T) {
			db := openStampDB(t)
			stampSetVersion(t, db, bad)

			st, err := EnsureWriterStamp(context.Background(), db, "v0.9.3", 1700000000)
			if err != nil {
				t.Fatalf("无法解析的版本应容错而非拒绝打开: %v", err)
			}
			if !st.Fresh {
				t.Fatal("无法解析时应视为首次打戳，Fresh=true")
			}
			kv := stampMetaKV(t, db)
			if kv["schema_version"] != strconv.Itoa(CurrentSchemaVersion) {
				t.Fatalf("应覆盖为当前版本 %d，got %q", CurrentSchemaVersion, kv["schema_version"])
			}
			if kv["written_by"] != "v0.9.3" {
				t.Fatalf("written_by 应写入当前二进制，got %q", kv["written_by"])
			}
		})
	}
}

// 幂等：重复调用 10 次不产生多余行。
func TestEnsureWriterStampIdempotentNoExtraRows(t *testing.T) {
	db := openStampDB(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := EnsureWriterStamp(ctx, db, "v0.9.3", 1700000000+int64(i)); err != nil {
			t.Fatalf("第 %d 次打戳失败: %v", i+1, err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM db_meta`).Scan(&n); err != nil {
		t.Fatalf("count db_meta: %v", err)
	}
	if n != 3 {
		t.Fatalf("db_meta 应恒为 3 行，got %d", n)
	}
}

// BinVersion 在测试环境里通常无版本信息，但必须非空（不许返回空串）。
func TestWriterStampBinVersionNonEmpty(t *testing.T) {
	v := BinVersion()
	t.Logf("BinVersion()=%q", v)
	if v == "" {
		t.Fatal("BinVersion 不应返回空串，缺构建信息时应回退 devel")
	}
}
