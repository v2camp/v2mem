package eval

import "strings"

// legacy 分桶：生产历史候选全部 bundle_id 为空，单轨后的新候选必有 bundle_id。
// 判定只认这个结构性字段，不引用硬编码日期（docs/design-eval-flows.md §3.1）。

// IsLegacyCandidate 判断候选是否为单轨前的 legacy 数据。
func IsLegacyCandidate(c FlowCandidate) bool { return strings.TrimSpace(c.BundleID) == "" }

// IsLegacyRun 判断运行账是否为单轨前的 legacy 数据。
func IsLegacyRun(r FlowRun) bool { return strings.TrimSpace(r.BundleID) == "" }
