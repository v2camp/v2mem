//go:build perf

// S14 大库性能基准（design-storage-layering.md §9）。
//
// 用合成数据在临时目录造大库（测完即删），对 8 个大库场景逐一计时：
//   1. mem stats --json          → store.Stats + store.Backlog（含积压单快照 SQL）
//   2. mem search                → store.Search（FTS 三路召回 + 可见性过滤）
//   3. proposal 调度             → NextRunnableProposalBundle / NextOpenProposalBundle
//   4. NextEvidenceBundle 出窗   → 跨源连续窗口（含序号洞记账）
//   5. mem review ls 四态        → store.ListReviewQueue
//   6. mem eval flow            → eval.LoadFlowSnapshot + CheckFlow（全表拉取）
//   7. audit ReadHistory         → 当前档 + .1–.3 合并（纯文件 I/O，无索引可加）
//   8. mem gc --retention        → GCTerminalBundles / GCCandidates / GCCommitDecisions
//
// 运行：make perf（= go test -tags=perf ./test/perf -count=1 -v）。
// 场景共享一份大库（TestMain 一次生成），编号即执行顺序：4/8 有写入，
// 排在只读场景之后；8 是删除，排最后。CI 不跑本包（不进默认 make test）。

package perf

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/audit"
	"github.com/v2camp/v2mem/internal/eval"
	"github.com/v2camp/v2mem/internal/similarity"
	"github.com/v2camp/v2mem/internal/store"
)

// 大库规模。总量按「生成 <30s、单场景测量秒级」控制，别照真实库无限外推。
const (
	nMemories   = 20000 // memories + mem_sigs
	nObservs    = 99000 // 165 源 × 600 seq
	nSources    = 165
	nArchives   = 5 * nSources
	nBundles    = 20 * nSources // 每源 20 窗，含退避/租约/终态
	nCands      = 10000
	nDecisions  = 20000
	nRuns       = 500
	memPerBatch = 2000 // 每个插入事务的行数，控制事务体积
)

var (
	dbPath   string
	bigStore *store.Store
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "v2mem-perf-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建临时目录失败: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	dbPath = filepath.Join(dir, "mem.db")
	buildStart := time.Now()
	if err := buildBigDB(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "生成大库失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("大库生成完成：%s（耗时 %s）\n", dbPath, time.Since(buildStart).Round(time.Millisecond))

	buildAuditLog(filepath.Join(dir, "audit.jsonl"))

	code := m.Run()
	if bigStore != nil {
		bigStore.Close()
	}
	os.Exit(code)
}

// measure 跑 warmup+iters 次并逐次报告毫秒数。基准数字只看相对变化，
// 绝对值受机器负载影响，报告里写「同一台机器同一次运行内的前后对比」。
func measure(t *testing.T, name string, warmup, iters int, fn func() error) {
	t.Helper()
	for i := 0; i < warmup; i++ {
		if err := fn(); err != nil {
			t.Fatalf("%s warmup: %v", name, err)
		}
	}
	for i := 0; i < iters; i++ {
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s iter %d: %v", name, i, err)
		}
		t.Logf("%s 第 %d 次：%s", name, i+1, time.Since(start).Round(time.Millisecond))
	}
}

// ---------- 场景 1：mem stats --json ----------

func TestPerf01Stats(t *testing.T) {
	st := openBig(t)
	now := time.Now().Unix()
	measure(t, "S1 stats(Stats+Backlog)", 1, 5, func() error {
		if _, err := st.Stats(); err != nil {
			return err
		}
		_, err := st.Backlog(now)
		return err
	})
}

// ---------- 场景 2：mem search ----------

func TestPerf02Search(t *testing.T) {
	st := openBig(t)
	measure(t, "S2 search(memory)", 1, 5, func() error {
		_, err := st.Search(store.SearchQuery{Query: "部署服务失败", Project: "proj-7", Limit: 10})
		return err
	})
	measure(t, "S2 search(observation)", 1, 5, func() error {
		_, err := st.Search(store.SearchQuery{Query: "部署服务失败", Layer: "observation", Limit: 10})
		return err
	})
}

