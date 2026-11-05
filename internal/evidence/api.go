// Package evidence 定义原始证据的统一契约。
//
// 本文件是第二波并行的冻结接口：只有类型、接口和常量，不含函数体。
package evidence

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
