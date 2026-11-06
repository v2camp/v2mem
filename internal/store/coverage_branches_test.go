package store

import (
	"testing"

	"github.com/v2camp/v2mem/internal/commit"
)

// 这一组补的是 store 里此前完全没有包内测试、或只覆盖到主路径的分支。
// 目标不是刷数字：DecideCandidate 是 CLI 预演的唯一入口、gradeOfEvidence
// 是 A/B/C 证据等级的唯一判定点，二者都该有明确的边界断言。

func TestDecideCandidateIsReadOnlyAndCoversMatrix(t *testing.T) {
	s := newTestStore(t)
	dec, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "decision",
		Content: "决定：预演不得写库", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	c, d, err := s.DecideCandidate(dec.Candidate.ID)
	if err != nil {
		t.Fatalf("DecideCandidate: %v", err)
	}
	if c.ID != dec.Candidate.ID || d.Action != commit.ActionHumanAccept {
		t.Fatalf("人工类型应预演为 human_accept: c=%+v d=%+v", c, d)
	}

	task, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Project: "p", Kind: "task",
		Content: "待办：已完成的任务可自动收尾", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE candidates SET task_state='done' WHERE id=?`, task.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	_, d2, err := s.DecideCandidate(task.Candidate.ID)
	if err != nil {
		t.Fatalf("DecideCandidate(task done): %v", err)
	}
	if d2.Action != commit.ActionAutoCommit {
		t.Fatalf("task done 应预演为 auto_commit，got %+v", d2)
	}

	if _, _, err := s.DecideCandidate("不存在的 id"); err == nil {
		t.Fatal("不存在的候选应报错")
	}

	// 预演只读：不能产生记忆，也不能产生裁决记录。
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("预演不得写 memories: n=%d err=%v", n, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM commit_decisions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("预演不得写 commit_decisions: n=%d err=%v", n, err)
	}
}

func TestGradeOfEvidenceBranches(t *testing.T) {
	cases := []struct {
		uid, archive string
		want         commit.Grade
	}{
		{"tx:u1", "arch-abc", commit.GradeA},
		{"tx:u1", "", commit.GradeB},
		{"", "arch-abc", commit.GradeB},
		{"  ", "  ", commit.GradeC},
		{"", "", commit.GradeC},
	}
	for _, c := range cases {
		if got := gradeOfEvidence(c.uid, c.archive); got != c.want {
			t.Errorf("gradeOfEvidence(%q,%q) = %s, want %s", c.uid, c.archive, got, c.want)
		}
	}
}

func TestParseProvenanceNullableAndCapLimit(t *testing.T) {
	if parseProvenance("") != nil || parseProvenance("not json") != nil {
		t.Fatal("空值与坏 JSON 都应返回 nil 溯源")
	}
	p := parseProvenance(`{"file":"a.go","line":3,"preview":"片段"}`)
	if p == nil || p.File != "a.go" || p.Line != 3 || p.Preview != "片段" {
		t.Fatalf("合法溯源没解析出来: %+v", p)
	}
	if nullable("") != nil || nullable("x") != "x" {
		t.Fatal("nullable 只把空串转成 NULL")
	}

	hits := []Hit{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if got := capLimit(hits, 2); len(got) != 2 || got[1].ID != "b" {
		t.Fatalf("应截断到 2 条并保持顺序: %+v", got)
	}
	if got := capLimit(hits, 5); len(got) != 3 {
		t.Fatalf("上限大于长度时不应截断: %+v", got)
	}
	if got := capLimit(hits, 0); len(got) != 0 {
		t.Fatalf("上限 0 应为空: %+v", got)
	}
	if got := capLimit(nil, 3); len(got) != 0 {
		t.Fatalf("nil 输入应返回空: %+v", got)
	}
}

func TestInsertCandidateValidationAndForgetEdges(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.InsertCandidate(CandidateInput{Content: "  ", Kind: "fact"}); err == nil {
		t.Fatal("空内容应被拒")
	}
	if _, err := s.InsertCandidate(CandidateInput{Content: "x", Kind: "unknown-kind"}); err == nil {
		t.Fatal("非法 kind 应被拒")
	}
	first, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "fact", Content: "重复候选不双插", Salience: 2,
	})
	if err != nil || !first.Created {
		t.Fatalf("首条应创建: %+v err=%v", first, err)
	}
	// 越界 salience 应回落到默认值，而不是把 2 写进库。
	if first.Candidate.Salience != 0.5 {
		t.Fatalf("salience 越界应回落到 0.5，got %v", first.Candidate.Salience)
	}
	again, err := s.InsertCandidate(CandidateInput{
		SessionID: "s", Kind: "fact", Content: "重复候选不双插",
	})
	if err != nil || again.Created {
		t.Fatalf("同会话同内容应命中去重: %+v err=%v", again, err)
	}

	if _, err := s.Forget(""); err == nil {
		t.Fatal("空 ID 应被拒")
	}
	if _, err := s.Forget("不存在的 id"); err == nil {
		t.Fatal("不存在的 ID 应报错")
	}
	id := mustAdd(t, s, "显式删除的记忆")
	got, err := s.Forget(id[:8]) // 前缀解析
	if err != nil || got != id {
		t.Fatalf("前缀应解析到完整 id: got=%q err=%v want=%q", got, err, id)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id=?`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("Forget 后记忆应消失: n=%d err=%v", n, err)
	}
}
