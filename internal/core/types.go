// Package core defines the shared domain types for Conveyor: tasks, jobs,
// pipeline stages, and their states. These mirror the data model in
// conveyor-spec.md §16. Postgres is the Phase 2 source of truth; the memory
// store remains an explicit test/development implementation.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Stage is a pipeline stage (spec §4). Phase 1 exercises only Implement;
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

func InitialStage(level EscalationLevel) Stage {
	if level == "" {
		return StageImplement
	}
	return StageTriage
}

// TaskMode controls whether an enrolled Conveyor worker may claim a task's
// work orders. Human-attached MCP agents may claim either mode (spec §21.13).
type TaskMode string

const (
	TaskModeAuto   TaskMode = "auto"
	TaskModeManual TaskMode = "manual"
)

func (m TaskMode) Valid() bool { return m == TaskModeAuto || m == TaskModeManual }

// LegacyPolicy preserves the accepted L0-L3 meaning for historical callers
// while new intake persists the three independent Phase 5.1 decisions.
func LegacyPolicy(level EscalationLevel) (TaskMode, bool, bool) {
	switch level {
	case L0:
		return TaskModeAuto, false, false
	case L1:
		return TaskModeAuto, false, true
	case L2:
		return TaskModeAuto, true, true
	default:
		return TaskModeManual, true, true
	}
}

func LegacyLevel(mode TaskMode, specApproval, mergeApproval bool) EscalationLevel {
	if mode == TaskModeManual {
		return L3
	}
	if specApproval {
		return L2
	}
	if mergeApproval {
		return L1
	}
	return L0
}

// EscalationLevel is the degree of human involvement (spec §13.1).
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

// Task is a unit of intended change (spec §2). One task spans many jobs.
type Task struct {
	ID            string           `json:"id"`
	Workspace     string           `json:"workspace"`
	Source        string           `json:"source"` // provenance: github:<slug>#<n>, cli, cron, monitor (spec §9)
	IntakeKey     string           `json:"-"`      // workspace-scoped MCP retry key (spec §21.5)
	Title         string           `json:"title"`
	Body          string           `json:"body"`  // free-form description; becomes part of the prompt
	Class         string           `json:"class"` // bug | feature | chore
	Level         EscalationLevel  `json:"level"`
	Mode          TaskMode         `json:"mode"`
	SpecApproval  bool             `json:"spec_approval"`
	MergeApproval bool             `json:"merge_approval"`
	PolicyVersion int              `json:"policy_version"`
	Repo          string           `json:"repo"` // repo name within the workspace; multi-repo sets are Phase 8
	BaseBranch    string           `json:"base_branch"`
	Branch        string           `json:"branch"` // assigned conveyor/task-<id> name; the ref may not exist yet (spec §21.7)
	State         TaskState        `json:"state"`
	NextStage     Stage            `json:"next_stage,omitempty"`     // durable pipeline transition selected at the preceding gate
	RecoveryStage Stage            `json:"recovery_stage,omitempty"` // explicit human redirect/pull target while the pipeline is halted
	ParentTaskID  string           `json:"parent_task_id,omitempty"` // stacked tasks (spec §8.6)
	FeatureID     string           `json:"feature_id,omitempty"`     // requirements-tree assignment (spec §21.4)
	GitHub        *GitHubLifecycle `json:"github,omitempty"`         // durable forge projection (spec §21.12 change 5)
	CreatedAt     time.Time        `json:"created_at"`
}

type GitHubPublicationState string

const (
	GitHubPublicationQueued    GitHubPublicationState = "queued"
	GitHubPublicationRetrying  GitHubPublicationState = "retrying"
	GitHubPublicationPublished GitHubPublicationState = "published"
	GitHubPublicationFailed    GitHubPublicationState = "failed"
)

