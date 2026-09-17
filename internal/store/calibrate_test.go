//go:build calib

package store

// 提取方法校准实验（手动跑：go test -tags calib -run Calib -v）。
//
// 目标：给「mem lexicon init --scan」的参数找证据驱动的默认值——
//   - 同义判定阈值（命中集重叠/Jaccard）
//   - top-k（回填近邻集大小）
//   - 高频泛词（跨概念词）的过滤方式
//
// 用既有检索当「行为 oracle」：同一概念的不同词面，检索后应命中同一批记忆。
// 构建含已知同义簇 + 干扰词 + 高频泛词的合成语料，度量 (阈值, k) → P/R/F1。

import (
	"fmt"
	"strings"
	"testing"
)

// 簇定义：簇名 → 该簇的同义词集合。
var calibClusters = map[string][]string{
	"ingest":  {"搬进", "导入", "灌库", "摄入", "载入"},
	"search":  {"查找", "查询", "搜索", "捞取", "检索"},
	"sync":    {"归并", "合并", "merge", "冲突解决", "同步"},
}

// 每个簇一条「术语清单式」标准记忆（specs/plans/glossary 的形态）。
var calibGlossary = []string{
	"ingest 入库动作 动词：我们常用的就是 搬进 导入 灌库 摄入 载入 这几种",
	"search 读取动作 动词：从记忆库取回内容用 查找 查询 搜索 捞取 检索 都行",
	"sync 同步合并 动词：跨设备同步靠 归并 合并 merge 冲突解决 来收敛",
}

// 干扰记忆：不含任何簇同义词，制造检索噪声。
var calibDistractors = []string{
	"备份策略 gc 前先用 VACUUM INTO 备份数据库到指定目录",
	"审计日志会按 5MB 轮转保留三份归档文件",
	"MCP 服务器通过 stdio 协议暴露 search add 工具",
	"任务在独立 git worktree 上开发然后合入主分支",
	"覆盖率门禁用 coverage-policy 文件做单一真理源",
	"安装支持 curl 管道 bash 一键脚本预编译二进制",
	"记忆条目包含 source 字段标记写入来源",
	"导出导入用于跨设备迁移记忆内容",
	"衰减机制靠 access_count 与 last_seen 淘汰久未命中的记忆",
	"token 消耗是评估驱动设计的主要约束之一",
	"硬件规则小字条注入 hook 会话开头的上下文",
	"gold 金标准文件用于评测检索质量",
	"向量检索的成本模型要权衡模型文件体积",
	"stopwords 用于过滤查询里的无关虚词",
	"跨工程 scope 决定检索是否忽略工程过滤",
}

// 候选词：全部簇同义词 + 若干干扰词 + 高频泛词（跨概念、出现在很多记忆里）。
func calibCandidates() []string {
	var c []string
	for _, syns := range calibClusters {
		c = append(c, syns...)
	}
	c = append(c, "备份", "轮转", "门禁", "安装", "权重", "作用域",
		// 高频泛词：预期造成假同义，用来测过滤
		"用于", "文件", "机制", "记忆", "数据库", "词", "相关", "内容")
	return c
}

// labelOf 返回词对应的簇名；干扰词返回空。
func calibLabelOf(w string) string {
	for name, syns := range calibClusters {
		for _, s := range syns {
			if s == w {
				return name
			}
		}
	}
	return ""
}

func calibSetUp(t *testing.T) (*Store, map[string][]string) {
	t.Helper()
	s := newTestStore(t)
	for _, g := range calibGlossary {
		mustAdd(t, s, g)
	}
	for _, d := range calibDistractors {
		mustAdd(t, s, d)
	}
	// 预计算每个候选词的 top-k 命中 id 集合（k 可选）
	sets := map[string][]string{}
	for _, w := range calibCandidates() {
		sets[w] = nil
	}
	return s, sets
}

func calibHitIDs(t *testing.T, s *Store, w string, k int) []string {
	t.Helper()
	h, err := s.Search(SearchQuery{Query: w, Limit: k})
	if err != nil {
		t.Fatalf("search %q: %v", w, err)
	}
	ids := make([]string, 0, len(h))
	for _, x := range h {
		ids = append(ids, x.ID)
	}
	return ids
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	aset := map[string]bool{}
	for _, x := range a {
		aset[x] = true
	}
	inter, union := 0, len(aset)
	seen := map[string]bool{}
	for _, y := range b {
		if aset[y] {
			inter++
			delete(aset, y) // 去重交集
		}
		if !seen[y] {
			union++
			seen[y] = true
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// containment = |A∩B| / min(|A|,|B|)：对大小不对称的命中集更宽容。
func containment(a, b []string) float64 {
	aset := map[string]bool{}
	for _, x := range a {
		aset[x] = true
	}
	inter := 0
	for _, y := range b {
		if aset[y] {
			inter++
		}
	}
	m := len(a)
	if len(b) < m {
		m = len(b)
	}
	if m == 0 {
		return 0
	}
	return float64(inter) / float64(m)
}

// TestCalibThresholdSweep 扫描同义判定阈值与 top-k，输出 P/R/F1。
func TestCalibThresholdSweep(t *testing.T) {
	s, _ := calibSetUp(t)
	cands := calibCandidates()

	// 候选词全解析一次，避免重复 Search。
	sets := map[string][]string{}
	suite := func(k int) {
		for _, w := range cands {
			sets[w] = calibHitIDs(t, s, w, k)
		}
	}

	metrics := map[string]func(a, b []string) float64{
		"jaccard":      jaccard,
		"containment":  containment,
	}
	for name, metric := range metrics {
		for _, k := range []int{3, 5, 8} {
			suite(k)
			fmt.Printf("\n[ metric=%s k=%d ]\n%s\n", name, k, strings.Repeat("-", 46))
			fmt.Printf("%-10s %8s %8s %8s\n", "th", "P", "R", "F1")
			for _, th := range []float64{0.0, 0.3, 0.5, 0.7, 0.8, 0.9} {
				tp, fp, fn := 0, 0, 0
				// 已判定同义的对
				pred := map[string]bool{}
				for i := 0; i < len(cands); i++ {
					for j := i + 1; j < len(cands); j++ {
						a, b := cands[i], cands[j]
						sim := metric(sets[a], sets[b])
						same := calibLabelOf(a) != "" && calibLabelOf(a) == calibLabelOf(b)
						li := a + "\x00" + b
						if sim >= th {
							pred[li] = true
							if same {
								tp++
							} else {
								fp++
							}
						} else if same {
							fn++
						}
					}
				}
				_ = pred
				P := float64(tp) / float64(tp+fp)
				R := float64(tp) / float64(tp+fn)
				var F1 float64
				if P+R > 0 {
					F1 = 2 * P * R / (P + R)
				}
				fmt.Printf("%-10.1f %7.1f%% %7.1f%% %7.1f%%  (tp=%d fp=%d fn=%d)\n",
					th, P*100, R*100, F1*100, tp, fp, fn)
			}
		}
	}

	calibHighFreqReport(t, s, sets, cands)
}

// calibHighFreqReport：量化「高频泛词」造成的假同义，及命中集广度过滤能否去掉。
func calibHighFreqReport(t *testing.T, s *Store, sets map[string][]string, cands []string) {
	fmt.Printf("\n=== 高频泛词 / 命中集广度 ===\n")
	// 按 top-k=8 的命中集大小排序展示候选
	for _, w := range cands {
		if len(sets[w]) > 0 {
			fmt.Printf("  %-6s top-8 命中 %d 条  [簇=%s]\n", w, len(sets[w]), calibLabelOf(w))
		}
	}
}