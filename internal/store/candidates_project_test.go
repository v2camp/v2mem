package store

import "testing"

// insertCandidateFor 用不同 session 造候选，避开 (session_id, content_hash) 部分唯一索引。
func insertCandidateFor(t *testing.T, s *Store, project, session, content string) {
	t.Helper()
	if _, err := s.InsertCandidate(CandidateInput{
		SessionID: session, Project: project, Kind: "decision",
		Content: content, Source: "distill",
	}); err != nil {
		t.Fatalf("InsertCandidate(%s/%s): %v", project, session, err)
	}
}

func projectFixture(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	insertCandidateFor(t, s, "alpha", "s1", "alpha 决策一")
	insertCandidateFor(t, s, "alpha", "s2", "alpha 决策二")
	insertCandidateFor(t, s, "beta", "s3", "beta 决策一")
	insertCandidateFor(t, s, "", "s4", "全局决策一")
	return s
}

// CountPendingByProject 是 hook 注入的数据源：只算当前工程、只算 pending。
// 空串是等值条件（全局候选），不是「不过滤」——与 ListCandidatesByProject 不同。
func TestCountPendingByProject(t *testing.T) {
	s := projectFixture(t)
	for _, tc := range []struct {
		project string
		want    int
	}{
		{"alpha", 2}, {"beta", 1}, {"", 1}, {"gamma", 0},
	} {
		got, err := s.CountPendingByProject(tc.project)
		if err != nil {
			t.Fatalf("CountPendingByProject(%q): %v", tc.project, err)
		}
		if got != tc.want {
			t.Errorf("CountPendingByProject(%q) = %d, want %d", tc.project, got, tc.want)
		}
	}

	// 被拒绝的候选退出 pending 计数。
	list, err := s.ListCandidatesByProject("pending", "", "alpha", 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListCandidatesByProject: %v %+v", err, list)
	}
	if _, err := s.RejectCandidate(list[0].ID[:8]); err != nil {
		t.Fatalf("RejectCandidate: %v", err)
	}
	got, err := s.CountPendingByProject("alpha")
	if err != nil || got != 1 {
		t.Fatalf("拒绝后 alpha 应剩 1 条 pending, got %d err %v", got, err)
	}
}

// ListPendingCandidatesByBundles 供 maintain 只裁决「本轮处理过的窗口」：
// 空列表必须返回空集（不是全表），否则没处理任何窗口的一轮会误裁决历史积压。
func TestListPendingCandidatesByBundles(t *testing.T) {
	s := newTestStore(t)
	insert := func(session, content string) string {
		t.Helper()
		res, err := s.InsertCandidate(CandidateInput{
			SessionID: session, Project: "p", Kind: "decision",
			Content: content, Source: "distill",
		})
		if err != nil {
			t.Fatalf("InsertCandidate(%s): %v", session, err)
		}
		return res.Candidate.ID
	}
	a1 := insert("s1", "b1 候选一")
	a2 := insert("s2", "b1 候选二")
	b1 := insert("s3", "b2 候选一")
	c1 := insert("s4", "b3 候选一")
	insert("s5", "unbundled 候选")
	for id, bundle := range map[string]string{a1: "bnd-1", a2: "bnd-1", b1: "bnd-2", c1: "bnd-3"} {
		if _, err := s.DB().Exec(
			`UPDATE candidates SET bundle_id = ?, revision = 2 WHERE id = ?`, bundle, id); err != nil {
			t.Fatalf("设置 bundle %s: %v", id, err)
		}
	}

	got, err := s.ListPendingCandidatesByBundles([]string{"bnd-1", "bnd-2"}, 10)
	if err != nil {
		t.Fatalf("ListPendingCandidatesByBundles: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应取 bnd-1/bnd-2 的 3 条 pending，got %d: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, ref := range got {
		seen[ref.ID] = true
		if ref.Revision != 2 {
			t.Errorf("%s revision 应为 2，got %d", ref.ID, ref.Revision)
		}
	}
	if seen[c1] {
		t.Error("不得混入未请求的 bnd-3 候选")
	}

	limited, err := s.ListPendingCandidatesByBundles([]string{"bnd-1", "bnd-2"}, 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limit 应生效：got %d err=%v", len(limited), err)
	}

	empty, err := s.ListPendingCandidatesByBundles(nil, 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空列表应立即返回空集，got %d err=%v", len(empty), err)
	}

	if _, err := s.DB().Exec(`UPDATE candidates SET status = 'rejected' WHERE id = ?`, a1); err != nil {
		t.Fatalf("标记 rejected: %v", err)
	}
	left, err := s.ListPendingCandidatesByBundles([]string{"bnd-1", "bnd-2"}, 10)
	if err != nil || len(left) != 2 {
		t.Fatalf("rejected 不应出现在 pending 结果里：got %d err=%v", len(left), err)
	}
}

// ListCandidatesByProject 的空 project 表示不过滤；旧签名必须保持等价。
func TestListCandidatesByProject(t *testing.T) {
	s := projectFixture(t)

	legacy, err := s.ListCandidates("pending", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 4 {
		t.Fatalf("旧签名应列出全部 4 条，got %d", len(legacy))
	}

	for _, tc := range []struct {
		name                  string
		status, session, proj string
		limit                 int
		want                  int
	}{
		{"全部", "pending", "", "", 10, 4},
		{"只 alpha", "pending", "", "alpha", 10, 2},
		{"只 beta", "pending", "", "beta", 10, 1},
		{"session + project", "pending", "s2", "alpha", 10, 1},
		{"session 不命中", "pending", "s3", "alpha", 10, 0},
		{"status 不命中", "accepted", "", "", 10, 0},
		{"limit 生效", "pending", "", "", 2, 2},
		{"limit<=0 归一 50", "pending", "", "", 0, 4},
	} {
		got, err := s.ListCandidatesByProject(tc.status, tc.session, tc.proj, tc.limit)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s = %d 条, want %d", tc.name, len(got), tc.want)
		}
		if tc.proj != "" {
			for _, c := range got {
				if c.Project != tc.proj {
					t.Errorf("%s 混入了 %q 的候选", tc.name, c.Project)
				}
			}
		}
	}
}
