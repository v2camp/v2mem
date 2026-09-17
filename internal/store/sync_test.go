package store

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// ---------- M4: 跨设备归集 ----------
//
// 归并身份键是 (content_hash, project)，不是 id：
// id 由 newID() 随机生成，两台设备独立写下同一事实会得到不同 id，
// 而唯一索引 ux_mem_hash 只允许 (content_hash, project) 一条。用 id 归并必然漏合。

func addWith(t *testing.T, s *Store, in AddInput) string {
	t.Helper()
	if in.Device == "" {
		in.Device = "dev-local"
	}
	res, err := s.Add(in)
	if err != nil {
		t.Fatalf("Add(%+v): %v", in, err)
	}
	return res.ID
}

// snap 是「内容相关字段」的规范投影，用于断言两个库收敛。
// 刻意不含 id 与 origin_device —— 前者是随机本地标识，后者是本地视角的溯源字段。
type snap struct {
	Kind        string
	Project     string
	Salience    float64
	AccessCount int
	CreatedAt   int64
	LastSeenAt  int64
	ExpiresAt   *int64
	Tags        string
}

func snapshot(t *testing.T, s *Store) map[string]snap {
	t.Helper()
	rows, err := s.db.Query(`SELECT content_hash, kind, project, salience, access_count,
	                                created_at, last_seen_at, expires_at FROM memories`)
	if err != nil {
		t.Fatalf("查询 memories: %v", err)
	}
	defer rows.Close()

	out := map[string]snap{}
	for rows.Next() {
		var h string
		var v snap
		if err := rows.Scan(&h, &v.Kind, &v.Project, &v.Salience, &v.AccessCount,
			&v.CreatedAt, &v.LastSeenAt, &v.ExpiresAt); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		out[h+"\x00"+v.Project] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// tags 以规范字符串形式并入投影
	tagRows, err := s.db.Query(
		`SELECT m.content_hash, m.project, t.key, t.value
		   FROM tags t JOIN memories m ON m.id = t.memory_id
		  ORDER BY m.content_hash, m.project, t.key, t.value`)
	if err != nil {
		t.Fatalf("查询 tags: %v", err)
	}
	defer tagRows.Close()
	joined := map[string][]string{}
	for tagRows.Next() {
		var h, p, k, v string
		if err := tagRows.Scan(&h, &p, &k, &v); err != nil {
			t.Fatalf("Scan tag: %v", err)
		}
		key := h + "\x00" + p
		joined[key] = append(joined[key], k+"="+v)
	}
	for key, pairs := range joined {
		v := out[key]
		v.Tags = strings.Join(pairs, ",")
		out[key] = v
	}
	return out
}

func TestExportReturnsOneRecordPerMemoryWithTags(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: "导出验证甲", Project: "p1", Kind: "fact",
		Tags: map[string]string{"machine": "mini"}})
	addWith(t, s, AddInput{Content: "导出验证乙", Project: "p1", Kind: "decision"})

	recs, err := s.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("应导出 2 条，got %d", len(recs))
	}
	byContent := map[string]ExportRecord{}
	for _, r := range recs {
		byContent[r.Content] = r
	}
	if got := byContent["导出验证甲"].Tags["machine"]; len(got) != 1 || got[0] != "mini" {
		t.Errorf("标记应随导出携带，got %v", got)
	}
	if byContent["导出验证甲"].ContentHash == "" {
		t.Error("导出必须带 content_hash（归并身份键）")
	}
	if byContent["导出验证甲"].OriginDevice == "" {
		t.Error("导出必须带 origin_device（溯源）")
	}
}