// GitHubCreateState records whether a remote create may still be attempted.
// Once reconciling, retries may only look up the exact task marker; this closes
// the ambiguity window where GitHub accepted a create but its acknowledgement
// never reached Conveyor (spec §21.12 change 5, amended by §21.15).
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
	TaskID            string                 `json:"task_id"`
	Repository        string                 `json:"repository"`
	SpecVersion       int                    `json:"spec_version"`
	Source            string                 `json:"source"`
	SourceIssueNumber int                    `json:"source_issue_number,omitempty"`
	IssueNumber       int                    `json:"issue_number,omitempty"`
	IssueURL          string                 `json:"issue_url,omitempty"`
	Outcome           string                 `json:"outcome,omitempty"` // created | reused
	State             GitHubPublicationState `json:"state"`
	CreateState       GitHubCreateState      `json:"create_state"`
	CreateAttempts    int                    `json:"create_attempts"`
	ReconcileMisses   int                    `json:"reconcile_misses"`
	Attempts          int                    `json:"attempts"`
	LastError         string                 `json:"last_error,omitempty"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
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
	CostUSD     float64   `json:"cost_usd"`
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
// identity model instead of retrofitting attribution (spec §16, §18.1).
type ActorRole string

const (
	ActorSystem ActorRole = "system"
	ActorHuman  ActorRole = "human"
	ActorAgent  ActorRole = "agent"
	ActorRunner ActorRole = "runner"
)

func (action InterventionAction) Valid() bool {
	switch action {
	case InterventionApprove, InterventionReject, InterventionRedirect, InterventionPull:
		return true
	default:
		return false
	}
}

// Event is the append-only source of truth for task state. Tasks and jobs are
// transactional projections optimized for queries (spec §3.1, §16).
type Event struct {
	ID        int64           `json:"id"`
	TaskID    string          `json:"task_id"`
	JobID     string          `json:"job_id,omitempty"`
	Kind      string          `json:"kind"`
	ActorID   string          `json:"actor_id"`
	ActorRole ActorRole       `json:"actor_role"`
	Payload   json.RawMessage `json:"payload"`
	At        time.Time       `json:"at"`
}

type InterventionAction string

const (
	InterventionApprove  InterventionAction = "approve"
	InterventionReject   InterventionAction = "reject"
	InterventionRedirect InterventionAction = "redirect"
	InterventionPull     InterventionAction = "pull_to_local"
)

// Intervention is a structured human decision from the review queue (spec
// §13.2). ReasonCode is intentionally data rather than an enum so workspaces
// can add taxonomy without a deploy; the API validates the required baseline.
type Intervention struct {
	ID         int64              `json:"id"`
	TaskID     string             `json:"task_id"`
	JobID      string             `json:"job_id,omitempty"`
	ActorID    string             `json:"actor_id"`
	ActorRole  ActorRole          `json:"actor_role"`
	Action     InterventionAction `json:"action"`
	ReasonCode string             `json:"reason_code"`
	Comment    string             `json:"comment"`
	At         time.Time          `json:"at"`
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

// HarnessSnapshot is the immutable worker execution contract captured for a
// implementation or review order when it is created. Workspace hot reloads
// must not alter an in-flight command, model arguments, effort arguments, or
// health probe (spec §21.19).
type HarnessSnapshot struct {
	Name                  string              `json:"name"`
	MCPTransport          string              `json:"mcp_transport"`
	Command               []string            `json:"command"`
	ModelArgs             []string            `json:"model_args,omitempty"`
	DefaultModelSentinels []string            `json:"default_model_sentinels,omitempty"`
	EffortArgs            map[string][]string `json:"effort_args,omitempty"`
	Effort                string              `json:"effort,omitempty"`
	EffortArgv            []string            `json:"effort_argv,omitempty"`
	ProbeCommand          []string            `json:"probe_command"`
	ProbeTimeoutText      string              `json:"probe_timeout"`
}

// WorkOrder is the durable protocol boundary between Conveyor and an
// operator-owned implementation or review agent (spec §21.4).
type WorkOrder struct {
	ID                    string           `json:"id"`
	TaskID                string           `json:"task_id"`
	JobID                 string           `json:"job_id"`
	Stage                 Stage            `json:"stage"`
	State                 WorkOrderState   `json:"state"`
	Claimable             bool             `json:"claimable"`
	ClaimantID            string           `json:"claimed_by,omitempty"`
	SessionID             string           `json:"session_id,omitempty"`
	ClientTokenHash       string           `json:"-"`
	Agent                 string           `json:"agent,omitempty"`
	Model                 string           `json:"model,omitempty"`
	WorkerID              string           `json:"worker_id,omitempty"`
	ReviewRound           int              `json:"review_round,omitempty"`
	ReviewSeat            int              `json:"review_seat,omitempty"`
	RequiredModel         string           `json:"required_model,omitempty"`
	RequiredHarness       string           `json:"required_harness,omitempty"`
	RequiredEffort        string           `json:"required_effort,omitempty"`
	RequiredHarnessConfig *HarnessSnapshot `json:"required_harness_config,omitempty"`
	ExecutionTimeoutText  string           `json:"execution_timeout,omitempty"`
	ModelEnforcement      string           `json:"model_enforcement,omitempty"`
	LeaseExpiresAt        time.Time        `json:"lease_expires_at,omitempty"`
	QueueEnteredAt        time.Time        `json:"queue_entered_at"`
	QueueDeadline         time.Time        `json:"queue_deadline"`
	ExecutionStartedAt    time.Time        `json:"execution_started_at,omitempty"`
	ExecutionDeadline     time.Time        `json:"execution_deadline,omitempty"`
	LastAttemptOutcome    string           `json:"last_attempt_outcome,omitempty"`
	LastFailureMessage    string           `json:"last_failure_message,omitempty"`
	LastFailureExitStatus *int             `json:"last_failure_exit_status,omitempty"`
	LastFailureAt         time.Time        `json:"last_failure_at,omitempty"`
	AutomaticRetryCount   int              `json:"automatic_retry_count"`
	NextRetryAt           time.Time        `json:"next_retry_at,omitempty"`
	RetrySuppressed       bool             `json:"retry_suppressed"`
	RedispatchCount       int              `json:"redispatch_count"`
	Progress              string           `json:"progress,omitempty"`
	CostUSD               float64          `json:"cost_usd"`
	TokensIn              int64            `json:"tokens_in"`
	TokensOut             int64            `json:"tokens_out"`
	SelfReported          bool             `json:"self_reported"`
	CreatedAt             time.Time        `json:"created_at"`
	UpdatedAt             time.Time        `json:"updated_at"`
}

// MarshalJSON keeps the three work-order clocks distinct on the wire and
// omits clocks that have not started. A queued order must not look as though
// execution or a claim lease has begun.
func (w WorkOrder) MarshalJSON() ([]byte, error) {
	type workOrderAlias WorkOrder
	wired := struct {
		workOrderAlias
		LeaseExpiresAt     *time.Time `json:"lease_expires_at,omitempty"`
		ExecutionStartedAt *time.Time `json:"execution_started_at,omitempty"`
		ExecutionDeadline  *time.Time `json:"execution_deadline,omitempty"`
		LastFailureAt      *time.Time `json:"last_failure_at,omitempty"`
		NextRetryAt        *time.Time `json:"next_retry_at,omitempty"`
	}{workOrderAlias: workOrderAlias(w)}
	if !w.LeaseExpiresAt.IsZero() {
		wired.LeaseExpiresAt = &w.LeaseExpiresAt
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
	return json.Marshal(wired)
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
	WorkerID         string
}

const (
	WorkOrderOutcomeChildFailure = "child_failure"
	WorkOrderOutcomeReleased     = "released"
	WorkOrderOutcomeCancelled    = "cancelled"
	WorkOrderOutcomeExpired      = "expired"
)

type WorkOrderRelease struct {
	SessionID           string
	Reason              string
	Outcome             string
	ExitStatus          *int
	InitialRetryDelay   time.Duration
	MaximumRetryDelay   time.Duration
	AutomaticRetryLimit int
}

type HarnessProbe struct {
	Harness     string    `json:"harness"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Healthy     bool      `json:"healthy"`
	Message     string    `json:"message,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

type WorkerPairing struct {
	TokenHash  string    `json:"-"`
	Workspace  string    `json:"workspace"`
	ExpiresAt  time.Time `json:"expires_at"`
	ConsumedAt time.Time `json:"consumed_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Worker struct {
	ID             string         `json:"id"`
	Workspace      string         `json:"workspace"`
	Name           string         `json:"name"`
	CredentialHash string         `json:"-"`
	LeaseExpiresAt time.Time      `json:"lease_expires_at,omitempty"`
	LastSeenAt     time.Time      `json:"last_seen_at,omitempty"`
	RevokedAt      time.Time      `json:"revoked_at,omitempty"`
	Probes         []HarnessProbe `json:"probes"`
	CreatedAt      time.Time      `json:"created_at"`
}

