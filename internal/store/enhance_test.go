package store

// 正文提升（repair content）的 store 层契约：
//   - 选择器只列「可参与提升」的存活记忆：撤回 / 被取代 / 过期都排除；
//   - 改写走与 review correct 相同的写路径：content/content_idx/content_hash/mem_sigs
//     同步重算，provenance 留 original_hash，状态回到未审核（模型改写不是人工确认）；
//   - 回滚只在「改写后没人再动过」（content_hash 匹配）时生效，且不得撞同工程其它记忆。

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/similarity"
)

type enhanceRow struct {
	content, contentIdx, hash, state, by, prov string
	reviewedAt, updatedAt                      int64
}

func readEnhanceRow(t *testing.T, s *Store, id string) enhanceRow {
	t.Helper()
	var r enhanceRow
	if err := s.db.QueryRow(`SELECT content, content_idx, content_hash, review_state, reviewed_by,
		reviewed_at, updated_at, COALESCE(provenance, '') FROM memories WHERE id = ?`, id).
		Scan(&r.content, &r.contentIdx, &r.hash, &r.state, &r.by,
			&r.reviewedAt, &r.updatedAt, &r.prov); err != nil {
		t.Fatalf("读取记忆 %s: %v", id, err)
	}
	return r
}

func readEnhanceSig(t *testing.T, s *Store, id string) similarity.Signature {
	t.Helper()
	var blob []byte
	if err := s.db.QueryRow(`SELECT sig FROM mem_sigs WHERE memory_id = ?`, id).Scan(&blob); err != nil {
		t.Fatalf("读取签名 %s: %v", id, err)
	}
	return decodeSig(blob)
}