func TestExportIsStableAcrossCalls(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []string{"稳定性验证甲", "稳定性验证乙", "稳定性验证丙"} {
		addWith(t, s, AddInput{Content: c, Project: "p1"})
	}
	a, err := s.Export()
	if err != nil {
		t.Fatalf("Export #1: %v", err)
	}
	b, err := s.Export()
	if err != nil {
		t.Fatalf("Export #2: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("两次导出应完全一致（顺序与内容都稳定），否则 JSONL 会无谓 diff")
	}
}

func TestImportInsertsNewMemoriesAndKeepsOriginDevice(t *testing.T) {
	remote := newTestStore(t)
	addWith(t, remote, AddInput{Content: "远端独有的事实", Project: "p1", Device: "dev-far"})
	recs, err := remote.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	local := newTestStore(t)
	res, err := local.Import(recs)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Inserted != 1 {
		t.Fatalf("应插入 1 条，got %+v", res)
	}

	exported, err := local.Export()
	if err != nil {
		t.Fatalf("Export local: %v", err)
	}
	if len(exported) != 1 {
		t.Fatalf("本地应有 1 条，got %d", len(exported))
	}
	if exported[0].OriginDevice != "dev-far" {
		t.Errorf("插入的记录应保留远端溯源设备，got %q", exported[0].OriginDevice)
	}
}

// 幂等：同一份 JSONL 导入两次，第二次必须什么都不改。
func TestImportIsIdempotent(t *testing.T) {
	remote := newTestStore(t)
	rid := addWith(t, remote, AddInput{Content: "幂等验证事实", Project: "p1", Kind: "decision",
		Salience: 0.9, Tags: map[string]string{"machine": "mini"}})
	// 计数与时间戳都必须非零：全为 0 时「求和」与「取 max」结果相同，
	// 会让本用例在归并规则写错的情况下巧合通过（曾实测漏报）。
	setColumn(t, remote, rid, "access_count", 3)
	setColumn(t, remote, rid, "last_seen_at", 4000)
	recs, err := remote.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	local := newTestStore(t)
	lid := addWith(t, local, AddInput{Content: "幂等验证事实", Project: "p1", Kind: "decision",
		Tags: map[string]string{"machine": "laptop"}})
	setColumn(t, local, lid, "access_count", 5)
	setColumn(t, local, lid, "last_seen_at", 3000)

	if _, err := local.Import(recs); err != nil {
		t.Fatalf("Import #1: %v", err)
	}
	first := snapshot(t, local)

	if _, err := local.Import(recs); err != nil {
		t.Fatalf("Import #2: %v", err)
	}
	second := snapshot(t, local)

	if !reflect.DeepEqual(first, second) {
		t.Errorf("重复导入不应改变库状态\n首次: %+v\n再次: %+v", first, second)
	}
	if n := countAll(t, local); n != 1 {
		t.Errorf("重复导入不应产生重复行，got %d", n)
	}
}

// access_count 取 max 而非求和，这正是幂等性的机制保证。
func TestImportTakesMaxAccessCountNotSum(t *testing.T) {
	remote := newTestStore(t)
	id := addWith(t, remote, AddInput{Content: "命中计数验证事实", Project: "p1"})
	setColumn(t, remote, id, "access_count", 7)
	recs, err := remote.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	local := newTestStore(t)
	lid := addWith(t, local, AddInput{Content: "命中计数验证事实", Project: "p1"})
	setColumn(t, local, lid, "access_count", 3)

	if _, err := local.Import(recs); err != nil {
		t.Fatalf("Import: %v", err)
	}
	for round := 1; round <= 3; round++ {
		if _, err := local.Import(recs); err != nil {
			t.Fatalf("Import #%d: %v", round, err)
		}
	}
	_, accessCount, _ := row(t, local, lid)
	if accessCount != 7 {
		t.Errorf("access_count 应取 max=7 且不随重复导入增长，got %d", accessCount)
	}
}

func TestImportMergesTagsAsUnion(t *testing.T) {
	const fact = "标记并集验证事实"
	remote := newTestStore(t)
	addWith(t, remote, AddInput{Content: fact, Project: "p1",
		Tags: map[string]string{"machine": "mini", "topic": "sqlite"}})
	recs, _ := remote.Export()

	local := newTestStore(t)
	addWith(t, local, AddInput{Content: fact, Project: "p1",
		Tags: map[string]string{"machine": "laptop", "owner": "wanghui"}})

	res, err := local.Import(recs)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Merged != 1 {
		t.Fatalf("应归并 1 条，got %+v", res)
	}

	got := snapshot(t, local)
	var tags string
	for _, v := range got {
		tags = v.Tags
	}
	for _, want := range []string{"machine=mini", "machine=laptop", "topic=sqlite", "owner=wanghui"} {
		if !strings.Contains(tags, want) {
			t.Errorf("标记应取并集，缺 %q，got %q", want, tags)
		}
	}
}

func TestImportKeepsEarliestCreatedAndLatestSeen(t *testing.T) {
	const fact = "时间戳归并验证事实"
	remote := newTestStore(t)
	rid := addWith(t, remote, AddInput{Content: fact, Project: "p1"})
	setColumn(t, remote, rid, "created_at", 1000)
	setColumn(t, remote, rid, "last_seen_at", 5000)
	recs, _ := remote.Export()

	local := newTestStore(t)
	lid := addWith(t, local, AddInput{Content: fact, Project: "p1"})
	setColumn(t, local, lid, "created_at", 3000)
	setColumn(t, local, lid, "last_seen_at", 2000)

	if _, err := local.Import(recs); err != nil {
		t.Fatalf("Import: %v", err)
	}
	lastSeen, _, _ := row(t, local, lid)
	if lastSeen != 5000 {
		t.Errorf("last_seen_at 应取较晚者 5000，got %d", lastSeen)
	}
	var created int64
	if err := local.db.QueryRow(`SELECT created_at FROM memories WHERE id = ?`, lid).Scan(&created); err != nil {
		t.Fatalf("查询 created_at: %v", err)
	}
	if created != 1000 {
		t.Errorf("created_at 应取最早者 1000，got %d", created)
	}
}

// NULL 表示永不过期；归并时它必须胜过任何具体过期时刻，否则同步会提前销毁记忆。
func TestImportTreatsNullExpiryAsNeverExpires(t *testing.T) {
	const fact = "过期归并验证事实"

	t.Run("远端永不过期则本地也永不过期", func(t *testing.T) {
		remote := newTestStore(t)
		addWith(t, remote, AddInput{Content: fact, Project: "p1"}) // TTL=0 → NULL
		recs, _ := remote.Export()

		local := newTestStore(t)
		lid := addWith(t, local, AddInput{Content: fact, Project: "p1"})
		setColumn(t, local, lid, "expires_at", 9999)

		if _, err := local.Import(recs); err != nil {
			t.Fatalf("Import: %v", err)
		}
		var exp *int64
		if err := local.db.QueryRow(`SELECT expires_at FROM memories WHERE id = ?`, lid).Scan(&exp); err != nil {
			t.Fatalf("查询 expires_at: %v", err)
		}
		if exp != nil {
			t.Errorf("任一边为 NULL（永不过期）时结果应为 NULL，got %d", *exp)
		}
	})

	t.Run("两边都有过期时刻则取较晚者", func(t *testing.T) {
		remote := newTestStore(t)
		rid := addWith(t, remote, AddInput{Content: fact, Project: "p1"})
		setColumn(t, remote, rid, "expires_at", 20000)
		recs, _ := remote.Export()

		local := newTestStore(t)
		lid := addWith(t, local, AddInput{Content: fact, Project: "p1"})
		setColumn(t, local, lid, "expires_at", 15000)

		if _, err := local.Import(recs); err != nil {
			t.Fatalf("Import: %v", err)
		}
		var exp *int64
		if err := local.db.QueryRow(`SELECT expires_at FROM memories WHERE id = ?`, lid).Scan(&exp); err != nil {
			t.Fatalf("查询 expires_at: %v", err)
		}
		if exp == nil || *exp != 20000 {
			t.Errorf("应取较晚的 20000，got %v", exp)
		}
	})
}

// 身份键是 (content_hash, project)，不是 id：远端换一个 id 也必须归并到同一条。
func TestImportIgnoresRemoteIDAndMatchesOnHashAndProject(t *testing.T) {
	const fact = "身份键验证事实"
	remote := newTestStore(t)
	rid := addWith(t, remote, AddInput{Content: fact, Project: "p1"})
	recs, _ := remote.Export()

	local := newTestStore(t)
	addWith(t, local, AddInput{Content: fact, Project: "p1"})
	if n := countAll(t, local); n != 1 {
		t.Fatalf("前置条件：本地应只有 1 条，got %d", n)
	}

	res, err := local.Import(recs)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Inserted != 0 || res.Merged != 1 {
		t.Errorf("应按 hash+project 归并而非按 id 新插，got %+v（远端 id=%s）", res, rid[:8])
	}
	if n := countAll(t, local); n != 1 {
		t.Errorf("归并后仍应只有 1 条，got %d", n)
	}
}

// 同一事实在不同工程里是两条独立记忆，不得互相同化。
func TestImportKeepsSameFactInDifferentProjectsSeparate(t *testing.T) {
	const fact = "跨工程隔离验证事实"
	remote := newTestStore(t)
	addWith(t, remote, AddInput{Content: fact, Project: "projA"})
	recs, _ := remote.Export()

	local := newTestStore(t)
	addWith(t, local, AddInput{Content: fact, Project: "projB"})

	res, err := local.Import(recs)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Inserted != 1 {
		t.Errorf("不同工程应各自独立插入，got %+v", res)
	}
	if n := countAll(t, local); n != 2 {
		t.Errorf("应共有 2 条，got %d", n)
	}
}

// 最强判据：双向导入后两侧的内容投影必须完全一致。
func TestImportConvergesForBothDirections(t *testing.T) {
	const shared = "两侧都有的共享事实"

	a := newTestStore(t)
	addWith(t, a, AddInput{Content: shared, Project: "p1", Device: "A",
		Salience: 0.9, Tags: map[string]string{"machine": "mini"}})
	addWith(t, a, AddInput{Content: "仅A有的事实", Project: "p1", Device: "A"})

	b := newTestStore(t)
	addWith(t, b, AddInput{Content: shared, Project: "p1", Device: "B",
		Salience: 0.3, Tags: map[string]string{"topic": "sqlite"}})
	addWith(t, b, AddInput{Content: "仅B有的事实", Project: "p1", Device: "B"})

	recsA, err := a.Export()
	if err != nil {
		t.Fatalf("Export A: %v", err)
	}
	recsB, err := b.Export()
	if err != nil {
		t.Fatalf("Export B: %v", err)
	}
	if _, err := b.Import(recsA); err != nil {
		t.Fatalf("B.Import(A): %v", err)
	}
	if _, err := a.Import(recsB); err != nil {
		t.Fatalf("A.Import(B): %v", err)
	}

	sa, sb := snapshot(t, a), snapshot(t, b)
	if !reflect.DeepEqual(sa, sb) {
		t.Errorf("双向导入后两侧应收敛\nA: %+v\nB: %+v", dumpSnap(sa), dumpSnap(sb))
	}
	if len(sa) != 3 {
		t.Errorf("收敛后应有 3 条（共享 + 各自独有），got %d", len(sa))
	}

	// origin_device 是「首次写入本库的设备」这一本地视角字段，刻意不在收敛范围内。
	// 这里显式固化该边界，避免日后被误当缺陷。
	for name, s := range map[string]*Store{"A": a, "B": b} {
		recs, err := s.Export()
		if err != nil {
			t.Fatalf("Export %s: %v", name, err)
		}
		for _, r := range recs {
			if r.Content == shared && r.OriginDevice != name {
				t.Errorf("%s 库中共享事实的 origin_device 应保持本地视角 %q，got %q",
					name, name, r.OriginDevice)
			}
		}
	}
}

func dumpSnap(m map[string]snap) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 稳定输出，便于失败时比对
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "\n  %.8s/%s -> %+v", k, strings.SplitN(k, "\x00", 2)[1], m[k])
	}
	return sb.String()
}

