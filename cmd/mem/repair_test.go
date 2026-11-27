package main

// mem repair evidence 的命令面测试：单条 / 批量 / 全量 / 预演，
// 以及「少于 100 条直接全量」的默认行为。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/store"
)

func newRepairDB(t *testing.T) (string, *store.Store) {
	t.Helper()
	db := t.TempDir() + "/mem.db"
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return db, st
}

// seedRepairableMemory 造一条「缺证据但补得回来」的记忆：
// 真观测存在，候选只写 obs_ids_json（模拟规则蒸馏路径的历史形态）。
func seedRepairableMemory(t *testing.T, st *store.Store, uid string) (memID, candID string) {
	t.Helper()
	rec, _, err := st.CaptureEvidence(store.EvidenceCaptureInput{
		EventUID: uid, SourceID: "src-p", SourceKind: "hook",
		ArchiveHash: "ah-" + uid, ArchivePath: "/tmp/" + uid + ".gz",
		SessionID: "s", Project: "p", EventType: "stop",
		Summary: "证据 " + uid, OccurredAtNS: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	added, err := st.Add(store.AddInput{Content: "记忆 " + uid, Project: "p", Kind: "fact"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	memID = added.ID
	candID = "cand-" + uid
	if _, err := st.DB().Exec(`INSERT INTO candidates(
		id, session_id, project, kind, content, content_hash, status, source,
		created_at, memory_id, obs_ids_json
	) VALUES (?, 's', 'p', 'fact', ?, ?, 'accepted', 'distill', ?, ?, ?)`,
		candID, "记忆 "+uid, "h-"+uid, time.Now().Unix(), memID,
		`["`+rec.ID+`"]`); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	return memID, candID
}

func TestRepairCLIHelpAndUnknown(t *testing.T) {
	if err := cmdRepair(nil); err == nil {
		t.Fatal("缺少子命令应报错")
	}
	if err := cmdRepair([]string{"nope"}); err == nil {
		t.Fatal("未知子命令应报错")
	}
	out, err := captureStdout(t, func() error { return cmdRepair([]string{"help"}) })
	if err != nil || !strings.Contains(out, "evidence") || !strings.Contains(out, "100") {
		t.Fatalf("help 应说明 evidence 与 100 条阈值，got %q err=%v", out, err)
	}
}

// 少于阈值时无参数直接全量，且预演不写库。
func TestRepairEvidenceDefaultAllAndDryRun(t *testing.T) {
	db, st := newRepairDB(t)
	memID, _ := seedRepairableMemory(t, st, "u1")

	out, err := captureStdout(t, func() error {
		return cmdRepairEvidence([]string{"--db", db, "--dry-run"})
	})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out, "按全量执行") || !strings.Contains(out, "待修复 1") {
		t.Fatalf("预演应识别出 1 条待修复，got %q", out)
	}
	var have int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_evidence WHERE memory_id = ?`, memID).Scan(&have); err != nil {
		t.Fatal(err)
	}
	if have != 0 {
		t.Fatalf("预演不得写库，got %d 条证据", have)
	}

	out, err = captureStdout(t, func() error {
		return cmdRepairEvidence([]string{"--db", db})
	})
	if err != nil {
		t.Fatalf("全量修复: %v", err)
	}
	if !strings.Contains(out, "修复 1 条") || !strings.Contains(out, "新增 1 条证据") {
		t.Fatalf("应修复 1 条，got %q", out)
	}
}

func TestRepairEvidenceJSONAndSingleID(t *testing.T) {
	db, st := newRepairDB(t)
	memID, _ := seedRepairableMemory(t, st, "u2")

	out, err := captureStdout(t, func() error {
		return cmdRepairEvidence([]string{memID[:8], "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("单条: %v", err)
	}
	var rep store.MemoryRepairReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("输出应是纯 JSON: %v\n%s", err, out)
	}
	if rep.Scanned != 1 || rep.Repaired != 1 || rep.AddedEvidence != 1 {
		t.Fatalf("单条修复结果不对: %+v", rep)
	}

	// 已修过的再跑幂等：不新增，计入 skipped。
	out, err = captureStdout(t, func() error {
		return cmdRepairEvidence([]string{"--db", db, "--all", "--json"})
	})
	if err != nil {
		t.Fatalf("幂等重跑: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if rep.Repaired != 0 || rep.Skipped != 1 {
		t.Fatalf("重跑应全部跳过，got %+v", rep)
	}
}

func TestRepairEvidenceRejectsAllWithIDs(t *testing.T) {
	db, _ := newRepairDB(t)
	if err := cmdRepairEvidence([]string{"abc", "--all", "--db", db}); err == nil {
		t.Fatal("--all 与具体 id 互斥")
	}
	if err := cmdRepairEvidence([]string{"no-such-memory", "--db", db}); err == nil {
		t.Fatal("不存在的 id 应报错")
	}
}

// renderRepairReport 的分支渲染（pending / repaired / failed）。
func TestRenderRepairReportBranches(t *testing.T) {
	rep := &store.MemoryRepairReport{
		Scanned: 3, Repaired: 1, Skipped: 1, Failed: 1, AddedEvidence: 2,
		Items: []store.MemoryRepairItem{
			{MemoryID: "aaaaaaaa1111", Status: "repaired", AddedEvidence: 2},
			{MemoryID: "bbbbbbbb2222", Status: "pending"},
			{MemoryID: "cccccccc3333", Status: "failed", Error: "boom"},
		},
	}
	out, err := captureStdout(t, func() error { return renderRepairReport(rep, false, false) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"修复 1 条", "+2 条证据", "失败: boom"} {
		if !strings.Contains(out, want) {
			t.Fatalf("执行态输出缺少 %q:\n%s", want, out)
		}
	}
	out, err = captureStdout(t, func() error { return renderRepairReport(rep, false, true) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "待修复 1 条") || !strings.Contains(out, "预演") {
		t.Fatalf("预演输出不对:\n%s", out)
	}
}
