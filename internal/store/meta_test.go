package store

import "testing"

func TestProjectMemoryCountIsScoped(t *testing.T) {
	s := newTestStore(t)
	for _, in := range []AddInput{
		{Content: "工程甲第一条", Device: "test", Project: "projA"},
		{Content: "工程甲第二条", Device: "test", Project: "projA"},
		{Content: "工程乙第一条", Device: "test", Project: "projB"},
		{Content: "全局条目", Device: "test"},
	} {
		if _, err := s.Add(in); err != nil {
			t.Fatalf("Add(%q): %v", in.Content, err)
		}
	}

	cases := []struct {
		project string
		want    int
	}{
		{"projA", 2},
		{"projB", 1},
		{"projMissing", 0},
	}
	for _, tc := range cases {
		got, err := s.ProjectMemoryCount(tc.project)
		if err != nil {
			t.Fatalf("ProjectMemoryCount(%q): %v", tc.project, err)
		}
		if got != tc.want {
			t.Errorf("ProjectMemoryCount(%q)=%d, want %d", tc.project, got, tc.want)
		}
	}
}

func TestMetaGetSetRoundTrip(t *testing.T) {
	s := newTestStore(t)

	if v, ok, err := s.MetaGet("missing"); err != nil || ok || v != "" {
		t.Fatalf("不存在的键应返回空值/false/nil，got %q %v %v", v, ok, err)
	}
	if err := s.MetaSet("k", "v1"); err != nil {
		t.Fatalf("MetaSet: %v", err)
	}
	v, ok, err := s.MetaGet("k")
	if err != nil || !ok || v != "v1" {
		t.Fatalf("MetaGet 应读到 v1，got %q %v %v", v, ok, err)
	}
	if err := s.MetaSet("k", "v2"); err != nil {
		t.Fatalf("MetaSet 覆盖: %v", err)
	}
	v, ok, err = s.MetaGet("k")
	if err != nil || !ok || v != "v2" {
		t.Fatalf("MetaSet 应幂等覆盖为 v2，got %q %v %v", v, ok, err)
	}
}

func TestMetaOpsReturnErrorOnClosedStore(t *testing.T) {
	s, err := Open(t.TempDir() + "/meta.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := s.MetaGet("k"); err == nil {
		t.Error("关闭后 MetaGet 应返回错误")
	}
	if err := s.MetaSet("k", "v"); err == nil {
		t.Error("关闭后 MetaSet 应返回错误")
	}
}