// ---------- M4.1: 取代关系的跨设备重建 ----------
//
// `superseded_by` 存的是本地随机 id，跨设备直接搬会变成悬挂引用；
// 而完全不搬，其他设备就不知道取代关系，会重新看到两条重复。
// 解法：导出时把取代关系表达成「身份键」(content_hash, project)，导入时解析回本地 id。

// supersessionView 把「谁的取代指针指向哪条知识」表达成与 id 无关的形式。
// 这是跨设备比较取代关系唯一有意义的口径 —— 本地 id 天然不同。
func supersessionView(t *testing.T, s *Store) map[string]string {
	t.Helper()
	rows, err := s.db.Query(
		`SELECT m.content_hash, m.project,
		        COALESCE(t.content_hash, ''), COALESCE(t.project, '')
		   FROM memories m
		   LEFT JOIN memories t ON t.id = m.superseded_by`)
	if err != nil {
		t.Fatalf("查询取代视图: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var selfHash, selfProj, tgtHash, tgtProj string
		if err := rows.Scan(&selfHash, &selfProj, &tgtHash, &tgtProj); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		val := ""
		if tgtHash != "" || tgtProj != "" {
			val = tgtHash + "\x00" + tgtProj
		}
		out[selfHash+"\x00"+selfProj] = val
	}
	return out
}

func TestExportCarriesSupersessionAsIdentityKey(t *testing.T) {
	s := newTestStore(t)
	keep := addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	dup := addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})
	if _, err := s.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	_ = keep

	recs, err := s.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var found bool
	for _, r := range recs {
		if r.ID != dup {
			continue
		}
		found = true
		if r.Superseded == nil {
			t.Fatal("被取代的记录导出时必须带取代引用")
		}
		if r.Superseded.Project != "p1" {
			t.Errorf("引用应带工程，got %q", r.Superseded.Project)
		}
		if r.Superseded.Hash != HashOf(simBase) {
			t.Errorf("引用应指向存活者的 content_hash，got %q", r.Superseded.Hash)
		}
	}
	if !found {
		t.Fatal("未在导出结果中找到被取代的记录")
	}
}

