package evidence

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 证据归档准出：观测还在（或仍在引用/活跃范围内），段就必须留；
// 观测没了、序号已发窗、也无引用，段与磁盘文件一起走。
// 中间那一步（只有观测被回收）正是 mem gc --obs 之后、--evidence 之前的状态。
// 夹具把序号推到已发窗：未发窗的活跃段受保护，不在本用例口径内。
func TestGCEvidenceRemovesSegmentsNoObservationPointsAt(t *testing.T) {
	e, db := newTestEngine(t)
	hashes := make([]string, 0, 3)
	for _, uid := range []string{"u1", "u2", "u3"} {
		ref, err := e.Capture(rawEvent(uid))
		if err != nil {
			t.Fatalf("Capture(%s): %v", uid, err)
		}
		hashes = append(hashes, ref.ArchiveHash)
	}
	if n := archiveFileCount(t, e.ArchiveDir()); n != 3 {
		t.Fatalf("初始归档文件 = %d，期望 3", n)
	}

	// 观测只留 1 条，另两条的归档成为孤儿；序号已发窗，孤儿段不再受活跃范围保护。
	if _, err := e.st.GCObservations(0, 1); err != nil {
		t.Fatalf("GCObservations: %v", err)
	}
	gcExecSQL(t, db, `UPDATE evidence_sources SET cursor_seq = last_seq`)

	res, err := e.GCEvidence(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("GCEvidence: %v", err)
	}
	if res.Segments != 2 || res.RawEvents != 2 {
		t.Fatalf("回收段数/事件数 = %d/%d，期望 2/2: %+v", res.Segments, res.RawEvents, res)
	}
	if res.Bytes <= 0 {
		t.Fatalf("释放字节应为正，got %d", res.Bytes)
	}
	if res.Failed != 0 {
		t.Fatalf("不该有失败项: %+v", res)
	}
	if n := archiveFileCount(t, e.ArchiveDir()); n != 1 {
		t.Fatalf("回收后归档文件 = %d，期望 1", n)
	}

	// 账本与磁盘必须同时收敛：留下的那段两边都在，删掉的两段两边都没有。
	var kept int
	for _, h := range hashes {
		ok, qerr := e.st.ArchiveExists(h)
		if qerr != nil {
			t.Fatal(qerr)
		}
		path := filepath.Join(e.ArchiveDir(), h+".jsonl.gz")
		_, statErr := os.Stat(path)
		if ok {
			kept++
			if statErr != nil {
				t.Fatalf("账本还在但文件没了: %s (%v)", h, statErr)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("账本已删但文件还在: %s", h)
		}
	}
	if kept != 1 {
		t.Fatalf("账本留存段 = %d，期望 1", kept)
	}
}

// 保护期：刚写入的段不参与回收。真实顺序是「先落归档文件，再写账」，
// 中间那一瞬段还没有观测指向它，没有保护期就会误删正在提交的证据。
func TestGCEvidenceRespectsWriteWindow(t *testing.T) {
	e, _ := newTestEngine(t)
	for _, uid := range []string{"u1", "u2"} {
		if _, err := e.Capture(rawEvent(uid)); err != nil {
			t.Fatalf("Capture: %v", err)
		}
	}
	if _, err := e.st.GCObservations(0, 1); err != nil {
		t.Fatalf("GCObservations: %v", err)
	}

	res, err := e.GCEvidence(0) // 1970 之前创建的段才回收
	if err != nil {
		t.Fatalf("GCEvidence: %v", err)
	}
	if res.Segments != 0 {
		t.Fatalf("保护期内回收了 %d 段", res.Segments)
	}
	if n := archiveFileCount(t, e.ArchiveDir()); n != 2 {
		t.Fatalf("保护期内文件数 = %d，期望 2", n)
	}
}

// 进程在「落盘」与「写账」之间退出会留下磁盘上有的、账本里没有的残段；
// 写归档时的临时文件在同样情况下也会残留。两者都不在任何账本里，
// 只能靠扫目录找回来——否则观测回收跑得越勤，孤儿文件反而越多。
func TestGCEvidenceSweepsUntrackedFilesAndTempResidue(t *testing.T) {
	e, _ := newTestEngine(t)
	if err := os.MkdirAll(e.ArchiveDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)

	orphan := filepath.Join(e.ArchiveDir(), strings.Repeat("a", 64)+".jsonl.gz")
	if err := os.WriteFile(orphan, []byte("账本外的残段"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(e.ArchiveDir(), ".seg-123456.tmp")
	if err := os.WriteFile(tmp, []byte("写一半的临时文件"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	// 名字不像归档段的文件不该被当成垃圾：归档目录里出现未知文件应报警，不是静默删。
	stranger := filepath.Join(e.ArchiveDir(), "NOTES.md")
	if err := os.WriteFile(stranger, []byte("别删我"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stranger, old, old); err != nil {
		t.Fatal(err)
	}

	res, err := e.GCEvidence(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("GCEvidence: %v", err)
	}
	if res.Untracked != 1 || res.TempFiles != 1 {
		t.Fatalf("残文件/临时文件 = %d/%d，期望 1/1: %+v", res.Untracked, res.TempFiles, res)
	}
	for _, p := range []string{orphan, tmp} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("应被删除却还在: %s", p)
		}
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Fatalf("非归档命名的文件不该被删: %v", err)
	}
}

// 还有历史的证据源不能被删：它是序号账本，删了下次捕获会从 seq=1 重新分配。
func TestGCEvidenceKeepsSourcesWithHistory(t *testing.T) {
	e, _ := newTestEngine(t)
	for _, uid := range []string{"u1", "u2"} {
		if _, err := e.Capture(rawEvent(uid)); err != nil {
			t.Fatalf("Capture: %v", err)
		}
	}
	if _, err := e.st.GCObservations(0, 1); err != nil {
		t.Fatalf("GCObservations: %v", err)
	}
	res, err := e.GCEvidence(time.Now().Unix() + 60)
	if err != nil {
		t.Fatalf("GCEvidence: %v", err)
	}
	if res.Sources != 0 {
		t.Fatalf("尚有观测的证据源不该被删，got %d", res.Sources)
	}
}
