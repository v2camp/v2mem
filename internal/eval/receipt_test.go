package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReceiptsOmitsBodyAndQuery(t *testing.T) {
	dir := t.TempDir()
	secret := "PRIVATE-BODY-不要进收据"
	cases := []GoldCase{{Query: secret, Gold: []string{"aaaa1111"}}}
	rep := Score(cases, [][]string{{"aaaa1111ffff"}}, 5)
	lines, err := BuildEvidence(cases, rep)
	if err != nil {
		t.Fatal(err)
	}
	man := Manifest{
		Dataset: "eval/sandbox", DatasetKind: "sandbox", ReleaseGate: true,
		DatasetSHA256: "abc", GitSHA: "deadbeef", GoVersion: "go1.27.1", Invalid: 0,
	}
	if err := WriteReceipts(dir, man, rep, lines); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "scores.ndjson", "evidence.ndjson"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), secret) {
			t.Fatalf("%s 含查询或正文: %s", name, b)
		}
		if strings.Contains(string(b), `"content"`) {
			t.Fatalf("%s 不应有 content 字段: %s", name, b)
		}
	}
	ev, err := os.ReadFile(filepath.Join(dir, "evidence.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ev), "aaaa1111ffff") || !strings.Contains(string(ev), `"strict_all":true`) {
		t.Fatalf("evidence 应含返回哈希与 strict_all: %s", ev)
	}
	var got Manifest
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !got.ReleaseGate || got.Disclaimer == "" || got.DatasetSHA256 != "abc" {
		t.Fatalf("manifest 缺字段: %+v", got)
	}
	if got.Schema != 1 || len(got.Metrics) != 4 {
		t.Fatalf("manifest 指标: %+v", got)
	}
}

func TestBuildEvidenceRejectsMismatch(t *testing.T) {
	if _, err := BuildEvidence(nil, nil); err == nil {
		t.Fatal("空 report 应报错")
	}
	rep := Score([]GoldCase{{Query: "q", Gold: []string{"aa"}}}, [][]string{{"aa"}}, 1)
	if _, err := BuildEvidence(nil, rep); err == nil {
		t.Fatal("样本与结果不一致应报错")
	}
}

func TestFingerprintStableAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(a, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := Fingerprint([]string{b, a})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Fingerprint([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" || h1 != h2 {
		t.Fatalf("指纹应与路径顺序无关: %s %s", h1, h2)
	}
	if _, err := Fingerprint([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("缺文件应报错")
	}
}

func TestWriteReceiptsRejectsEmpty(t *testing.T) {
	if err := WriteReceipts("", Manifest{}, &Report{}, nil); err == nil {
		t.Fatal("空目录应报错")
	}
	if err := WriteReceipts(t.TempDir(), Manifest{}, nil, nil); err == nil {
		t.Fatal("空 report 应报错")
	}
}

func TestBuildEvidenceNilSlices(t *testing.T) {
	rep := &Report{K: 1, Cases: 1, Results: []CaseResult{{Gold: 0}}}
	lines, err := BuildEvidence([]GoldCase{{Query: "q"}}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if lines[0].Gold == nil || lines[0].Returned == nil || !lines[0].Empty {
		t.Fatalf("空切片应写成空数组: %+v", lines[0])
	}
}

func TestWriteReceiptsIOErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceipts(filepath.Join(file, "child"), Manifest{}, &Report{K: 1}, nil); err == nil {
		t.Fatal("父路径是文件时建目录应失败")
	}

	dir := t.TempDir()
	rep := &Report{K: 1, Cases: 0}
	if err := WriteReceipts(dir, Manifest{Schema: 1, Kind: "dev-eval", Disclaimer: "x", CreatedAt: "t", Metrics: []string{"recall_at_k"}}, rep, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "manifest.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceipts(dir, Manifest{Schema: 1, Kind: "k", Disclaimer: "d", CreatedAt: "t", Metrics: []string{"m"}}, rep, nil); err == nil {
		t.Fatal("manifest 路径是目录时应失败")
	}

	dir2 := t.TempDir()
	if err := WriteReceipts(dir2, Manifest{}, rep, []EvidenceLine{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir2, "scores.ndjson")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir2, "scores.ndjson"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceipts(dir2, Manifest{}, rep, nil); err == nil {
		t.Fatal("scores 路径是目录时应失败")
	}
}