// 导入端必须把引用解析成「本地」id，而不是照搬远端 id（那会变成悬挂引用）。
func TestImportRebuildsSupersessionWithLocalIDs(t *testing.T) {
	remote := newTestStore(t)
	addWith(t, remote, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	dupRemote := addWith(t, remote, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})
	if _, err := remote.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	recs, err := remote.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	local := newTestStore(t)
	res, err := local.Import(recs)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Unresolved != 0 {
		t.Errorf("两条都在文件里，不应有无法解析的引用，got %+v", res)
	}

	view := supersessionView(t, local)
	key := HashOf(simVariant) + "\x00p1"
	if got := view[key]; got != HashOf(simBase)+"\x00p1" {
		t.Errorf("导入后取代指针应指向本地存活者的身份键，got %q", got)
	}

	// 且必须是真的本地 id，不是远端 id
	var localSup *string
	if err := local.db.QueryRow(`SELECT superseded_by FROM memories WHERE content_hash = ? AND project = ?`,
		HashOf(simVariant), "p1").Scan(&localSup); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if localSup == nil {
		t.Fatal("superseded_by 应已写入")
	}
	if *localSup == dupRemote {
		t.Error("superseded_by 不应照搬远端的随机 id（会成为悬挂引用）")
	}
	var exists int
	if err := local.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, *localSup).Scan(&exists); err != nil {
		t.Fatalf("查询存活者: %v", err)
	}
	if exists != 1 {
		t.Error("superseded_by 必须指向本地确实存在的记录")
	}
}

