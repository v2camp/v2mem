package main

// R5：修 S10a 遗留的 count/list JOIN 集合不一致，并给复核摘要接上真实口径
// （三类：低置信 / 高影响确定性抽样 / 冲突标记；其余默认信任不进摘要）。
//
// 全部通过现有 ListReviewQueue + 直查 SQL 表达，实现前逐 case FAIL。

import (
	"fmt"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// 候选被 GC 后，count 与 list 必须仍一致（S10a 遗留：count 1 / list 0）。
func TestReviewSummaryCountListConsistentAfterCandidateGC(t *testing.T) {
	st := newJudgeStore(t)
	memID, _ := seedActionMemory(t, st, "候选 GC 后计数与列表必须一致", "m@v1", "")
	// 低置信样本（三类之一）：修复后靠它进摘要。
	if _, err := st.DB().Exec(`UPDATE commit_decisions
		SET reason_codes_json = '["low_confidence"]' WHERE memory_id = ?`, memID); err != nil {
		t.Fatal(err)
	}
	// 模拟候选 GC：候选行没了，记忆与裁决账还在（行与证据保留）。
	if _, err := st.DB().Exec(`DELETE FROM candidates`); err != nil {
		t.Fatal(err)
	}
	q, err := st.ListReviewQueue(store.ReviewQueueSummary, 50)
	if err != nil {
		t.Fatalf("ListReviewQueue: %v", err)
	}
	if q.Counts[store.ReviewQueueSummary] != 1 || len(q.States[store.ReviewQueueSummary]) != 1 {
		t.Fatalf("count 与 list 必须同源：count=%d list=%d（候选 GC 后不得出现 1/0）",
			q.Counts[store.ReviewQueueSummary], len(q.States[store.ReviewQueueSummary]))
	}
	if q.States[store.ReviewQueueSummary][0].MemoryID != memID {
		t.Fatalf("列表应仍列出该记忆，got %+v", q.States[store.ReviewQueueSummary])
	}
}

// 高影响抽样：同一 id 多次结果相同（确定性），命中约 10%，不引状态列。
func TestReviewSummaryDeterministicSampleAbout10Percent(t *testing.T) {
	st := newJudgeStore(t)
	for i := 0; i < 200; i++ {
		content := fmt.Sprintf("高影响抽样样本 %d", i)
		res, err := st.Add(store.AddInput{
			Content: content, Source: "distill-accept", Project: "p", Kind: "decision",
		})
		if err != nil {
			t.Fatal(err)
		}
		cid := "cand-" + res.ID[:8]
		if _, err := st.DB().Exec(`INSERT INTO candidates
			(id, session_id, project, kind, content, content_hash, status, source, created_at)
			VALUES (?, 's', 'p', 'decision', ?, ?, 'accepted', 'distill', 1700000000)`,
			cid, content, "h-"+cid); err != nil {
			t.Fatal(err)
		}
		// reason='[]'：不带低置信/冲突标记，只能靠高影响抽样进摘要。
		if _, err := st.DB().Exec(`INSERT INTO commit_decisions
			(id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
			 memory_id, model_tag, created_memory, idempotency_key, created_at)
			VALUES (?,?,0,'','auto_commit','model-auto','[]',?,'m@v1',1,?,1700000000)`,
			"dec-"+cid, cid, res.ID, "key-"+cid); err != nil {
			t.Fatal(err)
		}
	}
	q1, err := st.ListReviewQueue(store.ReviewQueueSummary, 1000)
	if err != nil {
		t.Fatal(err)
	}
	q2, err := st.ListReviewQueue(store.ReviewQueueSummary, 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := q1.Counts[store.ReviewQueueSummary]
	// 200 条 × 10% ≈ 20；给足容差，只钉「约 10%」而不是精确值。
	if n < 5 || n > 40 {
		t.Fatalf("高影响抽样应约 10%%，got %d（修复前会把 200 条全列出来）", n)
	}
	ids1 := make([]string, 0, len(q1.States[store.ReviewQueueSummary]))
	for _, it := range q1.States[store.ReviewQueueSummary] {
		ids1 = append(ids1, it.MemoryID)
	}
	ids2 := make([]string, 0, len(q2.States[store.ReviewQueueSummary]))
	for _, it := range q2.States[store.ReviewQueueSummary] {
		ids2 = append(ids2, it.MemoryID)
	}
	if len(ids1) != len(ids2) {
		t.Fatalf("同 id 多次结果必须一致: %d vs %d", len(ids1), len(ids2))
	}
	for i := range ids1 {
		if ids1[i] != ids2[i] {
			t.Fatalf("抽样必须确定性: 第 %d 条 %s vs %s", i, ids1[i], ids2[i])
		}
	}
}

// 其余 model-auto 产物默认信任不进摘要（没有低置信、不是高影响、不带冲突）。
func TestReviewSummaryTrustsPlainModelAutoProducts(t *testing.T) {
	st := newJudgeStore(t)
	seedActionMemory(t, st, "普通 fact 产物默认信任不进摘要", "m@v1", "")
	q, err := st.ListReviewQueue(store.ReviewQueueSummary, 50)
	if err != nil {
		t.Fatal(err)
	}
	if q.Counts[store.ReviewQueueSummary] != 0 || len(q.States[store.ReviewQueueSummary]) != 0 {
		t.Fatalf("普通产物不该进复核摘要，got count=%d list=%d",
			q.Counts[store.ReviewQueueSummary], len(q.States[store.ReviewQueueSummary]))
	}
}
