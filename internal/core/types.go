// Package core defines the shared domain types for Conveyor: tasks, jobs,
// pipeline stages, and their states. Postgres is the durable source of truth;
// the in-memory store remains an explicit test/development implementation.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

const MaxWorkOrderOperatorDirectionRunes = 4096

func NormalizeWorkOrderOperatorDirection(value string) (string, error) {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > MaxWorkOrderOperatorDirectionRunes {
		return "", fmt.Errorf("operator direction must be at most %d characters", MaxWorkOrderOperatorDirectionRunes)
	}
	return value, nil
}

// TruncateUTF8Bytes shortens value to at most limit bytes without splitting a
// UTF-8 encoding. Callers that persist byte-limited user text share this helper
// so memory and durable stores cannot disagree at multibyte boundaries.
func TruncateUTF8Bytes(value string, limit int) string {
	if limit < 1 {
		return ""
	}
	value = strings.ToValidUTF8(value, "")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// Stage is a pipeline stage. Phase 1 exercises only Implement;
// the rest are declared so task records and routing config are stable
// from day one.
type Stage string

const (
	StageTriage    Stage = "triage"
	StageSpec      Stage = "spec"
	StageImplement Stage = "implement"
	StageReview    Stage = "review"
	StageVerify    Stage = "verify"
	StageGate      Stage = "gate"
	StageMerge     Stage = "merge"
	StageMonitor   Stage = "monitor"
)

// ValidWorkOrderStage is the closed executor-stage vocabulary (DEC-43;
// component-work-orders).
func ValidWorkOrderStage(stage Stage) bool {
	return stage == StageSpec || stage == StageImplement || stage == StageVerify || stage == StageReview
}

func InitialStage(level EscalationLevel) Stage {
	if level == "" {
		return StageImplement
	}
	return StageTriage
}

// TaskMode is the retired auto/manual execution mode. It
// survives only to parse deprecated intake input and render historical
// records; no task carries a worker reservation (DEC-125(3)).
type TaskMode string

const (
	TaskModeAuto   TaskMode = "auto"
	TaskModeManual TaskMode = "manual"
)

func (m TaskMode) Valid() bool { return m == TaskModeAuto || m == TaskModeManual }

// LegacyPolicy preserves the accepted L0-L3 meaning for historical callers:
// the retired spec-approval value, which the plan state replaced, and the
// merge approval gate. No level implies a reservation; L3 maps both on
// exactly like L2 (DEC-125(1), DEC-125(3);
// component-task-lifecycle).
func LegacyPolicy(level EscalationLevel) (bool, bool) {
	switch level {
	case L0:
		return false, false
	case L1:
		return false, true
	default:
		return true, true
	}
}

// EscalationLevel is the degree of human involvement.
type EscalationLevel string

const (
	L0 EscalationLevel = "L0" // fully automatic
	L1 EscalationLevel = "L1" // one-click approve
	L2 EscalationLevel = "L2" // spec review + PR review
	L3 EscalationLevel = "L3" // interactive pairing
)

// TaskState is the denormalized current state of a task.
type TaskState string

const (
	TaskClaiming TaskState = "claiming"
	TaskQueued   TaskState = "queued"
	TaskRunning  TaskState = "running"
	TaskAwaiting TaskState = "awaiting_human"
	TaskApproved TaskState = "approved"
	TaskMerged   TaskState = "merged"
	TaskClosed   TaskState = "closed"
	TaskParked   TaskState = "parked"
)

type JobState string

const (
	JobPending JobState = "pending"
	JobRunning JobState = "running"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
)

// Task is a unit of intended change. One task spans many jobs.
type Task struct {
	Supersedes              string `json:"supersedes,omitempty"`
	SupersededBy            string `json:"superseded_by,omitempty"`
	IntakeOperatorDirection string `json:"intake_operator_direction,omitempty"`

	RepositoryInstallAttempt int `json:"repository_install_attempt,omitempty"`

	ID        string          `json:"id"`
	Workspace string          `json:"workspace"`
	Source    string          `json:"source"` // provenance: github:<slug>#<n>, cli, cron, monitor
	IntakeKey string          `json:"-"`      // workspace-scoped MCP retry key
	Title     string          `json:"title"`
	Body      string          `json:"body"`  // free-form description; becomes part of the prompt
	Class     string          `json:"class"` // bug | feature | chore
	Level     EscalationLevel `json:"level"`
	Mode      TaskMode        `json:"-"`                  // legacy persistence only; never emitted or read for behavior
	Hold      bool            `json:"-"`                  // historical stored value only; read by no decision (DEC-125(3))
	Assignee  *TaskAssignee   `json:"assignee,omitempty"` // claim-eligibility identity; never an ordering field
	// SpecApproval is the retained tasks.spec_approval column. No decision
	// reads it as plan authority and no task read serves it; PlanState is the
	// task's plan state (component-plan-state invariant 1; DEC-125(5)).
	SpecApproval  bool      `json:"-"`
	PlanState     PlanState `json:"plan_state"`
	MergeApproval bool      `json:"merge_approval"`
	// IntakePlan is the plan filed with the task, recorded as plan version 1
	// in the creating transaction and never stored on the task row
	// (component-plan-state invariant 2).
	IntakePlan         *IntakePlan           `json:"-"`
	PolicyVersion      int                   `json:"policy_version"`
	SetupName          string                `json:"-"` // retired live selector; retained for legacy row decoding
	SetupContract      config.PolicyContract `json:"policy_contract"`
	ReviewedHeadSHA    string                `json:"reviewed_head_sha,omitempty"`
	ApprovedHeadSHA    string                `json:"approved_head_sha,omitempty"`
	ApprovalStale      bool                  `json:"approval_stale,omitempty"`
	RefreshBaselineSHA string                `json:"refresh_baseline_sha,omitempty"`
	RefreshHeadSHA     string                `json:"refresh_head_sha,omitempty"`
	RefreshReviewScope string                `json:"refresh_review_scope,omitempty"`
	Repo               string                `json:"repo"` // repo name within the workspace; multi-repo sets are Phase 8
	BaseBranch         string                `json:"base_branch"`
	Branch             string                `json:"branch"` // current assigned branch; intake default is conveyor/task-<id>
	State              TaskState             `json:"state"`
	GateKind           GateKind              `json:"gate_kind,omitempty"`
	// CheckStatusDeadline is when the recorded check-status wait of
	// CheckStatusDeadlineHead falls due (req-review-gates-evidence AC-1.5). A
	// head other than the task's approved head starts fresh.
	CheckStatusDeadline     *time.Time        `json:"check_status_deadline,omitempty"`
	CheckStatusDeadlineHead string            `json:"check_status_deadline_head,omitempty"`
	NextStage               Stage             `json:"next_stage,omitempty"`     // durable pipeline transition selected at the preceding gate
	RecoveryStage           Stage             `json:"recovery_stage,omitempty"` // explicit human redirect/pull target while the pipeline is halted
	ParentTaskID            string            `json:"parent_task_id,omitempty"` // blueprint parent
	OriginSpecVersion       int               `json:"origin_spec_version,omitempty"`
	OriginSubID             string            `json:"origin_sub_id,omitempty"`
	Dependencies            []TaskRelation    `json:"dependencies,omitempty"`
	BlockingTaskIDs         []string          `json:"blocking_task_ids,omitempty"`
	Children                []TaskRelation    `json:"children,omitempty"`
	Context                 TaskContext       `json:"context,omitempty"`
	PullRequestClose        *PullRequestClose `json:"pull_request_close,omitempty"`
	PullRequestCloseState   string            `json:"pull_request_close_state,omitempty"`
	GitHub                  *GitHubLifecycle  `json:"github,omitempty"` // durable forge projection
	CreatedAt               time.Time         `json:"created_at"`
}

// TaskAssignee is the member-safe identity rendered on task and work-order
// claimability surfaces. Persistence stores only UserID; display fields are
// projected from the workspace membership join.
type TaskAssignee struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// TaskContext is the operator-attached desired-state authority carried by a
// task independently of blueprint ancestry.
type TaskContext struct {
	Requirements []TaskRequirementContext `json:"requirements,omitempty"`
	Designs      []TaskDesignContext      `json:"designs,omitempty"`
	Proposals    []TaskContextProposal    `json:"proposals,omitempty"`
}

type TaskRequirementContext struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Version      int      `json:"version"`
	Archived     bool     `json:"archived"`
	SupersededBy []string `json:"superseded_by,omitempty"`
}

