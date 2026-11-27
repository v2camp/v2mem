package main

// Judge 落库撞上 SQLITE_BUSY 的重试。
//
// 为什么值得单独钉住：自动侧一天只有一个冷却窗口（默认 24h），一次瞬时锁就让
// 整批判定作废，积压要再等一天。代价对比很悬殊——多等几百毫秒 vs 多烂一天。

import (
	"errors"
	"fmt"
	"testing"
)

func TestBusyRetryRetriesTransientErrors(t *testing.T) {
	var calls int
	err := busyRetry(func() error {
		calls++
		if calls < judgeBusyAttempts {
			return errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("最后一次应成功，got %v", err)
	}
	if calls != judgeBusyAttempts {
		t.Fatalf("应一直重试到成功，calls=%d", calls)
	}
}

// 持续 BUSY 仍要如实报错：重试是「多给一次机会」，不是把故障吞掉。
func TestBusyRetryGivesUpAfterAttempts(t *testing.T) {
	calls := 0
	err := busyRetry(func() error {
		calls++
		return fmt.Errorf("写判定: %w", errors.New("database is locked"))
	})
	if err == nil {
		t.Fatal("持续 BUSY 应返回错误，而不是假装成功")
	}
	if calls != judgeBusyAttempts {
		t.Fatalf("应尝试 %d 次后放弃，calls=%d", judgeBusyAttempts, calls)
	}
}

// 语义错误不重试：候选已关闭、revision 冲突重试多少次都是同一个结果，
// 多试只会把真实原因埋在 last error 里。
func TestBusyRetryDoesNotRetrySemanticErrors(t *testing.T) {
	calls := 0
	err := busyRetry(func() error {
		calls++
		return errors.New("候选已关闭")
	})
	if err == nil {
		t.Fatal("应返回语义错误")
	}
	if calls != 1 {
		t.Fatalf("语义错误不应重试，calls=%d", calls)
	}
}
