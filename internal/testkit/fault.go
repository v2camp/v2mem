package testkit

import (
	"database/sql"
	"fmt"
)

// FailWrites 给 SQLite 表装写失败触发器（RAISE(ABORT)），拦 INSERT/UPDATE/DELETE
// 三类写，模拟磁盘满/只读等写入故障；返回 undo 恢复写入。
//
// 用触发器而不是 mock，是因为要拦的是**整笔事务**里的写，
// 与真实 SQLite 失败语义一致（事务回滚、错误冒泡到调用方）。
// table 必须是已存在的表名，否则直接报错 —— 装错表等于没注入。
func FailWrites(db *sql.DB, table string) (undo func() error, err error) {
	if db == nil {
		return nil, fmt.Errorf("testkit.FailWrites: db 为 nil")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
		return nil, fmt.Errorf("testkit.FailWrites: 查表失败: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("testkit.FailWrites: 表 %q 不存在", table)
	}
	var created []string
	for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
		trigger := fmt.Sprintf("testkit_fail_%s_%s", event, table)
		stmt := fmt.Sprintf(
			`CREATE TRIGGER %s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'testkit injected write failure'); END`,
			trigger, event, table)
		if _, err := db.Exec(stmt); err != nil {
			for _, trig := range created {
				_, _ = db.Exec(`DROP TRIGGER ` + trig)
			}
			return nil, fmt.Errorf("testkit.FailWrites: 建触发器失败: %w", err)
		}
		created = append(created, trigger)
	}
	undo = func() error {
		for _, trig := range created {
			if _, err := db.Exec(`DROP TRIGGER ` + trig); err != nil {
				return fmt.Errorf("testkit.FailWrites: 撤触发器失败: %w", err)
			}
		}
		return nil
	}
	return undo, nil
}
