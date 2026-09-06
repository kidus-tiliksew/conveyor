package singlestore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) GetLatestJob(ctx context.Context, taskID string) (core.Job, bool, error) {
	err := s.requireEmpty(ctx, "jobs")
	return zero[core.Job](), zero[bool](), err
}
func (s *Store) GetReferenceDocument(ctx context.Context, documentID string) (core.ReferenceDocument, error) {
	err := s.requireEmpty(ctx, "reference_documents")
	if err == nil {
		err = store.ErrNotFound
	}
	return zero[core.ReferenceDocument](), err
}
func (s *Store) GetReferenceDocumentVersion(ctx context.Context, documentID string, version int) (core.ReferenceDocumentVersion, error) {
	err := s.requireEmpty(ctx, "reference_documents")
	if err == nil {
		err = store.ErrNotFound
	}
	return zero[core.ReferenceDocumentVersion](), err
}
func (s *Store) GetSpecVersion(ctx context.Context, taskID string, version int) (core.SpecVersion, bool, error) {
	err := s.requireEmpty(ctx, "task_specs")
	return zero[core.SpecVersion](), zero[bool](), err
}
func (s *Store) GetSystemDesignVersion(ctx context.Context, a1 string, a2 int) (core.SystemDesignVersion, error) {
	err := s.requireEmpty(ctx, "system_designs")
	if err == nil {
		err = store.ErrNotFound
	}
	return zero[core.SystemDesignVersion](), err
}
func (s *Store) GetTaskByIntakeKey(ctx context.Context, key string) (core.Task, bool, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[core.Task](), zero[bool](), err
}
func (s *Store) GetTranscript(ctx context.Context, jobID string) (core.Transcript, error) {
	err := s.requireEmpty(ctx, "transcripts")
	if err == nil {
		err = store.ErrNotFound
	}
	return zero[core.Transcript](), err
}
func (s *Store) ListActiveSystemDesignDriftCounts(ctx context.Context) (map[string]int, error) {
	err := s.requireEmpty(ctx, "repository_drift")
	return zero[map[string]int](), err
}
func (s *Store) ListActivityMarkers(ctx context.Context) ([]store.ActivityMarker, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[[]store.ActivityMarker](), err
}
func (s *Store) ListActivityMarkersForTasks(ctx context.Context, taskIDs []string) ([]store.ActivityMarker, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[[]store.ActivityMarker](), err
}
func (s *Store) ListCallerAttentionTaskPage(ctx context.Context, query store.CallerAttentionQuery) (store.TaskPage, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[store.TaskPage](), err
}
func (s *Store) ListCheckpointContextCandidates(ctx context.Context, requirementID string) ([]store.CheckpointContextCandidate, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[[]store.CheckpointContextCandidate](), err
}
func (s *Store) ListDependentTaskIDs(ctx context.Context, taskID string) ([]string, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[[]string](), err
}
func (s *Store) ListFeatures(ctx context.Context) ([]core.Feature, error) {
	err := s.requireEmpty(ctx, "features")
	return zero[[]core.Feature](), err
}
func (s *Store) ListGovernanceDesigns(ctx context.Context, repository string) ([]core.GovernanceDesignContext, error) {
	err := s.requireEmpty(ctx, "system_designs")
	return zero[[]core.GovernanceDesignContext](), err
}
func (s *Store) ListJobs(ctx context.Context, taskID string) ([]core.Job, error) {
	err := s.requireEmpty(ctx, "jobs")
	return zero[[]core.Job](), err
}
func (s *Store) ListLineageContextRecords(ctx context.Context, nodes []core.LineageNode) (store.LineageContextRecords, error) {
	err := s.requireEmpty(ctx, "links")
	return zero[store.LineageContextRecords](), err
}
func (s *Store) ListLineageNodeRecords(ctx context.Context, nodes []core.LineageNode) (map[core.LineageNode]store.LineageNodeRecord, error) {
	err := s.requireEmpty(ctx, "links")
	return zero[map[core.LineageNode]store.LineageNodeRecord](), err
}
func (s *Store) ListPlanningBundles(ctx context.Context) ([]core.PlanningBundle, error) {
	err := s.requireEmpty(ctx, "planning_bundles")
	return zero[[]core.PlanningBundle](), err
}
func (s *Store) ListRequirementDeliveryEventsForTasks(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	err := s.requireEmpty(ctx, "requirements")
	return zero[map[string][]core.Event](), err
}
func (s *Store) ListRequirementDeliveryLineageByRequirement(ctx context.Context, requirementIDs []string, budget core.LineageTraversalBudget) (map[string][]core.LineageLink, error) {
	err := s.requireEmpty(ctx, "requirements")
	return zero[map[string][]core.LineageLink](), err
}
func (s *Store) ListRequirementEventsByRequirement(ctx context.Context) (map[string][]core.Event, error) {
	err := s.requireEmpty(ctx, "requirements")
	return zero[map[string][]core.Event](), err
}
func (s *Store) ListRequirementVersionsByRequirement(ctx context.Context) (map[string][]core.RequirementVersion, error) {
	err := s.requireEmpty(ctx, "requirements")
	return zero[map[string][]core.RequirementVersion](), err
}
func (s *Store) ListSystemDesignEventsByDocument(ctx context.Context) (map[string][]core.Event, error) {
	err := s.requireEmpty(ctx, "system_designs")
	return zero[map[string][]core.Event](), err
}
func (s *Store) ListSystemDesignVersionsByDocument(ctx context.Context) (map[string][]core.SystemDesignVersion, error) {
	err := s.requireEmpty(ctx, "system_designs")
	return zero[map[string][]core.SystemDesignVersion](), err
}
func (s *Store) ListTaskPage(ctx context.Context, query store.TaskOperationsQuery) (store.TaskPage, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[store.TaskPage](), err
}
func (s *Store) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	err := s.requireEmpty(ctx, "work_orders")
	return zero[[]core.WorkOrder](), err
}
func (s *Store) ListWorkOrdersForTasks(ctx context.Context, taskIDs []string) ([]core.WorkOrder, error) {
	err := s.requireEmpty(ctx, "work_orders")
	return zero[[]core.WorkOrder](), err
}
func (s *Store) ReconcileBlueprintClosures(ctx context.Context) (int, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[int](), err
}
func (s *Store) ReconcileGitHubLifecycles(ctx context.Context) (int, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[int](), err
}
func (s *Store) ReconcileQueuedTasks(ctx context.Context) (int, error) {
	err := s.requireEmpty(ctx, "tasks")
	return zero[int](), err
}
func (s *Store) RequirementExists(ctx context.Context, a1 string) (bool, error) {
	err := s.requireEmpty(ctx, "requirements")
	return zero[bool](), err
}

// requireEmpty proves the empty projection from persisted rows. Returning
// ErrNotImplemented for populated data prevents a partial backend from lying.
func (s *Store) requireEmpty(ctx context.Context, table string) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	// table is always a literal supplied by the methods in this file.
	var found int
	err = s.db.QueryRowContext(ctx, fmt.Sprintf("SELECT 1 FROM `%s` WHERE workspace_id=? LIMIT 1", table), ws).Scan(&found)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return translateBackendConflict(err)
	}
	return store.ErrNotImplemented
}
