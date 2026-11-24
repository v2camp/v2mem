package main

// 显式数据迁移入口。Open 只做 DDL 与版本守卫，批量写只发生在这里。

import (
	"errors"
	"flag"
	"fmt"

	"github.com/v2camp/v2mem/internal/store"
)

func cmdMigrate(args []string) error {
	var c common
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	c.register(fs)
	external := fs.Bool("summary-external", false, "执行 summary / summary_idx 外置迁移")
	dryRun := fs.Bool("dry-run", false, "只统计待迁行与预估体积，不落盘、不改库")
	noBackup := fs.Bool("no-backup", false, "跳过迁移前的库快照")
	backupDir := fs.String("backup-dir", "", "备份目录（默认 <库目录>/backup）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*external {
		return errors.New("缺少 --summary-external：目前只支持 summary / summary_idx 外置迁移")
	}

	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()

	if !*dryRun && !*noBackup {
		path, err := backupBeforeMutating(c.db, false, *backupDir)
		if err != nil {
			return err
		}
		if path != "" {
			fmt.Printf("已备份数据库: %s\n", path)
			fmt.Println("完整回滚还需复制 evidence/、spool/ 与 audit.jsonl（本命令不越界处理目录）")
		}
	}

	stats, err := st.MigrateSummaryExternal(*dryRun)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(stats)
	}
	if stats.AlreadyDone {
		fmt.Println("summary / summary_idx 已外置，无需迁移")
		return nil
	}
	fmt.Printf("待迁移/已迁移 %d 行，%d 个日期分片\n", stats.Migrated, stats.Shards)
	fmt.Printf("两列原字节 %.2f MiB → 外置 gzip %.2f MiB\n",
		float64(stats.BytesBefore)/(1<<20), float64(stats.BytesAfter)/(1<<20))
	if *dryRun {
		fmt.Println("dry-run：未写文件、未改库")
		return nil
	}
	if stats.PagesBefore > 0 {
		fmt.Printf("DB %.2f → %.2f MiB（-%.1f%%）；计入外置后净降 %.1f%%\n",
			float64(stats.PagesBefore)/(1<<20), float64(stats.PagesAfter)/(1<<20),
			100*float64(stats.PagesBefore-stats.PagesAfter)/float64(stats.PagesBefore),
			100*float64(stats.PagesBefore-stats.PagesAfter-stats.BytesAfter)/float64(stats.PagesBefore))
	}
	return nil
}
