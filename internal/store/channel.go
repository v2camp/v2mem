package store

import (
	"fmt"
	"strings"
)

// 写时通道。与 source 并存：source 是写入链路，channel 是谁产生了这条内容。
const (
	ChannelUser   = "user"
	ChannelAgent  = "agent"
	ChannelTool   = "tool"
	ChannelImport = "import"
)

// NormalizeChannel 接受空串或四个枚举。别的值拒绝。
func NormalizeChannel(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", ChannelUser, ChannelAgent, ChannelTool, ChannelImport:
		return s, nil
	default:
		return "", fmt.Errorf("channel 必须是 user|agent|tool|import，got %q", s)
	}
}

// DefaultChannel 在调用方没写 channel 时按 source 取默认。
//
//	llm / distill / distill-accept / harness-summary → agent
//	ingest / import / import-proposal → import
//	其余（含 human、bench、test、空）→ user
func DefaultChannel(explicit, source string) (string, error) {
	ch, err := NormalizeChannel(explicit)
	if err != nil {
		return "", err
	}
	if ch != "" {
		return ch, nil
	}
	switch source {
	case "llm", "distill", "distill-accept", "harness-summary":
		return ChannelAgent, nil
	case "ingest", "import", "import-proposal":
		return ChannelImport, nil
	default:
		return ChannelUser, nil
	}
}

// DefaultObsChannel 是观测的默认通道。
//
//	post-tool-use → tool
//	其余（stop、session-end，以及未识别事件）→ agent
//
// import-transcript 应显式传 import，不走这个默认。
func DefaultObsChannel(event, explicit string) (string, error) {
	ch, err := NormalizeChannel(explicit)
	if err != nil {
		return "", err
	}
	if ch != "" {
		return ch, nil
	}
	if strings.TrimSpace(event) == "post-tool-use" {
		return ChannelTool, nil
	}
	return ChannelAgent, nil
}