func (w Worker) Live(at time.Time) bool {
	return w.RevokedAt.IsZero() && w.LeaseExpiresAt.After(at)
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
	CheckRunID             int64                  `json:"check_run_id,omitempty"`
	CommentID              int64                  `json:"comment_id,omitempty"`
	LastError              string                 `json:"last_error,omitempty"`
	CreatedAt              time.Time              `json:"created_at"`
	UpdatedAt              time.Time              `json:"updated_at"`
}

// ReviewDecision is the atomic internal acceptance record for one completed
// review attempt. GitHub publication is queued in the same store transaction;
// the external GitHub side effects remain asynchronous.
type ReviewDecision struct {
	TaskID                 string
	JobID                  string
	ReviewWorkOrderID      string
	Verdict                string
	ReasonCode             string
	Summary                string
	Feedback               string
	ReviewedCommitSHA      string
	Reviewer               string
	ReviewerModel          string
	ReviewerSession        string
	SameModelAsImplementer string
	ReviewRound            int
	ReviewSeat             int
	RequiredModel          string
	RequiredHarness        string
	RequiredEffort         string
	ModelEnforcement       string
	InterventionActorID    string
	PublicationEligible    bool
	Level                  EscalationLevel
	PolicyVersion          int
	MergeApproval          bool
	MaxBounces             int
}

type Feature struct {
	ID          string    `json:"id"`
	Workspace   string    `json:"workspace"`
	ParentID    string    `json:"parent_id,omitempty"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Artifact struct {
	ID          string    `json:"id"` // sha256 content address
	Workspace   string    `json:"workspace"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	TaskID      string    `json:"task_id,omitempty"`
	FeatureID   string    `json:"feature_id,omitempty"`
	DownloadURL string    `json:"download_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// SpecVersion is an immutable spec-agent artifact. Approval is recorded on
// the exact version that becomes the implementation contract (spec §4.1).
type SpecVersion struct {
	TaskID          string          `json:"task_id"`
	Version         int             `json:"version"`
	Content         string          `json:"content"`
	AcceptanceCount int             `json:"acceptance_count"`
	Acceptance      json.RawMessage `json:"acceptance"`
	Decomposition   json.RawMessage `json:"decomposition"`
	Approved        bool            `json:"approved"`
	CreatedAt       time.Time       `json:"created_at"`
	ApprovedAt      time.Time       `json:"approved_at,omitempty"`
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