func TestListEnhanceCandidatesFiltersLive(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	confirmed := mustAdd(t, s, "已确认的正文 A")
	unreviewed, err := s.Add(AddInput{Content: "未审核的正文 B", Source: "distill-accept"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	future := mustAdd(t, s, "未到期的正文 F")
	withdrawn := mustAdd(t, s, "已撤回的正文 C")
	if err := s.SetMemoryReviewState(withdrawn, ReviewStateWithdrawn, "human", now); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	superseded := mustAdd(t, s, "被取代的正文 D")
	setColumn(t, s, superseded, "superseded_by", "other-id")
	expired := mustAdd(t, s, "已过期的正文 E")
	setColumn(t, s, expired, "expires_at", now-1)

	// 固定 created_at 让 ORDER BY created_at ASC 的断言不依赖随机 id。
	setColumn(t, s, confirmed, "created_at", 100)
	setColumn(t, s, unreviewed.ID, "created_at", 200)
	setColumn(t, s, future, "created_at", 300)
	setColumn(t, s, future, "expires_at", now+3600)

	got, err := s.ListEnhanceCandidates(0)
	if err != nil {
		t.Fatalf("ListEnhanceCandidates: %v", err)
	}
	wantIDs := []string{confirmed, unreviewed.ID, future}
	var gotIDs []string
	for _, c := range got {
		gotIDs = append(gotIDs, c.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("应只列存活记忆且按 created_at ASC：want %v, got %v", wantIDs, gotIDs)
	}
	if got[0].Content != "已确认的正文 A" || got[0].Kind != "fact" ||
		got[0].ContentHash != HashOf("已确认的正文 A") || got[0].ReviewState != ReviewStateConfirmed {
		t.Fatalf("投影字段不对: %+v", got[0])
	}
	if got[1].ReviewState != ReviewStateNone {
		t.Fatalf("未审核条目应保持空状态，got %q", got[1].ReviewState)
	}

	limited, err := s.ListEnhanceCandidates(1)
	if err != nil {
		t.Fatalf("ListEnhanceCandidates(1): %v", err)
	}
	if len(limited) != 1 || limited[0].ID != confirmed {
		t.Fatalf("limit 未生效: %+v", limited)
	}
}

func TestRewriteMemoryContentHappyPath(t *testing.T) {
	s := newTestStore(t)
	const oldContent = "旧正文：过程叙述"
	added, err := s.Add(AddInput{Content: oldContent, Project: "p", Kind: "fact"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Add 的直写来源默认已确认；改写必须把它打回未审核。
	if before := readEnhanceRow(t, s, added.ID); before.state != ReviewStateConfirmed {
		t.Fatalf("前置状态应为 confirmed，got %q", before.state)
	}

	const now int64 = 1700000000
	res, err := s.RewriteMemoryContent(added.ID, "  新正文：可复用规则  ", "test@v1", now)
	if err != nil {
		t.Fatalf("RewriteMemoryContent: %v", err)
	}
	const want = "新正文：可复用规则"
	if !res.Changed || res.MemoryID != added.ID || res.OldContent != oldContent ||
		res.OldHash != added.Hash || res.NewHash != HashOf(want) {
		t.Fatalf("回报不对: %+v", res)
	}

	r := readEnhanceRow(t, s, added.ID)
	if r.content != want {
		t.Fatalf("正文未更新: %q", r.content)
	}
	if r.contentIdx != indexText(want) {
		t.Fatalf("content_idx 未同步: %q", r.contentIdx)
	}
	if r.hash != HashOf(want) {
		t.Fatalf("content_hash 应随正文重算: %s", r.hash)
	}
	if r.state != ReviewStateNone || r.reviewedAt != 0 || r.by != "" {
		t.Fatalf("模型改写必须回到未审核: state=%q at=%d by=%q", r.state, r.reviewedAt, r.by)
	}
	if r.updatedAt != now {
		t.Fatalf("updated_at 应取传入时钟: %d", r.updatedAt)
	}
	for _, want := range []string{`"original_hash":"` + added.Hash + `"`, `"enhance_tag":"test@v1"`, `"enhanced_at":1700000000`} {
		if !strings.Contains(r.prov, want) {
			t.Fatalf("provenance 缺少 %s，got %s", want, r.prov)
		}
	}
	if sig := readEnhanceSig(t, s, added.ID); !reflect.DeepEqual(sig, similarity.Sign(want)) {
		t.Fatalf("mem_sigs 未随正文重算")
	}
}

func TestRewriteMemoryContentUnchangedIsNoop(t *testing.T) {
	s := newTestStore(t)
	const oldContent = "保持不变的正文"
	added, err := s.Add(AddInput{Content: oldContent, Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	setColumn(t, s, added.ID, "updated_at", 111)

	// 仅空白差异不算改写。
	res, err := s.RewriteMemoryContent(added.ID, "  "+oldContent+"\n", "test@v1", 222)
	if err != nil {
		t.Fatalf("RewriteMemoryContent: %v", err)
	}
	if res.Changed || res.OldHash != added.Hash || res.NewHash != added.Hash || res.OldContent != oldContent {
		t.Fatalf("正文未变应 Changed=false: %+v", res)
	}
	r := readEnhanceRow(t, s, added.ID)
	if r.updatedAt != 111 || r.prov != "" || r.state != ReviewStateConfirmed {
		t.Fatalf("未变时不应写库: %+v", r)
	}
}

func TestRewriteMemoryContentResolvesPrefixAndDefaultsNow(t *testing.T) {
	s := newTestStore(t)
	added, err := s.Add(AddInput{Content: "旧正文", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	before := time.Now().Unix()
	res, err := s.RewriteMemoryContent(added.ID[:8], "改写后正文", "test@v1", 0)
	if err != nil {
		t.Fatalf("短 id 应可解析: %v", err)
	}
	if res.MemoryID != added.ID || !res.Changed {
		t.Fatalf("前缀解析结果不对: %+v", res)
	}
	r := readEnhanceRow(t, s, added.ID)
	if r.updatedAt < before || r.updatedAt > time.Now().Unix() {
		t.Fatalf("now<=0 应取当前时间，got %d", r.updatedAt)
	}
}

func TestRewriteMemoryContentErrors(t *testing.T) {
	s := newTestStore(t)
	added, err := s.Add(AddInput{Content: "正常正文", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	withdrawn, err := s.Add(AddInput{Content: "已撤回正文", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.SetMemoryReviewState(withdrawn.ID, ReviewStateWithdrawn, "human", 1); err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	if _, err := s.RewriteMemoryContent(withdrawn.ID, "新正文", "test@v1", 1); err == nil ||
		!strings.Contains(err.Error(), "已撤回") {
		t.Fatalf("已撤回应报错，got %v", err)
	}
	if _, err := s.RewriteMemoryContent(added.ID, "   ", "test@v1", 1); err == nil {
		t.Fatal("空正文应报错")
	}
	if _, err := s.RewriteMemoryContent("ffffffffffffffffffffffffffffffff", "新正文", "test@v1", 1); err == nil {
		t.Fatal("未知 id 应报错")
	}
}

func TestRewriteMemoryContentRejectsDuplicateInProject(t *testing.T) {
	s := newTestStore(t)
	first, err := s.Add(AddInput{Content: "已被占用的正文", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	second, err := s.Add(AddInput{Content: "待改写的正文", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err = s.RewriteMemoryContent(second.ID, "已被占用的正文", "test@v1", 1)
	if err == nil || !strings.Contains(err.Error(), first.ID[:8]) {
		t.Fatalf("撞同工程正文应报错并指出冲突 id，got %v", err)
	}
	r := readEnhanceRow(t, s, second.ID)
	if r.content != "待改写的正文" || r.hash != second.Hash || r.prov != "" {
		t.Fatalf("冲突时原记忆不应变动: %+v", r)
	}
}

func TestRewriteMemoryContentKeepsFirstOriginalHash(t *testing.T) {
	s := newTestStore(t)
	added, err := s.Add(AddInput{Content: "旧正文", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.RewriteMemoryContent(added.ID, "第一次改写", "tag1", 1000); err != nil {
		t.Fatalf("first rewrite: %v", err)
	}
	if _, err := s.RewriteMemoryContent(added.ID, "第二次改写", "tag2", 2000); err != nil {
		t.Fatalf("second rewrite: %v", err)
	}
	r := readEnhanceRow(t, s, added.ID)
	for _, want := range []string{`"original_hash":"` + added.Hash + `"`, `"enhance_tag":"tag2"`, `"enhanced_at":2000`} {
		if !strings.Contains(r.prov, want) {
			t.Fatalf("二次改写应保留最初的 original_hash，缺 %s: %s", want, r.prov)
		}
	}
}

func TestRewritePreservesLegacyProvenance(t *testing.T) {
	s := newTestStore(t)
	added, err := s.Add(AddInput{Content: "旧正文", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	setColumn(t, s, added.ID, "provenance", "非 JSON 的自由文本")
	if _, err := s.RewriteMemoryContent(added.ID, "新正文", "tag", 1000); err != nil {
		t.Fatalf("RewriteMemoryContent: %v", err)
	}
	r := readEnhanceRow(t, s, added.ID)
	for _, want := range []string{`"legacy":"非 JSON 的自由文本"`, `"original_hash":"` + added.Hash + `"`} {
		if !strings.Contains(r.prov, want) {
			t.Fatalf("改写不应丢失旧 provenance，缺 %s: %s", want, r.prov)
		}
	}
}

func TestRestoreMemoryContentHappyPath(t *testing.T) {
	s := newTestStore(t)
	const oldContent = "旧正文"
	added, err := s.Add(AddInput{Content: oldContent, Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.SetMemoryReviewState(added.ID, ReviewStateConfirmed, "human", 1); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	res, err := s.RewriteMemoryContent(added.ID[:8], "新正文", "tag", 1000)
	if err != nil {
		t.Fatalf("RewriteMemoryContent: %v", err)
	}
	restored, reason, err := s.RestoreMemoryContent(added.ID[:8], oldContent, res.NewHash, 2000)
	if err != nil || !restored || reason != "" {
		t.Fatalf("应还原成功: restored=%v reason=%q err=%v", restored, reason, err)
	}
	r := readEnhanceRow(t, s, added.ID)
	if r.content != oldContent || r.contentIdx != indexText(oldContent) || r.hash != added.Hash {
		t.Fatalf("正文/索引/hash 未还原: %+v", r)
	}
	if r.state != ReviewStateNone || r.reviewedAt != 0 || r.by != "" {
		t.Fatalf("还原后应回到未审核: %+v", r)
	}
	if r.updatedAt != 2000 {
		t.Fatalf("updated_at 应取传入时钟: %d", r.updatedAt)
	}
	for _, want := range []string{`"original_hash":"` + added.Hash + `"`, `"enhance_restored_at":2000`} {
		if !strings.Contains(r.prov, want) {
			t.Fatalf("provenance 缺少 %s: %s", want, r.prov)
		}
	}
	if sig := readEnhanceSig(t, s, added.ID); !reflect.DeepEqual(sig, similarity.Sign(oldContent)) {
		t.Fatalf("mem_sigs 未随还原重算")
	}
}

func TestRestoreMemoryContentRejectsChangedContent(t *testing.T) {
	s := newTestStore(t)
	added, err := s.Add(AddInput{Content: "旧正文", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	res, err := s.RewriteMemoryContent(added.ID, "新正文", "tag", 1000)
	if err != nil {
		t.Fatalf("RewriteMemoryContent: %v", err)
	}
	before := readEnhanceRow(t, s, added.ID)
	restored, reason, err := s.RestoreMemoryContent(added.ID, "旧正文", "deadbeef", 0)
	if err != nil || restored || reason != "content_changed" {
		t.Fatalf("hash 不匹配应拒绝: restored=%v reason=%q err=%v", restored, reason, err)
	}
	if after := readEnhanceRow(t, s, added.ID); after != before || res.NewHash == "deadbeef" {
		t.Fatalf("拒绝时库不应变动: before=%+v after=%+v", before, after)
	}
}

func TestRestoreMemoryContentRejectsConflict(t *testing.T) {
	s := newTestStore(t)
	const oldContent = "待提升正文"
	victim, err := s.Add(AddInput{Content: oldContent, Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	res, err := s.RewriteMemoryContent(victim.ID, "提升后正文", "tag", 1000)
	if err != nil {
		t.Fatalf("RewriteMemoryContent: %v", err)
	}
	// 改写后旧正文的 hash 空出来，被同工程另一条记忆占用。
	occupied, err := s.Add(AddInput{Content: oldContent, Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	restored, reason, err := s.RestoreMemoryContent(victim.ID, oldContent, res.NewHash, 2000)
	if err != nil || restored || reason != "conflict:"+occupied.ID {
		t.Fatalf("旧 hash 被占用应报冲突: restored=%v reason=%q err=%v", restored, reason, err)
	}
	r := readEnhanceRow(t, s, victim.ID)
	if r.content != "提升后正文" || r.hash != res.NewHash {
		t.Fatalf("冲突时库不应变动: %+v", r)
	}
}

func TestRestoreMemoryContentErrors(t *testing.T) {
	s := newTestStore(t)
	added, err := s.Add(AddInput{Content: "旧正文", Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, _, err := s.RestoreMemoryContent(added.ID, "  ", "whatever", 1); err == nil {
		t.Fatal("空正文应报错")
	}
	if _, _, err := s.RestoreMemoryContent("ffffffffffffffffffffffffffffffff", "旧正文", "whatever", 1); err == nil {
		t.Fatal("未知 id 应报错")
	}
}

func TestRestorePreservesLegacyProvenance(t *testing.T) {
	s := newTestStore(t)
	const oldContent = "旧正文"
	added, err := s.Add(AddInput{Content: oldContent, Project: "p"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	res, err := s.RewriteMemoryContent(added.ID, "新正文", "tag", 1000)
	if err != nil {
		t.Fatalf("RewriteMemoryContent: %v", err)
	}
	setColumn(t, s, added.ID, "provenance", "还原前的自由文本")
	restored, reason, err := s.RestoreMemoryContent(added.ID, oldContent, res.NewHash, 2000)
	if err != nil || !restored || reason != "" {
		t.Fatalf("应还原成功: restored=%v reason=%q err=%v", restored, reason, err)
	}
	r := readEnhanceRow(t, s, added.ID)
	for _, want := range []string{`"legacy":"还原前的自由文本"`, `"enhance_restored_at":2000`} {
		if !strings.Contains(r.prov, want) {
			t.Fatalf("还原不应丢失旧 provenance，缺 %s: %s", want, r.prov)
		}
	}
}
