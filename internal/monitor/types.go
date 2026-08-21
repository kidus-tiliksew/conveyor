// Package monitor implements the Phase 5.6 repository observer. It turns
// external GitHub signals into ordinary task intake requests; it never owns a
// pipeline stage, worker claim, review, merge, or deployment capability.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

type SignalKind string

const (
	PostMergeFailure SignalKind = "post_merge_failure"
	DirectPush       SignalKind = "direct_push"
	ExternalPRMerge  SignalKind = "external_pr_merge"
	LineagedMerge    SignalKind = "lineaged_merge"
	Revert           SignalKind = "revert"
)

func (k SignalKind) Valid() bool {
	switch k {
	case PostMergeFailure, DirectPush, ExternalPRMerge, LineagedMerge, Revert:
		return true
	default:
		return false
	}
}

func (k SignalKind) Drift() bool {
	return k == DirectPush || k == ExternalPRMerge || k == LineagedMerge || k == Revert
}

type Observation struct {
	WorkspaceID       string            `json:"workspace_id"`
	Repository        string            `json:"repository"`
	Kind              SignalKind        `json:"kind"`
	OccurrenceID      string            `json:"occurrence_id"`
	SourceURL         string            `json:"source_url"`
	CommitSHA         string            `json:"commit_sha,omitempty"`
	PullRequestNumber int               `json:"pull_request_number,omitempty"`
	CheckRunID        string            `json:"check_run_id,omitempty"`
	Attempt           int               `json:"attempt,omitempty"`
	RecoveryObserved  bool              `json:"recovery_observed,omitempty"`
	RequirementID     string            `json:"requirement_id,omitempty"`
	ChangedPaths      []string          `json:"changed_paths,omitempty"`
	CausalEventID     int64             `json:"causal_event_id,omitempty"`
	ObservedAt        time.Time         `json:"observed_at"`
	Context           map[string]string `json:"context,omitempty"`
	Hints             *HintContext      `json:"hints,omitempty"`
}

func (o *Observation) Normalize(workspace string, now time.Time) error {
	o.WorkspaceID = strings.TrimSpace(o.WorkspaceID)
	o.Repository = strings.TrimSpace(o.Repository)
	o.OccurrenceID = strings.TrimSpace(o.OccurrenceID)
	o.SourceURL = strings.TrimSpace(o.SourceURL)
	o.CommitSHA = strings.TrimSpace(o.CommitSHA)
	o.CheckRunID = strings.TrimSpace(o.CheckRunID)
	o.RequirementID = strings.TrimSpace(o.RequirementID)
	paths := make([]string, 0, len(o.ChangedPaths))
	for _, changed := range o.ChangedPaths {
		changed = strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(changed, "\\", "/")), "./")
		if changed == "" || strings.HasPrefix(changed, "/") || changed == ".." || strings.HasPrefix(changed, "../") || strings.Contains(changed, "/../") {
			return fmt.Errorf("changed path %q must be repository-relative", changed)
		}
		paths = append(paths, changed)
	}
	sort.Strings(paths)
	o.ChangedPaths = compactStrings(paths)
	if o.WorkspaceID == "" {
		o.WorkspaceID = workspace
	}
	if o.WorkspaceID == "" || o.WorkspaceID != workspace {
		return errors.New("observation workspace must match the immutable workspace context")
	}
	if o.RecoveryObserved && o.Kind != PostMergeFailure {
		return errors.New("recovery_observed is only valid for post_merge_failure")
	}
	if o.Repository == "" || !o.Kind.Valid() || o.OccurrenceID == "" || o.SourceURL == "" {
		return errors.New("repository, supported kind, occurrence_id, and source_url are required")
	}
	parsedSource, err := url.Parse(o.SourceURL)
	if err != nil || (parsedSource.Scheme != "https" && parsedSource.Scheme != "http") || parsedSource.Host == "" || parsedSource.User != nil {
		return errors.New("source_url must be an absolute HTTP(S) URL without embedded credentials")
	}
	redactor := redact.New(nil)
	if clean, _ := redactor.Redact(o.SourceURL); clean != o.SourceURL {
		return errors.New("source_url contains credential-shaped data")
	}
	for key, value := range o.Context {
		lowerKey := strings.ToLower(key)
		if strings.Contains(lowerKey, "token") || strings.Contains(lowerKey, "secret") ||
			strings.Contains(lowerKey, "password") || strings.Contains(lowerKey, "api_key") {
			return fmt.Errorf("context key %q may contain credentials", key)
		}
		clean, _ := redactor.Redact(value)
		o.Context[key] = clean
	}
	if len(o.OccurrenceID) > 200 {
		return errors.New("occurrence_id must be at most 200 characters")
	}
	if o.ObservedAt.IsZero() {
		o.ObservedAt = now.UTC()
	}
	return nil
}

