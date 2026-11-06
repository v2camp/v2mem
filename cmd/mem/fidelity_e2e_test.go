package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
)

// 本文件是 T6 的端到端链路验证：不只测 observationText 构造层，而是让
// cmdHook 把 stdin 走完整条写路径（stdin → observationText → evidence.Engine
// → store.CaptureEvidence），再用 store.Open / evidence.New(st) 读回，证明
// 「构造层标注的 Fidelity」与「落库后用户可见的 summary / Ref」一致。
//
// 独占范围：本文件为 T6 新增。所有库都用 t.TempDir()，--audit - 关闭审计，
// 不触碰 ~/.v2mem，不跑 install/smoke/release。

func runFidelityHook(t *testing.T, db string, fields map[string]any) {
	t.Helper()
	in := hookJSON(t, fields)
	if _, err := withStdin(t, in, func() error {
		return cmdHook([]string{"--db", db, "--harness", "trae", "--dedup-window", "0", "--audit", "-"})
	}); err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
}

// fidelityRefs 出窗读回全部证据引用（含 Fidelity / ProposalEligible / Summary）。
func fidelityRefs(t *testing.T, st *store.Store) []evidence.Ref {
	t.Helper()
	e := evidence.New(st)
	b, err := e.NextBundle(50, 1<<20)
	if err != nil {
		t.Fatalf("NextBundle: %v", err)
	}
	if b == nil {
		t.Fatal("应能出窗至少包含本次观测")
	}
	refs, err := e.ListEvidence(b.ID)
	if err != nil {
		t.Fatalf("ListEvidence: %v", err)
	}
	return refs
}

