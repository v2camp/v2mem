package evidence

// P0 契约：ref_health 只读校验（文件存在 + 字节 hash + #seq 可定位）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

func healthTestSegment(t *testing.T, dir string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "observations.jsonl.gz")
	lines := make([]string, 0, 2)
	for _, seq := range []int{1, 2} {
		b, err := json.Marshal(segmentRecord{Version: 1, Seq: seq, Raw: []byte(fmt.Sprintf("raw-%d", seq))})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	writeGzipLines(t, path, lines...)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return path, hex.EncodeToString(sum[:])
}

func TestRefHealthAcceptsValidLocator(t *testing.T) {
	path, hash := healthTestSegment(t, t.TempDir())
	for _, locator := range []string{path, path + "#2"} {
		if err := RefHealth(locator, hash); err != nil {
			t.Fatalf("合法引用应通过 %q: %v", locator, err)
		}
	}
}

func TestRefHealthRejectsBrokenReferences(t *testing.T) {
	dir := t.TempDir()
	path, hash := healthTestSegment(t, dir)
	cases := []struct {
		name, locator, want string
	}{
		{"缺少 hash", path, ""},
		{"hash 不匹配", path + "#1", strings.Repeat("0", 64)},
		{"文件不存在", filepath.Join(dir, "gone.jsonl.gz") + "#1", hash},
		{"seq 不存在", path + "#99", hash},
		{"seq 非法", path + "#x", hash},
		{"空 locator", "  ", hash},
		{"路径是目录", dir + "#1", hash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := RefHealth(tc.locator, tc.want); err == nil {
				t.Fatalf("坏引用应报错: locator=%q hash=%q", tc.locator, tc.want)
			}
		})
	}
}

func TestRefHealthDoesNotChangeSegment(t *testing.T) {
	path, hash := healthTestSegment(t, t.TempDir())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := RefHealth(path+"#2", hash); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("巡检不应改写外置内容")
	}
}

func TestRefHealthLine(t *testing.T) {
	root := t.TempDir()
	ts := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local).Unix()
	ref, hash, _, err := store.AppendSummaryLine(root, ts, store.SummaryLine{
		V: 1, Seq: 1, ID: "ev-1", Summary: "原行",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := RefHealthLine(root, ref, hash); err != nil {
		t.Fatalf("合法行应通过: %v", err)
	}
	// append 新 member 后，旧行级 hash 仍稳定；整文件 hash 已变，但这里不依赖它。
	if _, _, _, err := store.AppendSummaryLine(root, ts, store.SummaryLine{
		V: 1, Seq: 2, ID: "ev-2", Summary: "新行",
	}); err != nil {
		t.Fatal(err)
	}
	if err := RefHealthLine(root, ref, hash); err != nil {
		t.Fatalf("append 后旧行仍应通过: %v", err)
	}
	cases := []struct{ name, ref, hash string }{
		{"坏 hash", ref, strings.Repeat("1", 64)},
		{"坏 seq", strings.Replace(ref, "#1", "#9", 1), hash},
		{"缺文件", "content/1999/01/01/observations.jsonl.gz#1", hash},
		{"空 locator", "", hash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := RefHealthLine(root, tc.ref, tc.hash); err == nil {
				t.Fatal("坏引用应报错")
			}
		})
	}
}