type TaskDesignContext struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Version      int      `json:"version"`
	Archived     bool     `json:"archived"`
	SupersededBy []string `json:"superseded_by,omitempty"`
}

// TaskRelation is the compact live reference used by dependency and blueprint
// read models. Blocking remains derived from relation state.
type TaskRelation struct {
	ID                string    `json:"id"`
	Title             string    `json:"title"`
	State             TaskState `json:"state"`
	OriginSpecVersion int       `json:"origin_spec_version,omitempty"`
	OriginSubID       string    `json:"origin_sub_id,omitempty"`
}

func TaskTerminal(state TaskState) bool {
	return state == TaskMerged || state == TaskClosed
}

type GitHubPublicationState string

const (
	GitHubPublicationQueued    GitHubPublicationState = "queued"
	GitHubPublicationRetrying  GitHubPublicationState = "retrying"
	GitHubPublicationPublished GitHubPublicationState = "published"
	GitHubPublicationFailed    GitHubPublicationState = "failed"
	// GitHubPublicationSkipped is terminal: issue publication was off when
	// the handler ran, so the row ended without a create or an edit and is
	// never published later (req-delivery-and-forge AC-8.4).
	GitHubPublicationSkipped GitHubPublicationState = "skipped"
)

// GitHubCreateState records whether a remote create may still be attempted.
// Once reconciling, retries may only look up the exact task marker; this closes
// the ambiguity window where GitHub accepted a create but its acknowledgement
// never reached Conveyor.
type GitHubCreateState string

const (
	GitHubCreateNotStarted  GitHubCreateState = "not_started"
	GitHubCreateReconciling GitHubCreateState = "reconciling"
	GitHubCreateConfirmed   GitHubCreateState = "confirmed"
)

// GitHubLifecycle is the durable task-to-issue projection created from the
// exact approved spec. TaskID is the idempotency key; SourceIssueNumber is
// retained separately so provenance remains reconstructible after publication.
type GitHubLifecycle struct {
	TaskID             string                 `json:"task_id"`
	Repository         string                 `json:"repository"`
	SpecVersion        int                    `json:"spec_version"`
	Source             string                 `json:"source"`
	SourceIssueNumber  int                    `json:"source_issue_number,omitempty"`
	IssueNumber        int                    `json:"issue_number,omitempty"`
	IssueURL           string                 `json:"issue_url,omitempty"`
	Outcome            string                 `json:"outcome,omitempty"` // created | reused
	State              GitHubPublicationState `json:"state"`
	CreateState        GitHubCreateState      `json:"create_state"`
	CreateAttempts     int                    `json:"create_attempts"`
	ReconcileMisses    int                    `json:"reconcile_misses"`
	Attempts           int                    `json:"attempts"`
	ForgeErrorCategory string                 `json:"forge_error_category,omitempty"`
	LastError          string                 `json:"last_error,omitempty"`
	ForgeAuthorClass   ForgeAuthorClass       `json:"forge_author_class,omitempty"`
	ForgeAuthorUserID  string                 `json:"forge_author_user_id,omitempty"`
	// AutomaticReenqueueAt records the bounded automatic re-enqueue of a failed
	// publication (AR-5). Zero means no automatic re-enqueue has happened.
	AutomaticReenqueueAt time.Time `json:"automatic_reenqueue_at,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// Workspace is a durable control-plane boundary. ID and Name are immutable in
// the v1.9 slice; ConfigVersion advances independently for each workspace.
type Workspace struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	ConfigVersion int64     `json:"config_version"`
	CreatedAt     time.Time `json:"created_at"`
}

// NewTaskID returns a short, human-typeable ID: date prefix for
// eyeballing, random suffix so concurrent submissions never collide.
func NewTaskID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("060102") + "-" + hex.EncodeToString(b)
}

// NewWorkOrderAttemptID returns an opaque identity for one successful claim.
// It is deliberately independent of the worker session, which may remain warm
// across several attempts.
func NewWorkOrderAttemptID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "attempt-" + hex.EncodeToString(b)
}

// Job is one execution of one pipeline stage.
type Job struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	Stage       Stage     `json:"stage"`
	Harness     string    `json:"harness"`
	ModelTier   string    `json:"model_tier"`
	AuthMode    string    `json:"auth_mode,omitempty"`
	Runner      string    `json:"runner"`
	PackVersion string    `json:"pack_version,omitempty"`
	Confinement string    `json:"confinement"`
	CostUSD     *float64  `json:"cost_usd,omitempty"`
	TokensIn    int64     `json:"tokens_in"`
	TokensOut   int64     `json:"tokens_out"`
	State       JobState  `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
}

// MarshalJSON makes the wire contract honor ended_at's optionality. The
// standard encoder does not consider a zero time.Time empty, so relying on
// omitempty alone serializes year 1 and freezes running-job durations at 0s.
func (j Job) MarshalJSON() ([]byte, error) {
	type jobAlias Job
	wired := struct {
		jobAlias
		StartedAt *time.Time `json:"started_at,omitempty"`
		EndedAt   *time.Time `json:"ended_at,omitempty"`
	}{jobAlias: jobAlias(j)}
	if !j.StartedAt.IsZero() {
		wired.StartedAt = &j.StartedAt
	}
	if !j.EndedAt.IsZero() {
		wired.EndedAt = &j.EndedAt
	}
	return json.Marshal(wired)
}

