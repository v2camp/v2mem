// Package commit 定义提交治理与证据链契约。
//
// 本文件是第二波并行的冻结接口：只有类型、接口和常量，不含函数体。
package commit

import "github.com/v2camp/v2mem/internal/proposal"

// Grade 是证据等级。C 级不得单独支持自动提交。
type Grade string

const (
	GradeA Grade = "A"
	GradeB Grade = "B"
	GradeC Grade = "C"
)

// Action 是提交决策的动作。
type Action string

const (
	ActionAutoCommit  Action = "auto_commit"
	ActionHumanAccept Action = "human_accept"
	ActionReject      Action = "reject"
	ActionDuplicate   Action = "duplicate"
	ActionConflict    Action = "conflict"
	ActionBacklog     Action = "backlog"
	ActionDefer       Action = "defer"
)

// Link 是一条记忆到原始证据的关联。
type Link struct {
	MemoryID     string
	EventUID     string
	ArchiveHash  string
	RawLocator   string
	Grade        Grade
	Seq          int
	OccurredAtNS int64
}

// Decision 是一次提交裁决。
type Decision struct {
	CandidateID    string
	Revision       int
	EvidenceHash   string
	Action         Action
	Actor          string
	ReasonCodes    []string
	MemoryID       string
	IdempotencyKey string
	CreatedAt      int64
}

// LinkReader 提供单条与批量的证据链读取。
// 批量接口供读侧一次取完 provenance，禁止逐条查询拖慢 hook。
type LinkReader interface {
	EvidenceLinks(memoryID string) ([]Link, error)
	EvidenceLinksBatch(memoryIDs []string) (map[string][]Link, error)
}

// ProvenanceFormatter 把一组证据压成适合注入的紧凑 provenance。
type ProvenanceFormatter interface {
	CompactProvenance(links []Link) string
}

// Policy 对候选做确定性裁决。
type Policy interface {
	Decide(candidate proposal.Candidate, evidence []Link) (Decision, error)
}
