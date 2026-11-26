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
// is left out, which sets environments_pending (invariant 20). Nothing here
// changes a task or reads a forge, and no read waits for the background work.
// environments_pending observes only: no dispatch, gate, review, merge,
// next-action, lineage, or work-order code reads it (invariant 1).

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

// feedbackTaskLive is a merged task's live status on a task read: one
// status per relevant environment, and whether a status was left out because
// an answer is not stored yet (component-production-feedback, Public
// surfaces).
type feedbackTaskLive struct {
	Statuses []feedback.TaskStatus
	Pending  bool
}

// feedbackTaskEnvironments returns a merged task's status in each relevant
// environment and its environments_pending value. merged is false for any
// other task, which gets neither field. A merged task always gets a list: it
// is empty in a workspace with no environment or when no environment is
// relevant, and it leaves out a status that still needs comparison evidence
// (feature-production-feedback-live v1, Rollout and closure;
// component-production-feedback invariants 15 and 20). Pending is false
// whenever no status is derived.
func (s *Server) feedbackTaskEnvironments(ctx context.Context, task core.Task) (live feedbackTaskLive, merged bool, err error) {
	if task.State != core.TaskMerged {
		return feedbackTaskLive{}, false, nil
	}
	live.Statuses = []feedback.TaskStatus{}
	backend, ok := s.Store.(store.FeedbackStore)
	if !ok {
		return live, true, nil
	}
	tasks, err := backend.ListFeedbackMergedTasks(ctx, store.FeedbackMergedTaskQuery{TaskIDs: []string{task.ID}})
	if err != nil {
		return feedbackTaskLive{}, true, err
	}
	if len(tasks) == 0 {
		return live, true, nil
	}
	inputs, ok, err := s.feedbackStatusInputs(ctx, tasks)
	if err != nil {
		return feedbackTaskLive{}, true, err
	}
	if !ok {
		return live, true, nil
	}
	derived, pending, missing := feedback.TaskStatuses(inputs, tasks[0])
	s.requestFeedbackMissing(ctx, missing)
	live.Statuses = append(live.Statuses, derived...)
	live.Pending = pending
	return live, true, nil
}

// feedbackCrateLive is a crate's live status on a crate read: its
// environment lines, where each merged member is live, and whether a
// member's status was left out because an answer is not stored yet
// (component-production-feedback, Public surfaces).
type feedbackCrateLive struct {
	Lines   []feedback.CrateLine
	LiveIn  map[string][]string
	Pending bool
}

// feedbackCrateEnvironments returns a crate's environment lines, where each
// merged member is live, and its environments_pending value. Lines is never
// nil, and LiveIn holds a list, empty when it is live nowhere, for every
// merged member (component-production-feedback invariant 18). It reads every
// merged member of the crate by crate scope, whatever window of members a
// read shows, so the counts are whole-crate counts. Pending is false whenever
// no status is derived.
func (s *Server) feedbackCrateEnvironments(ctx context.Context, crateID string) (feedbackCrateLive, error) {
	live := feedbackCrateLive{Lines: []feedback.CrateLine{}, LiveIn: map[string][]string{}}
	backend, ok := s.Store.(store.FeedbackStore)
	if !ok || crateID == "" {
		return live, nil
	}
	members, err := backend.ListFeedbackMergedTasks(ctx, store.FeedbackMergedTaskQuery{CrateID: crateID})
	if err != nil {
		return feedbackCrateLive{}, err
	}
	for _, member := range members {
		live.LiveIn[member.TaskID] = []string{}
	}
	if len(members) == 0 {
		return live, nil
	}
	inputs, ok, err := s.feedbackStatusInputs(ctx, members)
	if err != nil {
		return feedbackCrateLive{}, err
	}
	if !ok {
		return live, nil
	}
	derived, names, pending, missing := feedback.CrateStatus(inputs, members)
	s.requestFeedbackMissing(ctx, missing)
	for taskID, environments := range names {
		live.LiveIn[taskID] = append(live.LiveIn[taskID], environments...)
	}
	live.Lines = append(live.Lines, derived...)
	live.Pending = pending
	return live, nil
}

func (s *Server) requestFeedbackMissing(ctx context.Context, missing feedback.Missing) {
	if workspace, ok := store.WorkspaceFromContext(ctx); ok && workspace != "" && !missing.Empty() {
		s.requestFeedbackAnswers(workspace, missing)
	}
}

// withFeedbackEnvironments adds environments and environments_pending to a
// rendered task read of a merged task. The list is [] when the task has no
// status to show. A task that is not merged gets neither field.
func withFeedbackEnvironments(response any, live feedbackTaskLive, merged bool) (any, error) {
	if !merged {
		return response, nil
	}
	statuses := live.Statuses
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
	if fields["environments_pending"], err = json.Marshal(live.Pending); err != nil {
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
