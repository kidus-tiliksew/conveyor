// Package core defines the shared domain types for Conveyor: tasks, jobs,
// pipeline stages, and their states. These mirror the data model in
// conveyor-spec.md §16. Postgres is the Phase 2 source of truth; the memory
// store remains an explicit test/development implementation.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
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

func InitialStage(level EscalationLevel) Stage {
	if level == "" {
		return StageImplement
	}
	return StageTriage
}

// TaskMode is the retired auto/manual execution mode. It
// survives only to parse deprecated intake input and render historical
// records; behavior is governed by Task.Hold.
type TaskMode string

const (
	TaskModeAuto   TaskMode = "auto"
	TaskModeManual TaskMode = "manual"
)

func (m TaskMode) Valid() bool { return m == TaskModeAuto || m == TaskModeManual }

// LegacyPolicy preserves the accepted L0-L3 meaning for historical callers:
// hold (the §21.31 successor of Manual), spec approval, merge approval.
func LegacyPolicy(level EscalationLevel) (bool, bool, bool) {
	switch level {
	case L0:
		return false, false, false
	case L1:
		return false, false, true
	case L2:
		return false, true, true
	default:
		return true, true, true
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
	ID                 string                `json:"id"`
	Workspace          string                `json:"workspace"`
	Source             string                `json:"source"` // provenance: github:<slug>#<n>, cli, cron, monitor
	IntakeKey          string                `json:"-"`      // workspace-scoped MCP retry key
	Title              string                `json:"title"`
	Body               string                `json:"body"`  // free-form description; becomes part of the prompt
	Class              string                `json:"class"` // bug | feature | chore
	Level              EscalationLevel       `json:"level"`
	Mode               TaskMode              `json:"mode,omitempty"` // legacy historical record; never read for behavior
	Hold               bool                  `json:"hold,omitempty"` // reservation from the worker daemon
	SpecApproval       bool                  `json:"spec_approval"`
	MergeApproval      bool                  `json:"merge_approval"`
	PolicyVersion      int                   `json:"policy_version"`
	SetupName          string                `json:"setup"`
	SetupContract      config.ExecutionSetup `json:"setup_contract"`
	ReviewedHeadSHA    string                `json:"reviewed_head_sha,omitempty"`
	ApprovedHeadSHA    string                `json:"approved_head_sha,omitempty"`
	ApprovalStale      bool                  `json:"approval_stale,omitempty"`
	RefreshBaselineSHA string                `json:"refresh_baseline_sha,omitempty"`
	RefreshHeadSHA     string                `json:"refresh_head_sha,omitempty"`
	RefreshReviewScope string                `json:"refresh_review_scope,omitempty"`
	Repo               string                `json:"repo"` // repo name within the workspace; multi-repo sets are Phase 8
	BaseBranch         string                `json:"base_branch"`
	Branch             string                `json:"branch"` // assigned conveyor/task-<id> name; the ref may not exist yet
	State              TaskState             `json:"state"`
	NextStage          Stage                 `json:"next_stage,omitempty"`     // durable pipeline transition selected at the preceding gate
	RecoveryStage      Stage                 `json:"recovery_stage,omitempty"` // explicit human redirect/pull target while the pipeline is halted
	ParentTaskID       string                `json:"parent_task_id,omitempty"` // blueprint parent
	OriginSpecVersion  int                   `json:"origin_spec_version,omitempty"`
	OriginSubID        string                `json:"origin_sub_id,omitempty"`
	Dependencies       []TaskRelation        `json:"dependencies,omitempty"`
	BlockingTaskIDs    []string              `json:"blocking_task_ids,omitempty"`
	Children           []TaskRelation        `json:"children,omitempty"`
	Context            TaskContext           `json:"context,omitempty"`
	// FeatureID is deprecated migration history. Live task context and child
	// materialization use requirement/lineage records instead.
	FeatureID string           `json:"feature_id,omitempty"`
	GitHub    *GitHubLifecycle `json:"github,omitempty"` // durable forge projection
	CreatedAt time.Time        `json:"created_at"`
}

// TaskContext is the operator-attached desired-state authority carried by a
// task independently of blueprint ancestry.
type TaskContext struct {
	Requirements []TaskRequirementContext `json:"requirements,omitempty"`
	Designs      []TaskDesignContext      `json:"designs,omitempty"`
}

type TaskRequirementContext struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Version int    `json:"version"`
}

