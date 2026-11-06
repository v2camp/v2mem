// Package evidence 定义原始证据的统一契约。
//
// 本文件是第二波并行的冻结接口：只有类型、接口和常量，不含函数体。
package evidence

// Fidelity 是捕获层的内容保真度分级（design-capture 草案 §4.3）。
//
// 语义边界（W0 冻结，全波不可破）：
//   - 只控提案准入与蒸馏输入投影，不参与 A/B/C 证据等级判定；
//   - 不写进归档 segment —— 归档层保持字节级幂等；
//   - 空值表示「未声明」，由 Engine 按 Raw 是否有内容兜底。
type Fidelity string

const (
	FidelityFull    Fidelity = "full"
	FidelitySummary Fidelity = "summary"
	FidelityPointer Fidelity = "pointer"
	FidelityNone    Fidelity = "none"
)

// ProposalPayloadRunes 是提案 / 蒸馏输入的有界投影上限。
// 事实源（归档 Raw）仍按 32000 rune 保真；本上限只作用于查询源。
const ProposalPayloadRunes = 400

// Ref 是一条已归档证据的最小引用。
type Ref struct {
	EventUID         string
	SourceID         string
	Generation       int
	Seq              int
	OccurredAtNS     int64
	SessionID        string
	Project          string
	Harness          string
	EventType        string
	ArchiveHash      string
	RawRecordHash    string
	RawLocator       string
	Summary          string
	ParserVersion    string
	ProposalEligible bool
	Fidelity         Fidelity
}

// RawEvent 是待归档的一条原始事件。Raw 保留原始字节，不能只保存摘要。
type RawEvent struct {
	EventUID         string
	SourceID         string
	Generation       int
	Seq              int
	OccurredAtNS     int64
	SessionID        string
	Project          string
	Harness          string
	EventType        string
	ParserVersion    string
	ProposalEligible bool
	Fidelity         Fidelity
	Raw              []byte
}

// Bundle 是一段连续、可重放的证据窗口。
type Bundle struct {
	ID               string
	SourceID         string
	Generation       int
	FromSeqExclusive int
	ToSeqInclusive   int
	EvidenceIDs      []string
	EvidenceHash     string
	SourceTip        string
	Status           string
	Attempt          int
}

// Writer 负责把原始事件归档并建立可检索身份。
type Writer interface {
	Capture(RawEvent) (Ref, error)
}

// Reader 负责按连续游标取 Bundle 与其中的证据引用。
type Reader interface {
	NextBundle(limit int, maxBytes int64) (*Bundle, error)
	ListEvidence(bundleID string) ([]Ref, error)
}
