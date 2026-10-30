package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 收据是一次开发评测的可复现记录。
// evidence 只含 hash，不含记忆正文，也不含查询原文。

const receiptDisclaimer = "开发评测，非竞品排名"

// Manifest 描述这次评测用的代码与数据集，不含样本正文。
type Manifest struct {
	Schema        int      `json:"schema"`
	Kind          string   `json:"kind"`
	Disclaimer    string   `json:"disclaimer"`
	CreatedAt     string   `json:"created_at"`
	K             int      `json:"k"`
	Dataset       string   `json:"dataset"`
	DatasetKind   string   `json:"dataset_kind"` // sandbox | gold
	ReleaseGate   bool     `json:"release_gate"`
	DatasetSHA256 string   `json:"dataset_sha256"`
	GitSHA        string   `json:"git_sha,omitempty"`
	GoVersion     string   `json:"go_version,omitempty"`
	Cases         int      `json:"cases"`
	Invalid       int      `json:"invalid"`
	Metrics       []string `json:"metrics"`
}

// ScoreLine 是 scores.ndjson 的一行：各指标分开，不合成一个分数。
type ScoreLine struct {
	K            int     `json:"k"`
	Cases        int     `json:"cases"`
	RecallAtK    float64 `json:"recall_at_k"`
	MRR          float64 `json:"mrr"`
	EmptyRate    float64 `json:"empty_rate"`
	StrictAllAtK float64 `json:"strict_all_at_k"`
	Invalid      int     `json:"invalid"`
}

// EvidenceLine 是 evidence.ndjson 的一行。Returned 是命中的 content_hash，没有正文。
type EvidenceLine struct {
	QuerySHA256 string   `json:"query_sha256"`
	Gold        []string `json:"gold"`
	Returned    []string `json:"returned"`
	Hit         int      `json:"hit"`
	GoldCount   int      `json:"gold_count"`
	StrictAll   bool     `json:"strict_all"`
	FirstRank   int      `json:"first_rank"`
	Empty       bool     `json:"empty"`
}

// Fingerprint 对一组文件做稳定摘要：按路径排序，把路径和字节一起哈希。
func Fingerprint(paths []string) (string, error) {
	cp := append([]string(nil), paths...)
	sort.Strings(cp)
	h := sha256.New()
	for _, p := range cp {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n%d\n", p, len(b))
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BuildEvidence 从已评分的样本抽出收据行。cases 必须与 rep.Results 一一对应。
func BuildEvidence(cases []GoldCase, rep *Report) ([]EvidenceLine, error) {
	if rep == nil {
		return nil, fmt.Errorf("report 为空")
	}
	if len(cases) != len(rep.Results) {
		return nil, fmt.Errorf("样本数 %d 与结果数 %d 不一致", len(cases), len(rep.Results))
	}
	out := make([]EvidenceLine, len(cases))
	for i, c := range cases {
		res := rep.Results[i]
		gold := append([]string(nil), c.Gold...)
		if gold == nil {
			gold = []string{}
		}
		ret := append([]string(nil), res.Returned...)
		if ret == nil {
			ret = []string{}
		}
		sum := sha256.Sum256([]byte(c.Query))
		out[i] = EvidenceLine{
			QuerySHA256: hex.EncodeToString(sum[:]),
			Gold:        gold,
			Returned:    ret,
			Hit:         res.Hit,
			GoldCount:   res.Gold,
			StrictAll:   res.StrictAll,
			FirstRank:   res.FirstRank,
			Empty:       len(ret) == 0,
		}
	}
	return out, nil
}

// WriteReceipts 覆盖写出 manifest.json、scores.ndjson、evidence.ndjson。
func WriteReceipts(dir string, man Manifest, rep *Report, lines []EvidenceLine) error {
	if dir == "" {
		return fmt.Errorf("收据目录为空")
	}
	if rep == nil {
		return fmt.Errorf("report 为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if man.Schema == 0 {
		man.Schema = 1
	}
	if man.Kind == "" {
		man.Kind = "dev-eval"
	}
	if man.Disclaimer == "" {
		man.Disclaimer = receiptDisclaimer
	}
	if man.CreatedAt == "" {
		man.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if man.Metrics == nil {
		man.Metrics = []string{"recall_at_k", "mrr", "empty_rate", "strict_all_at_k"}
	}
	man.K = rep.K
	man.Cases = rep.Cases
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o600); err != nil {
		return err
	}
	score := ScoreLine{
		K: rep.K, Cases: rep.Cases, RecallAtK: rep.RecallAtK, MRR: rep.MRR,
		EmptyRate: rep.EmptyRate, StrictAllAtK: rep.StrictAllAtK, Invalid: man.Invalid,
	}
	if err := writeNDJSON(filepath.Join(dir, "scores.ndjson"), []ScoreLine{score}); err != nil {
		return err
	}
	if lines == nil {
		lines = []EvidenceLine{}
	}
	return writeNDJSON(filepath.Join(dir, "evidence.ndjson"), lines)
}

func writeNDJSON[T any](path string, rows []T) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}