// archivedRaw 从 Ref.RawLocator 指向的 gzip segment 解出事实源原始字节，
// 用来断言「无载荷事件没有把伪造内容写进归档」。
func archivedRaw(t *testing.T, ref evidence.Ref) []byte {
	t.Helper()
	path := ref.RawLocator
	if i := strings.LastIndex(path, "#"); i >= 0 {
		path = path[:i]
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开归档: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("读归档: %v", err)
	}
	var rec struct {
		Raw []byte `json:"raw"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("解析归档: %v", err)
	}
	return rec.Raw
}

// 用例 A：Trae Stop 带 last_assistant_message（真实正文）→ 展示摘要含正文、
// 不含 "stop" 字面量；证据读回 Fidelity=full 且 ProposalEligible=true。
func TestFidelityE2E_RealBodyIsFullAndEligible(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	sid := "fid-full"
	const body = "FIDELITY_MARKER_A 根因是 npx 缓存目录权限位，不是网络抖动"

	runFidelityHook(t, db, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
		"last_assistant_message": body,
	})

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	obs, err := st.ListObservations(sid, 10)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("一次带正文的 Stop 应产生 1 条 observation，got %d: %+v", len(obs), obs)
	}
	if !strings.Contains(obs[0].Summary, "FIDELITY_MARKER_A") {
		t.Fatalf("展示摘要必须含真实正文，got %q", obs[0].Summary)
	}
	if strings.Contains(obs[0].Summary, "stop") {
		t.Fatalf("展示摘要不得是 stop 字面量，got %q", obs[0].Summary)
	}

	// 端到端读回身份层：Fidelity=full 且总开关 ProposalEligible=true。
	rec, ok, err := st.LookupEvidence(obs[0].EventUID, obs[0].SourceID, 0, 0)
	if err != nil || !ok {
		t.Fatalf("LookupEvidence: ok=%v err=%v", ok, err)
	}
	if rec.Fidelity != "full" {
		t.Fatalf("带真实正文的 Stop Fidelity 应为 full，got %q", rec.Fidelity)
	}
	if !rec.ProposalEligible {
		t.Fatalf("带真实正文的 Stop 应可提案")
	}
	refs := fidelityRefs(t, st)
	if len(refs) != 1 || refs[0].Fidelity != evidence.FidelityFull || !refs[0].ProposalEligible {
		t.Fatalf("ListEvidence 读回应带 full + eligible，got %+v", refs)
	}
	if !strings.Contains(refs[0].Summary, "FIDELITY_MARKER_A") {
		t.Fatalf("Ref.Summary 应含真实正文，got %q", refs[0].Summary)
	}
}

// 用例 B：无任何载荷的 Stop → Fidelity=none、事实源为空、事件本身仍留痕。
//
// 【已知剩余缺口 / 需主会话裁决，见交付「风险/需裁决」】
// observations.summary 当前仍是 "stop"：T6 已让 engine.go 的 summarizeRaw
// 空载荷 Stop 有两处「事件名当正文」的回落：internal/evidence/engine.go 的
// summarizeRaw 与 internal/store/evidence.go 的 CaptureEvidence。两处均已删除，
// 因此展示摘要为空。本用例锁死整条链路的可证伪不变量：Fidelity=none、
// 归档 Raw 为空、event 列仍记录事件类型、evidence 与 observation 的摘要都
// 不含 "stop" 字面量；提案准入由 internal/proposal 依据 Fidelity=none 拦截
// （该私有判定已由 proposal 单元测试覆盖，cmd/mem 不直接调用）。
func TestFidelityE2E_NoPayloadIsNone(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	sid := "fid-none"

	runFidelityHook(t, db, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
	})

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	obs, err := st.ListObservations(sid, 10)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("缺载荷 Stop 仍应留下 1 条事件记录，got %d: %+v", len(obs), obs)
	}
	// event 列已记录事件类型：这正是「摘要无需再塞事件名」的依据。
	if obs[0].Event != "stop" {
		t.Fatalf("event 列应记录 stop，got %q", obs[0].Event)
	}

	rec, ok, err := st.LookupEvidence(obs[0].EventUID, obs[0].SourceID, 0, 0)
	if err != nil || !ok {
		t.Fatalf("LookupEvidence: ok=%v err=%v", ok, err)
	}
	if rec.Fidelity != "none" {
		t.Fatalf("缺载荷 Stop Fidelity 应为 none，got %q", rec.Fidelity)
	}
	refs := fidelityRefs(t, st)
	if len(refs) != 1 || refs[0].Fidelity != evidence.FidelityNone {
		t.Fatalf("ListEvidence 读回应为 none，got %+v", refs)
	}
	// 事实源（归档 Raw）必须为空：无载荷绝不伪造正文写进归档。
	if raw := archivedRaw(t, refs[0]); len(strings.TrimSpace(string(raw))) != 0 {
		t.Fatalf("无载荷事件归档 Raw 应为空，got %q", raw)
	}
	// 没有真实正文被伪造进 evidence 的任何可见字段。
	if strings.Contains(obs[0].Summary, "FIDELITY_MARKER") ||
		strings.Contains(refs[0].Summary, "FIDELITY_MARKER") {
		t.Fatalf("无载荷事件不得出现伪造正文，obs=%q ref=%q", obs[0].Summary, refs[0].Summary)
	}

	// 展示层止血：两处回落都删掉后，空载荷不得再把事件名当正文显示。
	if strings.Contains(obs[0].Summary, "stop") {
		t.Fatalf("无载荷 Stop 展示摘要不得是 stop 字面量，got %q", obs[0].Summary)
	}
	if strings.Contains(refs[0].Summary, "stop") {
		t.Fatalf("无载荷证据摘要不得是 stop 字面量，got %q", refs[0].Summary)
	}
	// 证据行带 canonical 身份，不受 ux_obs_dedup_legacy 的
	// (session_id, content_hash) 部分索引约束；空摘要不触发 legacy 去重。
	if strings.TrimSpace(obs[0].EventUID) == "" {
		t.Fatal("证据行应带 event_uid，空摘要不应落入 legacy 内容去重路径")
	}
}

// 用例 C：transcript 的 unparsed 行仍 FidelityPointer + ProposalEligible=false，
// 确认 T4 行为未回退（原文保住但语义未解析，不进提案）。
func TestFidelityE2E_UnparsedTranscriptStaysPointer(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	path := filepath.Join(t.TempDir(), "tx-fid.jsonl")
	sid := "fid-pointer"
	const marker = "FIDELITY_UNPARSED_MARKER_原文不丢"
	writeClaudeTranscript(t, path,
		`{"role":"user","content":"`+marker+`","sessionId":"`+sid+`"}`,
	)

	runFidelityHook(t, db, map[string]any{
		"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
		"transcript_path": path,
	})

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	obs, err := st.ListObservations(sid, 30)
	if err != nil {
		t.Fatalf("ListObservations: %v", err)
	}
	var unparsedUID string
	for _, o := range obs {
		if o.Event == "unparsed" {
			unparsedUID = o.EventUID
		}
	}
	if unparsedUID == "" {
		t.Fatalf("应留下 unparsed 证据行，got %+v", obs)
	}
	rec, ok, err := st.LookupEvidence(unparsedUID, "", 0, 0)
	if err != nil || !ok {
		t.Fatalf("LookupEvidence(unparsed): ok=%v err=%v", ok, err)
	}
	if rec.Fidelity != "pointer" {
		t.Fatalf("unparsed 行 Fidelity 应为 pointer，got %q", rec.Fidelity)
	}
	if rec.ProposalEligible {
		t.Fatalf("unparsed 行仍须 ProposalEligible=false")
	}
}
