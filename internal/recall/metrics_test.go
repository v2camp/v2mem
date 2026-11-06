package recall

import (
	"fmt"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/store"
)

// T4 验收指标实测（计划 §5）：供给率 ≥95%、注入 precision 首版 ≥85%、
// 无合格项弃答 100%。p95 与 Recall@5 分别由 cmd/mem/measure_test.go 和
// make eval-gate 把关，不在此重复。
//
// 这是一份**固定的合成基准**，不是真实语料抽样：gold 用 ID 前缀标注
// （rel- 相关 / irr- 干扰），干扰项故意带单路 precise 使其同样「合格」，
// 从而能真的被注入并拉低 precision —— 避免构造出一个恒为 100% 的自证测试。
// 口径、数据、阈值、断言全在本文件内，任何人都能 `go test -run TestT4AcceptanceMetrics -v` 复现。
const (
	minSupplyRate  = 0.95
	minPrecision   = 0.85
	minAbstainRate = 1.00
)

type metricsCase struct {
	query        string
	rel          int  // 相关条目：两路结构支持（score 3.0）
	irr          int  // 干扰条目：单路 precise（score 1.0），仍属合格池
	noSignal     int  // 无结构支持条目（score 0），应被质量筛选丢弃
	expectSupply bool // 池中存在应被供给的相关条目
}

// metricsSupport: 2=强结构支持, 1=弱结构支持但合格, 0=无支持。
func metricsHit(caseNo, seq int, prefix string, metricsSupport int) store.Hit {
	id := fmt.Sprintf("%s-c%02d-%02d", prefix, caseNo, seq)
	h := store.Hit{
		ID: id,
		// 正文刻意不含 query 里的技术标识符，让 gold 完全由召回信号决定，
		// 而不是靠「标识符全命中」白送 3.0 分。
		Content: fmt.Sprintf("候选正文 第%02d组 第%02d条 %s", caseNo, seq, prefix),
		Kind:    "fact", Project: "demo", Rank: float64(seq),
	}
	switch metricsSupport {
	case 2:
		h.Signals = &store.HitSignals{Precise: 0.8, Broad: 0.4}
		h.Score = 3.0
	case 1:
		h.Signals = &store.HitSignals{Precise: 0.2}
		h.Score = 1.0
	default:
		h.Signals = nil
		h.Score = 0
	}
	return h
}

func metricsCases() []metricsCase {
	return []metricsCase{
		// 12 个「有相关条目」的查询：前 10 个相关项充足，后 2 个相关项偏少，
		// 会从合格池补位进干扰项，用来真实压低 precision。
		{"FTS5 索引怎么建", 3, 2, 1, true},
		{"read_traces 表字段", 3, 2, 0, true},
		{"pnpm test 失败排查", 3, 1, 1, true},
		{"BEGIN IMMEDIATE 锁超时", 2, 2, 0, true},
		{"evidence_bundles 游标", 3, 2, 0, true},
		{"gzip segment 归档", 2, 1, 1, true},
		{"provenance 紧凑串", 3, 2, 0, true},
		{"coverage-policy 门禁", 3, 1, 0, true},
		{"spool drain 认领", 2, 2, 1, true},
		{"HitSignals 融合分", 3, 2, 0, true},
		{"ux_obs_event_uid 唯一索引", 2, 2, 0, true},
		{"markLegacyOnce 幂等", 2, 2, 0, true},
		// 4 个「无合格项」查询：只有无支持命中 / 空池 / 客套 gate，必须全部弃答。
		{"FTS5 索引怎么建", 0, 0, 3, false},
		{"read_traces 表字段", 0, 0, 1, false},
		{"pnpm test 失败排查", 0, 0, 0, false},
		{"你好", 0, 0, 2, false},
	}
}

func TestT4AcceptanceMetrics(t *testing.T) {
	cases := metricsCases()
	var (
		supplyTotal, supplyOK       int
		abstainTotal, abstainOK     int
		injectedTotal, injectedGood int
	)
	for i, c := range cases {
		var hits []store.Hit
		seq := 0
		for n := 0; n < c.rel; n++ {
			seq++
			hits = append(hits, metricsHit(i, seq, "rel-", 2))
		}
		for n := 0; n < c.irr; n++ {
			seq++
			hits = append(hits, metricsHit(i, seq, "irr-", 1))
		}
		for n := 0; n < c.noSignal; n++ {
			seq++
			hits = append(hits, metricsHit(i, seq, "irr-", 0))
		}
		f := &fakeStore{hits: hits}
		res, err := Query(f, QueryOptions{Query: c.query, Project: "demo", Limit: 3})
		if err != nil {
			t.Fatalf("case %d %q: Query: %v", i, c.query, err)
		}
		if c.expectSupply && !res.Gate.Run {
			t.Fatalf("case %d %q 被 query gate 拦下，基准构造失效: %+v", i, c.query, res.Gate)
		}

		good := 0
		for _, in := range res.Injected {
			injectedTotal++
			if strings.HasPrefix(in.Hit.ID, "rel-") {
				injectedGood++
				good++
			}
		}
		if c.expectSupply {
			supplyTotal++
			if res.Outcome == OutcomeInjected && good > 0 {
				supplyOK++
			} else {
				t.Errorf("case %d %q 未供给相关记忆: outcome=%s injected=%d good=%d",
					i, c.query, res.Outcome, len(res.Injected), good)
			}
		} else {
			abstainTotal++
			if len(res.Injected) == 0 {
				abstainOK++
			} else {
				t.Errorf("case %d %q 无合格项却注入了 %d 条: outcome=%s",
					i, c.query, len(res.Injected), res.Outcome)
			}
		}
	}

	supplyRate := float64(supplyOK) / float64(supplyTotal)
	precision := float64(injectedGood) / float64(injectedTotal)
	abstainRate := float64(abstainOK) / float64(abstainTotal)
	t.Logf("T4 验收实测: 供给率 %.1f%% (%d/%d, 门槛 ≥%.0f%%), 注入 precision %.1f%% (%d/%d, 门槛 ≥%.0f%%), 无合格项弃答 %.1f%% (%d/%d, 门槛 ≥%.0f%%)",
		supplyRate*100, supplyOK, supplyTotal, minSupplyRate*100,
		precision*100, injectedGood, injectedTotal, minPrecision*100,
		abstainRate*100, abstainOK, abstainTotal, minAbstainRate*100)

	if supplyRate < minSupplyRate {
		t.Errorf("供给率 %.1f%% 低于 %.0f%%", supplyRate*100, minSupplyRate*100)
	}
	if precision < minPrecision {
		t.Errorf("注入 precision %.1f%% 低于 %.0f%%", precision*100, minPrecision*100)
	}
	if abstainRate < minAbstainRate {
		t.Errorf("无合格项弃答率 %.1f%% 低于 %.0f%%", abstainRate*100, minAbstainRate*100)
	}
}
