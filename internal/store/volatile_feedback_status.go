package store

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// feedbackResolutionKey names a version within a workspace.
type feedbackResolutionKey struct {
	workspace     string
	environmentID string
	service       string
	version       FeedbackVersion
}

type feedbackBranchKey struct {
	workspace  string
	repository string
	branch     string
}

type feedbackComparisonScope struct {
	workspace string
	key       FeedbackComparisonKey
}

// feedbackStatusState mirrors the FB-3 tables of the durable backends under
// the memory write mutex.
type feedbackStatusState struct {
	resolutions map[feedbackResolutionKey]FeedbackResolution
	comparisons map[feedbackComparisonScope]FeedbackComparison
	admissions  map[feedbackComparisonScope]time.Time
	branches    map[feedbackBranchKey]FeedbackBranchHead
}

func newFeedbackStatusState() feedbackStatusState {
	return feedbackStatusState{
		resolutions: map[feedbackResolutionKey]FeedbackResolution{},
		comparisons: map[feedbackComparisonScope]FeedbackComparison{},
		admissions:  map[feedbackComparisonScope]time.Time{},
		branches:    map[feedbackBranchKey]FeedbackBranchHead{},
	}
}

func feedbackResolutionKeyOf(workspace string, ref FeedbackVersionRef) feedbackResolutionKey {
	return feedbackResolutionKey{workspace: workspace, environmentID: ref.EnvironmentID, service: ref.Service, version: ref.FeedbackVersion}
}

// ClaimFeedbackResolution implements FeedbackStatusStore.
func (m *volatileMemory) ClaimFeedbackResolution(ctx context.Context, ref FeedbackVersionRef, signalAt, now time.Time) (bool, error) {
	key := feedbackResolutionKeyOf(workspaceOrDefault(ctx, ""), ref)
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, found := m.feedbackState.status.resolutions[key]
	if !FeedbackClaimable(stored, found, EnvironmentTime(signalAt)) {
		return false, nil
	}
	m.feedbackState.status.resolutions[key] = FeedbackPendingResolution(ref, now)
	return true, nil
}

// RecordFeedbackResolution implements FeedbackStatusStore.
func (m *volatileMemory) RecordFeedbackResolution(ctx context.Context, resolution FeedbackResolution) error {
	resolution, err := NormalizeFeedbackResolution(resolution)
	if err != nil {
		return err
	}
	workspace := workspaceOrDefault(ctx, "")
	key := feedbackResolutionKeyOf(workspace, resolution.FeedbackVersionRef)
	m.mu.Lock()
	defer m.mu.Unlock()
	if stored, found := m.feedbackState.status.resolutions[key]; found && stored.Resolved() {
		return nil
	}
	m.feedbackState.status.resolutions[key] = resolution
	if !resolution.Resolved() {
		return nil
	}
	scope := memoryScopedKey{workspace: workspace, id: resolution.EnvironmentID}
	running := slices.Clone(m.feedbackState.running[scope])
	for i := range running {
		if running[i].Service == resolution.Service && running[i].FeedbackVersion == resolution.FeedbackVersion {
			running[i].Resolved = true
		}
	}
	m.feedbackState.running[scope] = running
	deploys := slices.Clone(m.feedbackState.deploys[scope])
	for i := range deploys {
		if deploys[i].Service == resolution.Service && deploys[i].FeedbackVersion == resolution.FeedbackVersion {
			deploys[i].Resolved = true
		}
	}
	m.feedbackState.deploys[scope] = deploys
	return nil
}

// ReadFeedbackInputs implements FeedbackStatusStore.
func (m *volatileMemory) ReadFeedbackInputs(ctx context.Context, environmentIDs []string) (FeedbackInputs, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	inputs := FeedbackInputs{Signals: map[string]FeedbackSignalState{}, Running: map[string][]FeedbackRunningVersion{}, Deploys: map[string][]FeedbackDeploy{}, Resolutions: []FeedbackResolution{}}
	wanted := map[string]bool{}
	for _, id := range environmentIDs {
		if wanted[id] {
			continue
		}
		wanted[id] = true
		scope := memoryScopedKey{workspace: workspace, id: id}
		if state, ok := m.feedbackState.signals[scope]; ok {
			inputs.Signals[id] = state
		}
		if running := m.feedbackState.running[scope]; len(running) > 0 {
			inputs.Running[id] = slices.Clone(running)
			SortFeedbackRunningVersions(inputs.Running[id])
		}
		if deploys := m.feedbackState.deploys[scope]; len(deploys) > 0 {
			out := slices.Clone(deploys)
			for i := range out {
				if out[i].StoppedAt != nil {
					at, by := *out[i].StoppedAt, *out[i].StopOrigin
					out[i].StoppedAt, out[i].StopOrigin = &at, &by
				}
			}
			SortFeedbackDeploys(out)
			inputs.Deploys[id] = out
		}
	}
	for key, resolution := range m.feedbackState.status.resolutions {
		if key.workspace == workspace && wanted[key.environmentID] {
			inputs.Resolutions = append(inputs.Resolutions, resolution)
		}
	}
	SortFeedbackResolutions(inputs.Resolutions)
	return inputs, nil
}

