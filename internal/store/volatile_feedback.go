package store

import (
	"context"
	"slices"
)

// The volatile feedback state mirrors the four feedback tables of the
// durable backends. Every mutation runs under the memory write mutex, the
// volatile backend's critical section. Each map is keyed by workspace and
// environment ID.
type feedbackState struct {
	signals  map[memoryScopedKey]FeedbackSignalState
	deploys  map[memoryScopedKey][]FeedbackDeploy
	running  map[memoryScopedKey][]FeedbackRunningVersion
	accepted map[memoryScopedKey][]FeedbackAcceptedSignal
	// status holds what resolving and comparing learn (FB-3).
	status feedbackStatusState
}

func newFeedbackState() feedbackState {
	return feedbackState{
		signals:  map[memoryScopedKey]FeedbackSignalState{},
		deploys:  map[memoryScopedKey][]FeedbackDeploy{},
		running:  map[memoryScopedKey][]FeedbackRunningVersion{},
		accepted: map[memoryScopedKey][]FeedbackAcceptedSignal{},
		status:   newFeedbackStatusState(),
	}
}

var _ FeedbackStore = (*volatileMemory)(nil)

// CheckFeedbackRepositories implements FeedbackStore.
func (m *volatileMemory) CheckFeedbackRepositories(ctx context.Context, signal FeedbackSignal) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	repositories, err := m.environmentRepositoriesLocked(workspaceOrDefault(ctx, ""))
	if err != nil {
		return err
	}
	return CheckFeedbackRepositories(signal, repositories)
}

// RecordFeedbackSignal implements FeedbackStore. Every check runs before the
// first write.
func (m *volatileMemory) RecordFeedbackSignal(ctx context.Context, signal FeedbackSignal) error {
	signal, err := NormalizeFeedbackSignal(signal)
	if err != nil {
		return err
	}
	workspace := workspaceOrDefault(ctx, "")
	scope := memoryScopedKey{workspace: workspace, id: signal.EnvironmentID}
	keyScope := memoryScopedKey{workspace: workspace, id: signal.KeyID}
	m.mu.Lock()
	defer m.mu.Unlock()
	environment, found := m.environmentState.environments[scope]
	key, issued := m.environmentState.keys[keyScope]
	if !found || environment.Archived || !issued || key.EnvironmentID != signal.EnvironmentID || key.RevokedAt != nil {
		return ErrInvalidEnvironmentKey
	}
	repositories, err := m.environmentRepositoriesLocked(workspace)
	if err != nil {
		return err
	}
	if err := CheckFeedbackRepositories(signal, repositories); err != nil {
		return err
	}
	origin := FeedbackSignalOrigin(key.Label, m.nextFeedbackReportLocked(scope))
	// The plan reads the memory's own slices and returns new values.
	changes, err := PlanFeedbackChanges(signal, origin, FeedbackHistory{Running: m.feedbackState.running[scope], Later: m.feedbackState.accepted[scope], Deploys: m.feedbackState.deploys[scope]})
	if err != nil {
		return err
	}
	m.applyFeedbackChangesLocked(scope, signal, origin, changes)
	if newest, exists := m.feedbackState.signals[scope]; !exists || !newest.LastSignalAt.After(signal.ArrivedAt) {
		m.feedbackState.signals[scope] = FeedbackSignalState{EnvironmentID: signal.EnvironmentID, LastSignalAt: signal.ArrivedAt, NextSignalSeconds: signal.NextSignalSeconds, Origin: FeedbackSignalOrigin(key.Label, 0)}
	}
	m.feedbackState.accepted[scope] = append(m.feedbackState.accepted[scope], NewFeedbackAcceptedSignal(signal, origin))
	m.touchEnvironmentKeyLocked(keyScope)
	return nil
}

// RecordFeedbackGitHubDeploy implements FeedbackStore. Every check runs before
// the first write.
func (m *volatileMemory) RecordFeedbackGitHubDeploy(ctx context.Context, deploy FeedbackGitHubDeploy) (bool, error) {
	signal, err := FeedbackGitHubDeploySignal(deploy)
	if err != nil {
		return false, err
	}
	workspace := workspaceOrDefault(ctx, "")
	scope := memoryScopedKey{workspace: workspace, id: signal.EnvironmentID}
	m.mu.Lock()
	defer m.mu.Unlock()
	if environment, found := m.environmentState.environments[scope]; !found || environment.Archived {
		return false, nil
	}
	repositories, err := m.environmentRepositoriesLocked(workspace)
	if err != nil {
		return false, err
	}
	if !repositories[deploy.Repository] {
		return false, nil
	}
	origin := FeedbackGitHubOrigin(m.nextFeedbackReportLocked(scope))
	changes, err := PlanFeedbackGitHubDeploy(signal, origin, FeedbackHistory{Running: m.feedbackState.running[scope], Later: m.feedbackState.accepted[scope], Deploys: m.feedbackState.deploys[scope]})
	if err != nil {
		return false, err
	}
	m.applyFeedbackChangesLocked(scope, signal, origin, changes)
	m.feedbackState.accepted[scope] = append(m.feedbackState.accepted[scope], NewFeedbackAcceptedSignal(signal, origin))
	return true, nil
}

