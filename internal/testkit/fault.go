package testkit

import (
	"database/sql"
	"fmt"
	"strings"
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

// FailWritesOn 是 FailWrites 的按操作子集版本：只有列出的写操作被拦，
// 其余操作照常。用于验证「事务内某一步失败后整体回滚」——例如 gc 先 UPDATE
// 再 DELETE 时，只让 DELETE 失败，才能看出 UPDATE 是否也被回滚。
//
// ops 只接受 INSERT / UPDATE / DELETE（大小写不敏感），其余值直接报错。
func FailWritesOn(db *sql.DB, table string, ops ...string) (undo func() error, err error) {
	if db == nil {
		return nil, fmt.Errorf("testkit.FailWritesOn: db 为 nil")
	}
	if err := requireTable(db, "FailWritesOn", table); err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("testkit.FailWritesOn: 至少给一个写操作")
	}
	seen := map[string]bool{}
	created := make([]string, 0, len(ops))
	for _, raw := range ops {
		event := strings.ToUpper(strings.TrimSpace(raw))
		if !validWriteOp(event) {
			for _, trig := range created {
				_, _ = db.Exec(`DROP TRIGGER ` + trig)
			}
			return nil, fmt.Errorf("testkit.FailWritesOn: 未知写操作 %q（INSERT|UPDATE|DELETE）", raw)
		}
		if seen[event] {
			continue
		}
		seen[event] = true
		trigger := fmt.Sprintf("testkit_fail_%s_%s", event, table)
		stmt := fmt.Sprintf(
			`CREATE TRIGGER %s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'testkit injected write failure'); END`,
			trigger, event, table)
		if _, err := db.Exec(stmt); err != nil {
			for _, trig := range created {
				_, _ = db.Exec(`DROP TRIGGER ` + trig)
			}
			return nil, fmt.Errorf("testkit.FailWritesOn: 建触发器失败: %w", err)
		}
		created = append(created, trigger)
	}
	return dropTriggers(db, "FailWritesOn", created), nil
}

// FailWritesWhere 只拦满足 where 条件的单个写操作，条件里可用 NEW/OLD
// （SQLite 触发器语义）。用于制造「同一事务里部分写成功、部分失败」：
// 例如两张候选都合法，只让第二条 INSERT 失败，验证 partial 与重试幂等。
//
// where 是原样拼进 WHEN 的测试条件，**只接受测试代码传入**，不要接用户输入。
func FailWritesWhere(db *sql.DB, table, op, where string) (undo func() error, err error) {
	if db == nil {
		return nil, fmt.Errorf("testkit.FailWritesWhere: db 为 nil")
	}
	if err := requireTable(db, "FailWritesWhere", table); err != nil {
		return nil, err
	}
	event := strings.ToUpper(strings.TrimSpace(op))
	if !validWriteOp(event) {
		return nil, fmt.Errorf("testkit.FailWritesWhere: 未知写操作 %q（INSERT|UPDATE|DELETE）", op)
	}
	if strings.TrimSpace(where) == "" {
		return nil, fmt.Errorf("testkit.FailWritesWhere: where 不能为空")
	}
	trigger := fmt.Sprintf("testkit_fail_where_%s_%s", event, table)
	stmt := fmt.Sprintf(
		`CREATE TRIGGER %s BEFORE %s ON %s WHEN %s BEGIN SELECT RAISE(ABORT, 'testkit injected conditional write failure'); END`,
		trigger, event, table, where)
	if _, err := db.Exec(stmt); err != nil {
		return nil, fmt.Errorf("testkit.FailWritesWhere: 建触发器失败: %w", err)
	}
	return dropTriggers(db, "FailWritesWhere", []string{trigger}), nil
}

func validWriteOp(event string) bool {
	switch event {
	case "INSERT", "UPDATE", "DELETE":
		return true
	}
	return false
}

func requireTable(db *sql.DB, label, table string) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
		return fmt.Errorf("testkit.%s: 查表失败: %w", label, err)
	}
	if n == 0 {
		return fmt.Errorf("testkit.%s: 表 %q 不存在", label, table)
	}
	return nil
}

func dropTriggers(db *sql.DB, label string, triggers []string) func() error {
	return func() error {
		for _, trig := range triggers {
			if _, err := db.Exec(`DROP TRIGGER ` + trig); err != nil {
				return fmt.Errorf("testkit.%s: 撤触发器失败: %w", label, err)
			}
		}
		return nil
	}
}