func (o Observation) Identity() string {
	return string(o.Kind) + ":" + o.Repository + ":" + o.OccurrenceID
}

// IntakeKey preserves occurrence-level idempotency. Reuse across distinct
// occurrences is derived separately from the open task for the repository and
// signal class.
func (o Observation) IntakeKey() string {
	return "monitor:" + o.Identity()
}

func (k SignalKind) ReusesOpenTask() bool {
	return k == PostMergeFailure || k == DirectPush || k == ExternalPRMerge
}

type ObservationRecord struct {
	Observation
	TaskID             string    `json:"task_id,omitempty"`
	TaskOutcome        string    `json:"task_outcome,omitempty"`
	State              string    `json:"state"`
	DeduplicatedCount  int       `json:"deduplicated_count"`
	ForgeErrorCategory string    `json:"forge_error_category,omitempty"`
	LastError          string    `json:"last_error,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Drift struct {
	ID                  string     `json:"id"`
	WorkspaceID         string     `json:"workspace_id"`
	Repository          string     `json:"repository"`
	Kind                SignalKind `json:"kind"`
	SourceURL           string     `json:"source_url"`
	CommitSHA           string     `json:"commit_sha,omitempty"`
	RequirementID       string     `json:"requirement_id,omitempty"`
	SystemDesignID      string     `json:"system_design_id,omitempty"`
	SystemDesignVersion int        `json:"system_design_version,omitempty"`
	CausalEventID       int64      `json:"causal_event_id,omitempty"`
	MatchingPaths       []string   `json:"matching_paths,omitempty"`
	TaskID              string     `json:"task_id"`
	DetectedAt          time.Time  `json:"detected_at"`
	ResolvedAt          time.Time  `json:"resolved_at,omitempty"`
	Outcome             string     `json:"outcome,omitempty"`
}

type Status struct {
	WorkspaceID        string              `json:"workspace_id"`
	Enabled            bool                `json:"enabled"`
	LastSuccessfulAt   time.Time           `json:"last_successful_observation,omitempty"`
	CurrentError       string              `json:"current_error,omitempty"`
	ForgeErrorCategory string              `json:"forge_error_category,omitempty"`
	BackoffUntil       time.Time           `json:"backoff_until,omitempty"`
	Observations       []ObservationRecord `json:"observations"`
	Drift              []Drift             `json:"drift"`
	DriftCount         int                 `json:"drift_count"`
	OldestDriftAge     time.Duration       `json:"oldest_drift_age"`
	Activity           []Activity          `json:"activity"`
}

type Activity struct {
	ID          int64          `json:"id"`
	WorkspaceID string         `json:"workspace_id"`
	Kind        string         `json:"kind"`
	Payload     map[string]any `json:"payload"`
	At          time.Time      `json:"at"`
}

type ProposalSuppression struct {
	EventID int64
	Status  string
}

type SystemDesignMergeJudgment struct {
	CausalEventValid bool
	Proposal         ProposalSuppression
	AttachedVersion  int
	Consulted        bool
}

type TaskRequest struct {
	Body               string
	Repository         string
	Source             string
	IntakeKey          string
	ReuseExistingByKey bool
	Hints              *HintContext
}

type IntakeResult struct {
	Task    core.Task
	Created bool
}

type Store interface {
	RequirementExists(context.Context, string) (bool, error)
	Observe(context.Context, Observation) (ObservationRecord, bool, error)
	LinkTask(context.Context, string, string, string) (ObservationRecord, error)
	RecordDrift(context.Context, Drift) (Drift, bool, error)
	ResolveDrift(context.Context, string, string, string) (Drift, error)
	MonitorStatus(context.Context, bool, time.Time) (Status, error)
	RecordMonitorSuccess(context.Context, time.Time) error
	RecordMonitorFailure(context.Context, string, string, time.Time) error
	AuditTask(context.Context, string, string, map[string]any) error
	AuditMonitor(context.Context, string, map[string]any) error
	WithMonitorSignalClassLock(context.Context, string, SignalKind, func(context.Context) error) error
	FindOpenMonitorTask(context.Context, string, SignalKind) (string, bool, error)
	ListSystemDesigns(context.Context) ([]core.SystemDesign, error)
	GetSystemDesignVersion(context.Context, string, int) (core.SystemDesignVersion, error)
	// ResolveCausalSystemDesignMerge atomically resolves the delivering task's
	// same-task proposal and merge-time pinned design attachment. When requested,
	// an attached/no-proposal result appends one retry-safe consulted event. The
	// commit is the PR head SHA recorded in merge.confirmed, not the landed squash
	// or merge-commit SHA.
	ResolveCausalSystemDesignMerge(context.Context, string, string, string, int64, string, []string, bool) (SystemDesignMergeJudgment, error)
}

var (
	ErrUnknownRequirementID    = errors.New("unknown monitor requirement_id")
	ErrRequirementIDMissing    = errors.New("monitor requirement_id is missing")
	ErrRequirementIDInvalid    = errors.New("monitor requirement_id is invalid")
	ErrRequirementIDNotAllowed = errors.New("monitor requirement_id is only allowed for requirements_amended")
)

type Service struct {
	Store            Store
	Intake           func(context.Context, TaskRequest) (IntakeResult, error)
	WorkspaceID      string
	Enabled          bool
	Repositories     map[string]struct{}
	Now              func() time.Time
	ResolveScope     func(context.Context) (workspace string, enabled bool, repositories map[string]struct{}, err error)
	RedactionSecrets redact.SecretSource
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	enabled := s.Enabled
	if s.ResolveScope != nil {
		_, resolved, _, err := s.ResolveScope(ctx)
		if err != nil {
			return Status{}, err
		}
		enabled = resolved
	}
	return s.Store.MonitorStatus(ctx, enabled, now)
}

func (s *Service) Resolve(ctx context.Context, id, outcome, requirementID string) (Drift, error) {
	if s.Store == nil {
		return Drift{}, errors.New("monitor storage is unavailable")
	}
	drift, err := s.Store.ResolveDrift(ctx, strings.TrimSpace(id), strings.TrimSpace(outcome), strings.TrimSpace(requirementID))
	if err == nil {
		_ = s.Store.AuditMonitor(ctx, "monitor.drift_reconciled", map[string]any{
			"drift_id": drift.ID, "task_id": drift.TaskID, "outcome": drift.Outcome, "requirement_id": drift.RequirementID,
		})
	}
	return drift, err
}

// ProcessDesignMerge records a Conveyor-owned merge observation and evaluates
// only system-design drift. The delivery already has a durable task, so this
// path must not create a second intake task.
func (s *Service) ProcessDesignMerge(ctx context.Context, observation Observation, deliveryTaskID string) (ObservationRecord, error) {
	workspace, enabled, repositories := s.WorkspaceID, s.Enabled, s.Repositories
	if s.ResolveScope != nil {
		var err error
		workspace, enabled, repositories, err = s.ResolveScope(ctx)
		if err != nil {
			return ObservationRecord{}, err
		}
	}
	if !enabled {
		return ObservationRecord{}, errors.New("monitor is disabled for this workspace")
	}
	if s.Store == nil || strings.TrimSpace(deliveryTaskID) == "" {
		return ObservationRecord{}, errors.New("monitor storage and delivery task are required")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if err := observation.Normalize(workspace, now); err != nil {
		return ObservationRecord{}, err
	}
	if _, ok := repositories[observation.Repository]; !ok {
		return ObservationRecord{}, fmt.Errorf("repository %q is outside configured monitor scope", observation.Repository)
	}
	record, fresh, err := s.Store.Observe(ctx, observation)
	if err != nil {
		return ObservationRecord{}, err
	}
	_ = s.Store.AuditMonitor(ctx, "system_design.drift_evaluated", map[string]any{
		"identity": observation.Identity(), "delivery_task_id": deliveryTaskID, "causal_event_id": observation.CausalEventID,
		"repository": observation.Repository, "changed_paths": observation.ChangedPaths, "fresh": fresh,
	})
	if err = s.recordSystemDesignDrift(ctx, observation, deliveryTaskID); err != nil {
		return ObservationRecord{}, err
	}
	_ = s.Store.RecordMonitorSuccess(ctx, now)
	return record, nil
}

func (s *Service) Process(ctx context.Context, observation Observation) (ObservationRecord, error) {
	workspace, enabled, repositories := s.WorkspaceID, s.Enabled, s.Repositories
	if s.ResolveScope != nil {
		var err error
		workspace, enabled, repositories, err = s.ResolveScope(ctx)
		if err != nil {
			return ObservationRecord{}, err
		}
	}
	if !enabled {
		return ObservationRecord{}, errors.New("monitor is disabled for this workspace")
	}
	if s.Store == nil || s.Intake == nil {
		return ObservationRecord{}, errors.New("monitor storage or normal task intake is unavailable")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if err := observation.Normalize(workspace, now); err != nil {
		return ObservationRecord{}, err
	}
	if _, ok := repositories[observation.Repository]; !ok {
		return ObservationRecord{}, fmt.Errorf("repository %q is outside configured monitor scope", observation.Repository)
	}
	if observation.RequirementID != "" {
		exists, err := s.Store.RequirementExists(ctx, observation.RequirementID)
		if err != nil {
			return ObservationRecord{}, err
		}
		if !exists {
			return ObservationRecord{}, fmt.Errorf("%w: %s", ErrUnknownRequirementID, observation.RequirementID)
		}
	}
	record, fresh, err := s.Store.Observe(ctx, observation)
	if err != nil {
		return ObservationRecord{}, err
	}
	_ = s.Store.AuditMonitor(ctx, "monitor.observed", map[string]any{
		"identity": observation.Identity(), "repository": observation.Repository,
		"kind": observation.Kind, "source_url": observation.SourceURL, "fresh": fresh,
	})
	if observation.RecoveryObserved {
		if !fresh {
			_ = s.Store.RecordMonitorSuccess(ctx, now)
			return record, nil
		}
		var taskID string
		err = s.Store.WithMonitorSignalClassLock(ctx, observation.Repository, PostMergeFailure, func(lockedCtx context.Context) error {
			var found bool
			taskID, found, err = s.Store.FindOpenMonitorTask(lockedCtx, observation.Repository, PostMergeFailure)
			if err != nil || !found {
				return err
			}
			record, err = s.Store.LinkTask(lockedCtx, observation.Identity(), taskID, "recovery_observed")
			return err
		})
		if err != nil {
			return ObservationRecord{}, err
		}
		if taskID != "" {
			_ = s.Store.AuditTask(ctx, taskID, "monitor.recovery_observed", map[string]any{
				"identity": observation.Identity(), "repository": observation.Repository,
				"commit_sha": observation.CommitSHA, "source_url": observation.SourceURL,
			})
			_ = s.Store.AuditMonitor(ctx, "monitor.recovery_observed", map[string]any{
				"identity": observation.Identity(), "task_id": taskID, "repository": observation.Repository,
			})
		}
		_ = s.Store.RecordMonitorSuccess(ctx, now)
		return record, nil
	}
	if record.TaskID != "" {
		_ = s.Store.AuditMonitor(ctx, "monitor.deduplicated", map[string]any{
			"identity": observation.Identity(), "task_id": record.TaskID,
			"deduplicated_count": record.DeduplicatedCount,
		})
		_ = s.Store.AuditTask(ctx, record.TaskID, "monitor.observation_deduplicated", map[string]any{
			"identity": observation.Identity(), "occurrence_id": observation.OccurrenceID,
			"deduplicated_count": record.DeduplicatedCount,
		})
		if observation.Kind.Drift() && len(observation.ChangedPaths) > 0 {
			if err = s.recordSystemDesignDrift(ctx, observation, record.TaskID); err != nil {
				return ObservationRecord{}, err
			}
		}
		_ = s.Store.RecordMonitorSuccess(ctx, now)
		return record, nil
	}
	body := taskBody(observation)
	request := TaskRequest{
		Body: body, Repository: observation.Repository,
		Source: "monitor:" + string(observation.Kind), IntakeKey: observation.IntakeKey(),
		ReuseExistingByKey: observation.Kind.ReusesOpenTask(), Hints: observation.Hints,
	}
	var result IntakeResult
	reusedOpenTask := false
	createOrReuse := func(lockedCtx context.Context) error {
		if observation.Kind.ReusesOpenTask() {
			if taskID, found, findErr := s.Store.FindOpenMonitorTask(lockedCtx, observation.Repository, observation.Kind); findErr != nil {
				return findErr
			} else if found {
				result.Task.ID = taskID
				reusedOpenTask = true
				return nil
			}
		}
		result, err = s.Intake(lockedCtx, request)
		return err
	}
	if observation.Kind.ReusesOpenTask() {
		err = s.Store.WithMonitorSignalClassLock(ctx, observation.Repository, observation.Kind, createOrReuse)
	} else {
		err = createOrReuse(ctx)
	}
	if err != nil {
		return ObservationRecord{}, err
	}
	outcome := "reused"
	if result.Created {
		outcome = "created"
	}
	record, err = s.Store.LinkTask(ctx, observation.Identity(), result.Task.ID, outcome)
	if err != nil {
		return ObservationRecord{}, err
	}
	_ = s.Store.AuditMonitor(ctx, "monitor.task_"+outcome, map[string]any{
		"identity": observation.Identity(), "task_id": result.Task.ID,
		"repository": observation.Repository,
	})
	if reusedOpenTask {
		_ = s.Store.AuditTask(ctx, result.Task.ID, "monitor.task_reused", map[string]any{
			"workspace_id": observation.WorkspaceID, "repository": observation.Repository,
			"source": request.Source, "intake_key": request.IntakeKey,
		})
	}
	_ = s.Store.AuditTask(ctx, result.Task.ID, "monitor.observed", map[string]any{
		"identity": observation.Identity(), "kind": observation.Kind,
		"repository": observation.Repository, "source_url": observation.SourceURL,
		"commit_sha": observation.CommitSHA, "check_run_id": observation.CheckRunID,
		"attempt": observation.Attempt, "occurrence_id": observation.OccurrenceID,
	})
	if observation.Kind.ReusesOpenTask() && fresh {
		_ = s.Store.AuditTask(ctx, result.Task.ID, "monitor.occurrence_observed", map[string]any{
			"identity": observation.Identity(), "kind": observation.Kind,
			"repository": observation.Repository, "source_url": observation.SourceURL,
			"commit_sha": observation.CommitSHA, "check_run_id": observation.CheckRunID,
			"attempt": observation.Attempt, "occurrence_id": observation.OccurrenceID,
		})
	}
	_ = s.Store.AuditTask(ctx, result.Task.ID, "monitor.classified", map[string]any{
		"identity": observation.Identity(), "classification": observation.Kind,
		"drift": observation.Kind.Drift(),
	})
	if observation.Kind.Drift() {
		_, _, err = s.Store.RecordDrift(ctx, Drift{
			ID: observation.Identity(), WorkspaceID: observation.WorkspaceID,
			Repository: observation.Repository, Kind: observation.Kind,
			SourceURL: observation.SourceURL, CommitSHA: observation.CommitSHA,
			RequirementID: observation.RequirementID, TaskID: result.Task.ID,
			DetectedAt: observation.ObservedAt,
		})
		if err != nil {
			return ObservationRecord{}, err
		}
		_ = s.Store.AuditMonitor(ctx, "monitor.drift_detected", map[string]any{
			"drift_id": observation.Identity(), "task_id": result.Task.ID,
			"commit_sha": observation.CommitSHA,
		})
		_ = s.Store.AuditTask(ctx, result.Task.ID, "monitor.drift_detected", map[string]any{
			"drift_id": observation.Identity(), "commit_sha": observation.CommitSHA,
			"requirement_id": observation.RequirementID,
		})
		if err = s.recordSystemDesignDrift(ctx, observation, result.Task.ID); err != nil {
			return ObservationRecord{}, err
		}
	}
	_ = s.Store.RecordMonitorSuccess(ctx, now)
	return record, nil
}

func (s *Service) recordSystemDesignDrift(ctx context.Context, observation Observation, taskID string) error {
	if len(observation.ChangedPaths) == 0 {
		return nil
	}
	documents, err := s.Store.ListSystemDesigns(ctx)
	if err != nil {
		return err
	}
	designs := make([]core.GovernanceDesignContext, 0, len(documents))
	for _, document := range documents {
		if document.CurrentVersion == 0 {
			continue
		}
		version, getErr := s.Store.GetSystemDesignVersion(ctx, document.ID, document.CurrentVersion)
		if getErr != nil {
			return getErr
		}
		designs = append(designs, core.GovernanceDesignContext{ID: document.ID, Title: document.Title, Category: document.Category, Version: version.Version, Content: version.Content, Governs: version.Governs})
	}
	for _, match := range core.ResolveGovernedDesigns(designs, observation.Repository, observation.ChangedPaths) {
		design := match.Design
		id := "design:" + design.ID + ":" + observation.Identity()
		judgment, judgmentErr := s.Store.ResolveCausalSystemDesignMerge(ctx, design.ID, observation.Repository, observation.CommitSHA, observation.CausalEventID, id, match.MatchingPaths, observation.Kind == LineagedMerge)
		if judgmentErr != nil {
			return judgmentErr
		}
		if judgment.Proposal.EventID != 0 || judgment.Consulted {
			continue
		}
		causalEventID := int64(0)
		if judgment.CausalEventValid {
			causalEventID = observation.CausalEventID
		}
		_, fresh, recordErr := s.Store.RecordDrift(ctx, Drift{ID: id, WorkspaceID: observation.WorkspaceID, Repository: observation.Repository, Kind: observation.Kind, SourceURL: observation.SourceURL, CommitSHA: observation.CommitSHA, SystemDesignID: design.ID, SystemDesignVersion: design.Version, CausalEventID: causalEventID, MatchingPaths: match.MatchingPaths, TaskID: taskID, DetectedAt: observation.ObservedAt})
		if recordErr != nil {
			return recordErr
		}
		if fresh {
			_ = s.Store.AuditMonitor(ctx, "system_design.drift_detected", map[string]any{"drift_id": id, "document_id": design.ID, "version": design.Version, "causal_event_id": causalEventID, "matching_paths": match.MatchingPaths})
		}
	}
	return nil
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func taskBody(o Observation) string {
	purpose := "Investigate and repair the post-merge failure through Conveyor's normal pipeline."
	if o.Kind.Drift() {
		purpose = "Reconcile this out-of-pipeline repository change: propose a requirements amendment if intentional, or surface the requirement/code conflict for human decision. Do not silently rewrite approved requirements."
	}
	body := fmt.Sprintf("Monitor signal: %s\nRepository: %s\nOccurrence: %s\nSource: %s\nCommit: %s\n\n%s",
		o.Kind, o.Repository, o.OccurrenceID, o.SourceURL, o.CommitSHA, purpose)
	if failedChecks := strings.TrimSpace(o.Context["failed_check_runs"]); failedChecks != "" {
		body += "\n\nFailed checks:\n" + failedChecks
	}
	if o.Hints != nil {
		body += "\n\n" + o.Hints.AdvisoryText()
	}
	return body
}
