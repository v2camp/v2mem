// Package scrub 是写路径共用的密钥打码。
//
// 观测摘要、候选正文、mem add、审计 query 都走 Apply。
// 检索用的查询原文不打码，避免把「能搜到」和「能留痕」绑死。
// off=true 只给本地调试（CLI --no-scrub）；MCP 不提供这个开关。
package scrub

import "regexp"

// patterns 的顺序有意义：先拿掉 PEM 块，再按赋值形态打码，最后处理 URL 凭据。
// 只打「看起来像密钥」的形态。单独出现 token / password 这类词不会被替换。
var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[redacted]"},
	{regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|authorization|passwd)\s*[:=]\s*\S+`), "[redacted]"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}\b`), "[redacted]"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[redacted]"},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}`), "[redacted]"},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/\s:@]+:[^/\s@]+)@`), "${1}[redacted]@"},
}

// Text 按统一规则打码。已打码的文本再跑一遍是幂等的。
func Text(s string) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// Apply 在 off 时原样返回。off 只用于本地调试。
func Apply(s string, off bool) string {
	if off || s == "" {
		return s
	}
	return Text(s)
}
