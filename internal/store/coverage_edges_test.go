package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/commit"
)

// fillEvidenceLocator 是「证据链能否定位到原始记录」的补齐逻辑：既按本机 id
// 查，也按跨设备可解析的 event_uid 兜底。这里逐个分支锁住它的行为。
func TestFillEvidenceLocatorBranches(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	put := func(id, uid, archive, rawRef string, seq int, occurred int64) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO observations(
			id, session_id, event, content_hash, created_at,
			event_uid, archive_hash, raw_ref, source_seq, occurred_at_ns
		) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, "s", "user-prompt", "h-"+id, 1, uid, archive, rawRef, seq, occurred); err != nil {
			t.Fatal(err)
		}
	}
	put("obs-a", "uid-a", "arch-a", "raw-a", 5, 111)
	put("obs-b", "uid-b", "", "", 0, 0)

	fill := func(e *candidateEvidenceRow) {
		t.Helper()
		if err := s.fillEvidenceLocator(ctx, s.db, e); err != nil {
			t.Fatalf("fillEvidenceLocator: %v", err)
		}
	}

	// 1) 完全没有身份：直接定 C 级，不查库。
	e := &candidateEvidenceRow{}
	fill(e)
	if e.Grade != commit.GradeC || e.EvidenceID != "" {
		t.Fatalf("无身份行应为 C 级且不造 id: %+v", e)
	}

	// 2) 按本机 id 命中：把 canonical 定位补齐。
	e = &candidateEvidenceRow{EvidenceID: "obs-a"}
	fill(e)
	if e.EventUID != "uid-a" || e.ArchiveHash != "arch-a" || e.RawLocator != "raw-a" ||
		e.Seq != 5 || e.OccurredAtNS != 111 || e.Grade != commit.GradeA {
		t.Fatalf("按 id 命中应补齐身份与定位: %+v", e)
	}

	// 3) 无本机 id、只有 event_uid：走 uid 查询，缺归档只能到 B 级。
	e = &candidateEvidenceRow{EventUID: "uid-b"}
	fill(e)
	if e.EvidenceID != "obs-b" || e.Grade != commit.GradeB {
		t.Fatalf("按 uid 查询应补回本机 id 并定 B 级: %+v", e)
	}

	// 4) 本机 id 失效但 uid 还能命中（跨设备 sync 常见）：必须回退到 uid。
	e = &candidateEvidenceRow{EvidenceID: "ghost-id", EventUID: "uid-a"}
	fill(e)
	if e.EvidenceID != "obs-a" || e.RawLocator != "raw-a" {
		t.Fatalf("id 查不到时应按 uid 兜底，不能让证据链指向空处: %+v", e)
	}

	// 5) 查不到且无 uid：保留调用方原值，用 archive#seq 合成定位，不伪造身份。
	e = &candidateEvidenceRow{EvidenceID: "ghost-id", ArchiveHash: "arch-x", Seq: 3}
	fill(e)
	if e.EventUID != "" || e.RawLocator != "arch-x#3" || e.Grade != commit.GradeB {
		t.Fatalf("无 uid 时不得伪造身份，定位回退到 archive#seq: %+v", e)
	}

	// 6) seq=0 时定位退化为 archive 本身。
	e = &candidateEvidenceRow{EvidenceID: "ghost-id", ArchiveHash: "arch-y"}
	fill(e)
	if e.RawLocator != "arch-y" {
		t.Fatalf("seq=0 时定位应为 archive: %+v", e)
	}
}

// ClaimProposalBundle 的边界：默认参数、重复认领、不存在的窗口。
func TestClaimProposalBundleEdgeCases(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ClaimProposalBundle("", "", 0, 0); err == nil {
		t.Fatal("空 bundle id 应被拒")
	}
	b := mustBundle(t, s, "uid-edge-", 1)

	// now/owner/ttl 全给零值，仍要落到可用的默认租约。
	pb, err := s.ClaimProposalBundle(b.ID, "", 0, 0)
	if err != nil {
		t.Fatalf("默认参数认领失败: %v", err)
	}
	if pb.ID != b.ID || pb.Status != "leased" || pb.LeaseOwner != "proposal" {
		t.Fatalf("默认 owner 应回填为 proposal: %+v", pb)
	}
	// 条件是 lease_until <= now，取 lease_until-1 才是「租约未过期」。
	if _, err := s.ClaimProposalBundle(b.ID, "other", pb.LeaseUntil-1, time.Minute); !errors.Is(err, ErrProposalBundleLeased) {
		t.Fatalf("租约未过期重复认领应报 ErrProposalBundleLeased, got %v", err)
	}
	if _, err := s.ClaimProposalBundle("不存在的窗口", "", time.Now().Unix(), time.Minute); err == nil {
		t.Fatal("不存在的窗口应报错，不能静默当成被租用")
	}
}