// ActorRole is recorded on every append-only event and intervention from the
// beginning of Phase 2 so later RBAC adds enforcement to an existing audit
// identity model instead of retrofitting attribution.
type ActorRole string

const (
	ActorSystem ActorRole = "system"
	ActorHuman  ActorRole = "human"
	ActorAgent  ActorRole = "agent"
	ActorRunner ActorRole = "runner"

	// User and worker are the credential-derived actor classes. The persisted
	// role labels remain compatible with append-only historical events.
	ActorUser   = ActorHuman
	ActorWorker = ActorRunner
)

type CredentialKind string

const (
	CredentialUser CredentialKind = "user"
)

type CredentialScope string

const (
	CredentialScopeOperator CredentialScope = "operator"
	CredentialScopeUser     CredentialScope = "user"
)

// AuthenticatedCredential is the non-secret identity resolved from a person's
// bearer credential or dashboard session (DEC-116(1)).
type AuthenticatedCredential struct {
	ID          string
	OwnerUserID string
	Kind        CredentialKind
	Scope       CredentialScope
	// Method distinguishes browser sessions from bearer credentials without
	// changing the user actor or live capability model.
	Method CredentialMethod
	// SessionExpiresAt and SessionEstablishedByLink are populated only for
	// browser sessions. They let the HTTP boundary refresh the opaque cookie and
	// enforce link-authorized password recovery without widening bearer access.
	SessionExpiresAt         time.Time
	SessionEstablishedByLink bool
}

type CredentialMethod string

const (
	CredentialMethodBearer  CredentialMethod = "bearer"
	CredentialMethodSession CredentialMethod = "session"
)

// TaskRunClaimantPrefix distinguishes an explicit user invocation from worker
// auto-dispatch without storing local execution configuration (REQ-5, AC-5.1).
const TaskRunClaimantPrefix = "run:"

func TaskRunClaimantID(userID string) string { return TaskRunClaimantPrefix + userID }

func IsTaskRunClaimantID(claimantID string) bool {
	return strings.HasPrefix(claimantID, TaskRunClaimantPrefix) && len(claimantID) > len(TaskRunClaimantPrefix)
}

func (action InterventionAction) Valid() bool {
	for _, valid := range InterventionActions() {
		if action == valid {
			return true
		}
	}
	return false
}

// Event is the append-only source of truth for task state. Tasks and jobs are
// transactional projections optimized for queries.
type Event struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"task_id"`
	JobID     string    `json:"job_id,omitempty"`
	Kind      string    `json:"kind"`
	ActorID   string    `json:"actor_id"`
	ActorRole ActorRole `json:"actor_role"`
	// ActorOwnerUserID is set only on a read, for an act recorded under a
	// retired agent credential: the user who owns that credential. It is never
	// stored, and the recorded ActorID stays as written
	// (req-security-boundaries AC-1.3; DEC-116(3)).
	ActorOwnerUserID string          `json:"actor_owner_user_id,omitempty"`
	Payload          json.RawMessage `json:"payload"`
	At               time.Time       `json:"at"`
}

type InterventionAction string

const (
	InterventionApprove  InterventionAction = "approve"
	InterventionReject   InterventionAction = "reject"
	InterventionRedirect InterventionAction = "redirect"
	InterventionPull     InterventionAction = "pull_to_local"
	InterventionCancel   InterventionAction = "cancel"
)

// InterventionActions is the canonical persisted action set shared by API
// validation and migration-alignment tests. Applied migrations stay immutable;
// adding an action requires a new forward migration.
func InterventionActions() []InterventionAction {
	return []InterventionAction{
		InterventionApprove,
		InterventionReject,
		InterventionRedirect,
		InterventionPull,
		InterventionCancel,
	}
}

// Intervention is a structured human decision from the review queue.
// ReasonCode is intentionally data rather than an enum so workspaces
// can add taxonomy without a deploy; the API validates the required baseline.
type Intervention struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"task_id"`
	JobID     string    `json:"job_id,omitempty"`
	ActorID   string    `json:"actor_id"`
	ActorRole ActorRole `json:"actor_role"`
	// ActorOwnerUserID is set only on a read; see Event.ActorOwnerUserID.
	ActorOwnerUserID string             `json:"actor_owner_user_id,omitempty"`
	Action           InterventionAction `json:"action"`
	ReasonCode       string             `json:"reason_code"`
	Comment          string             `json:"comment"`
	At               time.Time          `json:"at"`
}

type RedactionStats struct {
	Exact   int64 `json:"exact"`
	Encoded int64 `json:"encoded"`
	Pattern int64 `json:"pattern"`
	Entropy int64 `json:"entropy"`
}

func (s RedactionStats) Total() int64 { return s.Exact + s.Encoded + s.Pattern + s.Entropy }

func (s *RedactionStats) Add(other RedactionStats) {
	s.Exact += other.Exact
	s.Encoded += other.Encoded
	s.Pattern += other.Pattern
	s.Entropy += other.Entropy
}

type Transcript struct {
	JobID          string         `json:"job_id"`
	URI            string         `json:"uri"`
	RedactionStats RedactionStats `json:"redaction_stats"`
	CreatedAt      time.Time      `json:"created_at"`
}

type WorkOrderState string

const (
	WorkOrderQueued    WorkOrderState = "queued"
	WorkOrderClaimed   WorkOrderState = "claimed"
	WorkOrderSubmitted WorkOrderState = "submitted"
	WorkOrderCompleted WorkOrderState = "completed"
	WorkOrderCancelled WorkOrderState = "cancelled"
	WorkOrderStale     WorkOrderState = "stale"
	WorkOrderTimedOut  WorkOrderState = "timed_out"
)

// WorkOrderActiveForConflictDispatch is the single reason-coded admission
// definition for merge-conflict fix work. Holds reserve queued orders from the
// worker supervisor; they do not make the order inactive.
func WorkOrderActiveForConflictDispatch(order WorkOrder) bool {
	if order.State == WorkOrderClaimed {
		return true
	}
	// An expired or otherwise terminal child can be projected back to queued
	// while recovery is deliberately suppressed. It has no pending attempt and
	// must not block a new episode-local dispatch.
	return order.State == WorkOrderQueued && !order.RetrySuppressed
}

