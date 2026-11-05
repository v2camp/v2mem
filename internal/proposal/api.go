// Package proposal 定义语义提炼的共享契约。
//
// 本文件是第二波并行的冻结接口：只有类型、接口和常量，不含函数体。
package proposal

// Draft 是执行器返回的一条 ADD-only 草案。
// EvidenceIdx 使用 Bundle 内的 1 起短序号，禁止让模型回填 UUID。
type Draft struct {
	Kind        string
	Scope       string
	Content     string
	Rationale   string
	SubjectKey  string
	TaskState   string
	EvidenceIdx []int
}

// Candidate 是经校验、准备进入隔离区的候选。
type Candidate struct {
	ID          string
	SessionID   string
	Project     string
	BundleID    string
	RunID       string
	ProposalIdx int
	Kind        string
	Scope       string
	Status      string
	ReviewClass string
	Revision    int
	Content     string
	ContentHash string
	SubjectKey  string
	TaskState   string
	Rationale   string
	Salience    float64
	EvidenceIDs []string
}

// Result 是一次 Proposal 运行的账目。
type Result struct {
	RunID     string
	Executor  string
	Status    string
	Generated int
	Valid     int
	Invalid   int
	Duplicate int
	Drafts    []Draft
}

// Executor 是 Host、Local 或 BYOK 的统一入口。
type Executor interface {
	Run(bundleID string) (Result, error)
}
