package main

import (
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

func TestAcceptAutoCommitsOnlyMatrixEligible(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Project: "p", Kind: "decision",
		Content: "决定：自动矩阵不得替人做判断", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Project: "p", Kind: "task",
		Content: "待办：已完成的任务可以自动收尾", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	execSQL(t, db, `UPDATE candidates SET task_state='done' WHERE id='`+task.Candidate.ID+`'`)

	out, err := captureStdout(t, func() error {
		return cmdAccept([]string{"--db", db, "--auto", "--json", "--audit-file", "-", decision.Candidate.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONOut(t, out)
	if num(got, "inserted") != 0 || num(got, "deferred_count") != 1 {
		t.Fatalf("decision 应 deferred 而非落库，got %v", got)
	}
	if countAllMemories(t, db) != 0 {
		t.Fatal("人工类型不得自动写记忆")
	}

	out, err = captureStdout(t, func() error {
		return cmdAccept([]string{"--db", db, "--auto", "--json", "--audit-file", "-", task.Candidate.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	got = decodeJSONOut(t, out)
	if num(got, "inserted") != 1 || num(got, "deferred_count") != 0 {
		t.Fatalf("task done 应自动采纳，got %v", got)
	}
	if countAllMemories(t, db) != 1 {
		t.Fatalf("应落 1 条记忆，got %d", countAllMemories(t, db))
	}

	// 自动采纳也会推进 revision；重复调用落到重放路径而不是再建一条。
	view, err := st2CommitCandidate(db, task.Candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != 1 {
		t.Fatalf("采纳应推进 revision，got %d", view.Revision)
	}
	out, err = captureStdout(t, func() error {
		return cmdAccept([]string{"--db", db, "--auto", "--json", "--audit-file", "-", task.Candidate.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	got = decodeJSONOut(t, out)
	if num(got, "inserted") != 1 {
		t.Fatalf("重放仍应报成功，got %v", got)
	}
	if countAllMemories(t, db) != 1 {
		t.Fatal("重放不得新建记忆")
	}
}

func st2CommitCandidate(db, id string) (*store.CommitCandidate, error) {
	st, err := store.Open(db)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.CommitCandidate(id)
}

func TestRejectRevisionConflict(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Kind: "pitfall", Content: "报错：重复拒绝要走 revision CAS", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	if err := cmdReject([]string{"--db", db, "--audit-file", "-", "--revision", "0", res.Candidate.ID}); err != nil {
		t.Fatalf("revision=0 应拒绝成功: %v", err)
	}
	if err := cmdReject([]string{"--db", db, "--audit-file", "-", "--revision", "0", res.Candidate.ID}); err == nil {
		t.Fatal("过期 revision 必须报冲突")
	}
	// 按当前 revision 重放应幂等。
	if err := cmdReject([]string{"--db", db, "--audit-file", "-", res.Candidate.ID}); err != nil {
		t.Fatalf("重复拒绝应幂等，got %v", err)
	}
	if err := cmdReject([]string{"--db", db, "--audit-file", "-"}); err == nil {
		t.Fatal("缺 id 应报错")
	}
	if err := cmdReject([]string{"--db", db, "--audit-file", "-", "nope"}); err == nil {
		t.Fatal("不存在的 id 应报错")
	}
}

func TestCandidatesPolicyViewIsReadOnly(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Project: "p", Kind: "decision", Content: "决定：需要人看", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Project: "p", Kind: "task", Content: "待办：完成的自动", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	execSQL(t, db, `UPDATE candidates SET task_state='done' WHERE id='`+task.Candidate.ID+`'`)

	out, err := captureStdout(t, func() error {
		return cmdCandidates([]string{"ls", "--db", db, "--policy"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "action=human_accept") || !strings.Contains(out, "action=auto_commit") {
		t.Fatalf("策略视图应区分人工/自动，got:\n%s", out)
	}
	if !strings.Contains(out, "rev=0") {
		t.Fatalf("策略视图应展示 revision，got:\n%s", out)
	}
	if !strings.Contains(out, "理由:") {
		t.Fatalf("策略视图应展示原因码，got:\n%s", out)
	}

	out, err = captureStdout(t, func() error {
		return cmdCandidates([]string{"ls", "--db", db, "--policy", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"action":"auto_commit"`) || !strings.Contains(out, `"revision":0`) {
		t.Fatalf("JSON 策略视图字段不全，got:\n%s", out)
	}

	// 只读：不得产生裁决记录或记忆。
	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{dec.Candidate.ID, task.Candidate.ID} {
		decisions, derr := st.CommitDecisions(id)
		if derr != nil {
			t.Fatal(derr)
		}
		if len(decisions) != 0 {
			t.Fatalf("--policy 不得写裁决，got %+v", decisions)
		}
	}
	if countAllMemories(t, db) != 0 {
		t.Fatal("--policy 不得写记忆")
	}
}

// 计划 §4.4：mem accept/reject --revision N --json 必须带 decision 字段。
// 同时回归「reject 原有候选字段仍平铺」，避免为补字段破坏既有消费者。
func TestAcceptRejectJSONDecisionField(t *testing.T) {
	db := testDB(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Project: "p", Kind: "task",
		Content: "待办：裁决字段要能被自动化读到", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	pit, err := st.InsertCandidate(store.CandidateInput{
		SessionID: "s", Project: "p", Kind: "pitfall",
		Content: "坑：拒绝也要带 decision", Source: "distill",
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	execSQL(t, db, `UPDATE candidates SET task_state='done' WHERE id='`+task.Candidate.ID+`'`)

	out, err := captureStdout(t, func() error {
		return cmdAccept([]string{"--db", db, "--auto", "--json", "--audit-file", "-",
			"--revision", "0", task.Candidate.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONOut(t, out)
	d, ok := got["decision"].(map[string]any)
	if !ok {
		t.Fatalf("accept --json 缺 decision 对象，got:\n%s", out)
	}
	if d["action"] != "auto_commit" || num(d, "revision") != 0 || d["committed"] != true {
		t.Fatalf("accept decision 字段不全，got %v", d)
	}
	if _, ok := got["decisions"].([]any); !ok {
		t.Fatalf("accept --json 缺全量 decisions 数组，got:\n%s", out)
	}

	out, err = captureStdout(t, func() error {
		return cmdReject([]string{"--db", db, "--json", "--audit-file", "-",
			"--revision", "0", pit.Candidate.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	got = decodeJSONOut(t, out)
	if got["id"] != pit.Candidate.ID {
		t.Fatalf("reject --json 应保留候选字段平铺，got:\n%s", out)
	}
	d, ok = got["decision"].(map[string]any)
	if !ok {
		t.Fatalf("reject --json 缺 decision 对象，got:\n%s", out)
	}
	if d["action"] != "reject" || num(d, "revision") != 0 {
		t.Fatalf("reject decision 字段不全，got %v", d)
	}
	// CommitResult.Committed 表示「状态迁移真的落库」；reject 也是一次成功迁移，
	// 与 accept 的 committed=true 同义，不代表写进了 memories。
	if d["committed"] != true {
		t.Fatalf("reject 应完成状态迁移，got %v", d)
	}
}
