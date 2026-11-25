package testkit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// HoldWriteLock 在 path 上开一条独立连接并持有写事务（BEGIN IMMEDIATE），
// 模拟另一个进程占用写锁；返回的 release 回滚、关连接。
//
// 被锁期间其它写者会按自己的 busy_timeout 等待或报 SQLITE_BUSY，因此
// release 必须在测试结束前调用，否则后续清理可能一直等到超时。
func HoldWriteLock(path string) (release func() error, err error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("testkit.HoldWriteLock: 打开库失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("testkit.HoldWriteLock: 取连接失败: %w", err)
	}
	closeAll := func() error {
		cerr := conn.Close()
		derr := db.Close()
		return errors.Join(cerr, derr)
	}
	// 不设 busy_timeout：SQLite 默认就是 0，拿不到写锁立即失败，
	// 不会把测试挂在「等另一个持锁者释放」上。
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = closeAll()
		return nil, fmt.Errorf("testkit.HoldWriteLock: 获取写锁失败: %w", err)
	}
	return func() error {
		_, rerr := conn.ExecContext(ctx, `ROLLBACK`)
		cerr := closeAll()
		return errors.Join(rerr, cerr)
	}, nil
}
