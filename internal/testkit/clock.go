// Package testkit 是测试共享基建：可拨时间与故障注入。
//
// 只给测试用（生产代码只依赖 Clock 接口缝），但作为普通包登记覆盖率，
// 自己必须有自测 —— 基建坏了会让所有下游切片得出假结论。
package testkit

import (
	"sync"
	"time"
)

// Clock 是被测代码的时间缝。生产传真实时钟，测试传 ManualClock。
type Clock interface {
	Now() time.Time
}

// ManualClock 是冻结时间：只有 Advance / Set 能推动它。
// 加锁是因为后续 maintain 的租约与退避路径可能在 goroutine 里读时间。
type ManualClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewManualClock 返回停在 t 上的时钟。
func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{t: t}
}

// Now 返回当前冻结值。
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance 把时间向前拨 d；d 为负等价于回拨（测试允许）。
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Set 直接把时间定到 t。
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}
