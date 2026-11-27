package store

// 复核「修正」动作的两个不变量：
//   - 正文变了，内容指纹（content_hash）必须跟着变，否则「同内容不重复」仍按
//     旧文本判定；原始 hash 留在 provenance.original_hash 供追溯。
//   - 修正成同工程已有内容时明确报错并指出冲突 id，而不是把唯一索引的裸错误
//     抛给调用方。

import (
	"strings"
	"testing"
)

func TestReviewCorrectRehashesContentAndResolvesPrefix(t *testing.T) {
	s := newTestStore(t)
	added, err := s.Add(AddInput{Content: "旧正文：过程叙述", Project: "p", Kind: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	// 顺带验证前缀解析：mem ls 默认只展示前 8 位。
	rep, err := s.ApplyMemoryReview(MemoryReviewInput{
		MemoryID: added.ID[:8], Action: ReviewActionCorrect, Content: "新正文：可复用规则",
	})
	if err != nil {
		t.Fatalf("短 id 应可解析: %v", err)
	}
	if rep.MemoryID != added.ID || rep.After.ReviewState != ReviewStateCorrected {
		t.Fatalf("动作回报不对: %+v", rep)
	}
	var content, hash, prov string
	if err := s.db.QueryRow(
		`SELECT content, content_hash, provenance FROM memories WHERE id = ?`, added.ID).
		Scan(&content, &hash, &prov); err != nil {
		t.Fatal(err)
	}
	if content != "新正文：可复用规则" {
		t.Fatalf("正文未更新: %q", content)
	}
	if hash != HashOf("新正文：可复用规则") {
		t.Fatalf("content_hash 应随正文重算，got %s", hash)
	}
	if !strings.Contains(prov, added.Hash) {
		t.Fatalf("provenance 应保留 original_hash=%s，got %s", added.Hash, prov)
	}
}

func TestReviewCorrectRejectsDuplicateContent(t *testing.T) {
	s := newTestStore(t)
	first, err := s.Add(AddInput{Content: "已被占用的正文", Project: "p", Kind: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Add(AddInput{Content: "待修正的正文", Project: "p", Kind: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyMemoryReview(MemoryReviewInput{
		MemoryID: second.ID, Action: ReviewActionCorrect, Content: "已被占用的正文",
	})
	if err == nil {
		t.Fatal("修正成同工程已有内容应报错")
	}
	if !strings.Contains(err.Error(), first.ID[:8]) {
		t.Fatalf("错误应指出冲突的记忆 id，got %v", err)
	}
}
