// Package monitor implements the Phase 5.6 repository observer. It turns
// external GitHub signals into ordinary task intake requests; it never owns a
// pipeline stage, worker claim, review, merge, or deployment capability.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
	Revert           SignalKind = "revert"
)

func (k SignalKind) Valid() bool {
	switch k {
	case PostMergeFailure, DirectPush, ExternalPRMerge, Revert:
		return true
	default:
		return false
	}
}

func (k SignalKind) Drift() bool {
	return k == DirectPush || k == ExternalPRMerge || k == Revert
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
	FeatureID         string            `json:"feature_id,omitempty"`
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
	o.FeatureID = strings.TrimSpace(o.FeatureID)
	if o.WorkspaceID == "" {
		o.WorkspaceID = workspace
	}
	if o.WorkspaceID == "" || o.WorkspaceID != workspace {
		return errors.New("observation workspace must match the immutable workspace context")
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

func (o Observation) IntakeKey() string {
	return "monitor:" + o.Identity()
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
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Repository  string     `json:"repository"`
	Kind        SignalKind `json:"kind"`
	SourceURL   string     `json:"source_url"`
	CommitSHA   string     `json:"commit_sha,omitempty"`
	FeatureID   string     `json:"feature_id,omitempty"`
	TaskID      string     `json:"task_id"`
	DetectedAt  time.Time  `json:"detected_at"`
	ResolvedAt  time.Time  `json:"resolved_at,omitempty"`
	Outcome     string     `json:"outcome,omitempty"`
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

type TaskRequest struct {
	Body       string
	Repository string
	Source     string
	IntakeKey  string
	Hints      *HintContext
}

type IntakeResult struct {
	Task    core.Task
	Created bool
}

type Store interface {
	Observe(context.Context, Observation) (ObservationRecord, bool, error)
	LinkTask(context.Context, string, string, string) (ObservationRecord, error)
	RecordDrift(context.Context, Drift) (Drift, bool, error)
	ResolveDrift(context.Context, string, string) (Drift, error)
	MonitorStatus(context.Context, bool, time.Time) (Status, error)
	RecordMonitorSuccess(context.Context, time.Time) error
	RecordMonitorFailure(context.Context, string, string, time.Time) error
	AuditTask(context.Context, string, string, map[string]any) error
	AuditMonitor(context.Context, string, map[string]any) error
}

type Service struct {
	Store        Store
	Intake       func(context.Context, TaskRequest) (IntakeResult, error)
	WorkspaceID  string
	Enabled      bool
	Repositories map[string]struct{}
	Now          func() time.Time
	ResolveScope func(context.Context) (workspace string, enabled bool, repositories map[string]struct{}, err error)
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

func (s *Service) Resolve(ctx context.Context, id, outcome string) (Drift, error) {
	if s.Store == nil {
		return Drift{}, errors.New("monitor storage is unavailable")
	}
	drift, err := s.Store.ResolveDrift(ctx, strings.TrimSpace(id), strings.TrimSpace(outcome))
	if err == nil {
		_ = s.Store.AuditMonitor(ctx, "monitor.drift_reconciled", map[string]any{
			"drift_id": drift.ID, "task_id": drift.TaskID, "outcome": drift.Outcome,
		})
	}
	return drift, err
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
	record, fresh, err := s.Store.Observe(ctx, observation)
	if err != nil {
		return ObservationRecord{}, err
	}
	_ = s.Store.AuditMonitor(ctx, "monitor.observed", map[string]any{
		"identity": observation.Identity(), "repository": observation.Repository,
		"kind": observation.Kind, "source_url": observation.SourceURL, "fresh": fresh,
	})
	if record.TaskID != "" {
		_ = s.Store.AuditMonitor(ctx, "monitor.deduplicated", map[string]any{
			"identity": observation.Identity(), "task_id": record.TaskID,
			"deduplicated_count": record.DeduplicatedCount,
		})
		_ = s.Store.AuditTask(ctx, record.TaskID, "monitor.observation_deduplicated", map[string]any{
			"identity": observation.Identity(), "occurrence_id": observation.OccurrenceID,
			"deduplicated_count": record.DeduplicatedCount,
		})
		_ = s.Store.RecordMonitorSuccess(ctx, now)
		return record, nil
	}
	body := taskBody(observation)
	result, err := s.Intake(ctx, TaskRequest{
		Body: body, Repository: observation.Repository,
		Source: "monitor:" + string(observation.Kind), IntakeKey: observation.IntakeKey(),
		Hints: observation.Hints,
	})
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
	_ = s.Store.AuditTask(ctx, result.Task.ID, "monitor.observed", map[string]any{
		"identity": observation.Identity(), "kind": observation.Kind,
		"repository": observation.Repository, "source_url": observation.SourceURL,
	})
	_ = s.Store.AuditTask(ctx, result.Task.ID, "monitor.classified", map[string]any{
		"identity": observation.Identity(), "classification": observation.Kind,
		"drift": observation.Kind.Drift(),
	})
	if observation.Kind.Drift() {
		_, _, err = s.Store.RecordDrift(ctx, Drift{
			ID: observation.Identity(), WorkspaceID: observation.WorkspaceID,
			Repository: observation.Repository, Kind: observation.Kind,
			SourceURL: observation.SourceURL, CommitSHA: observation.CommitSHA,
			FeatureID: observation.FeatureID, TaskID: result.Task.ID,
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
			"feature_id": observation.FeatureID,
		})
	}
	_ = s.Store.RecordMonitorSuccess(ctx, now)
	return record, nil
}

func taskBody(o Observation) string {
	purpose := "Investigate and repair the post-merge failure through Conveyor's normal pipeline."
	if o.Kind.Drift() {
		purpose = "Reconcile this out-of-pipeline repository change: propose a requirements amendment if intentional, or surface the requirement/code conflict for human decision. Do not silently rewrite approved requirements."
	}
	body := fmt.Sprintf("Monitor signal: %s\nRepository: %s\nOccurrence: %s\nSource: %s\nCommit: %s\n\n%s",
		o.Kind, o.Repository, o.OccurrenceID, o.SourceURL, o.CommitSHA, purpose)
	if o.Hints != nil {
		body += "\n\n" + o.Hints.AdvisoryText()
	}
	return body
}