// AdmitFeedbackComparisons implements FeedbackStatusStore.
func (m *volatileMemory) AdmitFeedbackComparisons(ctx context.Context, keys []FeedbackComparisonKey, now time.Time, most int) ([]FeedbackComparisonKey, error) {
	workspace := workspaceOrDefault(ctx, "")
	waited := time.Now()
	keys = NormalizeFeedbackComparisonKeys(keys)
	m.mu.Lock()
	defer m.mu.Unlock()
	now = FeedbackAdmissionTime(now, waited)
	stored := map[FeedbackComparisonKey]bool{}
	attempts := map[FeedbackComparisonKey]time.Time{}
	for _, key := range keys {
		scope := feedbackComparisonScope{workspace: workspace, key: key}
		if _, ok := m.feedbackState.status.comparisons[scope]; ok {
			stored[key] = true
		}
		if at, ok := m.feedbackState.status.admissions[scope]; ok && at.After(now.Add(-FeedbackAttemptMemory)) {
			attempts[key] = at
		}
	}
	used := 0
	for scope, at := range m.feedbackState.status.admissions {
		if scope.workspace != workspace {
			continue
		}
		if !at.After(now.Add(-FeedbackAttemptMemory)) {
			delete(m.feedbackState.status.admissions, scope)
			continue
		}
		if at.After(now.Add(-FeedbackComparisonWindow)) {
			used++
		}
	}
	admitted := PlanFeedbackAdmission(keys, stored, attempts, used, now, most)
	for _, key := range admitted {
		m.feedbackState.status.admissions[feedbackComparisonScope{workspace: workspace, key: key}] = now
	}
	return admitted, nil
}

// RecordFeedbackComparisons implements FeedbackStatusStore.
func (m *volatileMemory) RecordFeedbackComparisons(ctx context.Context, comparisons []FeedbackComparison) error {
	normalized := make([]FeedbackComparison, 0, len(comparisons))
	for _, comparison := range comparisons {
		comparison, err := NormalizeFeedbackComparison(comparison)
		if err != nil {
			return err
		}
		normalized = append(normalized, comparison)
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, comparison := range normalized {
		scope := feedbackComparisonScope{workspace: workspace, key: comparison.FeedbackComparisonKey}
		if _, ok := m.feedbackState.status.comparisons[scope]; !ok {
			m.feedbackState.status.comparisons[scope] = comparison
		}
	}
	return nil
}

// ListFeedbackComparisons implements FeedbackStatusStore.
func (m *volatileMemory) ListFeedbackComparisons(ctx context.Context, keys []FeedbackComparisonKey) ([]FeedbackComparison, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []FeedbackComparison{}
	for _, key := range NormalizeFeedbackComparisonKeys(keys) {
		if comparison, ok := m.feedbackState.status.comparisons[feedbackComparisonScope{workspace: workspace, key: key}]; ok {
			out = append(out, comparison)
		}
	}
	SortFeedbackComparisons(out)
	return out, nil
}

// RecordFeedbackBranchHead implements FeedbackStatusStore.
func (m *volatileMemory) RecordFeedbackBranchHead(ctx context.Context, head FeedbackBranchHead) error {
	head, err := NormalizeFeedbackBranchHead(head)
	if err != nil {
		return err
	}
	key := feedbackBranchKey{workspace: workspaceOrDefault(ctx, ""), repository: head.Repository, branch: head.Branch}
	m.mu.Lock()
	defer m.mu.Unlock()
	if stored, ok := m.feedbackState.status.branches[key]; ok && stored.ObservedAt.After(head.ObservedAt) {
		return nil
	}
	m.feedbackState.status.branches[key] = head
	return nil
}

// ListFeedbackBranchHeads implements FeedbackStatusStore.
func (m *volatileMemory) ListFeedbackBranchHeads(ctx context.Context) ([]FeedbackBranchHead, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []FeedbackBranchHead{}
	for key, head := range m.feedbackState.status.branches {
		if key.workspace == workspace {
			out = append(out, head)
		}
	}
	SortFeedbackBranchHeads(out)
	return out, nil
}

// ListFeedbackMergedTasks implements FeedbackStatusStore.
func (m *volatileMemory) ListFeedbackMergedTasks(ctx context.Context, query FeedbackMergedTaskQuery) ([]FeedbackMergedTask, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := map[string]bool{}
	for _, id := range query.TaskIDs {
		ids[id] = true
	}
	out := []FeedbackMergedTask{}
	for _, task := range m.tasks {
		if task.Workspace != workspace || task.State != core.TaskMerged {
			continue
		}
		if (len(query.TaskIDs) > 0 && !ids[task.ID]) || (query.CrateID != "" && task.TaskCrateID() != query.CrateID) || (query.Repository != "" && task.Repo != query.Repository) {
			continue
		}
		merged := FeedbackMergedTask{TaskID: task.ID, Repository: task.Repo}
		// The latest merge event wins before its commit is checked.
		var latest core.Event
		found := false
		for _, event := range m.events[task.ID] {
			if event.Kind != "merge.confirmed" && event.Kind != "merge.reconciled" {
				continue
			}
			if !found || TaskEventBefore(latest, event) {
				latest, found = event, true
			}
		}
		if found {
			var payload struct {
				Repository     json.RawMessage `json:"repository"`
				MergeCommitSHA string          `json:"merge_commit_sha"`
				PullRequest    json.RawMessage `json:"pull_request"`
			}
			_ = json.Unmarshal(latest.Payload, &payload)
			merged.MergeCommit, merged.MergedAt, merged.EventID = FeedbackMergeCommit(payload.MergeCommitSHA), latest.At.UTC(), latest.ID
			// The pull request number comes from the same latest event
			// (component-feedback-reports invariant 12).
			merged.PullRequest = FeedbackMergePullRequest(string(payload.PullRequest))
			merged.GitHubRepository = FeedbackMergeRepository(string(payload.Repository))
		}
		out = append(out, merged)
	}
	SortFeedbackMergedTasks(out)
	return out, nil
}
