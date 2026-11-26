package httpapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The task read, the crate read, and MCP get_task and get_crate take live
// status from this one adapter over feedback.TaskStatuses and
// feedback.CrateStatus (component-production-feedback invariant 19;
// req-production-feedback AC-10.1; DEC-61). It reads storage only. An answer
// storage lacks is requested in the background, and the status that needs it
// is left out (invariant 20). Nothing here changes a task or reads a forge.

// feedbackStatusInputs loads every stored input for a derivation over the
// tasks. ok is false when the store keeps no feedback or the workspace has no
// unarchived environment, so the reads add no field.
func (s *Server) feedbackStatusInputs(ctx context.Context, tasks []store.FeedbackMergedTask) (feedback.Inputs, bool, error) {
	backend, isFeedback := s.Store.(store.FeedbackStore)
	environments, isEnvironment := s.Store.(store.EnvironmentStore)
	if !isFeedback || !isEnvironment || len(tasks) == 0 {
		return feedback.Inputs{}, false, nil
	}
	unarchived := false
	listed, err := environments.ListEnvironments(ctx, store.EnvironmentListQuery{Archived: &unarchived})
	if err != nil || len(listed) == 0 {
		return feedback.Inputs{}, false, err
	}
	ids := make([]string, len(listed))
	for i, environment := range listed {
		ids[i] = environment.ID
	}
	state, err := backend.ReadFeedbackInputs(ctx, ids)
	if err != nil {
		return feedback.Inputs{}, false, err
	}
	repositories, err := feedbackRepositories(ctx, s.Store)
	if err != nil {
		return feedback.Inputs{}, false, err
	}
	slugs := make(map[string]string, len(repositories))
	for _, repository := range repositories {
		slugs[repository.Name] = repository.Slug
	}
	heads, err := backend.ListFeedbackBranchHeads(ctx)
	if err != nil {
		return feedback.Inputs{}, false, err
	}
	inputs := feedback.Inputs{Now: s.feedbackNow(), Environments: listed, State: state, BranchHeads: heads, Slugs: slugs}
	if inputs.Comparisons, err = backend.ListFeedbackComparisons(ctx, feedback.RequiredComparisons(inputs, tasks)); err != nil {
		return feedback.Inputs{}, false, err
	}
	return inputs, true, nil
}

// feedbackTaskEnvironments returns a merged task's status in each relevant
// environment. merged is false for any other task, which gets no
// environments field. A merged task always gets a list: it is empty in a
// workspace with no environment or when no environment is relevant, and it
// leaves out a status that still needs comparison evidence
// (feature-production-feedback-live v1, Rollout and closure;
// component-production-feedback invariants 15 and 20).
func (s *Server) feedbackTaskEnvironments(ctx context.Context, task core.Task) (statuses []feedback.TaskStatus, merged bool, err error) {
	if task.State != core.TaskMerged {
		return nil, false, nil
	}
	statuses = []feedback.TaskStatus{}
	backend, ok := s.Store.(store.FeedbackStore)
	if !ok {
		return statuses, true, nil
	}
	tasks, err := backend.ListFeedbackMergedTasks(ctx, store.FeedbackMergedTaskQuery{TaskIDs: []string{task.ID}})
	if err != nil || len(tasks) == 0 {
		return statuses, true, err
	}
	inputs, ok, err := s.feedbackStatusInputs(ctx, tasks)
	if err != nil || !ok {
		return statuses, true, err
	}
	derived, missing := feedback.TaskStatuses(inputs, tasks[0])
	s.requestFeedbackMissing(ctx, missing)
	return append(statuses, derived...), true, nil
}

// feedbackCrateEnvironments returns a crate's environment lines, where each
// merged member is live, and the IDs of the crate's merged members. lines is
// never nil, and liveIn holds a list, empty when it is live nowhere, for
// every merged member (component-production-feedback invariant 18). It reads
// every merged member of the crate by crate scope, whatever window of
// members a read shows, so the counts are whole-crate counts.
func (s *Server) feedbackCrateEnvironments(ctx context.Context, crateID string) (lines []feedback.CrateLine, liveIn map[string][]string, err error) {
	lines, liveIn = []feedback.CrateLine{}, map[string][]string{}
	backend, ok := s.Store.(store.FeedbackStore)
	if !ok || crateID == "" {
		return lines, liveIn, nil
	}
	members, err := backend.ListFeedbackMergedTasks(ctx, store.FeedbackMergedTaskQuery{CrateID: crateID})
	if err != nil {
		return nil, nil, err
	}
	for _, member := range members {
		liveIn[member.TaskID] = []string{}
	}
	if len(members) == 0 {
		return lines, liveIn, nil
	}
	inputs, ok, err := s.feedbackStatusInputs(ctx, members)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return lines, liveIn, nil
	}
	derived, names, missing := feedback.CrateStatus(inputs, members)
	s.requestFeedbackMissing(ctx, missing)
	for taskID, environments := range names {
		liveIn[taskID] = append(liveIn[taskID], environments...)
	}
	return append(lines, derived...), liveIn, nil
}

func (s *Server) requestFeedbackMissing(ctx context.Context, missing feedback.Missing) {
	if workspace, ok := store.WorkspaceFromContext(ctx); ok && workspace != "" && !missing.Empty() {
		s.requestFeedbackAnswers(workspace, missing)
	}
}

// withFeedbackEnvironments adds environments to a rendered task read of a
// merged task. The list is [] when the task has no status to show.
func withFeedbackEnvironments(response any, statuses []feedback.TaskStatus, merged bool) (any, error) {
	if !merged {
		return response, nil
	}
	if statuses == nil {
		statuses = []feedback.TaskStatus{}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	if fields["environments"], err = json.Marshal(statuses); err != nil {
		return nil, err
	}
	return fields, nil
}

// feedbackUnresolvedRow is the newest unresolved version of an environment
// on GET /v1/environments (component-production-feedback invariant 21;
// req-production-feedback AC-2.3).
type feedbackUnresolvedRow struct {
	Service    string    `json:"service"`
	Repository string    `json:"repository,omitempty"`
	Commit     string    `json:"commit,omitempty"`
	Version    string    `json:"version,omitempty"`
	DeployedAt time.Time `json:"deployed_at"`
	Reason     string    `json:"reason"`
}

// feedbackNewestUnresolved returns each listed environment's newest
// unresolved running version with its reason.
func feedbackNewestUnresolved(state store.FeedbackInputs, environmentIDs []string) map[string]*feedbackUnresolvedRow {
	out := map[string]*feedbackUnresolvedRow{}
	for _, id := range environmentIDs {
		if version, reason, ok := feedback.NewestUnresolved(state, id); ok {
			out[id] = &feedbackUnresolvedRow{Service: version.Service, Repository: version.Repository, Commit: version.Commit, Version: version.Version, DeployedAt: version.DeployedAt, Reason: reason}
		}
	}
	return out
}
