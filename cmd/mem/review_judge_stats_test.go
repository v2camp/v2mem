package main

// P4 Wave 1 / C：`mem review stats --judge` 的 Judge 维度标定。
// 关键约束是向后兼容：不带 --judge 时旧 JSON/文本一字不改。

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

func seedJudgeRow(t *testing.T, st *store.Store, n int, judgeModel, verdict string, decisions ...struct{ action, actor string }) {
	t.Helper()
	id := fmt.Sprintf("cj-%03d", n)
	if _, err := st.DB().Exec(`INSERT INTO candidates
		(id, session_id, project, kind, content, content_hash, status, source, created_at,
		 judge_verdict, judge_model)
		VALUES (?, 's', 'p', 'fact', ?, ?, 'pending', 'distill', 1700000000, ?, ?)`,
		id, "CLI judge 标定 "+id, "h-"+id, verdict, judgeModel); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	for i, d := range decisions {
		if _, err := st.DB().Exec(`INSERT INTO commit_decisions
			(id, candidate_id, action, actor, idempotency_key, created_at)
			VALUES (?,?,?,?,?,1700000000)`,
			fmt.Sprintf("cd-%s-%d", id, i), id, d.action, d.actor, fmt.Sprintf("ck-%s-%d", id, i)); err != nil {
			t.Fatalf("insert decision: %v", err)
		}
	}
}

func TestReviewStatsJudgeJSONAndLegacyUnchanged(t *testing.T) {
	st := newJudgeStore(t)
	// 旧口径样本：按 commit_decisions.model_tag 归因的记忆后审核。
	seedActionMemory(t, st, "旧口径样本", "m@v1", "confirmed")
	// Judge 口径样本：judge_model 分组。
	type dec = struct{ action, actor string }
	seedJudgeRow(t, st, 1, "ollama:qwen2.5@judge-v4", "accept", dec{"auto_commit", "judge"})
	seedJudgeRow(t, st, 2, "ollama:qwen2.5@judge-v4", "reject", dec{"reject", "judge"}, dec{"reopen", "human"})
	seedJudgeRow(t, st, 3, "ollama:llama3@judge-v3", "accept")
	db := st.Path()
	st.Close()

	// --judge --json：稳定字段的数组。
	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"stats", "--judge", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("stats --judge 应成功: %v\n%s", err, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("--judge --json 应为数组: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("应按 judge_model 分 2 组，got %d: %s", len(rows), out)
	}
	byModel := map[string]map[string]any{}
	for _, r := range rows {
		if _, ok := r["judge_model"].(string); !ok {
			t.Fatalf("字段 judge_model 应稳定存在: %v", r)
		}
		for _, k := range []string{"verdicts", "reject", "applied", "reopened"} {
			if _, ok := r[k].(float64); !ok {
				t.Fatalf("字段 %s 应稳定存在: %v", k, r)
			}
		}
		m, _ := r["judge_model"].(string)
		byModel[m] = r
	}
	v4 := byModel["ollama:qwen2.5@judge-v4"]
	v3 := byModel["ollama:llama3@judge-v3"]
	if v4 == nil || v3 == nil {
		t.Fatalf("分组键应为 judge_model，got %v", byModel)
	}
	if jnum(v4, "verdicts") != 2 || jnum(v4, "reject") != 1 ||
		jnum(v4, "applied") != 2 || jnum(v4, "reopened") != 1 {
		t.Fatalf("v4 统计错: %v", v4)
	}
	if jnum(v3, "verdicts") != 1 || jnum(v3, "reject") != 0 {
		t.Fatalf("v3 统计错: %v", v3)
	}

	// 文本形态也要有 Judge 表。
	text, err := captureStdout(t, func() error {
		return cmdReview([]string{"stats", "--judge", "--db", db})
	})
	if err != nil {
		t.Fatalf("stats --judge 文本应成功: %v", err)
	}
	if !strings.Contains(text, "judge-v4") || !strings.Contains(text, "judge-v3") {
		t.Fatalf("文本应列出 judge_model，got:\n%s", text)
	}

	// 不带 --judge：旧 JSON 仍是 ReviewStatsRow 数组，不含 judge_model 字段。
	legacy, err := captureStdout(t, func() error {
		return cmdReview([]string{"stats", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("旧 stats 应成功: %v", err)
	}
	var old []map[string]any
	if err := json.Unmarshal([]byte(legacy), &old); err != nil {
		t.Fatalf("旧 stats --json 应可解析: %v\n%s", err, legacy)
	}
	if len(old) != 1 || old[0]["model_tag"] != "m@v1" {
		t.Fatalf("旧 stats 输出不应被 Judge 数据污染，got %v", old)
	}
	if _, has := old[0]["judge_model"]; has {
		t.Fatalf("旧 stats 不应出现 judge_model 字段: %v", old[0])
	}
}

func TestReviewStatsJudgeEmpty(t *testing.T) {
	st := newJudgeStore(t)
	db := st.Path()
	st.Close()

	out, err := captureStdout(t, func() error {
		return cmdReview([]string{"stats", "--judge", "--db", db, "--json"})
	})
	if err != nil {
		t.Fatalf("空样本应成功: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("空样本 JSON 应为 []，got %q", out)
	}
	text, err := captureStdout(t, func() error {
		return cmdReview([]string{"stats", "--judge", "--db", db})
	})
	if err != nil {
		t.Fatalf("空样本文本应成功: %v", err)
	}
	if !strings.Contains(text, "暂无") {
		t.Fatalf("空样本应有「暂无」提示，got %q", text)
	}
}
