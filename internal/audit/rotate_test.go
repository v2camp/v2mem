package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRecords(t *testing.T, path string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := Append(path, Record{TS: time.Now().Unix() + int64(i), Event: "manual-search", Query: "q"}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}
}

// 低于阈值不滚动；达到阈值在下一次追加前把当前日志归档为 .1。
func TestMaybeRotateDoesNothingUnderThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	writeRecords(t, path, 3)
	beforeSize := statSize(t, path)

	if err := MaybeRotate(path, beforeSize+1024); err != nil {
		t.Fatalf("MaybeRotate: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("未超限不应被轮转: %v", err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Error("未超限不应产生 .1 归档")
	}
}

func TestMaybeRotateArchivesCurrentAndShifts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	writeRecords(t, path, 3)

	// 预置满 3 个归档：第 4 份（.3）在轮转时应被挤出删除
	for i, content := range map[int]string{1: "old1\n", 2: "old2\n", 3: "old3\n"} {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile .%d: %v", i, err)
		}
	}

	// 用极小阈值强制轮转
	if err := MaybeRotate(path, 10); err != nil {
		t.Fatalf("MaybeRotate: %v", err)
	}

	// 当前被清空为新日志起点（.0 归档为 .1）
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Fatalf("轮转后应重建空当前日志，got err=%v", err)
	}
	// 原 .1 → .2：最旧的那份（old3，超出保留数）被删除
	if b, err := os.ReadFile(path + ".2"); err != nil || string(b) != "old1\n" {
		t.Errorf("原 .1 应移位为 .2，got %q err=%v", b, err)
	}
	if b, err := os.ReadFile(path + ".3"); err != nil || string(b) != "old2\n" {
		t.Errorf("原 .2 应移位为 .3，got %q err=%v", b, err)
	}
	if b, err := os.ReadFile(path + ".1"); err != nil || len(strings.TrimSpace(string(b))) == 0 {
		t.Errorf(".1 应为归档的旧日志，got len=%d err=%v", len(b), err)
	}
}

func statSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return fi.Size()
}

// Append 在追加前自动轮转：超过体量上限的旧日志被归档，新增记录进入新文件。
func TestAppendAutoRotatesWhenOversized(t *testing.T) {
	oldMax := MaxArchiveBytes
	MaxArchiveBytes = 64
	t.Cleanup(func() { MaxArchiveBytes = oldMax })

	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	writeRecords(t, path, 3) // 超限
	// 第四次的追加必须能成功，且触发轮转后日志仍可读
	r := Record{TS: time.Now().Unix(), Event: "manual-search", Query: "触发轮转后的记录"}
	if err := Append(path, r); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("超限后应产生 .1 归档: %v", err)
	}
	all, err := Read(path, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(all) < 1 || !strings.Contains(all[len(all)-1].Query, "触发轮转后的记录") {
		t.Errorf("新记录应可读，got %d 条（末端=%+v）", len(all), lastDesc(all))
	}
}

func lastDesc(rs []Record) string {
	if len(rs) == 0 {
		return "<空>"
	}
	return rs[len(rs)-1].Query
}

// 归档目标被同名非空目录阻塞时，轮转应返回错误而不是静默吞掉。
func TestRotateReturnsErrorWhenArchiveTargetBlocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	writeRecords(t, path, 3)
	if err := os.WriteFile(path+".2", []byte("old2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .2: %v", err)
	}
	// 用非空目录阻塞 .2→.3 的移位
	if err := os.MkdirAll(path+".3", 0o755); err != nil {
		t.Fatalf("MkdirAll .3: %v", err)
	}
	if err := os.WriteFile(path+".3/x", []byte("1"), 0o644); err != nil {
		t.Fatalf("WriteFile .3/x: %v", err)
	}
	if err := MaybeRotate(path, 10); err == nil {
		t.Fatal("归档目标被同名目录阻塞时应返回错误")
	}
}

// 父路径不是目录（是普通文件）时，Append 应报错而不是崩溃或成功。
func TestAppendFailsWhenParentIsAFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := filepath.Join(blocker, "audit.jsonl")
	if err := Append(p, Record{Event: "x"}); err == nil {
		t.Fatal("父路径是文件时应报错")
	}
}