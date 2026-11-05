package commit

import (
	"fmt"
	"strings"
)

// 本文件实现注入侧使用的紧凑 provenance。
//
// 为什么必须短：provenance 会随每条记忆进入 hook 注入的上下文预算。完整证据链
// 是给人排障用的，注入只需要回答「这条有证据吗、多强、最早一条在哪」。
// 上限 40 rune 是硬约束，超出按 rune 截断（不是 byte，避免把汉字切坏）。

// MaxProvenanceRunes 是 CompactProvenance 的输出上限。
const MaxProvenanceRunes = 40

// Compact 是 ProvenanceFormatter 的默认实现。零值可用。
type Compact struct{}

// CompactProvenance 把一组证据压成一行。
//
// 输出形态（示例）：
//
//	证据1条·A级·a1b2c3d4
//	证据3条·B级·a1b2c3d4+2
//
// 计数按「事件身份」去重：同一 event_uid 的多条记忆证据只算一条，
// 否则重放补边会让条数虚高，读侧会高估证据强度。
func (Compact) CompactProvenance(links []Link) string {
	if len(links) == 0 {
		return ""
	}
	sorted := make([]Link, len(links))
	copy(sorted, links)
	SortLinks(sorted)

	seen := make(map[string]bool, len(sorted))
	var first string
	strongest := GradeC
	for i, l := range sorted {
		key := evidenceKey(l)
		if key == "" {
			// 没有任何身份的条目无法互相去重，按位置各算一条，宁可多报不可少报。
			key = fmt.Sprintf("idx:%d", i)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if first == "" {
			first = shortEvidenceToken(l)
		}
		if gradeRank(l.Grade) > gradeRank(strongest) {
			strongest = l.Grade
		}
	}
	n := len(seen)
	if n == 0 {
		return ""
	}
	if strongest != GradeA && strongest != GradeB {
		strongest = GradeC
	}

	var b strings.Builder
	fmt.Fprintf(&b, "证据%d条·%s级", n, strongest)
	if first != "" {
		b.WriteString("·")
		b.WriteString(first)
		if n > 1 {
			fmt.Fprintf(&b, "+%d", n-1)
		}
	}
	return truncateRunes(b.String(), MaxProvenanceRunes)
}

func gradeRank(g Grade) int {
	switch g {
	case GradeA:
		return 3
	case GradeB:
		return 2
	case GradeC:
		return 1
	}
	return 0
}

// evidenceKey 是去重身份。event_uid 是 canonical 身份，其次用归档哈希，
// 最后退回证据 id；三者都空时返回空串（调用方仍会把这条计入，不能静默丢）。
func evidenceKey(l Link) string {
	if s := strings.TrimSpace(l.EventUID); s != "" {
		return "e:" + s
	}
	if s := strings.TrimSpace(l.ArchiveHash); s != "" {
		return "a:" + s
	}
	if s := strings.TrimSpace(l.RawLocator); s != "" {
		return "r:" + s
	}
	return ""
}

// shortEvidenceToken 取一个可读短标识：优先 event_uid 的前 8 个有效字符。
func shortEvidenceToken(l Link) string {
	for _, s := range []string{l.EventUID, l.ArchiveHash, l.RawLocator} {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// 去掉常见前缀，避免 token 全被 "evt_" / "ev_" 占满。
		s = strings.TrimPrefix(s, "evt_")
		s = strings.TrimPrefix(s, "ev_")
		if s == "" {
			continue
		}
		return truncateRunes(s, 8)
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