// WorkOrderHoldsStage is the creation-guard definition for a task stage.
// For non-review stages, orders are one task's stored orders sorted by
// (created_at, id), so the newest same-stage claimed order is selected first.
// A claimed order holds even when a newer stage is current. A dead claim must
// remain a hold until the order clock persists its expiry; projecting it first
// would turn it into queued or timed_out history and lose that distinction.
// Review rounds own their latest-round admission, so review delegates to
// WorkOrderBlocksStageMint rather than treating an older claimed seat as a
// cross-round hold (component-work-orders "Stages and order minting"; DEC-62).
func WorkOrderHoldsStage(orders []WorkOrder, stage Stage) (WorkOrder, bool) {
	if stage == StageReview {
		return WorkOrderBlocksStageMint(orders, stage)
	}
	for i := len(orders) - 1; i >= 0; i-- {
		order := orders[i]
		if order.Stage == stage && order.State == WorkOrderClaimed {
			return order, true
		}
	}
	return WorkOrderBlocksStageMint(orders, stage)
}

// WorkOrderBlocksStageMint is the single admission definition for minting a
// new <task>-<stage>-N order outside merge-conflict work. orders are one
// task's orders sorted by (created_at, id). The task's latest order blocks a
// successor at stage while it is queued, claimed, stale, or timed_out: those
// are recoverable stops that the order clock redispatches or recovers under
// the DEC-62 bound, and past the bound they wait for the operator
// (component-work-orders "Stages and order minting"; feature-bounded-automatic-recovery
// AR-3). The rule holds even when automatic recovery is disabled. An order at
// another stage created later means the task moved on and bounced back, so the
// older order no longer blocks. For review the latest order is one seat of
// the latest round, and any seat of that round in a holding state holds the
// stage, so a stale or timed-out seat is redispatched rather than re-rounded.
// Stored and read-time projected states map onto the same set, so durable
// reconcile filters use the same predicate (component-durable-queue
// "Reconciliation between tasks and jobs").

func WorkOrderBlocksStageMint(orders []WorkOrder, stage Stage) (WorkOrder, bool) {
	if len(orders) == 0 {
		return WorkOrder{}, false
	}
	latest := orders[len(orders)-1]
	if latest.Stage != stage {
		return WorkOrder{}, false
	}
	if stage != StageReview {
		return latest, WorkOrderStateBlocksMint(latest.State)
	}
	for i := len(orders) - 1; i >= 0; i-- {
		seat := orders[i]
		if seat.Stage == StageReview && seat.ReviewRound == latest.ReviewRound && WorkOrderStateBlocksMint(seat.State) {
			return seat, true
		}
	}
	return WorkOrder{}, false
}

// WorkOrderStateBlocksMint lists the order states that hold a stage open:
// every state that is not submitted, completed, or cancelled.
func WorkOrderStateBlocksMint(state WorkOrderState) bool {
	switch state {
	case WorkOrderQueued, WorkOrderClaimed, WorkOrderStale, WorkOrderTimedOut:
		return true
	}
	return false
}

// HarnessSnapshot is legacy server-pinned execution metadata that older orders
// persisted. The control plane no longer creates, resolves, or refreshes
// one: queue re-entry clears it, and the claiming machine's local execution
// setup supplies harness, model, and effort (req-worker AC-2.2; DEC-56).
type HarnessSnapshot struct {
	Name                  string              `json:"name"`
	MCPTransport          string              `json:"mcp_transport"`
	MCPAttachment         string              `json:"mcp_attachment,omitempty"`
	Command               []string            `json:"command"`
	ModelArgs             []string            `json:"model_args,omitempty"`
	DefaultModelSentinels []string            `json:"default_model_sentinels,omitempty"`
	EffortArgs            map[string][]string `json:"effort_args,omitempty"`
	Effort                string              `json:"effort,omitempty"`
	EffortArgv            []string            `json:"effort_argv,omitempty"`
	ProbeCommand          []string            `json:"probe_command"`
	ProbeTimeoutText      string              `json:"probe_timeout"`
	StallTimeoutText      string              `json:"stall_timeout"`
}

// DefaultWorkOrderClaimLease is the renewable lease used whenever a claim
// omits an explicit duration. It is distinct from worker liveness.
const DefaultWorkOrderClaimLease = 5 * time.Minute