// nextFeedbackReportLocked numbers the next accepted report of an
// environment.
func (m *volatileMemory) nextFeedbackReportLocked(scope memoryScopedKey) int64 {
	numbers := []int64{}
	for _, accepted := range m.feedbackState.accepted[scope] {
		numbers = append(numbers, accepted.Report)
	}
	for _, deploy := range m.feedbackState.deploys[scope] {
		numbers = append(numbers, deploy.Origin.Report)
		if deploy.StopOrigin != nil {
			numbers = append(numbers, deploy.StopOrigin.Report)
		}
	}
	return NextFeedbackReport(numbers...)
}

// applyFeedbackChangesLocked writes a planned change set: the stops, the
// updated deploys, the moved and started running versions, the newest report
// of each service, and the new deploys.
func (m *volatileMemory) applyFeedbackChangesLocked(scope memoryScopedKey, signal FeedbackSignal, origin FeedbackOrigin, changes FeedbackChanges) {
	deploys := slices.Clone(m.feedbackState.deploys[scope])
	running := slices.Clone(m.feedbackState.running[scope])
	stopped := make(map[string]bool, len(changes.Stopped))
	for _, version := range changes.Stopped {
		stopped[version.DeployID] = true
	}
	updated := make(map[string]FeedbackDeploy, len(changes.Updated))
	for _, deploy := range changes.Updated {
		updated[deploy.ID] = deploy
	}
	for i := range deploys {
		if stopped[deploys[i].ID] {
			at, by := signal.ArrivedAt, origin
			deploys[i].StoppedAt, deploys[i].StopOrigin = &at, &by
		}
		if deploy, ok := updated[deploys[i].ID]; ok {
			deploys[i] = deploy
		}
	}
	running = slices.DeleteFunc(running, func(v FeedbackRunningVersion) bool { return stopped[v.DeployID] })
	for i := range running {
		for _, moved := range changes.Moved {
			if running[i].Service == moved.Service && running[i].FeedbackVersion == moved.FeedbackVersion {
				running[i] = moved
			}
		}
	}
	running = append(running, changes.Started...)
	for i := range running {
		if slices.Contains(changes.Reported, running[i].Service) {
			running[i].ReportedAt = signal.ArrivedAt
		}
	}
	m.feedbackState.deploys[scope] = append(deploys, changes.Deploys...)
	m.feedbackState.running[scope] = running
}

// ListFeedbackSignals implements FeedbackStore.
func (m *volatileMemory) ListFeedbackSignals(ctx context.Context, environmentIDs []string) (map[string]FeedbackSignalState, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]FeedbackSignalState{}
	for _, id := range environmentIDs {
		if state, ok := m.feedbackState.signals[memoryScopedKey{workspace: workspace, id: id}]; ok {
			out[id] = state
		}
	}
	return out, nil
}

// ListFeedbackRunningVersions implements FeedbackStore.
func (m *volatileMemory) ListFeedbackRunningVersions(ctx context.Context, environmentID string) ([]FeedbackRunningVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]FeedbackRunningVersion{}, m.feedbackState.running[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: environmentID}]...)
	SortFeedbackRunningVersions(out)
	return out, nil
}

// ListFeedbackDeploys implements FeedbackStore.
func (m *volatileMemory) ListFeedbackDeploys(ctx context.Context, environmentID string) ([]FeedbackDeploy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]FeedbackDeploy{}, m.feedbackState.deploys[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: environmentID}]...)
	for i := range out {
		if out[i].StoppedAt != nil {
			at, by := *out[i].StoppedAt, *out[i].StopOrigin
			out[i].StoppedAt, out[i].StopOrigin = &at, &by
		}
	}
	SortFeedbackDeploys(out)
	return out, nil
}

// FindFeedbackServiceSignal implements FeedbackStore. The accepted reports
// are kept in the order of their numbers, and a GitHub deployment report has
// an empty label.
func (m *volatileMemory) FindFeedbackServiceSignal(ctx context.Context, query FeedbackServiceSignalQuery) (FeedbackBoundary, bool, error) {
	if err := CheckFeedbackServiceSignalQuery(query); err != nil {
		return FeedbackBoundary{}, false, err
	}
	digest := FeedbackServiceDigest(query.Service)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var found *FeedbackBoundary
	for _, accepted := range m.feedbackState.accepted[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: query.EnvironmentID}] {
		if accepted.Label == "" || !slices.Contains(accepted.Services, digest) {
			continue
		}
		place := FeedbackBoundary{At: accepted.ArrivedAt, Report: accepted.Report}
		switch {
		case query.Before != nil:
			if place.Compare(*query.Before) < 0 && (found == nil || place.Compare(*found) > 0) {
				found = &place
			}
		case place.At.After(*query.After):
			if found == nil || place.Compare(*found) < 0 {
				found = &place
			}
		}
	}
	if found == nil {
		return FeedbackBoundary{}, false, nil
	}
	return *found, true, nil
}
