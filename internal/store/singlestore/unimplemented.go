// Explicit unimplemented backend methods. Aggregate tasks replace their own lines.
package singlestore

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) AttachSubmissionGovernance(ctx context.Context, taskID, repository string, changedPaths []string, attribution store.SubmissionGovernanceAttribution) ([]core.TaskDesignContext, error) {
	return zero[[]core.TaskDesignContext](), store.ErrNotImplemented
}
func (s *Store) GetGitHubLifecycle(ctx context.Context, taskID string) (core.GitHubLifecycle, bool, error) {
	return zero[core.GitHubLifecycle](), zero[bool](), store.ErrNotImplemented
}
func (s *Store) GetReviewPublication(ctx context.Context, reviewWorkOrderID string) (core.ReviewPublication, error) {
	return zero[core.ReviewPublication](), store.ErrNotImplemented
}
func (s *Store) QueueGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	return store.ErrNotImplemented
}
func (s *Store) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	return store.ErrNotImplemented
}
func (s *Store) ReconcileReviewPublications(ctx context.Context) (int, error) {
	return zero[int](), store.ErrNotImplemented
}
func (s *Store) ResolveCausalSystemDesignMerge(context.Context, string, string, string, int64, string, []string, bool) (monitor.SystemDesignMergeJudgment, error) {
	return zero[monitor.SystemDesignMergeJudgment](), store.ErrNotImplemented
}
func (s *Store) SetTaskAssigneeCommand(ctx context.Context, lease taskops.TaskLease, id, assigneeUserID string) (core.Task, error) {
	return zero[core.Task](), store.ErrNotImplemented
}
func (s *Store) UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	return store.ErrNotImplemented
}
func (s *Store) UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	return store.ErrNotImplemented
}
func zero[T any]() T { var value T; return value }