type TaskDesignContext struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Version int    `json:"version"`
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
	CreatedAt          time.Time              `json:"created_at"`
	UpdatedAt          time.Time              `json:"updated_at"`
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
)

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

// HarnessSnapshot is the immutable worker execution contract captured for a
// spec, implementation, or review order when it is created. Workspace hot reloads
// must not alter an in-flight command, model arguments, effort arguments, or
// health probe; on queue re-entry the server re-resolves the
// snapshot from the current registry.
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

// RefreshedHarnessSnapshot re-resolves a pinned harness snapshot against the
// current registry for an order re-entering the queue. The
// pinned effort is preserved; the refresh is refused — retaining the prior
// snapshot — when the name is gone, the pinned effort is no longer declared,
// or the current definition is already identical.
func RefreshedHarnessSnapshot(harnesses []config.Harness, prior *HarnessSnapshot) (*HarnessSnapshot, bool) {
	if prior == nil || prior.Name == "" {
		return nil, false
	}
	for _, harness := range harnesses {
		if harness.Name != prior.Name {
			continue
		}
		next := &HarnessSnapshot{
			Name:                  harness.Name,
			MCPTransport:          harness.MCPTransport,
			MCPAttachment:         harness.MCPAttachment,
			Command:               append([]string(nil), harness.Command...),
			ModelArgs:             append([]string(nil), harness.ModelArgs...),
			DefaultModelSentinels: append([]string(nil), harness.DefaultModelSentinels...),
			EffortArgs:            cloneEffortArgs(harness.EffortArgs),
			Effort:                prior.Effort,
			ProbeCommand:          append([]string(nil), harness.ProbeCommand...),
			ProbeTimeoutText:      harness.ProbeTimeoutText,
			StallTimeoutText:      harness.StallTimeoutText,
		}
		if prior.Effort != "" {
			argv := harness.EffortArgs[prior.Effort]
			if len(argv) == 0 {
				return nil, false
			}
			next.EffortArgv = append([]string(nil), argv...)
		}
		if reflect.DeepEqual(next, prior) {
			return nil, false
		}
		return next, true
	}
	return nil, false
}

func cloneEffortArgs(source map[string][]string) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string][]string, len(source))
	for effort, args := range source {
		result[effort] = append([]string(nil), args...)
	}
	return result
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

type RateLimitHealth struct {
	WorkOrderID string          `json:"work_order_id"`
	WorkerID    string          `json:"worker_id,omitempty"`
	Harness     string          `json:"harness"`
	Model       string          `json:"model,omitempty"`
	RateLimit   RateLimitStatus `json:"rate_limit"`
	ObservedAt  time.Time       `json:"observed_at"`
}

