package evidence

// 删除阶段前的保留集回读校验（design-data-lifecycle §8.1 顺序铁律）：
// 仍被引用保留的归档必须先证明读得回，证明不了就整体拒绝删除。

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// gcExecSQL 直接改库况（观测回收、游标推进），这些状态在真实链路由
// GCObservations / NextBundle 推进，测试要控制水位只能自己给。
func gcExecSQL(t *testing.T, db, query string) {
	t.Helper()
	c, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Exec(query); err != nil {
		t.Fatalf("执行 %s: %v", query, err)
	}
}

// 保留集里任何一段读不回来（坏文件/丢文件），整轮 GC 一个文件都不许删：
// 坏的保留集意味着库况已经超出自动 GC 的理解范围，删除只会让损失扩大。
// 坏文件用「截断 gzip 尾部」制造——整文件 hash 与回读都会失败。
func TestGCEvidenceRefusesWhenKeepSetUnreadable(t *testing.T) {
	e, db := newTestEngine(t)
	keep, err := e.Capture(rawEvent("u-keep"))
	if err != nil {
		t.Fatalf("Capture(u-keep): %v", err)
	}
	orphan, err := e.Capture(rawEvent("u-orphan"))
	if err != nil {
		t.Fatalf("Capture(u-orphan): %v", err)
	}
	// orphan 的观测被回收 + 序号已发窗（Bundle 行已按保留期回收的库况）→ 进待删集合；
	// keep 的观测还在 → 留在保留集。
	gcExecSQL(t, db, `DELETE FROM observations WHERE event_uid = 'u-orphan'`)
	gcExecSQL(t, db, `UPDATE evidence_sources SET cursor_seq = last_seq`)

	keepPath := filepath.Join(e.ArchiveDir(), keep.ArchiveHash+".jsonl.gz")
	fi, err := os.Stat(keepPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(keepPath, fi.Size()-8); err != nil {
		t.Fatalf("截断保留段 gzip 尾部: %v", err)
	}

	if _, err := e.GCEvidence(time.Now().Unix() + 60); err == nil {
		t.Fatal("保留集回读校验失败应整体拒绝删除")
	}
	// 一个文件都不删：待删段的文件与账本行都必须原样在。
	orphanPath := filepath.Join(e.ArchiveDir(), orphan.ArchiveHash+".jsonl.gz")
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("拒绝删除时待删段文件不许消失: %v", err)
	}
	if ok, err := e.st.ArchiveExists(orphan.ArchiveHash); err != nil || !ok {
		t.Fatalf("拒绝删除时账本不许动: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("保留段文件本身也不许被删: %v", err)
	}
}
