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
	KindCheckHold               = "check_hold"
	KindMerging                 = "merging"
	KindContextProposal         = "context_proposal"
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
	// StallClaimFailing is an observed reason: the order's consecutive claims
	// failed in the backend at least ClaimFailureStallThreshold times
	// (feature-operator-next-action NA-3, Claim-failure stall).
	StallClaimFailing = "claim_failing"
)

// ClaimFailureStallThreshold is the consecutive backend-failed claim count
// at which a queued order states the claim_failing stall.
const ClaimFailureStallThreshold = 3

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
	// PendingProposals is the workspace's pending document proposals, read
	// for agent-question presentation only. It never selects a row or changes
	// claim eligibility (req-260810-23b69f REQ-3; DEC-102(6);
	// req-operator-next-action AC-1.6, AC-2.3).
	PendingProposals []ProposalRef
	// CitedDocuments is filled for an authority_conflict checkpoint only:
	// each cited document's current confirmed version and, for a System
	// Design attached to the task, its pinned version. A missing, archived,
	// or unconfirmed document is absent (feature-operator-next-action NA-3,
	// Agent-question presentation).
	CitedDocuments map[string]CitedDocument
	// ContextProposals is the task's pending task-context proposals. It
	// selects only the context_proposal row and never enters PendingProposals
	// (req-intake-and-triage AC-7.3).
	ContextProposals []ContextProposalRef
	// MonitorObservation is the originating observation of a monitor-filed
	// task with a pending context proposal, or nil when none is linked.
	MonitorObservation *MonitorObservationRef
	Blockers           []BlockerRef
	// Blocks is the task's open dependents: the tasks whose latest queued
	// order names this task in blocking_task_ids, ordered by task ID. The
	// assembler reads them from the same dependency read that fills
	// Blockers on the dependent side (feature-operator-next-action NA-3,
	// Dependents).
	Blocks []BlockerRef
	// Dispositions is the row-10 recovery binding read from the sealed
	// verification checkpoint of a verify order, keyed by that order's ID
	// (feature-operator-next-action NA-3, Disposition binding).
	Dispositions   map[string]DispositionInput
	MergeConflict  MergeConflict
	MergeReadiness *MergeReadiness
	// CheckStatus is the recorded check status of the approved head of an
	// approved task with the merge gate off, read only for such a task
	// (component-submission-merge, Check status).
	CheckStatus    CheckStatus
	Recommendation string
	Assignee       *core.TaskAssignee
	Capabilities   map[core.Capability]bool
	LiveClaimants  *ClaimantView
	Recovery       map[string]AutomaticRecovery
	// ClaimFailures is the batch's claim-failure records, keyed by order ID.
	// An absent entry means the order's last claim did not fail in the
	// backend (component-work-orders, Claim admission).
	ClaimFailures map[string]core.WorkOrderClaimFailure
	Now           time.Time
	// PullRequest is the recorded pull-request number, zero when none is
	// recorded.
	PullRequest int
	// BounceCount is the recorded bounce count that produced a
	// review_bounce_limit gate, zero when none is recorded.
	BounceCount int
	// GateSource is the source recorded by the event that produced the
	// current review_bounce_limit or job_failure gate, such as
	// "spec.output_invalid", and empty when that event records none. It
	// selects the gate's catalogue wording and is never interpolated into it
	// (component-next-action invariant 18).
	GateSource string
}

// Disposition is the verification body binding a client sends to
// POST /v1/work-orders/{id}/recover with a disposition and a reason
// (feature-operator-next-action NA-3; component-verification-service).
type Disposition struct {
	ContextID    string   `json:"context_id"`
	RunID        string   `json:"run_id"`
	OperationIDs []string `json:"operation_ids"`
	InputDigest  string   `json:"input_digest"`
}

// DispositionInput is the assembler's reading of one verify order's sealed
// checkpoint: a binding the recovery validator accepts, or the refusal that
// explains why none can be offered.
type DispositionInput struct {
	Binding *Disposition
	Refusal string
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
	CheckpointClass          string
	Citations                []core.WorkOrderAuthorityConflictCitation
	ReleaseOutcome           string
}

// Reason codes and catalogue variants of a merging task whose last check-status
// read went unanswered.
const (
	CheckReadFailed           = "check_read_failed"
	CheckReadRetriesExhausted = "check_read_retries_exhausted"
)

// Reasons a check-status hold names the operator.
const (
	CheckHoldFailed     = "failed"
	CheckHoldStuck      = "stuck"
	CheckHoldPermission = "permission"
)

// CheckHoldStuckRead is the catalogue variant of a stuck hold whose deadline
// read failed: it names the failure, not a check GitHub reported at that read.
const CheckHoldStuckRead = "stuck_read_failed"

// CheckRef is one check as a hold or a wait names it. URL is the check's page
// on GitHub, which is how the operator reruns it.
type CheckRef struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

// CheckHold is a standing hold, folded from merge.check_hold events. AfterStuck
// marks a permission hold that replaced a stuck hold, so the next read GitHub
// answers decides as if the stuck hold stood (req-review-gates-evidence AC-1.11;
// component-submission-merge invariant 62).
type CheckHold struct {
	Reason      string
	Checks      []CheckRef
	ReadFailure string
	Remedy      string
	AfterStuck  bool
}

// CheckStatus is the recorded check status of one head, already folded from
// events. Waiting names what the task waits on while a recorded deadline
// stands, ReadFailure names a failed read in that wait, and RetriesExhausted
// says the job's bounded retries for that failure ended.
type CheckStatus struct {
	Hold             *CheckHold
	Waiting          []CheckRef
	ReadFailure      string
	RetriesExhausted bool
}

