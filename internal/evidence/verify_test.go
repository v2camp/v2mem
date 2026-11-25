package evidence

// S1 生产级回读证明：VerifyRef 是 GC 扩大删除前的唯一放行依据。
// 口径（design-data-lifecycle §8.1）：读不回、校验不过，就不许扩大自动 GC。

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyRefReadsBackRecordAndChecksHash(t *testing.T) {
	path, hash := healthTestSegment(t, t.TempDir())

	// 非首行（#2）与兼容首行都必须真正把记录读回手里，不是只看文件在不在。
	ev, err := VerifyRef(path+"#2", hash)
	if err != nil {
		t.Fatalf("VerifyRef(#2): %v", err)
	}
	if ev.Seq != 2 || string(ev.Raw) != "raw-2" {
		t.Fatalf("读回内容 = seq %d raw %q, want seq 2 raw \"raw-2\"", ev.Seq, ev.Raw)
	}

	ev, err = VerifyRef(path, hash)
	if err != nil {
		t.Fatalf("VerifyRef(无 seq): %v", err)
	}
	if ev.Seq != 1 {
		t.Fatalf("无 seq 应读首行, got seq %d", ev.Seq)
	}
}

func TestVerifyRefRejectsBrokenReferences(t *testing.T) {
	dir := t.TempDir()
	path, hash := healthTestSegment(t, dir)

	corrupt := filepath.Join(dir, "corrupt.jsonl.gz")
	if err := os.WriteFile(corrupt, []byte("not gzip at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 坏 gzip 要走到「解包失败」分支，必须先用坏字节自己的 hash 过掉文件 hash 校验。
	corruptHash := fileHashOf(t, corrupt)

	cases := []struct {
		name, locator, want string
	}{
		{"hash 不匹配", path + "#1", strings.Repeat("0", 64)},
		{"hash 缺失", path + "#1", ""},
		{"seq 不存在", path + "#99", hash},
		{"坏 gzip", corrupt + "#1", corruptHash},
		{"文件不存在", filepath.Join(dir, "gone.jsonl.gz") + "#1", hash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := VerifyRef(tc.locator, tc.want); err == nil {
				t.Fatalf("坏引用必须报错（GC 要靠它拒绝扩大）: locator=%q", tc.locator)
			}
		})
	}
}

func fileHashOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
