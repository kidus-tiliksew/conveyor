package nextaction

import (
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

const (
	ActorNone       = "none"
	ActorOperator   = "operator"
	ActorFactory    = "factory"
	ActorAgent      = "agent"
	ActorDependency = "dependency"
)

const (
	KindTerminal                = "terminal"
	KindParked                  = "parked"
	KindDispatchExhausted       = "dispatch_exhausted"
	KindPlanApproval            = "plan_approval"
	KindPlanRevision            = "plan_revision"
	KindMergeApproval           = "merge_approval"
	KindReviewBounceLimit       = "review_bounce_limit"
	KindJobFailure              = "job_failure"
	KindMergeConflict           = "merge_conflict"
	KindMergeExecution          = "merge_execution"
	KindMerging                 = "merging"
	KindDocumentProposal        = "document_proposal"
	KindAgentQuestion           = "agent_question"
	KindVerificationDisposition = "verification_disposition"
	KindForgeFailure            = "forge_failure"
	KindReviewRecovery          = "review_recovery"
	KindDependencyUnsatisfiable = "dependency_unsatisfiable"
	KindStalled                 = "stalled"
	KindBlocked                 = "blocked"
	KindWorking                 = "working"
	KindQueued                  = "queued"
	KindRetrying                = "retrying"
)

const (
	StallRetrySuppressed            = "retry_suppressed"
	StallStale                      = "stale"
	StallTimedOut                   = "timed_out"
	StallUnclaimable                = "unclaimable"
	StallDispatchFailing            = "dispatch_failing"
	StallAutomaticRecoveryExhausted = "automatic_recovery_exhausted"
)

const (
	ReleaseCheckpoint = "checkpoint"
	ReleaseExpired    = "expired"
	ReleaseReleased   = "released"
)

const ConflictFixReason = "merge-conflict"

// Input is everything Project needs. The store assembler fills it; Project
// does not read the store.
type Input struct {
	Task            core.Task
	Orders          []core.WorkOrder
	LatestSpec      *SpecRef
	GateSpecVersion int
	Markers         Markers
	ClaimWaiting    []ProposalRef
	Blockers        []BlockerRef
	MergeConflict   MergeConflict
	MergeReadiness  *MergeReadiness
	Recommendation  string
	Assignee        *core.TaskAssignee
	Capabilities    map[core.Capability]bool
	LiveClaimants   *ClaimantView
	Recovery        map[string]AutomaticRecovery
	Now             time.Time
	PullRequest     int
}

// Markers are the activity facts the assembler already folded.
type Markers struct {
	ForgeFailure             bool
	ReviewRecovery           bool
	InterruptedReview        bool
	UserChangesRequested     bool
	ParkCommand              string
	VerificationHoldDeadline *time.Time
	VerificationDisposition  bool
	CheckpointDecision       bool
	Checkpoint               string
	ReleaseOutcome           string
}

// MergeConflict is the open conflict episode, already folded from events.
type MergeConflict struct {
	Open        bool
	Exhausted   bool
	NextRetryAt time.Time
}

// AutomaticRecovery is the AR-6 hook. An empty map means the ledger is absent.
type AutomaticRecovery struct {
	NextAt    *time.Time
	Exhausted bool
}

type SpecRef struct {
	Version int
}

type ProposalRef struct {
	Tier    string `json:"tier"`
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type BlockerRef struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

type MergeReadiness struct {
	State   string `json:"state,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
	Number  int    `json:"number,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ClaimantView is nil when workers are not configured. A non-nil empty set
// means no harness is live.
type ClaimantView struct {
	UserIDs []string
}

type NextAction struct {
	Label        string       `json:"label"`
	Actor        string       `json:"actor"`
	Kind         string       `json:"kind"`
	Judgment     bool         `json:"judgment"`
	Summary      string       `json:"summary"`
	Question     string       `json:"question,omitempty"`
	Choices      []Choice     `json:"choices"`
	IfIgnored    string       `json:"if_ignored,omitempty"`
	Deadline     *time.Time   `json:"deadline"`
	BlockedBy    []BlockerRef `json:"blocked_by,omitempty"`
	CurrentOrder *OrderView   `json:"current_order,omitempty"`
	Attempt      *AttemptView `json:"attempt,omitempty"`
	Details      Details      `json:"details"`
}

type Choice struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Effect     string `json:"effect"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Capability string `json:"capability"`
	Allowed    bool   `json:"allowed"`
}

type OrderView struct {
	ID                string     `json:"id"`
	Stage             string     `json:"stage"`
	State             string     `json:"state"`
	QueueDeadline     *time.Time `json:"queue_deadline"`
	ExecutionDeadline *time.Time `json:"execution_deadline"`
	LeaseExpiresAt    *time.Time `json:"lease_expires_at"`
	RecoveryRefusal   string     `json:"recovery_refusal,omitempty"`
}

type AttemptView struct {
	Harness        string     `json:"harness,omitempty"`
	Model          string     `json:"model,omitempty"`
	User           string     `json:"user,omitempty"`
	ClaimedBy      string     `json:"claimed_by,omitempty"`
	SessionID      string     `json:"session_id,omitempty"`
	AttemptID      string     `json:"attempt_id,omitempty"`
	ClaimedAt      *time.Time `json:"claimed_at"`
	LastCheckIn    *time.Time `json:"last_check_in"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at"`
	Progress       string     `json:"progress,omitempty"`
	ElapsedSeconds int64      `json:"elapsed_seconds"`
}

type Secondary struct {
	Kind       string `json:"kind"`
	ReasonCode string `json:"reason_code,omitempty"`
}

type Refusal struct {
	Choice string `json:"choice"`
	Text   string `json:"text"`
}

type Details struct {
	GateKind         string        `json:"gate_kind,omitempty"`
	ReasonCode       string        `json:"reason_code,omitempty"`
	StallReason      string        `json:"stall_reason,omitempty"`
	ReleaseOutcome   string        `json:"release_outcome,omitempty"`
	OrderIDs         []string      `json:"order_ids,omitempty"`
	Checkpoint       string        `json:"checkpoint,omitempty"`
	PendingProposals []ProposalRef `json:"pending_proposals,omitempty"`
	Secondary        []Secondary   `json:"secondary,omitempty"`
	Refusals         []Refusal     `json:"refusals,omitempty"`
	MergeReadiness   string        `json:"merge_readiness,omitempty"`
}

// Compact is the list-item shape. Label is the same catalogue entry as the full shape.
type Compact struct {
	Label     string       `json:"label"`
	Actor     string       `json:"actor"`
	Kind      string       `json:"kind"`
	Judgment  bool         `json:"judgment"`
	Summary   string       `json:"summary"`
	Deadline  *time.Time   `json:"deadline"`
	BlockedBy []BlockerRef `json:"blocked_by,omitempty"`
}

// Act is one factory recovery the inbox log shows. Automatic is always true.
type Act struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"task_id"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
	Automatic bool      `json:"automatic"`
	Summary   string    `json:"summary,omitempty"`
}