// StuckStands reports a stuck hold, or a permission hold that took its place.
// Either way no new deadline is recorded, and a read that finds a check pending
// keeps or restores the stuck hold (component-submission-merge invariants 61, 62).
func (s CheckStatus) StuckStands() bool {
	return s.Hold != nil && (s.Hold.Reason == CheckHoldStuck || s.Hold.AfterStuck)
}

// MergeConflict is the open conflict episode, already folded from events.
type MergeConflict struct {
	Open        bool
	Exhausted   bool
	NextRetryAt time.Time
}

// AutomaticRecovery is the AR-6 hook, keyed by work-order ID. An absent entry
// means the order has no scheduled or exhausted automatic recovery. Attempt is
// the window-local number of the scheduled recovery.
type AutomaticRecovery struct {
	NextAt    *time.Time
	Exhausted bool
	Attempt   int
}

// CitedDocument is one cited document's authority as the assembler read it.
// PinnedVersion is zero unless the document is a System Design attached to
// the task.
type CitedDocument struct {
	CurrentVersion int
	PinnedVersion  int
}

type SpecRef struct {
	Version int
}

type ProposalRef struct {
	Tier    string `json:"tier"`
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// ContextProposalRef is one pending task-context proposal: its identity, the
// target kind and target ID, plus the stored record's presentation metadata.
// Deduplication and ordering read the identity alone
// (feature-operator-next-action NA-3, Context-proposal attention).
type ContextProposalRef struct {
	Kind          string    `json:"kind"`
	TargetID      string    `json:"target_id"`
	TargetTitle   string    `json:"target_title,omitempty"`
	Source        string    `json:"source,omitempty"`
	Justification string    `json:"justification,omitempty"`
	ProposedBy    string    `json:"proposed_by,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitzero"`
}

// ContextProposalDetail states one pending task-context proposal on a
// context_proposal next action, in choice order (feature-operator-next-action
// NA-3, Context-proposal presentation).
type ContextProposalDetail struct {
	Kind          string    `json:"kind"`
	TargetID      string    `json:"target_id"`
	TargetTitle   string    `json:"target_title"`
	TierLabel     string    `json:"tier_label"`
	Justification string    `json:"justification"`
	Source        string    `json:"source"`
	ProposedBy    string    `json:"proposed_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// MonitorObservationRef is the originating monitor observation the assembler
// read for a monitor-filed task (component-monitor-drift).
type MonitorObservationRef struct {
	Kind       string
	SourceURL  string
	ObservedAt time.Time
}

// MonitorOrigin states where a monitor-filed task came from. SourceURL and
// ObservedAt are absent when no linked observation of the task's kind exists
// (feature-operator-next-action NA-3, Monitor origin).
type MonitorOrigin struct {
	Kind       string     `json:"kind"`
	Summary    string     `json:"summary"`
	SourceURL  string     `json:"source_url,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
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
	Blocks       []BlockerRef `json:"blocks"`
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
	// Direction is the suggested recovery direction a client may offer as
	// the editable default. Only the answer choice of a checkpoint whose
	// cited revisions are all confirmed carries it (req-260820-394cac AC-2.4;
	// feature-operator-next-action NA-4).
	Direction string `json:"direction,omitempty"`
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
	GateKind        string   `json:"gate_kind,omitempty"`
	ReasonCode      string   `json:"reason_code,omitempty"`
	StallReason     string   `json:"stall_reason,omitempty"`
	ReleaseOutcome  string   `json:"release_outcome,omitempty"`
	OrderIDs        []string `json:"order_ids,omitempty"`
	Checkpoint      string   `json:"checkpoint,omitempty"`
	CheckpointClass string   `json:"checkpoint_class,omitempty"`
	// Citations are the checkpoint's citations with the cited version and
	// statement or section unchanged (req-260820-394cac AC-4.1).
	Citations        []core.WorkOrderAuthorityConflictCitation `json:"citations,omitempty"`
	PendingProposals []ProposalRef                             `json:"pending_proposals,omitempty"`
	Secondary        []Secondary                               `json:"secondary,omitempty"`
	Refusals         []Refusal                                 `json:"refusals,omitempty"`
	MergeReadiness   string                                    `json:"merge_readiness,omitempty"`
	// Disposition is present only on verification_disposition.
	Disposition *Disposition `json:"disposition,omitempty"`
	// ContextProposals and MonitorOrigin are present only on context_proposal.
	ContextProposals []ContextProposalDetail `json:"context_proposals,omitempty"`
	MonitorOrigin    *MonitorOrigin          `json:"monitor_origin,omitempty"`
	// ClaimFailure is present only on the claim_failing stall. It carries
	// the record's count, times, and class, never driver text.
	ClaimFailure *core.WorkOrderClaimFailure `json:"claim_failure,omitempty"`
	// Checks lists each check a check_hold names, or what a merging task waits
	// on, with its name, state, and GitHub URL (component-next-action, The
	// statement). CheckHoldReason, CheckReadFailure, and CheckRemedy say why:
	// the remedy appears only when GitHub refused the read.
	Checks           []CheckRef `json:"checks,omitempty"`
	CheckHoldReason  string     `json:"check_hold_reason,omitempty"`
	CheckReadFailure string     `json:"check_read_failure,omitempty"`
	CheckRemedy      string     `json:"check_remedy,omitempty"`
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
	Blocks    []BlockerRef `json:"blocks"`
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
