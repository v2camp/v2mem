package main

// S10b 复核动作面（B6–B9）的命令面红 case。
//
// 全部通过现有可编译 API（cmdReview / cmdAccept / store 现有方法 / 直查 SQL）
// 表达「期望的新行为」，不引用任何尚未落地的符号：实现前命令返回未知子命令或
// flag 解析错误，逐 case FAIL；实现后逐个转绿。纯函数与新 store API 的分支级
// 单测随实现同批补齐（见 review_action_store_test.go 等）。

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// seedActionMemory 造一条记忆 + 产生它的候选 + 一条 model-auto 裁决账，
// 模拟 S10b 的真实生产者（reviewState 可预置 confirmed/corrected/withdrawn）。
func seedActionMemory(t *testing.T, st *store.Store, content, modelTag, reviewState string) (memID, candID string) {
	t.Helper()
	res, err := st.Add(store.AddInput{Content: content, Source: "distill-accept", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	memID, candID = res.ID, "cand-"+res.ID[:8]
	if _, err := st.DB().Exec(`UPDATE memories SET review_state = ?, reviewed_at = ?, reviewed_by = ?
		WHERE id = ?`, reviewState, 1700000000, "human", memID); err != nil {
		t.Fatalf("preset review_state: %v", err)
	}
	if _, err := st.DB().Exec(`INSERT INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at, memory_id)
		VALUES (?, 's', 'p', 'fact', ?, ?, 'accepted', 'distill', ?, ?)`,
		candID, content, "h-"+candID, 1700000000, memID); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	if _, err := st.DB().Exec(`INSERT INTO commit_decisions
		(id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
		 memory_id, model_tag, created_memory, idempotency_key, created_at)
		VALUES (?,?,0,'','auto_commit','model-auto','[]',?,?,1,?,1700000000)`,
		"dec-"+candID, candID, memID, modelTag, "key-"+candID); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	return memID, candID
}

// jnum 读 JSON 里的整数（json.Unmarshal 到 map 时数字是 float64）。
func jnum(m map[string]any, k string) int {
	v, ok := m[k].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

func jmap(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func jslice(m map[string]any, k string) []any {
	v, _ := m[k].([]any)
	return v
}

func decodeActionJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json 输出应可解析: %v\n%s", err, out)
	}
	return got
}

func memoryReviewRow(t *testing.T, db, id string) (state, by string, at int64) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.DB().QueryRow(`SELECT review_state, reviewed_by, reviewed_at FROM memories WHERE id = ?`, id).
		Scan(&state, &by, &at); err != nil {
		t.Fatalf("读记忆: %v", err)
	}
	return
}

func memoryVisible(t *testing.T, db, id string) bool {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	listed, err := st.List(store.ListQuery{Scope: "all", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range listed {
		if h.ID == id {
			return true
		}
	}
	return false
}

// R1：confirm 写 confirmed + reviewed_by=human，检索仍可见，--json 带前后状态。
func TestReviewConfirmSetsConfirmedByHuman(t *testing.T) {
	st := newJudgeStore(t)
	memID, _ := seedActionMemory(t, st, "确认这条模型自动写库的记忆", "m@v1", "")
	db := st.Path()
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"confirm", memID, "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("mem review confirm 应成功: %v\n%s", err, out)
	}
	got := decodeActionJSON(t, out)
	if got["action"] != "confirm" || got["memory_id"] != memID {
		t.Fatalf("JSON 应带 action/memory_id，got %v", got)
	}
	if jmap(got, "before")["review_state"] != "" || jmap(got, "after")["review_state"] != "confirmed" {
		t.Fatalf("JSON 应带 ''→confirmed 的前后状态，got %v", got)
	}
	state, by, at := memoryReviewRow(t, db, memID)
	if state != "confirmed" || by != "human" || at == 0 {
		t.Fatalf("确认应写 confirmed/human/ reviewed_at，got %q/%q/%d", state, by, at)
	}
	if !memoryVisible(t, db, memID) {
		t.Fatal("确认不改变检索可见性")
	}
}

// R1：correct 保存人工修订正文，状态 corrected，原提案写进裁决账供比较。
func TestReviewCorrectSavesHumanContentAndKeepsOriginal(t *testing.T) {
	st := newJudgeStore(t)
	memID, _ := seedActionMemory(t, st, "模型原提案：端口是 8080", "m@v1", "")
	db := st.Path()
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"correct", memID, "--content", "人工修订：端口是 9090", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("mem review correct 应成功: %v\n%s", err, out)
	}
	got := decodeActionJSON(t, out)
	if got["action"] != "correct" || jmap(got, "after")["review_state"] != "corrected" {
		t.Fatalf("correct 应转 corrected，got %v", got)
	}
	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var content string
	if err := st2.DB().QueryRow(`SELECT content FROM memories WHERE id = ?`, memID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "人工修订：端口是 9090" {
		t.Fatalf("人工修订正文应落库，got %q", content)
	}
	// 模型原提案不新增 schema 列，写进 commit_decisions.reason_codes_json 的 before/after。
	var reasons string
	if err := st2.DB().QueryRow(`SELECT reason_codes_json FROM commit_decisions
		WHERE action = 'review_action' AND memory_id = ?`, memID).Scan(&reasons); err != nil {
		t.Fatalf("correct 应记一条 review_action: %v", err)
	}
	if !strings.Contains(reasons, "模型原提案") || !strings.Contains(reasons, "人工修订") {
		t.Fatalf("裁决账应保留 before/after 供比较，got %s", reasons)
	}
	state, by, _ := memoryReviewRow(t, db, memID)
	if state != "corrected" || by != "human" {
		t.Fatalf("correct 应记 corrected/human，got %q/%q", state, by)
	}
}

// R1：withdraw 立即退出检索，行与证据保留，候选恢复 rejected 明确终态，记 review_action。
func TestReviewWithdrawHidesMemoryAndRejectsCandidate(t *testing.T) {
	st := newJudgeStore(t)
	memID, candID := seedActionMemory(t, st, "写错了要一步撤回的记忆", "m@v1", "")
	db := st.Path()
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"withdraw", memID, "--reason", "事实有误", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("mem review withdraw 应成功: %v\n%s", err, out)
	}
	got := decodeActionJSON(t, out)
	if got["action"] != "withdraw" || jmap(got, "after")["review_state"] != "withdrawn" {
		t.Fatalf("withdraw 应转 withdrawn，got %v", got)
	}
	if len(jslice(got, "affected_candidates")) == 0 {
		t.Fatalf("JSON 应报告受影响候选 id，got %v", got)
	}
	if memoryVisible(t, db, memID) {
		t.Fatal("撤回必须立即退出 Search/List")
	}
	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var n int
	if err := st2.DB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, memID).Scan(&n); err != nil || n != 1 {
		t.Fatal("撤回只撤状态，行与证据必须保留")
	}
	var status, reason string
	if err := st2.DB().QueryRow(`SELECT status, status_reason FROM candidates WHERE id = ?`, candID).
		Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || reason == "" {
		t.Fatalf("产生该记忆的候选应恢复 rejected + status_reason，got %q/%q", status, reason)
	}
	var actions int
	if err := st2.DB().QueryRow(`SELECT COUNT(*) FROM commit_decisions
		WHERE action = 'review_action' AND actor = 'human' AND memory_id = ?`, memID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != 1 {
		t.Fatalf("withdraw 应记一条 review_action 裁决，got %d", actions)
	}
}

// R1：restore 是撤回的逆动作，review_state=” 且清 reviewed_*，检索重新可见。
func TestReviewRestoreUnwithdrawsMemory(t *testing.T) {
	st := newJudgeStore(t)
	memID, _ := seedActionMemory(t, st, "撤回后还能恢复的记忆", "m@v1", "withdrawn")
	db := st.Path()
	st.Close()

	// 先真撤回一次（夹具只预置了状态，这里走动作面把撤销链补齐）。
	_, err := captureStdout(t, func() error {
		return cmdReview([]string{"restore", memID, "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("mem review restore 应成功: %v\n%s", err, "")
	}
	state, by, at := memoryReviewRow(t, db, memID)
	if state != "" || by != "" || at != 0 {
		t.Fatalf("restore 应清 review_state/reviewed_*，got %q/%q/%d", state, by, at)
	}
	if !memoryVisible(t, db, memID) {
		t.Fatal("restore 后应重新进入检索")
	}
}

// R2：未知 id 与重复撤回如实报错（退出码由 main 的 err!=nil 兜底）。
func TestReviewUnknownIDAndDuplicateWithdrawFail(t *testing.T) {
	st := newJudgeStore(t)
	db := st.Path()
	st.Close()
	for _, sub := range []string{"confirm", "correct", "withdraw", "restore"} {
		if err := cmdReview([]string{sub, "no-such-memory", "--db", db}); err == nil {
			t.Errorf("review %s 未知 id 必须报错", sub)
		}
	}
	memID, _ := seedActionMemory(t, reopenForAction(t, db), "撤回一次就不能再撤的记忆", "m@v1", "")
	if err := cmdReview([]string{"withdraw", memID, "--db", db, "--json"}); err != nil {
		t.Fatalf("首次 withdraw 应成功: %v", err)
	}
	if err := cmdReview([]string{"withdraw", memID, "--db", db, "--json"}); err == nil {
		t.Fatal("重复撤回必须报错（状态不合法），不得静默成功")
	}
	if err := cmdReview([]string{"restore", memID, "--db", db, "--json"}); err != nil {
		t.Fatalf("撤回后 restore 应可恢复: %v", err)
	}
	if err := cmdReview([]string{"restore", memID, "--db", db, "--json"}); err == nil {
		t.Fatal("对非 withdrawn 的 restore 必须报错（状态不合法）")
	}
}

// reopenForAction 在 st.Close() 之后重新打开同一个 db 供夹具写入。
func reopenForAction(t *testing.T, db string) *store.Store {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// R3：按版本回滚跳过人工确认/修正、只命中标的 tag、幂等、dry-run 零写入。
func TestReviewRollbackSkipsHumanAndOnlyTargetsTag(t *testing.T) {
	st := newJudgeStore(t)
	m1, _ := seedActionMemory(t, st, "m1 待撤的自动记忆", "m@v1", "")
	m2, _ := seedActionMemory(t, st, "m2 人工确认过要跳过", "m@v1", "confirmed")
	m3, _ := seedActionMemory(t, st, "m3 人工修正过要跳过", "m@v1", "corrected")
	m4, _ := seedActionMemory(t, st, "m4 已经撤回计 already", "m@v1", "withdrawn")
	m5, _ := seedActionMemory(t, st, "m5 别的版本不许碰", "m@v2", "")
	m6, _ := seedActionMemory(t, st, "m6 别的版本不许碰2", "m@v2", "")
	db := st.Path()
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"rollback", "--model-tag", "m@v1", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("mem review rollback 应成功: %v\n%s", err, out)
	}
	got := decodeActionJSON(t, out)
	if got["model_tag"] != "m@v1" || jnum(got, "matched") != 4 || jnum(got, "withdrawn") != 1 ||
		jnum(got, "skipped") != 2 || jnum(got, "already") != 1 {
		t.Fatalf("计数应为 matched4/withdrawn1/skipped2/already1，got %v", got)
	}
	if jnum(got, "reopened") != 1 {
		t.Fatalf("应为被撤的候选写一条 reopen 决策，got %v", got)
	}
	if state, _, _ := memoryReviewRow(t, db, m1); state != "withdrawn" {
		t.Errorf("m1 应撤回，got %q", state)
	}
	for id, want := range map[string]string{m2: "confirmed", m3: "corrected", m5: "", m6: ""} {
		if state, _, _ := memoryReviewRow(t, db, id); state != want {
			t.Errorf("%s 不得被回滚改动，got %q want %q", id, state, want)
		}
	}
	// 幂等重跑：m1 已 withdrawn → already，不再产生动作。
	out, err = captureStdout(t, func() error {
		return cmdReview([]string{"rollback", "--model-tag", "m@v1", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	got = decodeActionJSON(t, out)
	if jnum(got, "withdrawn") != 0 || jnum(got, "already") != 2 || jnum(got, "reopened") != 0 {
		t.Fatalf("重跑不得重复动作，got %v", got)
	}
	_ = m4 // already 的明细由计数断言覆盖
}

func TestReviewRollbackDryRunIsZeroWrite(t *testing.T) {
	st := newJudgeStore(t)
	m1, candID := seedActionMemory(t, st, "dry-run 不许改库的记忆", "m@v1", "")
	db := st.Path()
	st.Close()

	before := snapshotActionDB(t, db)
	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"rollback", "--model-tag", "m@v1", "--dry-run", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("--dry-run 应成功: %v\n%s", err, out)
	}
	got := decodeActionJSON(t, out)
	if got["dry_run"] != true || jnum(got, "withdrawn") != 1 {
		t.Fatalf("--dry-run 要照常报计划动作，got %v", got)
	}
	after := snapshotActionDB(t, db)
	if before != after {
		t.Fatalf("--dry-run 必须零写入:\nbefore=%+v\nafter=%+v", before, after)
	}
	if state, _, _ := memoryReviewRow(t, db, m1); state != "" {
		t.Fatalf("--dry-run 不得改 review_state，got %q", state)
	}
	st2, _ := store.Open(db)
	defer st2.Close()
	var status string
	_ = st2.DB().QueryRow(`SELECT status FROM candidates WHERE id = ?`, candID).Scan(&status)
	if status != "accepted" {
		t.Fatalf("--dry-run 不得改候选状态，got %q", status)
	}
}

// snapshotActionDB 取「会被回滚改到的列」的指纹，用于 dry-run 零写入断言。
func snapshotActionDB(t *testing.T, db string) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var s string
	if err := st.DB().QueryRow(`SELECT
		(SELECT GROUP_CONCAT(id||':'||review_state||':'||reviewed_at||':'||reviewed_by) FROM
			(SELECT id, review_state, reviewed_at, reviewed_by FROM memories ORDER BY id)) || '|' ||
		(SELECT GROUP_CONCAT(id||':'||status||':'||status_reason) FROM
			(SELECT id, status, status_reason FROM candidates ORDER BY id)) || '|' ||
		(SELECT COUNT(*) FROM commit_decisions)`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// R4：stats 按 model_tag 分组给标定率，阈值只提示 WARN，不改提示词、不自动动作。
func TestReviewStatsGroupsByModelTagAndWarnsOnly(t *testing.T) {
	st := newJudgeStore(t)
	for _, c := range []struct{ content, tag, state string }{
		{"v1 已确认样本", "m@v1", "confirmed"},
		{"v1 已修正样本", "m@v1", "corrected"},
		{"v1 已撤回样本", "m@v1", "withdrawn"},
		{"v1 未审核样本", "m@v1", ""},
		{"v2 已确认样本", "m@v2", "confirmed"},
	} {
		seedActionMemory(t, st, c.content, c.tag, c.state)
	}
	db := st.Path()
	st.Close()

	before := snapshotActionDB(t, db)
	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"stats", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("mem review stats 应成功: %v\n%s", err, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("stats --json 应可解析为数组: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("应按 model_tag 分 2 组，got %d: %s", len(rows), out)
	}
	byTag := map[string]map[string]any{}
	for _, r := range rows {
		tag, _ := r["model_tag"].(string)
		byTag[tag] = r
	}
	v1, v2 := byTag["m@v1"], byTag["m@v2"]
	if v1 == nil || v2 == nil {
		t.Fatalf("分组键应为 model_tag，got %v", byTag)
	}
	if jnum(v1, "samples") != 4 || jnum(v1, "confirmed") != 1 ||
		jnum(v1, "corrected") != 1 || jnum(v1, "withdrawn") != 1 {
		t.Fatalf("v1 样本统计错，got %v", v1)
	}
	if jnum(v2, "samples") != 1 || jnum(v2, "confirmed") != 1 {
		t.Fatalf("v2 样本统计错，got %v", v2)
	}
	// 阈值只提示：撤回率越线的组带 warn=true，但不产生任何写库动作。
	if v1["warn"] != true {
		t.Fatalf("撤回率越线只应提示 WARN，got warn=%v", v1["warn"])
	}
	if snapshotActionDB(t, db) != before {
		t.Fatal("stats 必须只读：不得改库、不得自动动作")
	}
	// 文本形态也要有 WARN 提示。
	text, err := captureStdout(t, func() error {
		return cmdReview([]string{"stats", "--db", db})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "m@v1") || !strings.Contains(text, "m@v2") {
		t.Fatalf("文本应列出各版本，got:\n%s", text)
	}
}

// ---------- R6：model-auto 默认关 + 真实生产者 ----------

// actionSeedSeq 保证批量夹具的内容各不相同：InsertCandidate 与 upsertAcceptedMemory
// 都按 content_hash 去重，同内容只会收敛成一条，批量抽样断言就失效了。
var actionSeedSeq int64

// seedModelAutoCandidate 造一条可选带 A 级直接证据的候选，供 --model-auto 验证提交矩阵。
func seedModelAutoCandidate(t *testing.T, st *store.Store, kind, scope, project string, withEvidence bool) string {
	t.Helper()
	res, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Project: project, Kind: kind,
		Content: fmt.Sprintf("候选正文-%s-%s-%s-%d", kind, scope, project,
			atomic.AddInt64(&actionSeedSeq, 1)), Source: "distill",
	})
	if err != nil {
		t.Fatalf("InsertCandidate: %v", err)
	}
	id := res.Candidate.ID
	if _, err := st.DB().Exec(`UPDATE candidates SET scope = ? WHERE id = ?`, scope, id); err != nil {
		t.Fatalf("set scope: %v", err)
	}
	if withEvidence {
		if _, err := st.DB().Exec(`INSERT INTO candidate_evidence(
			candidate_id, evidence_id, event_uid, source_id, source_seq, archive_hash,
			evidence_role, created_at) VALUES (?,?,?,?,?,?,?,1700000000)`,
			id, "ev-"+id[:8], "uid-"+id[:8], "src-p", 1, "ah-"+id[:8], "direct"); err != nil {
			t.Fatalf("insert candidate_evidence: %v", err)
		}
	}
	return id
}

// 默认关：不带 --model-auto 时 decision 仍是 human_accept（不改变既有行为）。
func TestAcceptModelAutoOffByDefault(t *testing.T) {
	st := newJudgeStore(t)
	id := seedModelAutoCandidate(t, st, "decision", "project", "p", true)
	db := st.Path()
	st.Close()
	out, err := captureStdout(t, func() error {
		return cmdAccept([]string{"--db", db, "--auto", "--json", "--audit-file", "-", id})
	})
	if err != nil {
		t.Fatalf("默认行为不应受 model-auto 影响: %v\n%s", err, out)
	}
	if num(decodeJSONOut(t, out), "inserted") != 0 {
		t.Fatal("默认关时 decision 不得自动落库")
	}
}

// 开启后：裁决 actor=model-auto、model_tag 落 commit_decisions、记忆留未审核态，
// 且复核摘要（高影响约 10% 抽样）从此有真实生产者的数据而不是纯夹具。
func TestAcceptModelAutoProducesModelAutoDecision(t *testing.T) {
	st := newJudgeStore(t)
	var ids []string
	for i := 0; i < 40; i++ {
		ids = append(ids, seedModelAutoCandidate(t, st, "decision", "project", "p", true))
	}
	db := st.Path()
	st.Close()
	for _, id := range ids {
		out, err := captureStdout(t, func() error {
			return cmdAccept([]string{"--model-auto", "--model-tag", "m@v3", "--db", db,
				"--json", "--audit-file", "-", id})
		})
		if err != nil {
			t.Fatalf("mem accept --model-auto 应成功: %v\n%s", err, out)
		}
		if num(decodeJSONOut(t, out), "inserted") != 1 {
			t.Fatalf("开启后 manualKind 应可自动 accept，got %s", out)
		}
	}
	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var actor, tag, memID string
	if err := st2.DB().QueryRow(`SELECT actor, model_tag, memory_id FROM commit_decisions
		WHERE candidate_id = ? AND action = 'auto_commit'`, ids[0]).Scan(&actor, &tag, &memID); err != nil {
		t.Fatalf("应写一条 model-auto 裁决: %v", err)
	}
	if actor != "model-auto" || tag != "m@v3" || memID == "" {
		t.Fatalf("归因错: actor=%q tag=%q mem=%q", actor, tag, memID)
	}
	var state string
	if err := st2.DB().QueryRow(`SELECT review_state FROM memories WHERE id = ?`, memID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		t.Fatalf("model-auto 产物应留未审核态供后审核，got %q", state)
	}
	q, err := st2.ListReviewQueue(store.ReviewQueueSummary, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 40 条高影响产物按确定性 10% 抽样，几乎必有样本；不再依赖手工夹具。
	if q.Counts[store.ReviewQueueSummary] < 1 {
		t.Fatalf("summary 段应从真实生产者拿到数据，got %+v", q.Counts)
	}
}

// 开启后 Conflict / Defer 仍优先保留：不得为消灭 pending 强行写入。
func TestAcceptModelAutoKeepsConflictAndDefer(t *testing.T) {
	st := newJudgeStore(t)
	// scope=conflict 触发矩阵的 ActionConflict（InsertCandidate 的 kind 白名单不含 conflict）。
	conflict := seedModelAutoCandidate(t, st, "fact", "conflict", "p", true)
	deferKind := seedModelAutoCandidate(t, st, "fact", "project", "p", false) // 无 A 级证据 → Defer
	db := st.Path()
	st.Close()
	for _, id := range []string{conflict, deferKind} {
		out, err := captureStdout(t, func() error {
			return cmdAccept([]string{"--model-auto", "--model-tag", "m@v3", "--db", db,
				"--json", "--audit-file", "-", id})
		})
		if err != nil {
			t.Fatalf("Conflict/Defer 应是「需人工」而非故障: %v\n%s", err, out)
		}
		if num(decodeJSONOut(t, out), "inserted") != 0 {
			t.Fatalf("%s 不得写记忆，got %s", id, out)
		}
	}
	if countAllMemories(t, db) != 0 {
		t.Fatal("Conflict/Defer 一律不得自动落库")
	}
}

// 幂等重放：同一候选重复 --model-auto 不新增记忆，裁决账稳定后不再增长。
func TestAcceptModelAutoIdempotentReplay(t *testing.T) {
	st := newJudgeStore(t)
	id := seedModelAutoCandidate(t, st, "decision", "project", "p", true)
	db := st.Path()
	st.Close()
	decisionCounts := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		out, err := captureStdout(t, func() error {
			return cmdAccept([]string{"--model-auto", "--model-tag", "m@v3", "--db", db,
				"--json", "--audit-file", "-", id})
		})
		if err != nil {
			t.Fatalf("第 %d 次重放: %v\n%s", i+1, err, out)
		}
		st2, err := store.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		_ = st2.DB().QueryRow(`SELECT COUNT(*) FROM commit_decisions WHERE candidate_id = ?`, id).Scan(&n)
		st2.Close()
		decisionCounts = append(decisionCounts, n)
	}
	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var mems int
	_ = st2.DB().QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&mems)
	if mems != 1 {
		t.Fatalf("重放不得重复新建记忆: memories=%d", mems)
	}
	if decisionCounts[2] != decisionCounts[1] {
		t.Fatalf("稳定后重放不得新增裁决: %v", decisionCounts)
	}
}

// R3（Wave 2/E）：review rollback 同时回滚该 tag 的 Judge 驳回（候选回 pending），
// JSON 增 judge_reopened/judge_skipped 且旧字段不变；文本输出要能看懂这件事。
func TestReviewRollbackJudgeRejectByModelTag(t *testing.T) {
	st := newJudgeStore(t)
	res, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Project: "p", Kind: "fact",
		Content: "巡检已完成，报告已输出", Source: "distill",
	})
	if err != nil {
		t.Fatalf("InsertCandidate: %v", err)
	}
	candID := res.Candidate.ID
	if _, err := st.DB().Exec(`UPDATE candidates
		SET status = 'rejected', status_reason = 'judge_reject', decided_at = 1700000100
		WHERE id = ?`, candID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO commit_decisions
		(id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
		 memory_id, model_tag, created_memory, idempotency_key, created_at)
		VALUES (?,?,0,'','reject','judge','["judge_reject"]','',?,0,?,1700000100)`,
		"dec-"+candID, candID, "judge@prompt-v4", "key-"+candID); err != nil {
		t.Fatal(err)
	}
	db := st.Path()
	st.Close()

	candStatus := func() string {
		s2, err := store.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		defer s2.Close()
		var status string
		_ = s2.DB().QueryRow(`SELECT status FROM candidates WHERE id = ?`, candID).Scan(&status)
		return status
	}

	// dry-run：JSON 报计划动作且零写入。
	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"rollback", "--model-tag", "judge@prompt-v4", "--dry-run", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	got := decodeActionJSON(t, out)
	if jnum(got, "judge_reopened") != 1 || jnum(got, "judge_skipped") != 0 {
		t.Fatalf("dry-run 应报 judge_reopened=1，got %v", got)
	}
	// 旧字段仍在，计数为零而不是缺键。
	for _, k := range []string{"matched", "withdrawn", "skipped", "already", "reopened"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("旧字段 %q 不得缺失: %v", k, got)
		}
	}
	if status := candStatus(); status != "rejected" {
		t.Fatalf("--dry-run 不得改候选状态，got %q", status)
	}

	// 文本输出要能看懂 Judge 驳回这一段。
	out, err = captureStdout(t, func() error {
		return cmdReview([]string{"rollback", "--model-tag", "judge@prompt-v4", "--db", db})
	})
	if err != nil {
		t.Fatalf("文本输出: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Judge 驳回") {
		t.Fatalf("文本输出应说明 Judge 驳回回滚，got %q", out)
	}
	if status := candStatus(); status != "pending" {
		t.Fatalf("judge 驳回候选应回 pending，got %q", status)
	}

	// 重跑幂等：JSON 计数归零。
	out, err = captureStdout(t, func() error {
		return cmdReview([]string{"rollback", "--model-tag", "judge@prompt-v4", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("重跑: %v\n%s", err, out)
	}
	got = decodeActionJSON(t, out)
	if jnum(got, "judge_reopened") != 0 || jnum(got, "judge_skipped") != 0 {
		t.Fatalf("重跑不得重复动作，got %v", got)
	}
}