// 文件里被取代者出现在存活者之前时，两遍解析仍须成功。
func TestImportResolvesSupersessionRegardlessOfFileOrder(t *testing.T) {
	remote := newTestStore(t)
	addWith(t, remote, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	addWith(t, remote, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})
	if _, err := remote.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	recs, err := remote.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// 导出按 (content_hash, project) 排序，被取代者可能排在存活者之前；
	// 这里显式倒序，确保测的是「与顺序无关」而不是「恰好顺序合适」。
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}

	local := newTestStore(t)
	res, err := local.Import(recs)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Unresolved != 0 {
		t.Errorf("两遍解析应不依赖文件顺序，got %+v", res)
	}
	view := supersessionView(t, local)
	if got := view[HashOf(simVariant)+"\x00p1"]; got != HashOf(simBase)+"\x00p1" {
		t.Errorf("倒序文件也应正确重建取代关系，got %q", got)
	}
}

// 存活者不在文件里时，引用无法解析：必须计数上报，而不是静默留空或写悬挂 id。
func TestImportReportsUnresolvedSupersession(t *testing.T) {
	remote := newTestStore(t)
	addWith(t, remote, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	addWith(t, remote, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})
	if _, err := remote.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	recs, err := remote.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// 只保留被取代者，丢掉存活者
	var partial []ExportRecord
	for _, r := range recs {
		if r.Content == simVariant {
			partial = append(partial, r)
		}
	}
	if len(partial) != 1 {
		t.Fatalf("应只剩 1 条，got %d", len(partial))
	}

	local := newTestStore(t)
	res, err := local.Import(partial)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Unresolved != 1 {
		t.Errorf("存活着缺失时应报 1 条无法解析，got %+v", res)
	}
	var sup *string
	if err := local.db.QueryRow(`SELECT superseded_by FROM memories`).Scan(&sup); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if sup != nil {
		t.Errorf("无法解析的引用不得写入（会成悬挂），got %v", *sup)
	}
}

// ---------- WS3: 三方合并 ----------

// mkRec 构造一条远端导出的记录（身份键由内容哈希计算）。
func mkRec(content, proj string, updatedAt int64, device string) ExportRecord {
	return ExportRecord{
		Content:      content,
		ContentHash:  HashOf(content),
		Kind:         "fact",
		Project:      proj,
		Salience:     0.5,
		CreatedAt:    updatedAt,
		UpdatedAt:    updatedAt,
		LastSeenAt:   updatedAt,
		OriginDevice: device,
		OriginTool:   "cli",
		Source:       "human",
		AccessCount:  1,
	}
}

// syncContent 返回本地库中该 (hash, project) 的原文内容。
func syncContent(t *testing.T, s *Store, h, proj string) string {
	t.Helper()
	var c string
	if err := s.db.QueryRow(
		`SELECT content FROM memories WHERE content_hash = ? AND project = ?`, h, proj,
	).Scan(&c); err != nil {
		t.Fatalf("查询 content: %v", err)
	}
	return c
}

