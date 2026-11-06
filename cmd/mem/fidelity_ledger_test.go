package main

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/evidence"
	"github.com/v2camp/v2mem/internal/store"
)

// 空摘要是 T6 删除两处回落后的新行为，必须证明：多条同源空载荷事件能并存，
// 且落库列正确。否则「删回落」可能悄悄触发 legacy 内容去重或写坏索引列。
//
// 三条事件的 eventType / tool / summary 完全相同 ⇒ content_hash 相同；
// 共存靠 ux_obs_event_uid，而不是靠 hash 不同——这正是要实证的语义。
func TestFidelityE2E_EmptySummaryLedgerColumnsAndLegacyDedup(t *testing.T) {
	db := testDB(t)
	proj, _ := hookProjectDir(t, "demo-repo")
	sid := "fid-ledger"

	for _, id := range []string{"evt-a", "evt-b", "evt-c"} {
		runFidelityHook(t, db, map[string]any{
			"hook_event_name": "Stop", "cwd": proj, "session_id": sid,
			"event_id": id,
		})
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	obs, err := st.ListObservations(sid, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 3 {
		t.Fatalf("三条同摘要空载荷事件应全部落库，got %d: %+v", len(obs), obs)
	}

	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	rows, err := raw.Query(`SELECT summary, summary_idx, content_hash, event_uid
		FROM observations WHERE session_id = ? ORDER BY event_uid`, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var firstHash string
	uids := map[string]bool{}
	n := 0
	for rows.Next() {
		var summary, idx, hash, uid string
		if err := rows.Scan(&summary, &idx, &hash, &uid); err != nil {
			t.Fatal(err)
		}
		if summary != "" {
			t.Fatalf("空载荷摘要应为空，got %q", summary)
		}
		if idx != "-" {
			t.Fatalf("空摘要索引应为 '-' 哨兵，got %q", idx)
		}
		if hash == "" {
			t.Fatal("content_hash 不应为空")
		}
		if uid == "" || uids[uid] {
			t.Fatalf("event_uid 应非空且互不相同，got %q", uid)
		}
		uids[uid] = true
		if firstHash == "" {
			firstHash = hash
		}
		if hash != firstHash {
			t.Fatalf("同源空载荷 content_hash 本应相同（共存靠 event_uid 而非 hash 不同），got %q vs %q", hash, firstHash)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("应读到 3 行，got %d", n)
	}

	// legacy 部分索引仍在，且 WHERE 明确排除带身份的证据行。
	var indexSQL string
	if err := raw.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type='index' AND name='ux_obs_dedup_legacy'`).Scan(&indexSQL); err != nil {
		t.Fatalf("ux_obs_dedup_legacy 索引应存在: %v", err)
	}
	if !strings.Contains(indexSQL, "event_uid") || !strings.Contains(indexSQL, "source_id") {
		t.Fatalf("legacy 去重索引应带 WHERE event_uid='' AND source_id=''，got %q", indexSQL)
	}

	refs := fidelityRefs(t, st)
	if len(refs) != 3 {
		t.Fatalf("证据引用应有 3 条，got %d: %+v", len(refs), refs)
	}
	for _, r := range refs {
		if r.Fidelity != evidence.FidelityNone {
			t.Fatalf("空载荷证据 Fidelity 应为 none，got %+v", r)
		}
		// 分层语义（本轮铁律）：ProposalEligible 是事件级总开关，只排除记忆
		// 管道流量；FidelityNone 是内容级「无正文」。两者取与后由
		// internal/proposal 的 proposalEligible fail-closed 拦下（见 parse.go
		// 与 TestProposalEligible）。此处刻意不要求总开关变 false——让
		// Fidelity 反向改写事件级语义会造成两层耦合，破坏统一语义体系。
		if !r.ProposalEligible {
			t.Fatalf("非管道事件的总开关应保持 true，内容准入交给 Fidelity，got %+v", r)
		}
	}
}
