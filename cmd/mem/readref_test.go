package main

// S1：mem audit --read-ref 是归档回读的生产入口 —— 唯一由生产命令触发的
// 「读回 + 校验 archive_hash」路径（此前只有测试在调）。

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type readRefSeg struct {
	V        int    `json:"v"`
	EventUID string `json:"event_uid"`
	Seq      int    `json:"seq"`
	Raw      []byte `json:"raw"`
}

func writeReadRefSegment(t *testing.T, dir string) (path, hash string) {
	t.Helper()
	path = filepath.Join(dir, "observations.jsonl.gz")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for i, uid := range []string{"ev-1", "ev-2"} {
		b, err := json.Marshal(readRefSeg{V: 1, EventUID: uid, Seq: i + 1, Raw: []byte("raw-" + uid)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := zw.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return path, hex.EncodeToString(sum[:])
}

func TestCmdAuditReadRefVerifiesAndReports(t *testing.T) {
	path, hash := writeReadRefSegment(t, t.TempDir())

	out, err := captureStdout(t, func() error {
		return cmdAudit([]string{"--read-ref", path + "#2", "--hash", hash})
	})
	if err != nil {
		t.Fatalf("read-ref 应成功: %v", err)
	}
	for _, want := range []string{"ev-2", "校验通过"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}
}

func TestCmdAuditReadRefJSONAndFailure(t *testing.T) {
	dir := t.TempDir()
	path, hash := writeReadRefSegment(t, dir)

	out, err := captureStdout(t, func() error {
		return cmdAudit([]string{"--read-ref", path + "#1", "--hash", hash, "-json"})
	})
	if err != nil {
		t.Fatalf("json 模式应成功: %v", err)
	}
	if !strings.Contains(out, `"verified"`) || !strings.Contains(out, "ev-1") {
		t.Fatalf("json 输出缺字段:\n%s", out)
	}

	// hash 不符：生产入口必须报错退出，而不是「看起来读到了」。
	if err := cmdAudit([]string{"--read-ref", path + "#1", "--hash", strings.Repeat("0", 64)}); err == nil {
		t.Fatal("hash 不符必须返回错误")
	}
	// seq 不存在同理。
	if err := cmdAudit([]string{"--read-ref", path + "#9", "--hash", hash}); err == nil {
		t.Fatal("seq 缺失必须返回错误")
	}
}