func hasTag(t *testing.T, s *Store, id, k, v string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tags WHERE memory_id = ? AND key = ? AND value = ?`, id, k, v,
	).Scan(&n); err != nil {
		t.Fatalf("查询 tag: %v", err)
	}
	return n > 0
}

// 本地没有的键 → 新插入，且不因同步覆盖本地源信息以外的语义。
func TestSyncMergeInsertsNewRecords(t *testing.T) {
	local := newTestStore(t)
	rec := mkRec("远端独有事实", "p1", 900, "dev-far")

	stats, err := local.SyncMerge([]ExportRecord{rec})
	if err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	if stats.Inserted != 1 || stats.Merged != 0 || stats.NewerLocal != 0 {
		t.Errorf("应插入 1 条，got %+v", stats)
	}
	if countAll(t, local) != 1 {
		t.Errorf("应只有 1 条，got %d", countAll(t, local))
	}
	exp, err := local.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(exp) != 1 || exp[0].OriginDevice != "dev-far" {
		t.Errorf("插入记录应保留远端溯源设备，got %+v", exp)
	}
}

// 同 hash、远端更新（> 本地 + 阈值）→ 最近者胜：远端内容写法胜出、时间戳推进。
func TestSyncMergeRemoteNewerWins(t *testing.T) {
	const proj = "p1"
	// 不同间距的同义写法 → 归一化后 hash 相同，但原文串可区分「谁新」。
	oldSpell := "远程同步  事实"
	newSpell := "远程同步 事实"
	if HashOf(oldSpell) != HashOf(newSpell) {
		t.Fatal("前置：两种写法应归一化为同一 content_hash")
	}
	s := newTestStore(t)
	lid := addWith(t, s, AddInput{Content: oldSpell, Project: proj})
	// 本地 updated_at 置旧
	if _, err := s.db.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, 1000, lid); err != nil {
		t.Fatalf("set updated_at: %v", err)
	}

	remote := mkRec(newSpell, proj, 5000, "dev-far")
	stats, err := s.SyncMerge([]ExportRecord{remote})
	if err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	if stats.Merged != 1 {
		t.Errorf("远端更新应归并（最近者胜），got %+v", stats)
	}

	if got := syncContent(t, s, HashOf(newSpell), proj); got != newSpell {
		t.Errorf("最近者胜应采纳远端新写法 %q，got %q", newSpell, got)
	}
	var at int64
	if err := s.db.QueryRow(`SELECT updated_at FROM memories WHERE id = ?`, lid).Scan(&at); err != nil {
		t.Fatalf("查询 updated_at: %v", err)
	}
	if at != 5000 {
		t.Errorf("updated_at 应推进到远端 5000，got %d", at)
	}
	// 冲突标记不应当出现在「明确最近者胜」分支
	if hasTag(t, s, lid, "sync", "conflict:dev-far") {
		t.Error("最近者胜分支不应留下冲突标记")
	}
}

// 同 hash、本地更新：保持本地，远端不做任何覆盖，也不打冲突标记。
func TestSyncMergeLocalNewerKeepsLocal(t *testing.T) {
	s := newTestStore(t)
	lid := addWith(t, s, AddInput{Content: "本地更新事实", Project: "p1"})
	if _, err := s.db.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, 8000, lid); err != nil {
		t.Fatalf("set updated_at: %v", err)
	}

	remote := mkRec("本地更新事实", "p1", 100, "dev-far")
	stats, err := s.SyncMerge([]ExportRecord{remote})
	if err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	if stats.NewerLocal != 1 || stats.Merged != 0 {
		t.Errorf("本地更新应保持，got %+v", stats)
	}
	if got := syncContent(t, s, HashOf("本地更新事实"), "p1"); got != "本地更新事实" {
		t.Errorf("本地内容应保持不变，got %q", got)
	}
	if hasTag(t, s, lid, "sync", "conflict:dev-far") {
		t.Error("本地更新分支不应留下冲突标记")
	}
}

// 同 hash、时间戳撞车（≤ 阈值）→ 冲突并存：保留本地写法并打 sync 冲突标记，
// 不静默吞掉任一设备；两端都记录在案供 mem merge 人工收敛。
func TestSyncMergeConflictMarksAndKeeps(t *testing.T) {
	// 两端 updated_at 取同一值，精确命中冲突分支。
	at := int64(3000)
	local := newTestStore(t)
	lid := addWith(t, local, AddInput{Content: "并行编辑事实", Project: "p1"})
	if _, err := local.db.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, at, lid); err != nil {
		t.Fatalf("set updated_at: %v", err)
	}

	remote := mkRec("并行编辑事实", "p1", at, "dev-far")
	stats, err := local.SyncMerge([]ExportRecord{remote})
	if err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	if len(stats.Conflicts) != 1 {
		t.Fatalf("应记录 1 条冲突，got %+v", stats)
	}
	if stats.Conflicts[0].LocalAt != at || stats.Conflicts[0].RemoteAt != at {
		t.Errorf("冲突应如实记录两侧时间戳，got %+v", stats.Conflicts[0])
	}
	if stats.Conflicts[0].RemoteDevice != "dev-far" {
		t.Errorf("冲突应记录远端设备，got %+v", stats.Conflicts[0])
	}
	if !hasTag(t, local, lid, "sync", "conflict:dev-far") {
		t.Error("冲突时应打 sync 冲突标记（不静默吞远端来源）")
	}
	if got := syncContent(t, local, HashOf("并行编辑事实"), "p1"); got != "并行编辑事实" {
		t.Errorf("冲突时保留本地写法，got %q", got)
	}
	if countAll(t, local) != 1 {
		t.Errorf("冲突不应新增行（唯一索引），got %d", countAll(t, local))
	}
}

// 阈值内（差 1 秒）也被视为撞车冲突，而不是「远端更」。
func TestSyncMergeWithinToleranceIsConflict(t *testing.T) {
	local := newTestStore(t)
	lid := addWith(t, local, AddInput{Content: "阈值边界事实", Project: "p1"})
	if _, err := local.db.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, 1000, lid); err != nil {
		t.Fatalf("set updated_at: %v", err)
	}
	remote := mkRec("阈值边界事实", "p1", 1001, "dev-far")
	stats, err := local.SyncMerge([]ExportRecord{remote})
	if err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	if len(stats.Conflicts) != 1 {
		t.Errorf("差 1 秒（≤2）应判撞车冲突，got %+v", stats)
	}
	if !hasTag(t, local, lid, "sync", "conflict:dev-far") {
		t.Error("应打冲突标记")
	}
}

// 远程 tags 在归并（并入/保持/冲突）后都应并集保留。
func TestSyncMergeUnionsTags(t *testing.T) {
	local := newTestStore(t)
	lid := addWith(t, local, AddInput{Content: "标记并合事实", Project: "p1"})
	if _, err := local.db.Exec(`UPDATE memories SET updated_at = ? WHERE id = ?`, 1000, lid); err != nil {
		t.Fatalf("set updated_at: %v", err)
	}
	rec := mkRec("标记并合事实", "p1", 5000, "dev-far")
	rec.Tags = map[string][]string{"machine": {"mini"}, "owner": {"wanghui"}}
	if _, err := local.SyncMerge([]ExportRecord{rec}); err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	for k, v := range map[string]string{"machine": "mini", "owner": "wanghui"} {
		if !hasTag(t, local, lid, k, v) {
			t.Errorf("远端标记 %s=%s 应并集保留", k, v)
		}
	}
}

// 空内容记录应计入 skipped，不落库。
func TestSyncMergeSkipsEmptyContent(t *testing.T) {
	local := newTestStore(t)
	rec := mkRec("   ", "p1", 100, "dev-far")
	stats, err := local.SyncMerge([]ExportRecord{rec})
	if err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	if stats.Skipped != 1 {
		t.Errorf("空内容应跳过，got %+v", stats)
	}
	if countAll(t, local) != 0 {
		t.Errorf("不应落库，got %d", countAll(t, local))
	}
}

// 远端导出里的取代关系经 SyncMerge 后应在本地重建为本地 id。
func TestSyncMergeResolvesSupersession(t *testing.T) {
	remote := newTestStore(t)
	addWith(t, remote, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	addWith(t, remote, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})
	if _, err := remote.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	recs := exportOf(t, remote)

	local := newTestStore(t)
	res, err := local.SyncMerge(recs)
	if err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	if res.Unresolved != 0 {
		t.Errorf("存活者与被取代者都在文件里，不应有未解析引用，got %+v", res)
	}
	view := supersessionView(t, local)
	if got := view[HashOf(simVariant)+"\x00p1"]; got != HashOf(simBase)+"\x00p1" {
		t.Errorf("导入后取代指针应指向存活者身份键，got %q", got)
	}
}

// 存活者缺失时引用无法解析：SyncMerge 必须计数上报，不写悬挂 id。
func TestSyncMergeReportsUnresolvedSupersession(t *testing.T) {
	remote := newTestStore(t)
	addWith(t, remote, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	dup := addWith(t, remote, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})
	if _, err := remote.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	recs := exportOf(t, remote)
	var partial []ExportRecord
	for _, r := range recs {
		if r.ID == dup {
			partial = append(partial, r)
		}
	}
	if len(partial) != 1 {
		t.Fatalf("应只剩被取代者 1 条，got %d", len(partial))
	}

	local := newTestStore(t)
	res, err := local.SyncMerge(partial)
	if err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	if res.Unresolved != 1 {
		t.Errorf("存活者缺失应报 1 条未解析，got %+v", res)
	}
	var sup *string
	if err := local.db.QueryRow(`SELECT superseded_by FROM memories WHERE content_hash = ?`, HashOf(simVariant)).Scan(&sup); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if sup != nil {
		t.Errorf("未解析引用不得写入，got %v", *sup)
	}
}

// 远端导出缺 content_hash / kind 时，SyncMerge 应回退到本地归一化计算与默认 fact。
func TestSyncMergeFallsBackMissingHashAndKind(t *testing.T) {
	local := newTestStore(t)
	rec := mkRec("缺hash与kind的事实", "", 900, "dev-far")
	rec.ContentHash = "" // 手工编辑的导出可能漏了身份键
	rec.Kind = ""

	stats, err := local.SyncMerge([]ExportRecord{rec})
	if err != nil {
		t.Fatalf("SyncMerge: %v", err)
	}
	if stats.Inserted != 1 {
		t.Fatalf("应插入 1 条（用本地归一化回填 hash/kind），got %+v", stats)
	}
	var kind string
	h := HashOf("缺hash与kind的事实")
	if err := local.db.QueryRow(
		`SELECT kind FROM memories WHERE content_hash = ?`, h).Scan(&kind); err != nil {
		t.Fatalf("查询回填后的记录: %v", err)
	}
	if kind != "fact" {
		t.Errorf("缺 kind 时应回填默认 fact，got %q", kind)
	}
}

// 双向同步后两侧的事实投影应收敛（各自独有并入对方，共享事实一致）。
//
// 注意：只比「事实投影」，不比 tags——
// 冲突并存时会在同一条上打 sync 标记，整库回灌到它的来源端时该标记可能落在
// A 侧或 B 侧（取决于谁先合并），这是「来源可见性」的辅助噪音，不是数据分歧；
// 内容与元数据的收敛才是本测试守的口径。
func TestSyncMergeConvergesBothDirections(t *testing.T) {
	a := newTestStore(t)
	mustAdd(t, a, "仅A有的事实")
	mustAdd(t, a, "共享事实")
	b := newTestStore(t)
	mustAdd(t, b, "仅B有的事实")
	mustAdd(t, b, "共享事实")

	if _, err := b.SyncMerge(exportOf(t, a)); err != nil {
		t.Fatalf("b.SyncMerge(a): %v", err)
	}
	if _, err := a.SyncMerge(exportOf(t, b)); err != nil {
		t.Fatalf("a.SyncMerge(b): %v", err)
	}
	sa, sb := stripTags(snapshot(t, a)), stripTags(snapshot(t, b))
	if !reflect.DeepEqual(sa, sb) {
		t.Errorf("双向同步后两侧应收敛\nA: %s\nB: %s", dumpSnap(sa), dumpSnap(sb))
	}
	if len(sa) != 3 {
		t.Errorf("收敛后应有 3 条，got %d", len(sa))
	}
}

// stripTags 抛出 tags 投影，仅保留「事实 + 位置元数据」用于收敛比较。
func stripTags(m map[string]snap) map[string]snap {
	out := make(map[string]snap, len(m))
	for k, v := range m {
		v.Tags = ""
		out[k] = v
	}
	return out
}

// exportOf 导出某库的全部记录作为「远端输入」。
func exportOf(t *testing.T, s *Store) []ExportRecord {
	t.Helper()
	recs, err := s.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	return recs
}

// 一条记录已在本地被取代，随后导入同一份它仍然是被取代状态。
func TestImportKeepsAlreadySupersededRecordSuperseded(t *testing.T) {
	s := newTestStore(t)
	addWith(t, s, AddInput{Content: simBase, Project: "p1", Salience: 0.9})
	addWith(t, s, AddInput{Content: simVariant, Project: "p1", Salience: 0.3})
	if _, err := s.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	recs, err := s.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	before := supersessionView(t, s)
	if _, err := s.Import(recs); err != nil {
		t.Fatalf("Import: %v", err)
	}
	after := supersessionView(t, s)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("导入自身导出不应改变取代关系\n前: %v\n后: %v", before, after)
	}

	// 视图口径：search 应只看到 1 条
	hits, err := s.Search(SearchQuery{Query: "隐藏目录", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("被取代者仍应被检索隐去，got %d 条", len(hits))
	}
}

// 端到端最强判据：一端做过归并、另一端独立写过同一事实，
// 交叉导入后两侧的「记忆集合 + 取代关系」必须完全一致。
func TestCrossDeviceConvergenceWithSupersession(t *testing.T) {
	a := newTestStore(t)
	addWith(t, a, AddInput{Content: simBase, Project: "p1", Device: "A", Salience: 0.9})
	addWith(t, a, AddInput{Content: simVariant, Project: "p1", Device: "A", Salience: 0.3})
	a.Add(AddInput{Content: "仅A有的事实条目内容", Project: "p1", Device: "A"}) //nolint:errcheck
	if _, err := a.Consolidate(0.7); err != nil {
		t.Fatalf("Consolidate A: %v", err)
	}

	b := newTestStore(t)
	addWith(t, b, AddInput{Content: simBase, Project: "p1", Device: "B", Salience: 0.5})
	addWith(t, b, AddInput{Content: "仅B有的事实条目内容", Project: "p1", Device: "B"})

	recsA, err := a.Export()
	if err != nil {
		t.Fatalf("Export A: %v", err)
	}
	recsB, err := b.Export()
	if err != nil {
		t.Fatalf("Export B: %v", err)
	}
	if _, err := b.Import(recsA); err != nil {
		t.Fatalf("B.Import(A): %v", err)
	}
	if _, err := a.Import(recsB); err != nil {
		t.Fatalf("A.Import(B): %v", err)
	}

	if !reflect.DeepEqual(snapshot(t, a), snapshot(t, b)) {
		t.Errorf("内容投影未收敛\nA: %s\nB: %s", dumpSnap(snapshot(t, a)), dumpSnap(snapshot(t, b)))
	}
	va, vb := supersessionView(t, a), supersessionView(t, b)
	// B 端从未跑过 consolidate，其本地副本此时未被标记；
	// 但它收到了 A 的取代关系，故两侧的取代视图应一致。
	if !reflect.DeepEqual(va, vb) {
		t.Errorf("取代关系未收敛\nA: %v\nB: %v", va, vb)
	}
}
