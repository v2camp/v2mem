package recall

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// 真实库集成：过取深度、scope 配额与轨迹落库必须一起成立。
func TestQueryWithRealStoreOverfetchQuotaAndTrace(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	for i := 0; i < 30; i++ {
		if _, err := st.Add(store.AddInput{
			Content: fmt.Sprintf("ALPHA 项目记忆 %d：部署脚本放在 scripts 目录", i),
			Kind:    "decision", Project: "demo", Source: "test", Salience: 0.5,
		}); err != nil {
			t.Fatalf("Add project: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := st.Add(store.AddInput{
			Content: fmt.Sprintf("ALPHA 全局记忆 %d：构建统一用 make", i),
			Kind:    "preference", Source: "test", Salience: 1.0,
		}); err != nil {
			t.Fatalf("Add global: %v", err)
		}
	}

	res, err := Query(st, QueryOptions{
		Query: "ALPHA", Project: "demo", Scope: "current",
		Limit: 3, Budget: 1200, SessionID: "s", Harness: "claude",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Overfetch != Overfetch || res.RawHits != Overfetch {
		t.Fatalf("应过取 %d 条，got overfetch=%d raw=%d", Overfetch, res.Overfetch, res.RawHits)
	}
	if len(res.Injected) != 3 {
		t.Fatalf("应注入 3 条，got %d", len(res.Injected))
	}
	global, project := 0, 0
	for _, in := range res.Injected {
		switch in.Hit.Project {
		case "":
			global++
		case "demo":
			project++
		}
		if d, err := st.MemoryDetailByID(in.Hit.ID); err != nil || d.AccessCount != 0 || d.AdoptedCount != 0 {
			t.Fatalf("读侧供给不得自动 touch: %+v %v", d, err)
		}
	}
	if project < 2 || global > 1 {
		t.Fatalf("配额应为本工程≥2、global≤1，got project=%d global=%d", project, global)
	}

	tr, items, err := st.GetReadTrace(res.TraceID)
	if err != nil {
		t.Fatalf("GetReadTrace: %v", err)
	}
	if tr.Overfetch != Overfetch || tr.RawHits != Overfetch || tr.Injected != 3 ||
		tr.Qualified != res.Qualified || tr.Outcome != OutcomeInjected {
		t.Fatalf("轨迹计数不符: %+v", tr)
	}
	if len(items) != Overfetch {
		t.Fatalf("轨迹应保留池中全部 %d 条候选，got %d", Overfetch, len(items))
	}
	dropped := 0
	for _, it := range items {
		if !it.Eligible || it.DropReason != "" {
			dropped++
		}
		if it.InjectionRank > 0 && it.DropReason != "" {
			t.Fatalf("已注入条目不应带丢弃原因: %+v", it)
		}
	}
	if dropped == 0 {
		t.Fatal("limit/quota 应留下丢弃记录")
	}
	if res.TraceID == "" || res.TraceError != "" {
		t.Fatalf("轨迹写入应成功: %+v", res)
	}
}

// 显式 scope=all 才允许跨工程候选；默认 current 不得把别家工程带进来。
func TestQueryWithRealStoreCrossProject(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if _, err := st.Add(store.AddInput{
		Content: "ALPHA 只属于别家工程的记忆", Kind: "fact", Project: "other", Source: "test",
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	res, err := Query(st, QueryOptions{Query: "ALPHA", Project: "demo", Scope: "current", Limit: 3})
	if err != nil {
		t.Fatalf("Query current: %v", err)
	}
	if len(res.Injected) != 0 {
		t.Fatalf("current 不应跨工程: %+v", res.Injected)
	}
	resAll, err := Query(st, QueryOptions{Query: "ALPHA", Project: "demo", Scope: "all", Limit: 3})
	if err != nil {
		t.Fatalf("Query all: %v", err)
	}
	if len(resAll.Injected) != 1 || resAll.Injected[0].Hit.Project != "other" {
		t.Fatalf("显式 all 才允许跨工程: %+v", resAll.Injected)
	}
}