// RateLimitStatus is normalized provider telemetry reported by an agent.
// It is observational only and must never participate in dispatch decisions
type RateLimitStatus struct {
	Status    string     `json:"status"`
	Limit     *float64   `json:"limit,omitempty"`
	Remaining *float64   `json:"remaining,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

// WorkOrder is the durable protocol boundary between Conveyor and an
// operator-owned spec, implementation, or review agent.
// DeclaredReviewSeats is an implement order's declared review seat count,
// nil when it declared none; only the submission path writes it
// (feature-review-seat-minimum RS-2, RS-4).
type WorkOrder struct {
	VerificationContextID            string               `json:"verification_context_id,omitempty"`
	ID                               string               `json:"id"`
	TaskID                           string               `json:"task_id"`
	JobID                            string               `json:"job_id"`
	Stage                            Stage                `json:"stage"`
	State                            WorkOrderState       `json:"state"`
	Claimable                        bool                 `json:"claimable"`
	ClaimRefusalReason               string               `json:"claim_refusal_reason,omitempty"`
	RecoveryRefusal                  string               `json:"recovery_refusal,omitempty"`
	BlockingTaskIDs                  []string             `json:"blocking_task_ids,omitempty"`
	Assignee                         *TaskAssignee        `json:"assignee,omitempty"`
	UnsatisfiableTaskIDs             []string             `json:"unsatisfiable_task_ids,omitempty"`
	ClaimantID                       string               `json:"claimed_by,omitempty"`
	SessionID                        string               `json:"session_id,omitempty"`
	AttemptID                        string               `json:"attempt_id,omitempty"`
	ClientTokenHash                  string               `json:"-"`
	Agent                            string               `json:"agent,omitempty"`
	Model                            string               `json:"model,omitempty"`
	WorkerID                         string               `json:"worker_id,omitempty"`
	ReviewRound                      int                  `json:"review_round,omitempty"`
	ReviewSeat                       int                  `json:"review_seat,omitempty"`
	DeclaredReviewSeats              *int                 `json:"declared_review_seats,omitempty"`
	ReasonCode                       string               `json:"reason_code,omitempty"`
	ReviewKind                       string               `json:"review_kind,omitempty"`
	ReviewScope                      string               `json:"review_scope,omitempty"`
	BaselineSHA                      string               `json:"baseline_sha,omitempty"`
	HeadSHA                          string               `json:"head_sha,omitempty"`
	RequiredModel                    string               `json:"required_model,omitempty"`
	RequiredHarness                  string               `json:"required_harness,omitempty"`
	RequiredEffort                   string               `json:"required_effort,omitempty"`
	RequiredHarnessConfig            *HarnessSnapshot     `json:"required_harness_config,omitempty"`
	ExecutionTimeoutText             string               `json:"execution_timeout,omitempty"`
	ModelEnforcement                 string               `json:"model_enforcement,omitempty"`
	LeaseExpiresAt                   time.Time            `json:"lease_expires_at,omitempty"`
	QueueEnteredAt                   time.Time            `json:"queue_entered_at"`
	QueueDeadline                    time.Time            `json:"queue_deadline"`
	QueueBlockedAt                   time.Time            `json:"queue_blocked_at,omitempty"`
	ExecutionStartedAt               time.Time            `json:"execution_started_at,omitempty"`
	ExecutionDeadline                time.Time            `json:"execution_deadline,omitempty"`
	LastAttemptID                    string               `json:"last_attempt_id,omitempty"`
	LastAttemptOutcome               string               `json:"last_attempt_outcome,omitempty"`
	LastFailureCategory              string               `json:"last_failure_category,omitempty"`
	LastFailureMessage               string               `json:"last_failure_message,omitempty"`
	LastFailureDetail                string               `json:"last_failure_detail,omitempty"`
	LastFailureExitStatus            *int                 `json:"last_failure_exit_status,omitempty"`
	LastFailureAt                    time.Time            `json:"last_failure_at,omitempty"`
	AutomaticRetryCount              int                  `json:"automatic_retry_count"`
	NextRetryAt                      time.Time            `json:"next_retry_at,omitempty"`
	AutomaticRecoveryCount           int                  `json:"automatic_recovery_count"`
	AutomaticRecoveryWindowStartedAt time.Time            `json:"automatic_recovery_window_started_at,omitempty"`
	NextAutomaticRecoveryAt          time.Time            `json:"next_automatic_recovery_at,omitempty"`
	BaseConflictCheckedAt            time.Time            `json:"base_conflict_checked_at,omitempty"`
	RetrySuppressed                  bool                 `json:"retry_suppressed"`
	RetrySuppressionReason           string               `json:"retry_suppression_reason,omitempty"`
	RedispatchCount                  int                  `json:"redispatch_count"`
	OperatorDirection                string               `json:"operator_direction,omitempty"`
	Checkpoint                       *WorkOrderCheckpoint `json:"checkpoint,omitempty"`
	Progress                         string               `json:"progress,omitempty"`
	CostUSD                          float64              `json:"cost_usd,omitempty"`
	TokensIn                         int64                `json:"tokens_in"`
	TokensOut                        int64                `json:"tokens_out"`
	UsageReported                    bool                 `json:"usage_reported"`
	SelfReported                     bool                 `json:"self_reported"`
	RateLimit                        *RateLimitStatus     `json:"rate_limit,omitempty"`
	RateLimitObservedAt              time.Time            `json:"rate_limit_observed_at,omitempty"`
	LastAgentActivityAt              time.Time            `json:"last_agent_activity_at,omitempty"`
	LastAgentActivityLabel           string               `json:"last_agent_activity_label,omitempty"`
	CreatedAt                        time.Time            `json:"created_at"`
	UpdatedAt                        time.Time            `json:"updated_at"`
	// ServedRequirementSnapshot is the citation authority rendered for this
	// review order. A non-nil empty slice means the task had no served
	// requirements; nil is reserved for pre-snapshot compatibility handling.
	ServedRequirementSnapshot []ServedRequirementContext `json:"served_requirement_snapshot,omitempty"`
	// GovernanceSnapshot is the exact design and decision authority plus the
	// separately non-authoritative task proposal evidence refreshed for this
	// review claim. Nil is reserved for legacy compatibility handling.
	GovernanceSnapshot *GovernanceSnapshot `json:"governance_snapshot,omitempty"`
	// GovernanceSnapshotDigest is the content-addressed identity of GovernanceSnapshot.
	// Claim events carry this instead of document bodies.
	GovernanceSnapshotDigest string `json:"governance_snapshot_digest,omitempty"`

	// ClaimExpiryUnpersisted marks a read-time projection of a stored claim
	// whose lease or execution deadline has elapsed. The order clock has not
	// persisted the expiry, so the claim still holds its stage. It is never
	// stored or emitted (WorkOrderHoldsStage).
	ClaimExpiryUnpersisted bool `json:"-"`
}

// ClearExecutionPins removes every harness, model, and effort pin, including a
// legacy server-pinned harness snapshot, from an order re-entering the queue. The
// local execution setup of the machine that claims it supplies them for the
// next attempt. Review round and seat are not execution pins and stay
// unchanged (req-worker AC-2.2, AC-2.3; DEC-56).
func (w *WorkOrder) ClearExecutionPins() {
	w.RequiredModel, w.RequiredHarness, w.RequiredEffort = "", "", ""
	w.RequiredHarnessConfig = nil
}

// MarshalJSON keeps the three work-order clocks distinct on the wire and
// omits clocks that have not started. A queued order must not look as though
// execution or a claim lease has begun.
func (w WorkOrder) MarshalJSON() ([]byte, error) {
	type workOrderAlias WorkOrder
	wired := struct {
		workOrderAlias
		LeaseExpiresAt      *time.Time `json:"lease_expires_at,omitempty"`
		QueueBlockedAt      *time.Time `json:"queue_blocked_at,omitempty"`
		ExecutionStartedAt  *time.Time `json:"execution_started_at,omitempty"`
		ExecutionDeadline   *time.Time `json:"execution_deadline,omitempty"`
		LastFailureAt       *time.Time `json:"last_failure_at,omitempty"`
		NextRetryAt         *time.Time `json:"next_retry_at,omitempty"`
		RateLimitObservedAt *time.Time `json:"rate_limit_observed_at,omitempty"`
		LastAgentActivityAt *time.Time `json:"last_agent_activity_at,omitempty"`
	}{workOrderAlias: workOrderAlias(w)}
	if !w.LeaseExpiresAt.IsZero() {
		wired.LeaseExpiresAt = &w.LeaseExpiresAt
	}
	if !w.QueueBlockedAt.IsZero() {
		wired.QueueBlockedAt = &w.QueueBlockedAt
	}
	if !w.ExecutionStartedAt.IsZero() {
		wired.ExecutionStartedAt = &w.ExecutionStartedAt
	}
	if !w.ExecutionDeadline.IsZero() {
		wired.ExecutionDeadline = &w.ExecutionDeadline
	}
	if !w.LastFailureAt.IsZero() {
		wired.LastFailureAt = &w.LastFailureAt
	}
	if !w.NextRetryAt.IsZero() {
		wired.NextRetryAt = &w.NextRetryAt
	}
	if !w.RateLimitObservedAt.IsZero() {
		wired.RateLimitObservedAt = &w.RateLimitObservedAt
	}
	if !w.LastAgentActivityAt.IsZero() {
		wired.LastAgentActivityAt = &w.LastAgentActivityAt
	}
	return json.Marshal(wired)
}

// HistoricalCostUSD returns the order's retained historical reported cost for
// its job, or nil when the order carries none. Usage reports no longer carry
// cost, so a zero order cost is absent rather than measured: the wire omits it
// and a completed job never gains an invented zero (req-usage-telemetry
// AC-2.1; DEC-1).
func (w WorkOrder) HistoricalCostUSD() *float64 {
	if w.CostUSD == 0 {
		return nil
	}
	cost := w.CostUSD
	return &cost
}

func (w WorkOrder) ClaimableAt(at time.Time) bool {
	return w.State == WorkOrderQueued && !w.RetrySuppressed &&
		(w.NextRetryAt.IsZero() || !w.NextRetryAt.After(at))
}

type WorkOrderClaim struct {
	SessionID        string
	ClientToken      string
	ClaimantID       string
	Agent            string
	Model            string
	Lease            time.Duration
	ExecutionTimeout time.Duration
	OwnerUserID      string
	Requirements     []ServedRequirementContext
	Governance       *GovernanceSnapshot
}

// WorkOrderClaimIdentity names the authenticated owner of one active claim.
// Mutations compare all three fields. WorkerID is empty for every claim since
// DEC-97; an order claimed by a worker before then keeps its historical
// worker_id, which no current identity matches.
type WorkOrderClaimIdentity struct {
	WorkerID   string
	ClaimantID string
	SessionID  string
}

const (
	WorkOrderOutcomeChildFailure                    = "child_failure"
	WorkOrderOutcomeStalled                         = "stalled"
	WorkOrderOutcomeReleased                        = "released"
	WorkOrderOutcomeCancelled                       = "cancelled"
	WorkOrderOutcomeExpired                         = "expired"
	WorkOrderOutcomePreempted                       = "preempted"
	WorkOrderReleaseReasonOperatorCheckpointReached = "operator checkpoint reached"
	WorkOrderReleaseReasonPlanRevisionRequested     = "plan revision requested"
	WorkOrderReleaseCauseSessionExit                = "session_exit"
	WorkOrderReleaseCauseOperatorAction             = "operator_action"
	WorkOrderReleaseCauseLeaseLoss                  = "lease_loss"
	IdenticalFailureSuppressionReason               = "identical failure output on consecutive attempts"
	AttemptRetriesExhaustedReason                   = "attempt retries exhausted"
	AutomaticRecoveryExhaustedReason                = "automatic recovery exhausted"
	AutomaticRecoveryReason                         = "automatic recovery"
	WorkOrderFailureProviderUsageLimit              = "provider_usage_limit"
	WorkOrderFailureTransientConnectivity           = "transient_connectivity"
	WorkOrderFailureDetailLimit                     = 2 * 1024
	WorkOrderDecisionRequestLimit                   = 1024
	WorkOrderCheckpointClassAuthorityConflict       = "authority_conflict"
	TaskTerminalSuppressionReason                   = "task_terminal"
	// ReviewRoundSettledSuppressionReason retires an unfinished review seat
	// whose round a fail-fast changes-requested verdict already settled.
	ReviewRoundSettledSuppressionReason = "review_round_settled"
)

// WorkOrderCheckpoint is operator-facing context recorded by a deliberate
// checkpoint release. It is presentation and routing metadata, never
// lifecycle authority (req-260820-394cac REQ-1, REQ-2).
type WorkOrderCheckpoint struct {
	DecisionRequest string                               `json:"decision_request,omitempty"`
	Class           string                               `json:"class,omitempty"`
	Citations       []WorkOrderAuthorityConflictCitation `json:"citations,omitempty"`
	Verification    *WorkOrderVerificationCheckpoint     `json:"verification,omitempty"`
}

// WorkOrderVerificationCheckpoint references the sealed operator checkpoint of
// component-verification-service. It never satisfies review admission.
type WorkOrderVerificationCheckpoint struct {
	ContextID      string                                  `json:"context_id"`
	HeadSHA        string                                  `json:"head_sha"`
	Reason         string                                  `json:"reason"`
	RequiredAction string                                  `json:"required_action"`
	Summary        string                                  `json:"summary,omitempty"`
	Grounds        []WorkOrderVerificationCheckpointGround `json:"grounds"`
	OperationIDs   []string                                `json:"operation_ids,omitempty"`
}

// WorkOrderVerificationCheckpointGround is the bounded authenticated view of
// one sealed ground (component-verification-service): declared permissions for an unstarted subject,
// and the attempt, evidence and operation identifiers an operator opens.
// Lists hold at most WorkOrderVerificationGroundListLimit entries; Truncated
// reports any omission.
type WorkOrderVerificationCheckpointGround struct {
	Kind           string                                      `json:"kind"`
	Subject        VerificationSubject                         `json:"subject"`
	AttemptID      string                                      `json:"attempt_id,omitempty"`
	Explanation    string                                      `json:"explanation,omitempty"`
	Permissions    []WorkOrderVerificationCheckpointPermission `json:"permissions,omitempty"`
	EvidenceIDs    []string                                    `json:"evidence_ids,omitempty"`
	OperationIDs   []string                                    `json:"operation_ids,omitempty"`
	Truncated      bool                                        `json:"truncated,omitempty"`
	ServerVerified bool                                        `json:"server_verified"`
}

type WorkOrderVerificationCheckpointPermission struct {
	Kind          string `json:"kind"`
	TargetBinding string `json:"target_binding,omitempty"`
	Path          string `json:"path,omitempty"`
}

const (
	WorkOrderVerificationGroundListLimit = 20
	WorkOrderVerificationGroundTextLimit = 512
)

type WorkOrderAuthorityConflictCitation struct {
	DocumentID           string `json:"document_id"`
	CitedVersion         int    `json:"cited_version"`
	StatementOrSectionID string `json:"statement_or_section_id,omitempty"`
}

func WorkOrderOutcomeConsumesRetry(outcome string) bool {
	return outcome == WorkOrderOutcomeChildFailure || outcome == WorkOrderOutcomeStalled
}

// TransientConnectivityRetryDelay returns the fixed bounded worker pacing for
// one consecutive connectivity-failure streak.
func TransientConnectivityRetryDelay(consecutive int) time.Duration {
	switch consecutive {
	case 1:
		return 30 * time.Second
	case 2:
		return 2 * time.Minute
	default:
		return 8 * time.Minute
	}
}

// ConsecutiveTransientFailureCount advances or resets the connectivity-only
// streak without changing the separate bounded-attempt counter.
func ConsecutiveTransientFailureCount(category string, previous int, progressed, sameOutcome bool) int {
	if category != WorkOrderFailureTransientConnectivity {
		return 0
	}
	if progressed || !sameOutcome {
		return 1
	}
	return previous + 1
}

// TransientConnectivityFailureDetail keeps the redacted child tail and makes
// the derived wait visible without exceeding the existing detail bound.
func TransientConnectivityFailureDetail(detail string, consecutive int, nextRetryAt time.Time) string {
	nextAttempt := "none"
	if !nextRetryAt.IsZero() {
		nextAttempt = nextRetryAt.UTC().Format(time.RFC3339Nano)
	}
	context := fmt.Sprintf("retry pacing: category=%s consecutive_transient_failures=%d next_attempt_at=%s", WorkOrderFailureTransientConnectivity, consecutive, nextAttempt)
	if detail = strings.TrimSpace(detail); detail != "" {
		available := WorkOrderFailureDetailLimit - len(context) - 2
		if len(detail) > available {
			detail = strings.ToValidUTF8(detail[len(detail)-available:], "�")
		}
		return detail + "\n\n" + context
	}
	return context
}

func ValidWorkOrderReleaseCause(cause string) bool {
	return cause == WorkOrderReleaseCauseSessionExit ||
		cause == WorkOrderReleaseCauseOperatorAction || cause == WorkOrderReleaseCauseLeaseLoss
}

type WorkOrderRelease struct {
	SessionID           string
	Reason              string
	Checkpoint          *WorkOrderCheckpoint `json:"checkpoint,omitempty"`
	Cause               string               `json:"release_cause,omitempty"`
	Outcome             string
	FailureCategory     string
	ExitStatus          *int
	FailureDetail       string
	InitialRetryDelay   time.Duration
	MaximumRetryDelay   time.Duration
	AutomaticRetryLimit int
}

type ReviewPublicationState string

const (
	ReviewPublicationQueued    ReviewPublicationState = "queued"
	ReviewPublicationRetrying  ReviewPublicationState = "retrying"
	ReviewPublicationPublished ReviewPublicationState = "published"
	ReviewPublicationFailed    ReviewPublicationState = "failed"
)

// ReviewPublication is the durable, factory-owned GitHub publication for one
// completed review work order. The work-order ID is the idempotency key.
type ReviewPublication struct {
	ReviewWorkOrderID      string                 `json:"review_work_order_id"`
	TaskID                 string                 `json:"task_id"`
	JobID                  string                 `json:"job_id"`
	Verdict                string                 `json:"verdict"`
	ReasonCode             string                 `json:"reason_code"`
	Summary                string                 `json:"summary"`
	Feedback               string                 `json:"feedback"`
	ReviewedCommitSHA      string                 `json:"reviewed_commit_sha,omitempty"`
	ReviewerModel          string                 `json:"reviewer_model,omitempty"`
	ReviewerSession        string                 `json:"reviewer_session"`
	SameModelAsImplementer string                 `json:"same_model_as_implementer"`
	ReviewRound            int                    `json:"review_round,omitempty"`
	ReviewSeat             int                    `json:"review_seat,omitempty"`
	RequiredModel          string                 `json:"required_model,omitempty"`
	RequiredHarness        string                 `json:"required_harness,omitempty"`
	RequiredEffort         string                 `json:"required_effort,omitempty"`
	ModelEnforcement       string                 `json:"model_enforcement,omitempty"`
	State                  ReviewPublicationState `json:"state"`
	Attempts               int                    `json:"attempts"`
	// CheckRunID is retained for historical v1.22 publications. Portable v1.23
	// commit-status publications leave it zero.
	CheckRunID         int64            `json:"check_run_id,omitempty"`
	CommentID          int64            `json:"comment_id,omitempty"`
	ForgeErrorCategory string           `json:"forge_error_category,omitempty"`
	LastError          string           `json:"last_error,omitempty"`
	ForgeAuthorClass   ForgeAuthorClass `json:"forge_author_class,omitempty"`
	ForgeAuthorUserID  string           `json:"forge_author_user_id,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
	// AutomaticReenqueueAt records the bounded automatic re-enqueue of a failed
	// publication (AR-5). Zero means no automatic re-enqueue has happened.
	AutomaticReenqueueAt time.Time `json:"automatic_reenqueue_at,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// ReviewDecision is the atomic internal acceptance record for one completed
// review attempt. GitHub publication is queued in the same store transaction;
// the external GitHub side effects remain asynchronous.
type ReviewDecision struct {
	VerificationAssessment  *VerificationAssessment
	DocumentationAssessment *DocumentationAssessment
	TaskID                  string
	JobID                   string
	ReviewWorkOrderID       string
	Verdict                 string
	ReasonCode              string
	Summary                 string
	Feedback                string
	ReviewedCommitSHA       string
	EvidenceIDs             []string
	RequirementCitations    *RequirementCitationAssessment
	DoneCriteriaAssessment  *DoneCriteriaAssessment
	GovernanceAssessment    *GovernanceAssessment
	Reviewer                string
	ReviewerModel           string
	ReviewerSession         string
	ClaimSession            string
	SameModelAsImplementer  string
	ReviewRound             int
	ReviewSeat              int
	ReviewKind              string
	ReviewScope             string
	BaselineSHA             string
	HeadSHA                 string
	RequiredModel           string
	RequiredHarness         string
	RequiredEffort          string
	ModelEnforcement        string
	InterventionActorID     string
	PublicationEligible     bool
	Level                   EscalationLevel
	PolicyVersion           int
	MergeApproval           bool
	MaxBounces              int
	// RiskTier and RiskReason are the seat's stated rating of the head it
	// judged. They are set only when the task's frozen risk mode is record or
	// enforce and the seat stated a tier; acceptance then records one reviewer
	// rating in the same transaction (req-risk-based-review AC-5.2, AC-5.3;
	// feature-risk-based-review RR-3).
	RiskTier   string
	RiskReason string
	// WorkspaceReviewSeats and SeatQueueTimeout are trusted, service-derived
	// inputs for seats an enforcing tier adds in the verdict's own act: the
	// workspace seat-list length and the queue clock of an added seat
	// (req-risk-based-review AC-7.2; feature-risk-based-review RR-5). No
	// verdict body supplies them.
	WorkspaceReviewSeats int           `json:"-"`
	SeatQueueTimeout     time.Duration `json:"-"`
}

type ArtifactRole string

const (
	// ArtifactRoleTaskContext is durable user-supplied context that may be
	// attached to in-process model requests.
	ArtifactRoleTaskContext ArtifactRole = "task_context"
	// ArtifactRoleGeneratedAudit is Conveyor-generated evidence that remains
	// downloadable and task-owned, but must never become model input.
	ArtifactRoleGeneratedAudit ArtifactRole = "generated_audit"
	// ArtifactRoleGeneratedOutput reserves the same fail-closed input behavior
	// for future task-linked pipeline outputs that are not audit transcripts.
	ArtifactRoleGeneratedOutput ArtifactRole = "generated_output"
	// ArtifactRoleVerificationEvidence is implementer-supplied proof of an
	// exercised change. It is a review aid, never model input or CI authority
	ArtifactRoleVerificationEvidence ArtifactRole = "verification_evidence"
	// ArtifactRoleTypedVerificationEvidence stores typed evidence bytes (component-verification-evidence) without granting
	// model input eligibility or satisfying the legacy visual-only gate.
	ArtifactRoleTypedVerificationEvidence ArtifactRole = "typed_verification_evidence"
)

func (r ArtifactRole) Valid() bool {
	return r == ArtifactRoleTaskContext || r == ArtifactRoleGeneratedAudit ||
		r == ArtifactRoleGeneratedOutput || r == ArtifactRoleVerificationEvidence || r == ArtifactRoleTypedVerificationEvidence
}

func (r ArtifactRole) ModelInputEligible() bool {
	// Empty is the compatibility value for artifacts created by an older
	// in-memory process. Persisted links are backfilled by migration 027.
	return r == "" || r == ArtifactRoleTaskContext
}

const (
	MaxVerificationScreenshotBytes int64 = 10 << 20
	MaxVerificationRecordingBytes  int64 = 25 << 20
)

var verificationEvidenceLimits = map[string]int64{
	"image/jpeg": MaxVerificationScreenshotBytes,
	"image/png":  MaxVerificationScreenshotBytes,
	"image/webp": MaxVerificationScreenshotBytes,
	"video/mp4":  MaxVerificationRecordingBytes,
	"video/webm": MaxVerificationRecordingBytes,
}

// NormalizeVerificationEvidenceContentType is the control-plane eligibility
// policy. Parameters and casing are normalized; filenames are never consulted.
func NormalizeVerificationEvidenceContentType(contentType string, sizeBytes int64) (string, error) {
	normalized, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return "", fmt.Errorf("verification evidence content type %q is invalid", contentType)
	}
	normalized = strings.ToLower(normalized)
	limit, ok := verificationEvidenceLimits[normalized]
	if !ok {
		return "", fmt.Errorf("verification evidence content type %q is unsupported; use image/png, image/jpeg, image/webp, video/mp4, or video/webm", normalized)
	}
	if sizeBytes <= 0 {
		return "", fmt.Errorf("verification evidence must not be empty")
	}
	if sizeBytes > limit {
		return "", fmt.Errorf("verification evidence %s exceeds the %d byte limit", normalized, limit)
	}
	return normalized, nil
}

func (a Artifact) EligibleVerificationEvidence() bool {
	if a.Role != ArtifactRoleVerificationEvidence || a.TaskID == "" {
		return false
	}
	_, err := NormalizeVerificationEvidenceContentType(a.ContentType, a.SizeBytes)
	return err == nil
}

type Artifact struct {
	ID          string       `json:"id"` // sha256 content address
	Workspace   string       `json:"workspace"`
	Name        string       `json:"name"`
	ContentType string       `json:"content_type"`
	SizeBytes   int64        `json:"size_bytes"`
	Role        ArtifactRole `json:"role"`
	TaskID      string       `json:"task_id,omitempty"`
	// RequirementID is how a finalized planning transcript attaches to the
	// requirement it produced. At most one of TaskID, RequirementID, and
	// PlanningSessionID may be set (component-artifacts).
	RequirementID string `json:"requirement_id,omitempty"`
	// PlanningSessionID owns files uploaded while a planning conversation is
	// still active, including sessions that do not yet have requirement context.
	PlanningSessionID string    `json:"planning_session_id,omitempty"`
	DownloadURL       string    `json:"download_url,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// ValidateAttachmentTarget keeps the attachment owner exclusive, mirroring the
// artifact_links CHECK. An artifact may float unattached at workspace scope,
// but it never claims two owners.
func (a Artifact) ValidateAttachmentTarget() error {
	targets := 0
	for _, id := range []string{a.TaskID, a.RequirementID, a.PlanningSessionID} {
		if id != "" {
			targets++
		}
	}
	if targets > 1 {
		return fmt.Errorf("artifact attaches to one of a task, requirement, or planning session, not %d of them", targets)
	}
	return nil
}

// SpecVersion is an immutable spec-agent artifact. Approval is recorded on
// the exact version that becomes the implementation contract.
// ReviewSeats is the version's planned review seat count, nil when the plan
// stated none (feature-review-seat-minimum RS-2, RS-3).
type SpecVersion struct {
	TaskID               string          `json:"task_id"`
	Version              int             `json:"version"`
	Content              string          `json:"content"`
	AcceptanceCount      int             `json:"acceptance_count"`
	Acceptance           json.RawMessage `json:"acceptance"`
	Decomposition        json.RawMessage `json:"decomposition"`
	MaterializedChildren []TaskRelation  `json:"materialized_children,omitempty"`
	Agent                string          `json:"agent,omitempty"`
	Model                string          `json:"model,omitempty"`
	Approved             bool            `json:"approved"`
	ReviewSeats          *int            `json:"review_seats,omitempty"`
	// Origin names the act that recorded the version, and BaseCommit is the
	// full commit ID the plan was read against, empty when none was recorded
	// (component-plan-state; req-task-plan-state AC-2.5).
	Origin     PlanOrigin `json:"origin"`
	BaseCommit *string    `json:"base_commit"`
	// LegacyGate is true only for an unapproved pre-Phase-8.3 version captured
	// by migration 075. It grants approval-time compatibility, never creation.
	LegacyGate bool      `json:"legacy_gate,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ApprovedAt time.Time `json:"approved_at,omitempty"`
	// RiskTier and RiskReason are the risk tier and reason the version's plan
	// states, read from Content with the shared plan grammar when a read
	// serves the version; no column stores them. Both are empty when the plan
	// states none (req-risk-based-review AC-4.4; feature-risk-based-review
	// RR-4).
	RiskTier   riskpolicy.Tier `json:"risk_tier,omitempty"`
	RiskReason string          `json:"risk_reason,omitempty"`
}

// JSONPayload is the single fallback contract for JSON stored inside events.
// Domain payloads should always marshal; if a future caller supplies an
// unsupported value, preserve the event without panicking the control plane.
func JSONPayload(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"marshal_error":true}`)
	}
	return data
}

// VerificationDelivery is the mutable PR delivery projection (component-verification-publication); source publication rows remain immutable.
type VerificationDelivery struct {
	WorkspaceID, Repository, TaskID, ContextID, SourcePublicationID            string
	ID, IdempotencyKey, TargetHead, ObservedHead, TargetDigest, ObservedDigest string
	Generation                                                                 int
	SourceGeneration                                                           int
	PullRequestNumber                                                          int
	State                                                                      string
	CycleAttempts, Attempts                                                    int
	ErrorClass, ErrorMessage                                                   string
	CreatedAt, UpdatedAt                                                       time.Time
	LastAttemptAt, NextAttemptAt                                               *time.Time
	Summary                                                                    string
}