// ---------- 场景 3：proposal 调度 ----------

func TestPerf03ProposalScheduling(t *testing.T) {
	st := openBig(t)
	now := time.Now().Unix()
	measure(t, "S3 NextRunnableProposalBundle", 1, 10, func() error {
		_, found, err := st.NextRunnableProposalBundle(now)
		if err != nil {
			return err
		}
		_ = found
		return nil
	})
	measure(t, "S3 NextOpenProposalBundle", 1, 10, func() error {
		_, found, err := st.NextOpenProposalBundle()
		if err != nil {
			return err
		}
		_ = found
		return nil
	})
}

// ---------- 场景 4：NextEvidenceBundle 出窗 ----------

func TestPerf04NextEvidenceBundle(t *testing.T) {
	st := openBig(t)
	// 每次调用消费一个窗口（含写事务）；只测 30 次，避免把 165 源全部抽干。
	for i := 0; i < 30; i++ {
		start := time.Now()
		b, err := st.NextEvidenceBundle(50, 64<<10)
		if err != nil {
			t.Fatalf("NextEvidenceBundle: %v", err)
		}
		if b == nil {
			t.Logf("S4 第 %d 次：无窗口可出，提前结束", i+1)
			return
		}
		t.Logf("S4 NextEvidenceBundle 第 %d 次（窗口 %s...seq=%d）：%s",
			i+1, b.ID[:8], b.ToSeqInclusive, time.Since(start).Round(time.Millisecond))
	}
}

// ---------- 场景 5：mem review ls 四态 ----------

func TestPerf05ReviewQueue(t *testing.T) {
	st := openBig(t)
	measure(t, "S5 ListReviewQueue(all,50)", 1, 5, func() error {
		q, err := st.ListReviewQueue("all", 50)
		if err != nil {
			return err
		}
		_ = q
		return nil
	})
}

// ---------- 场景 6：mem eval flow ----------

func TestPerf06EvalFlow(t *testing.T) {
	st := openBig(t)
	measure(t, "S6 eval flow(13 检查快照)", 1, 3, func() error {
		snap, err := eval.LoadFlowSnapshot(st.DB(), time.Now().Unix())
		if err != nil {
			return err
		}
		rep := eval.CheckFlow(snap, 15*24*time.Hour)
		_ = rep
		return nil
	})
}

// ---------- 场景 7：audit ReadHistory ----------

func TestPerf07AuditHistory(t *testing.T) {
	path := filepath.Join(filepath.Dir(dbPath), "audit.jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("审计日志缺失: %v", err)
	}
	t.Logf("S7 审计日志总档：%d 字节（当前档 + .1–.3）", fi.Size())
	measure(t, "S7 ReadHistory(0)", 1, 5, func() error {
		recs, err := audit.ReadHistory(path, 0)
		if err != nil {
			return err
		}
		_ = recs
		return nil
	})
}

// ---------- 场景 8：mem gc --retention（库侧） ----------

func TestPerf08GCRetention(t *testing.T) {
	st := openBig(t)
	// dry-run 的 CLI 入口只做文件侧预览（cmd/mem/retention.go）：库侧计数查询
	// 都在三个 GC 方法里，这里按 cmdGC 的真实顺序逐个计时（有写入，排最后）。
	measure(t, "S8 GCTerminalBundles", 0, 1, func() error {
		r, err := st.GCTerminalBundles(0)
		if err != nil {
			return err
		}
		t.Logf("S8 GCTerminalBundles：bundles=%d items=%d skipped=%d", r.Bundles, r.Items, r.SkippedReferenced)
		return nil
	})
	measure(t, "S8 GCCandidates", 0, 1, func() error {
		_, err := st.GCCandidates(14*24*time.Hour, 90*24*time.Hour)
		return err
	})
	measure(t, "S8 GCCommitDecisions", 0, 1, func() error {
		_, err := st.GCCommitDecisions(0)
		return err
	})
}

func openBig(t *testing.T) *store.Store {
	t.Helper()
	if bigStore == nil {
		s, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("打开大库: %v", err)
		}
		bigStore = s
	}
	return bigStore
}

// ---------- 合成数据生成 ----------

