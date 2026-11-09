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
