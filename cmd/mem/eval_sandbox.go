package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/v2camp/v2mem/internal/eval"
	"github.com/v2camp/v2mem/internal/store"
)

// 沙箱评测只碰临时库。个人 gold 仍走 --gold / --gold-dir，不进发布门禁。

type sandboxSeed struct {
	Layer     string `json:"layer"`
	Content   string `json:"content"`
	Kind      string `json:"kind"`
	Project   string `json:"project"`
	Source    string `json:"source"`
	Channel   string `json:"channel"`
	SessionID string `json:"session_id"`
	Harness   string `json:"harness"`
	Event     string `json:"event"`
	ToolName  string `json:"tool_name"`
	Summary   string `json:"summary"`
	EventID   string `json:"event_id"`
}

func runSandboxRecall(c common, dir string, k int, receipts string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	seedPath := filepath.Join(dir, "seed.jsonl")
	if _, err := os.Stat(seedPath); err != nil {
		return fmt.Errorf("沙箱缺少 seed.jsonl: %s", seedPath)
	}
	cases, err := loadGoldDir(dir)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "v2mem-sandbox-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	dbPath := filepath.Join(tmp, "mem.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := seedSandbox(st, seedPath); err != nil {
		return err
	}
	man, err := datasetManifest(dir, true)
	if err != nil {
		return err
	}
	return runGoldRecall(st, c, cases, dir, k, receipts, man)
}

func seedSandbox(st *store.Store, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var row sandboxSeed
		if err := json.Unmarshal([]byte(text), &row); err != nil {
			return fmt.Errorf("seed 第 %d 行不是合法 JSON: %w", line, err)
		}
		if err := applySandboxRow(st, row); err != nil {
			return fmt.Errorf("seed 第 %d 行: %w", line, err)
		}
	}
	return sc.Err()
}

func applySandboxRow(st *store.Store, row sandboxSeed) error {
	layer := strings.TrimSpace(row.Layer)
	if layer == "" {
		if row.Summary != "" && row.Content == "" {
			layer = "observation"
		} else {
			layer = "memory"
		}
	}
	switch layer {
	case "observation":
		_, err := st.AppendObservation(store.ObsInput{
			SessionID: row.SessionID, Project: row.Project, Harness: row.Harness,
			Event: row.Event, ToolName: row.ToolName, Summary: row.Summary,
			Channel: row.Channel, EventID: row.EventID,
		})
		return err
	case "memory":
		_, err := st.Add(store.AddInput{
			Content: row.Content, Kind: row.Kind, Project: row.Project,
			Source: row.Source, Channel: row.Channel, Tool: "sandbox",
		})
		return err
	default:
		return fmt.Errorf("未知 layer %q", layer)
	}
}

func goldReceipt(path string, gate bool) eval.Manifest {
	man, err := datasetManifest(path, gate)
	if err != nil {
		return eval.Manifest{Dataset: path, DatasetKind: datasetKind(gate), ReleaseGate: gate}
	}
	return man
}

func datasetKind(gate bool) string {
	if gate {
		return "sandbox"
	}
	return "gold"
}

func datasetManifest(path string, gate bool) (eval.Manifest, error) {
	files, err := datasetFiles(path)
	if err != nil {
		return eval.Manifest{}, err
	}
	sum, err := eval.Fingerprint(files)
	if err != nil {
		return eval.Manifest{}, err
	}
	return eval.Manifest{
		Dataset: path, DatasetKind: datasetKind(gate), ReleaseGate: gate,
		DatasetSHA256: sum, GitSHA: gitSHA(), GoVersion: runtime.Version(),
	}, nil
}

func datasetFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	matches, err := filepath.Glob(filepath.Join(path, "*.gold.jsonl"))
	if err != nil {
		return nil, err
	}
	seed := filepath.Join(path, "seed.jsonl")
	if _, err := os.Stat(seed); err == nil {
		matches = append(matches, seed)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s 没有可指纹的金标准或 seed", path)
	}
	return matches, nil
}

func writeEvalReceipts(dir string, man eval.Manifest, valid []eval.GoldCase, rep *eval.Report, invalid int) error {
	lines, err := eval.BuildEvidence(valid, rep)
	if err != nil {
		return err
	}
	man.Invalid = invalid
	return eval.WriteReceipts(dir, man, rep, lines)
}

func gitSHA() string {
	out, err := exec.Command("git", "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
