package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

func TestCmdMigrateSummaryExternal(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add(store.AddInput{Content: "备份种子记忆"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendObservation(store.ObsInput{SessionID: "s", Event: "stop", Summary: "迁移测试摘要"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	if err := cmdMigrate([]string{"--db", db}); err == nil {
		t.Fatal("缺少 --summary-external 应报错")
	}

	out, err := captureStdout(t, func() error {
		return cmdMigrate([]string{"--summary-external", "--dry-run", "--db", db})
	})
	if err != nil || !strings.Contains(out, "dry-run") {
		t.Fatalf("dry-run 失败: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(db), "content")); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不应创建 content/: %v", err)
	}

	out, err = captureStdout(t, func() error {
		return cmdMigrate([]string{"--summary-external", "--db", db})
	})
	if err != nil || !strings.Contains(out, "已迁移 1 行") {
		t.Fatalf("迁移失败: %q %v", out, err)
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(db), "backup"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("默认应生成备份: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(db), "content")); err != nil {
		t.Fatalf("迁移应创建 content/: %v", err)
	}

	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var emptyRows, refRows int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM observations WHERE summary='' AND summary_ref<>''`).Scan(&emptyRows); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM observations WHERE summary_ref<>''`).Scan(&refRows); err != nil {
		t.Fatal(err)
	}
	if emptyRows != 1 || refRows != 1 {
		t.Fatalf("迁移后列不对: empty=%d ref=%d", emptyRows, refRows)
	}
	var version, marker string
	want := strconv.Itoa(store.CurrentSchemaVersion)
	if err := raw.QueryRow(`SELECT v FROM db_meta WHERE k='schema_version'`).Scan(&version); err != nil || version != want {
		t.Fatalf("schema_version 应为 %s: %q %v", want, version, err)
	}
	if err := raw.QueryRow(`SELECT v FROM db_meta WHERE k='summary_external_version'`).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("迁移标记缺失: %q %v", marker, err)
	}

	// 重跑幂等。
	out, err = captureStdout(t, func() error {
		return cmdMigrate([]string{"--summary-external", "--db", db})
	})
	if err != nil || !strings.Contains(out, "无需迁移") {
		t.Fatalf("重跑应幂等: %q %v", out, err)
	}
}