// WorkOrder is the durable protocol boundary between Conveyor and an
// operator-owned spec, implementation, or review agent.
type WorkOrder struct {
	ID                     string           `json:"id"`
	TaskID                 string           `json:"task_id"`
	JobID                  string           `json:"job_id"`
	Stage                  Stage            `json:"stage"`
	State                  WorkOrderState   `json:"state"`
	Claimable              bool             `json:"claimable"`
	BlockingTaskIDs        []string         `json:"blocking_task_ids,omitempty"`
	UnsatisfiableTaskIDs   []string         `json:"unsatisfiable_task_ids,omitempty"`
	ClaimantID             string           `json:"claimed_by,omitempty"`
	SessionID              string           `json:"session_id,omitempty"`
	AttemptID              string           `json:"attempt_id,omitempty"`
	ClientTokenHash        string           `json:"-"`
	Agent                  string           `json:"agent,omitempty"`
	Model                  string           `json:"model,omitempty"`
	WorkerID               string           `json:"worker_id,omitempty"`
	ReviewRound            int              `json:"review_round,omitempty"`
	ReviewSeat             int              `json:"review_seat,omitempty"`
	ReasonCode             string           `json:"reason_code,omitempty"`
	ReviewKind             string           `json:"review_kind,omitempty"`
	ReviewScope            string           `json:"review_scope,omitempty"`
	BaselineSHA            string           `json:"baseline_sha,omitempty"`
	HeadSHA                string           `json:"head_sha,omitempty"`
	RequiredModel          string           `json:"required_model,omitempty"`
	RequiredHarness        string           `json:"required_harness,omitempty"`
	RequiredEffort         string           `json:"required_effort,omitempty"`
	RequiredHarnessConfig  *HarnessSnapshot `json:"required_harness_config,omitempty"`
	ExecutionTimeoutText   string           `json:"execution_timeout,omitempty"`
	ModelEnforcement       string           `json:"model_enforcement,omitempty"`
	LeaseExpiresAt         time.Time        `json:"lease_expires_at,omitempty"`
	QueueEnteredAt         time.Time        `json:"queue_entered_at"`
	QueueDeadline          time.Time        `json:"queue_deadline"`
	QueueBlockedAt         time.Time        `json:"queue_blocked_at,omitempty"`
	ExecutionStartedAt     time.Time        `json:"execution_started_at,omitempty"`
	ExecutionDeadline      time.Time        `json:"execution_deadline,omitempty"`
	LastAttemptID          string           `json:"last_attempt_id,omitempty"`
	LastAttemptOutcome     string           `json:"last_attempt_outcome,omitempty"`
	LastFailureCategory    string           `json:"last_failure_category,omitempty"`
	LastFailureMessage     string           `json:"last_failure_message,omitempty"`
	LastFailureDetail      string           `json:"last_failure_detail,omitempty"`
	LastFailureExitStatus  *int             `json:"last_failure_exit_status,omitempty"`
	LastFailureAt          time.Time        `json:"last_failure_at,omitempty"`
	AutomaticRetryCount    int              `json:"automatic_retry_count"`
	NextRetryAt            time.Time        `json:"next_retry_at,omitempty"`
	RetrySuppressed        bool             `json:"retry_suppressed"`
	RetrySuppressionReason string           `json:"retry_suppression_reason,omitempty"`
	RedispatchCount        int              `json:"redispatch_count"`
	OperatorDirection      string           `json:"operator_direction,omitempty"`
	Progress               string           `json:"progress,omitempty"`
	CostUSD                float64          `json:"cost_usd"`
	TokensIn               int64            `json:"tokens_in"`
	TokensOut              int64            `json:"tokens_out"`
	UsageReported          bool             `json:"usage_reported"`
	SelfReported           bool             `json:"self_reported"`
	RateLimit              *RateLimitStatus `json:"rate_limit,omitempty"`
	RateLimitObservedAt    time.Time        `json:"rate_limit_observed_at,omitempty"`
	LastAgentActivityAt    time.Time        `json:"last_agent_activity_at,omitempty"`
	LastAgentActivityLabel string           `json:"last_agent_activity_label,omitempty"`
	CreatedAt              time.Time        `json:"created_at"`
	UpdatedAt              time.Time        `json:"updated_at"`
	// ServedRequirementSnapshot is the citation authority rendered for this
	// review order. A non-nil empty slice means the task had no served
	// requirements; nil is reserved for pre-snapshot compatibility handling.
	ServedRequirementSnapshot []ServedRequirementContext `json:"served_requirement_snapshot,omitempty"`
	// GovernanceSnapshot is the exact design and decision authority plus the
	// separately non-authoritative task proposal evidence refreshed for this
	// review claim. Nil is reserved for legacy compatibility handling.
	GovernanceSnapshot *GovernanceSnapshot `json:"governance_snapshot,omitempty"`
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
	Requirements     []ServedRequirementContext
	Governance       *GovernanceSnapshot
}

const (
	WorkOrderOutcomeChildFailure                    = "child_failure"
	WorkOrderOutcomeStalled                         = "stalled"
	WorkOrderOutcomeReleased                        = "released"
	WorkOrderOutcomeCancelled                       = "cancelled"
	WorkOrderOutcomeExpired                         = "expired"
	WorkOrderOutcomePreempted                       = "preempted"
	WorkOrderReleaseReasonOperatorCheckpointReached = "operator checkpoint reached"
	WorkOrderReleaseCauseSessionExit                = "session_exit"
	WorkOrderReleaseCauseOperatorAction             = "operator_action"
	WorkOrderReleaseCauseLeaseLoss                  = "lease_loss"
	IdenticalFailureSuppressionReason               = "identical failure output on consecutive attempts"
	WorkOrderFailureProviderUsageLimit              = "provider_usage_limit"
	WorkOrderFailureTransientConnectivity           = "transient_connectivity"
	WorkOrderFailureDetailLimit                     = 2 * 1024
)

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
	Cause               string `json:"release_cause,omitempty"`
	Outcome             string
	FailureCategory     string
	ExitStatus          *int
	FailureDetail       string
	ModelRejection      bool
	InitialRetryDelay   time.Duration
	MaximumRetryDelay   time.Duration
	AutomaticRetryLimit int
}

