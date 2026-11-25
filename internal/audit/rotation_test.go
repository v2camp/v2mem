package audit

// S13 红 case（P5）：audit .1–.3 合并查询能读到历史事件；轮转失败可观测。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeOneRecord(t *testing.T, path, query string) {
	t.Helper()
	b, err := json.Marshal(Record{Event: "manual-search", Query: query})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// P5-a：只查当前文件会让轮转后的历史断点；ReadHistory 必须把 .3/.2/.1/当前
// 按时间顺序合并，且 limit 语义取「最近 N 条」。
func TestP5ReadHistoryMergesArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	// .3 最旧 → 当前最新
	for i, suffix := range []string{".3", ".2", ".1", ""} {
		writeOneRecord(t, path+suffix, fmt.Sprintf("q%d-%s", i, suffix))
	}

	all, err := ReadHistory(path, 0)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("P5: 应合并 4 条历史记录，got %d", len(all))
	}
	want := []string{"q0-.3", "q1-.2", "q2-.1", "q3-"}
	for i, r := range all {
		if r.Query != want[i] {
			t.Errorf("P5: 第 %d 条应为 %q，got %q（历史顺序断点）", i, want[i], r.Query)
		}
	}

	last2, err := ReadHistory(path, 2)
	if err != nil {
		t.Fatalf("ReadHistory(limit=2): %v", err)
	}
	if len(last2) != 2 || last2[0].Query != "q2-.1" || last2[1].Query != "q3-" {
		t.Errorf("P5: limit=2 应取最近两条（.1 与当前），got %+v", last2)
	}

	// 坏行仍按 Read 的口径跳过，不让一条坏档毁掉整段历史。
	if err := os.WriteFile(path+".2", []byte("这不是 JSON\n"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	merged, err := ReadHistory(path, 0)
	if err != nil {
		t.Fatalf("ReadHistory(坏档): %v", err)
	}
	if len(merged) != 3 {
		t.Errorf("P5: 坏档应被跳过且不影响其它档，got %d 条", len(merged))
	}
}

// P5-b：轮转失败不许静默——Append 必须返回错误，且写明确告警。
// 用非空目录阻塞 .2→.3 的移位（与 TestRotateReturnsErrorWhenArchiveTargetBlocked 同手法）。
func TestP5RotateFailureIsObservable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	writeOneRecord(t, path, "当前日志")
	writeOneRecord(t, path+".2", "旧档2")
	if err := os.MkdirAll(path+".3", 0o755); err != nil {
		t.Fatalf("MkdirAll .3: %v", err)
	}
	if err := os.WriteFile(path+".3/x", []byte("1"), 0o644); err != nil {
		t.Fatalf("WriteFile .3/x: %v", err)
	}

	orig := warnRotate
	warned := 0
	var warnedErr error
	warnRotate = func(err error) { warned++; warnedErr = err }
	t.Cleanup(func() { warnRotate = orig })

	// 极小阈值强制轮转 → rotate 在 .2→.3 处失败。
	oldMax := MaxArchiveBytes
	MaxArchiveBytes = 1
	t.Cleanup(func() { MaxArchiveBytes = oldMax })

	err := Append(path, Record{Event: "manual-search", Query: "轮转失败后的记录"})
	if err == nil {
		t.Fatal("P5: 轮转失败被静默吞掉，Append 应返回错误")
	}
	if warned == 0 {
		t.Errorf("P5: 轮转失败应写明确告警（不静默）")
	}
	if warnedErr == nil || !errors.Is(err, warnedErr) {
		t.Errorf("P5: Append 返回的错误应包含轮转原因，got err=%v warn=%v", err, warnedErr)
	}
}

// 覆盖率补测：audit 包下限 89% 且余量极小，新增分支逐条覆盖（不降门槛）。

// 空路径兜底：与 Read 对称，落到默认位置；TestMain 已隔离 HOME，那里无日志。
// 注意：默认路径在整个测试二进制里共享——TestReadDefaultsEmptyPathLikeAppend
// （audit_test.go）会先往默认位置写一条且顺序在本测试之前，不清掉就会把
// 「干净 HOME 下无历史」污染成 got 1（根因是测试间共享状态，不是 ReadHistory
// 的产品 bug：兜底路径解析与单文件 Read 完全同源）。隔离 HOME 是临时目录，
// 删除安全；.1–.3 归档一并清，防止早前轮转残留让合并查询读到非零。
func TestReadHistoryEmptyPathFallsBack(t *testing.T) {
	p := Path()
	_ = os.Remove(p)
	for i := 1; i <= MaxArchives; i++ {
		_ = os.Remove(fmt.Sprintf("%s.%d", p, i))
	}
	rs, err := ReadHistory("", 0)
	if err != nil {
		t.Fatalf("空路径兜底: %v", err)
	}
	if len(rs) != 0 {
		t.Errorf("隔离 HOME 下应无历史，got %d", len(rs))
	}
}

// 归档档读取失败应冒泡，不让一段坏档悄悄变成「历史为空」。
func TestReadHistoryPropagatesArchiveReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	writeOneRecord(t, path, "current")
	// .1 做成目录：Read 打开后 Scanner 读目录返回错误。
	if err := os.MkdirAll(path+".1", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHistory(path, 0); err == nil {
		t.Fatal("归档档读取失败应冒泡")
	}
}

// 当前档读取失败同样冒泡。
func TestReadHistoryPropagatesCurrentReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHistory(path, 0); err == nil {
		t.Fatal("当前档读取失败应冒泡")
	}
}

// 默认告警出口：轮转失败时不替换 warnRotate，默认 stderr 告警应被执行，
// 且 Append 返回错误；同时覆盖 rotate 里 Rename 失败的冒泡分支。
func TestAppendRotateFailureUsesDefaultWarn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	writeOneRecord(t, path, "当前日志")
	writeOneRecord(t, path+".2", "旧档2")
	if err := os.MkdirAll(path+".3", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".3/x", []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldMax := MaxArchiveBytes
	MaxArchiveBytes = 1
	t.Cleanup(func() { MaxArchiveBytes = oldMax })
	if err := Append(path, Record{Event: "manual-search"}); err == nil {
		t.Fatal("轮转失败应返回错误（默认告警出口）")
	}
}