// buildBigDB 一次性造出 8 场景共用的大库。全部走批量事务 + 预编译语句，
// 保证生成 <30s；数据的列口径与真实写入路径一致（触发器/部分索引都生效）。
func buildBigDB(path string) error {
	s, err := store.Open(path)
	if err != nil {
		return err
	}
	defer s.Close()
	db := s.DB()
	ctx := context.Background()

	base := time.Now().Add(-90 * 24 * time.Hour).Unix()

	if err := seedMemories(ctx, db, base); err != nil {
		return fmt.Errorf("memories: %w", err)
	}
	if err := seedObservations(ctx, db, base); err != nil {
		return fmt.Errorf("observations: %w", err)
	}
	if err := seedEvidence(ctx, db, base); err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	if err := seedCandidates(ctx, db, base); err != nil {
		return fmt.Errorf("candidates/decisions: %w", err)
	}
	return nil
}

func txBatches(ctx context.Context, db *sql.DB, batches int, fn func(tx *sql.Tx, batch int) error) error {
	for b := 0; b < batches; b++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := fn(tx, b); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

var hanWords = []string{"部署", "服务", "失败", "重试", "索引", "缓存", "配置", "权限", "日志", "队列",
	"迁移", "回滚", "签名", "凭证", "预算", "评审", "蒸馏", "观测", "证据", "窗口"}

// seedMemories 造 memories + mem_sigs + tags。10% 标 withdrawn（检索可见性过滤的真实分布）。
func seedMemories(ctx context.Context, db *sql.DB, base int64) error {
	return txBatches(ctx, db, nMemories/memPerBatch, func(tx *sql.Tx, batch int) error {
		stmt, err := tx.Prepare(`INSERT INTO memories(
			id, content, content_idx, kind, content_hash, project, salience,
			created_at, updated_at, last_seen_at, origin_device, origin_tool,
			source, channel, scope, access_count, review_state)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		sigStmt, err := tx.Prepare(`INSERT INTO mem_sigs(memory_id, sig) VALUES (?,?)`)
		if err != nil {
			return err
		}
		defer sigStmt.Close()
		for i := 0; i < memPerBatch; i++ {
			n := batch*memPerBatch + i
			// 词序列既当正文也当可检索形态：FTS 按空格切 token，查询词能命中。
			content := fmt.Sprintf("%s %s 在 proj-%d 环境总是 %s，先 %s 再 %s (ref %06d)",
				hanWords[n%len(hanWords)], hanWords[(n*7)%len(hanWords)],
				n%40, hanWords[(n*3)%len(hanWords)], hanWords[(n*11)%len(hanWords)],
				hanWords[(n*13)%len(hanWords)], n)
			state := ""
			if n%10 == 9 {
				state = "withdrawn"
			}
			id := fmt.Sprintf("mem-%08d", n)
			at := base + int64(n)
			if _, err := stmt.Exec(id, content, content, "fact",
				fmt.Sprintf("hash-%08d", n), fmt.Sprintf("proj-%d", n%40),
				0.5, at, at, at, "bench-device", "bench", "bench", "tool", "project",
				n%50, state); err != nil {
				return err
			}
			sig := similarity.Sign(content)
			sigBytes := make([]byte, 8*len(sig))
			for j, v := range sig {
				binary.LittleEndian.PutUint64(sigBytes[j*8:], v)
			}
			if _, err := sigStmt.Exec(id, sigBytes); err != nil {
				return err
			}
		}
		return nil
	})
}

// seedObservations 造 165 源 × 600 seq 的观测，并按每源 2 处序号洞制造真实窗口形态。
func seedObservations(ctx context.Context, db *sql.DB, base int64) error {
	perSource := nObservs / nSources
	return txBatches(ctx, db, nSources, func(tx *sql.Tx, src int) error {
		stmt, err := tx.Prepare(`INSERT INTO observations(
			id, session_id, project, harness, event, tool_name, summary, summary_idx,
			summary_ref, summary_hash, payload_bytes, raw_ref, content_hash, created_at,
			meta_json, channel, event_uid, source_id, source_generation, source_seq,
			occurred_at_ns, archive_hash, raw_record_hash, parser_version)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		sourceID := fmt.Sprintf("src-%04d", src)
		for seq := 1; seq <= perSource; seq++ {
			// 每源第 100 与 300 号缺行：NextEvidenceBundle 会记账跳洞后继续出窗。
			if seq%300 == 100 {
				continue
			}
			n := src*perSource + seq
			at := base + int64(n%86400)
			if _, err := stmt.Exec(
				fmt.Sprintf("obs-%08d", n), fmt.Sprintf("sess-%04d", src%64),
				fmt.Sprintf("proj-%d", src%40), "workbuddy", "stop", "Bash", "", "",
				"", "", 0, "",
				fmt.Sprintf("ohash-%08d", n), at, "{}", "tool",
				fmt.Sprintf("uid-%08d", n), sourceID, 1, seq,
				int64(at)*1e9, fmt.Sprintf("arch-%s-%d", sourceID, seq/150),
				fmt.Sprintf("rhash-%08d", n), "v1"); err != nil {
				return err
			}
		}
		return nil
	})
}

// seedEvidence 造 evidence 账本全链：sources / archives / bundles（含退避、租约、终态）/
// items / gaps / distill_runs（部分带 bundle_id，喂 GC 引用保护）。
func seedEvidence(ctx context.Context, db *sql.DB, base int64) error {
	return txBatches(ctx, db, 1, func(tx *sql.Tx, _ int) error {
		srcStmt, err := tx.Prepare(`INSERT INTO evidence_sources(
			id, kind, locator, generation, status, last_seq, cursor_seq, source_tip, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer srcStmt.Close()
		perSource := nObservs / nSources
		for src := 0; src < nSources; src++ {
			id := fmt.Sprintf("src-%04d", src)
			if _, err := srcStmt.Exec(id, "hook", id+":hook", 1, "active",
				perSource, 0, fmt.Sprintf("tip-%04d", src), base, base); err != nil {
				return err
			}
		}

		archStmt, err := tx.Prepare(`INSERT INTO evidence_archives(
			archive_hash, source_id, generation, from_seq, to_seq, raw_count, byte_size, path, created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer archStmt.Close()
		for src := 0; src < nSources; src++ {
			for a := 0; a < 5; a++ {
				id := fmt.Sprintf("src-%04d", src)
				lo := a*150 + 1
				if _, err := archStmt.Exec(
					fmt.Sprintf("arch-%s-%d", id, a), id, 1, lo, lo+149,
					150, 2048, fmt.Sprintf("evidence/%s/%d.jsonl.gz", id, a), base+int64(a)); err != nil {
					return err
				}
			}
		}

		// 165 源 × 19 窗：ok/noop（老 updated_at，喂 GC）占七成，其余 open 态。
		// w 从 1 起：UNIQUE(source_id, generation, from_seq_exclusive) 必须把
		// from=0 留给 NextEvidenceBundle 的真实首窗（cursor 从 0 起窗），
		// 种子占位会让真实出窗撞唯一键后走到「既有 Bundle 不存在」的错误分支。
		bundleStmt, err := tx.Prepare(`INSERT INTO evidence_bundles(
			id, source_id, generation, from_seq_exclusive, to_seq_inclusive, evidence_hash,
			source_tip, status, attempt, not_before, lease_until, lease_owner, last_run_id,
			created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer bundleStmt.Close()
		itemStmt, err := tx.Prepare(`INSERT INTO evidence_bundle_items(
			bundle_id, ordinal, evidence_id, event_uid) VALUES (?,?,?,?)`)
		if err != nil {
			return err
		}
		defer itemStmt.Close()
		now := time.Now().Unix()
		for src := 0; src < nSources; src++ {
			id := fmt.Sprintf("src-%04d", src)
			for w := 1; w < 20; w++ {
				from := w * 30
				bid := fmt.Sprintf("bundle-%04d-%02d", src, w)
				status := "ok"
				notBefore := int64(0)
				leaseUntil := int64(0)
				leaseOwner := ""
				switch w % 10 {
				case 7: // pending：可调度
					status = "pending"
				case 8: // fail 带退避
					status = "fail"
					notBefore = now + int64(w)*60
				case 9: // leased 占着租约
					status = "leased"
					leaseUntil = now + 3600
					leaseOwner = "bench-runner"
				}
				at := base + int64(w)
				// ok/noop 的 updated_at 拉到 40 天前：超过 30 天保留期，GC 有真实工作量。
				updated := at
				if status == "ok" {
					updated = now - 40*24*3600
				}
				if _, err := bundleStmt.Exec(bid, id, 1, from, from+30,
					fmt.Sprintf("ehash-%s", bid), "", status, w%3, notBefore,
					leaseUntil, leaseOwner, "", at, updated); err != nil {
					return err
				}
				if status == "pending" {
					for k := 0; k < 5; k++ {
						if _, err := itemStmt.Exec(bid, k,
							fmt.Sprintf("obs-%08d", src*600+from+k+1),
							fmt.Sprintf("uid-%08d", src*600+from+k+1)); err != nil {
							return err
						}
					}
				}
			}
		}

		gapStmt, err := tx.Prepare(`INSERT INTO evidence_gaps(
			id, source_id, generation, from_seq, to_seq, detected_at) VALUES (?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer gapStmt.Close()
		for src := 0; src < nSources; src += 3 { // 每源两洞，隔 3 源登记一部分
			id := fmt.Sprintf("src-%04d", src)
			if _, err := gapStmt.Exec(fmt.Sprintf("gap-%s-100", id), id, 1, 100, 100, base); err != nil {
				return err
			}
			if _, err := gapStmt.Exec(fmt.Sprintf("gap-%s-300", id), id, 1, 300, 300, base); err != nil {
				return err
			}
		}

		runStmt, err := tx.Prepare(`INSERT INTO distill_runs(
			id, session_id, provider, status, cand_count, started_at, finished_at, bundle_id)
			VALUES (?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer runStmt.Close()
		for r := 0; r < nRuns; r++ {
			var bundleID string
			if r%2 == 0 { // 一半挂在完成 Bundle 上：GC 引用保护要逐条反查
				bundleID = fmt.Sprintf("bundle-%04d-%02d", (r*7)%nSources, r%19+1)
			}
			at := base + int64(r)
			if _, err := runStmt.Exec(fmt.Sprintf("run-%05d", r), fmt.Sprintf("sess-%04d", r%64),
				"none", "ok", r%7, at, at, bundleID); err != nil {
				return err
			}
		}
		return nil
	})
}

// seedCandidates 造 candidates（四态复核分布）+ commit_decisions（model-auto 归因链）
// + candidate_evidence / memory_evidence（GC 引用保护的反查面）。
func seedCandidates(ctx context.Context, db *sql.DB, base int64) error {
	return txBatches(ctx, db, nCands/memPerBatch, func(tx *sql.Tx, batch int) error {
		candStmt, err := tx.Prepare(`INSERT INTO candidates(
			id, session_id, project, kind, content, content_hash, status, source, channel,
			salience, obs_ids_json, created_at, decided_at, memory_id, run_id, scope,
			judge_verdict, judge_model, attributed_to)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer candStmt.Close()
		decStmt, err := tx.Prepare(`INSERT INTO commit_decisions(
			id, candidate_id, revision, evidence_hash, action, actor, reason_codes_json,
			memory_id, model_tag, created_memory, idempotency_key, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer decStmt.Close()
		ceStmt, err := tx.Prepare(`INSERT INTO candidate_evidence(
			candidate_id, evidence_id, event_uid, source_id, source_seq, evidence_role, created_at)
			VALUES (?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ceStmt.Close()
		meStmt, err := tx.Prepare(`INSERT INTO memory_evidence(
			memory_id, evidence_id, event_uid, grade, seq, occurred_at_ns, created_at)
			VALUES (?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer meStmt.Close()

		for i := 0; i < memPerBatch; i++ {
			n := batch*memPerBatch + i
			cid := fmt.Sprintf("cand-%08d", n)
			at := base + int64(n%86400)
			// 复核四态分布：pending(未判定)/accepted/rejected/failed/quarantined。
			status := "accepted"
			verdict := ""
			switch n % 20 {
			case 0, 1, 2, 3:
				status, verdict = "pending", ""
			case 4, 5:
				status = "rejected"
			case 6:
				status = "failed"
			case 7:
				status = "quarantined"
			}
			memoryID := ""
			if status == "accepted" {
				memoryID = fmt.Sprintf("mem-%08d", n)
			}
			if _, err := candStmt.Exec(cid, fmt.Sprintf("sess-%04d", n%64),
				fmt.Sprintf("proj-%d", n%40), "fact",
				fmt.Sprintf("候选事实 %d", n), fmt.Sprintf("chash-%08d", n),
				status, "distill", "agent", 0.5, "[]", at, at, memoryID,
				fmt.Sprintf("run-%05d", n%nRuns), "project", verdict,
				"ollama:qwen2.5@judge-v1", "assistant"); err != nil {
				return err
			}
			// 每 4 条候选挂一条证据引用：GC guard 的反查有真实行数。
			if n%4 == 0 {
				if _, err := ceStmt.Exec(cid, fmt.Sprintf("obs-%08d", n%99000),
					fmt.Sprintf("uid-%08d", n%99000), fmt.Sprintf("src-%04d", n%nSources),
					(n%590)+1, "direct", at); err != nil {
					return err
				}
			}
			if n%4 == 1 && memoryID != "" {
				if _, err := meStmt.Exec(memoryID, fmt.Sprintf("obs-%08d", n%99000),
					fmt.Sprintf("uid-%08d", n%99000), "B", (n%590)+1, int64(at)*1e9, at); err != nil {
					return err
				}
			}
			// 裁决账：accepted 走 model-auto（复核摘要的归因链），其余 human。
			actor, action, tag := "human", "reject", ""
			createdMemory := 0
			switch status {
			case "pending":
				action = "defer"
			case "failed":
				action = "defer"
			case "quarantined":
				action = "defer"
			case "accepted":
				actor, action = "model-auto", "auto_commit"
				tag = "ollama:qwen2.5@prompt-v1"
				createdMemory = 1
			}
			if _, err := decStmt.Exec(fmt.Sprintf("dec-%08d", n), cid, 0, "",
				action, actor, "[]", memoryID, tag, createdMemory,
				fmt.Sprintf("idem-%08d", n), at); err != nil {
				return err
			}
		}
		return nil
	})
}

// buildAuditLog 造当前档 + .1–.3 共 ~40MB 的审计日志（场景 7，纯文件无库）。
func buildAuditLog(path string) {
	line := func(seq int) []byte {
		r := audit.Record{
			TS: time.Now().Unix() - int64(seq), Event: "prompt-submit",
			Harness: "workbuddy", Project: "proj-1",
			SessionID: fmt.Sprintf("sess-%04d", seq%64),
			Query:     fmt.Sprintf("帮我看看 %s 服务 %s 的原因，日志在哪，怎么重试", hanWords[seq%20], hanWords[(seq*7)%20]),
			Hashes:    []string{fmt.Sprintf("hash-%08d", seq%10000), fmt.Sprintf("hash-%08d", (seq*3)%10000)},
			Kinds:     []string{"fact"}, MS: 42, Count: 2, Rows: 0, Batch: false,
			OpenMS: 12, SearchMS: 3, ClaimMS: 1, EvidenceMS: 8, MaintMS: 5,
		}
		b, _ := json.Marshal(r)
		return append(b, '\n')
	}
	// .3（最老）→ .1：每档 ~10MB；当前档 ~10MB。
	for i := 3; i >= 1; i-- {
		writeAuditFile(fmt.Sprintf("%s.%d", path, i), 100000, func(seq int) []byte { return line(seq + i*100000) })
	}
	writeAuditFile(path, 100000, func(seq int) []byte { return line(seq + 400000) })
}

func writeAuditFile(path string, n int, line func(int) []byte) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 0, 1<<20)
	for i := 0; i < n; i++ {
		buf = append(buf, line(i)...)
		if len(buf) >= 1<<20 {
			f.Write(buf)
			buf = buf[:0]
		}
	}
	f.Write(buf)
}