// WorkOrderAttemptCheckpoint identifies one additive Git preservation commit
// made for an implementation attempt. SessionID authorizes the caller's
// current claim; AttemptID identifies the predecessor work being preserved.
type WorkOrderAttemptCheckpoint struct {
	SessionID         string `json:"session_id"`
	AttemptID         string `json:"attempt_id"`
	TerminationReason string `json:"termination_reason"`
	CommitSHA         string `json:"commit_sha"`
	PushResult        string `json:"push_result"`
}

// AuthorizesAttemptCheckpoint permits either the active attempt to preserve
// its own work or its active successor to preserve the immediately recorded
// predecessor. It never grants authority to an inactive or different task
// session.
func (w WorkOrder) AuthorizesAttemptCheckpoint(workerID string, checkpoint WorkOrderAttemptCheckpoint, now time.Time) bool {
	if w.Stage != StageImplement || w.State != WorkOrderClaimed || w.WorkerID != workerID ||
		w.SessionID == "" || w.SessionID != checkpoint.SessionID || !w.LeaseExpiresAt.After(now) {
		return false
	}
	return checkpoint.AttemptID != "" && (checkpoint.AttemptID == w.AttemptID || checkpoint.AttemptID == w.LastAttemptID)
}

// HarnessModelFailure is retained evidence that one frozen harness/model pair
// was rejected by its provider. It is advisory only and never changes routing.
type HarnessModelFailure struct {
	Harness     string    `json:"harness"`
	Model       string    `json:"model"`
	Detail      string    `json:"detail"`
	WorkOrderID string    `json:"work_order_id"`
	ObservedAt  time.Time `json:"observed_at"`
}

type HarnessProbe struct {
	Harness     string    `json:"harness"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Healthy     bool      `json:"healthy"`
	Message     string    `json:"message,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
	Transition  string    `json:"transition,omitempty"`
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
	// CheckRunID is retained for historical v1.22 publications. Portable v1.23
	// commit-status publications leave it zero.
	CheckRunID         int64     `json:"check_run_id,omitempty"`
	CommentID          int64     `json:"comment_id,omitempty"`
	ForgeErrorCategory string    `json:"forge_error_category,omitempty"`
	LastError          string    `json:"last_error,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
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
	EvidenceIDs            []string
	RequirementCitations   *RequirementCitationAssessment
	DoneCriteriaAssessment *DoneCriteriaAssessment
	GovernanceAssessment   *GovernanceAssessment
	Reviewer               string
	ReviewerModel          string
	ReviewerSession        string
	ClaimSession           string
	SameModelAsImplementer string
	ReviewRound            int
	ReviewSeat             int
	ReviewKind             string
	ReviewScope            string
	BaselineSHA            string
	HeadSHA                string
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
)

func (r ArtifactRole) Valid() bool {
	return r == ArtifactRoleTaskContext || r == ArtifactRoleGeneratedAudit ||
		r == ArtifactRoleGeneratedOutput || r == ArtifactRoleVerificationEvidence
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
	if a.Role != ArtifactRoleVerificationEvidence || a.TaskID == "" || a.FeatureID != "" {
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
	// FeatureID remains readable only for pre-retirement persistence
	// compatibility; live attachment creation targets tasks or requirements.
	FeatureID string `json:"feature_id,omitempty"`
	// RequirementID is the attachment target that replaces FeatureID as the
	// feature tree retires. It is how a finalized
	// planning transcript attaches to the requirement it produced (§9), and
	// where migration 046 re-homes feature-scoped attachments. Exactly one of
	// TaskID, FeatureID, RequirementID, and PlanningSessionID may be set.
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
	for _, id := range []string{a.TaskID, a.FeatureID, a.RequirementID, a.PlanningSessionID} {
		if id != "" {
			targets++
		}
	}
	if targets > 1 {
		return fmt.Errorf("artifact attaches to one of a task, feature, requirement, or planning session, not %d of them", targets)
	}
	return nil
}

// SpecVersion is an immutable spec-agent artifact. Approval is recorded on
// the exact version that becomes the implementation contract.
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
	// LegacyGate is true only for an unapproved pre-Phase-8.3 version captured
	// by migration 075. It grants approval-time compatibility, never creation.
	LegacyGate bool      `json:"legacy_gate,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ApprovedAt time.Time `json:"approved_at,omitempty"`
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
