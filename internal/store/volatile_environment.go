package store

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The volatile environment state mirrors the three environment tables of the
// durable backends. Every mutation runs under the memory write mutex, the
// volatile backend's critical section.

type environmentKeyRecord struct {
	EnvironmentKey
	EnvironmentID string
	Hash          []byte
	RevokedAt     *time.Time
}

// environmentSuggestionRecord is one suggested or ignored name. An ignored
// name that GitHub never reported has no report.
type environmentSuggestionRecord struct {
	EnvironmentSuggestion
	Ignored bool
}

type environmentState struct {
	environments map[memoryScopedKey]Environment
	keys         map[memoryScopedKey]environmentKeyRecord
	// suggestions is keyed by workspace and the fold of the name.
	suggestions map[memoryScopedKey]environmentSuggestionRecord
}

func newEnvironmentState() environmentState {
	return environmentState{
		environments: map[memoryScopedKey]Environment{},
		keys:         map[memoryScopedKey]environmentKeyRecord{},
		suggestions:  map[memoryScopedKey]environmentSuggestionRecord{},
	}
}

var _ EnvironmentStore = (*volatileMemory)(nil)

func (m *volatileMemory) environmentRepositoriesLocked(workspace string) (map[string]bool, error) {
	record, ok := m.workspaces[workspace]
	if !ok {
		return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, workspace)
	}
	return EnvironmentRepositories(record.ConfigYAML)
}

func (m *volatileMemory) environmentsLocked(workspace string, query EnvironmentListQuery) []Environment {
	out := []Environment{}
	for key, environment := range m.environmentState.environments {
		if key.workspace == workspace && query.Matches(environment.Archived) {
			out = append(out, CloneEnvironment(environment))
		}
	}
	SortEnvironments(out)
	return out
}

func (m *volatileMemory) environmentKeysLocked(workspace string) map[string][]EnvironmentKey {
	out := map[string][]EnvironmentKey{}
	for key, record := range m.environmentState.keys {
		if key.workspace == workspace && record.RevokedAt == nil {
			out[record.EnvironmentID] = append(out[record.EnvironmentID], cloneEnvironmentKey(record.EnvironmentKey))
		}
	}
	return out
}

func cloneEnvironmentKey(key EnvironmentKey) EnvironmentKey {
	if key.LastUsedAt != nil {
		key.LastUsedAt = timePtr(*key.LastUsedAt)
	}
	return key
}

// forgetEnvironmentSuggestionsLocked drops the suggestion and the ignore of
// each name the unarchived environment lists (component-environments
// invariant 16).
func (m *volatileMemory) forgetEnvironmentSuggestionsLocked(environment Environment) {
	if environment.Archived {
		return
	}
	for _, name := range environment.GitHubEnvironments {
		delete(m.environmentState.suggestions, memoryScopedKey{workspace: environment.Workspace, id: EnvironmentFold(name)})
	}
}

// CreateEnvironment implements EnvironmentStore.
func (m *volatileMemory) CreateEnvironment(ctx context.Context, input EnvironmentInput) (EnvironmentRecord, error) {
	actor, err := RequireActor(ctx)
	if err != nil {
		return EnvironmentRecord{}, err
	}
	if input, err = NormalizeEnvironmentInput(input); err != nil {
		return EnvironmentRecord{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	repositories, err := m.environmentRepositoriesLocked(workspace)
	if err != nil {
		return EnvironmentRecord{}, err
	}
	if err := CheckEnvironmentRepositories(input.TrackedBranches, repositories); err != nil {
		return EnvironmentRecord{}, err
	}
	environment, err := NewEnvironment(workspace, actor.ID, input, time.Now())
	if err != nil {
		return EnvironmentRecord{}, err
	}
	if err := CheckEnvironmentConflicts(environment, m.environmentsLocked(workspace, EnvironmentListQuery{})); err != nil {
		return EnvironmentRecord{}, err
	}
	key := memoryScopedKey{workspace: workspace, id: environment.ID}
	if _, exists := m.environmentState.environments[key]; exists {
		return EnvironmentRecord{}, fmt.Errorf("environment %s already exists", environment.ID)
	}
	m.environmentState.environments[key] = CloneEnvironment(environment)
	m.forgetEnvironmentSuggestionsLocked(environment)
	m.appendEventLocked(ctx, core.Event{Kind: EnvironmentCreatedEvent, At: environment.CreatedAt, Payload: EnvironmentCreatedPayload(environment)})
	return EnvironmentRecord{Environment: environment, Keys: []EnvironmentKey{}}, nil
}

// UpdateEnvironment implements EnvironmentStore.
func (m *volatileMemory) UpdateEnvironment(ctx context.Context, id string, patch EnvironmentPatch) (EnvironmentRecord, error) {
	if _, err := RequireActor(ctx); err != nil {
		return EnvironmentRecord{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: strings.TrimSpace(id)}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.environmentState.environments[key]
	if !ok {
		return EnvironmentRecord{}, &EnvironmentNotFoundError{ID: key.id}
	}
	if err := CheckEnvironmentEditable(current, patch); err != nil {
		return EnvironmentRecord{}, err
	}
	patch, err := NormalizeEnvironmentPatch(patch)
	if err != nil {
		return EnvironmentRecord{}, err
	}
	next, kind, payload := ApplyEnvironmentPatch(current, patch)
	if patch.TrackedBranches != nil {
		repositories, err := m.environmentRepositoriesLocked(workspace)
		if err != nil {
			return EnvironmentRecord{}, err
		}
		if err := CheckEnvironmentRepositories(*patch.TrackedBranches, repositories); err != nil {
			return EnvironmentRecord{}, err
		}
	}
	if kind != "" {
		if err := CheckEnvironmentConflicts(next, m.environmentsLocked(workspace, EnvironmentListQuery{})); err != nil {
			return EnvironmentRecord{}, err
		}
		m.environmentState.environments[key] = CloneEnvironment(next)
		m.forgetEnvironmentSuggestionsLocked(next)
		m.appendEventLocked(ctx, core.Event{Kind: kind, At: EnvironmentTime(time.Now()), Payload: payload})
	}
	return EnvironmentRecords([]Environment{next}, m.environmentKeysLocked(workspace))[0], nil
}

// ListEnvironments implements EnvironmentStore.
func (m *volatileMemory) ListEnvironments(ctx context.Context, query EnvironmentListQuery) ([]Environment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.environmentsLocked(workspaceOrDefault(ctx, ""), query), nil
}

// ListEnvironmentRecords implements EnvironmentStore.
func (m *volatileMemory) ListEnvironmentRecords(ctx context.Context, query EnvironmentListQuery) ([]EnvironmentRecord, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	return EnvironmentRecords(m.environmentsLocked(workspace, query), m.environmentKeysLocked(workspace)), nil
}

// IssueEnvironmentKey implements EnvironmentStore.
func (m *volatileMemory) IssueEnvironmentKey(ctx context.Context, environmentID, label string) (IssuedEnvironmentKey, error) {
	actor, err := RequireActor(ctx)
	if err != nil {
		return IssuedEnvironmentKey{}, err
	}
	if label, err = NormalizeEnvironmentKeyLabel(label); err != nil {
		return IssuedEnvironmentKey{}, err
	}
	issued, hash, err := MintEnvironmentKey(label, actor.ID, time.Now())
	if err != nil {
		return IssuedEnvironmentKey{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	environmentID = strings.TrimSpace(environmentID)
	m.mu.Lock()
	defer m.mu.Unlock()
	environment, ok := m.environmentState.environments[memoryScopedKey{workspace: workspace, id: environmentID}]
	if !ok {
		return IssuedEnvironmentKey{}, &EnvironmentNotFoundError{ID: environmentID}
	}
	if environment.Archived {
		return IssuedEnvironmentKey{}, &EnvironmentArchivedError{ID: environmentID}
	}
	if len(m.environmentKeysLocked(workspace)[environmentID]) >= MaxEnvironmentKeys {
		return IssuedEnvironmentKey{}, &EnvironmentKeyLimitError{ID: environmentID}
	}
	m.environmentState.keys[memoryScopedKey{workspace: workspace, id: issued.ID}] = environmentKeyRecord{
		EnvironmentKey: issued.EnvironmentKey, EnvironmentID: environmentID, Hash: hash,
	}
	m.appendEventLocked(ctx, core.Event{Kind: EnvironmentKeyIssuedEvent, At: issued.CreatedAt, Payload: EnvironmentKeyPayload(workspace, environmentID, issued.ID, label)})
	return issued, nil
}

// RevokeEnvironmentKey implements EnvironmentStore.
func (m *volatileMemory) RevokeEnvironmentKey(ctx context.Context, environmentID, keyID string) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	workspace := workspaceOrDefault(ctx, "")
	environmentID, keyID = strings.TrimSpace(environmentID), strings.TrimSpace(keyID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.environmentState.environments[memoryScopedKey{workspace: workspace, id: environmentID}]; !ok {
		return &EnvironmentNotFoundError{ID: environmentID}
	}
	key := memoryScopedKey{workspace: workspace, id: keyID}
	record, ok := m.environmentState.keys[key]
	if !ok || record.EnvironmentID != environmentID {
		return &EnvironmentNotFoundError{ID: environmentID, KeyID: keyID}
	}
	if record.RevokedAt != nil {
		return nil
	}
	now := EnvironmentTime(time.Now())
	record.RevokedAt = &now
	m.environmentState.keys[key] = record
	m.appendEventLocked(ctx, core.Event{Kind: EnvironmentKeyRevokedEvent, At: now, Payload: EnvironmentKeyPayload(workspace, environmentID, keyID, record.Label)})
	return nil
}

// VerifyEnvironmentKey implements EnvironmentStore. The check and the last
// use stamp share the write mutex.
func (m *volatileMemory) VerifyEnvironmentKey(_ context.Context, value string) (VerifiedEnvironmentKey, error) {
	hash := EnvironmentKeyDigest(value)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, record := range m.environmentState.keys {
		if subtle.ConstantTimeCompare(record.Hash, hash) != 1 {
			continue
		}
		environment, ok := m.environmentState.environments[memoryScopedKey{workspace: key.workspace, id: record.EnvironmentID}]
		if !ok || environment.Archived || record.RevokedAt != nil {
			return VerifiedEnvironmentKey{}, ErrInvalidEnvironmentKey
		}
		now := EnvironmentTime(time.Now())
		if record.LastUsedAt == nil || now.Sub(*record.LastUsedAt) > EnvironmentKeyUseGranularity {
			record.LastUsedAt = &now
			m.environmentState.keys[key] = record
		}
		return VerifiedEnvironmentKey{Workspace: key.workspace, EnvironmentID: record.EnvironmentID, KeyID: record.ID, Label: record.Label}, nil
	}
	return VerifiedEnvironmentKey{}, ErrInvalidEnvironmentKey
}

// RecordEnvironmentReport implements EnvironmentStore.
func (m *volatileMemory) RecordEnvironmentReport(ctx context.Context, report EnvironmentReport) (bool, error) {
	report, err := NormalizeEnvironmentReport(report)
	if err != nil {
		return false, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	repositories, err := m.environmentRepositoriesLocked(workspace)
	if err != nil || !repositories[report.Repository] {
		return false, err
	}
	if _, listed := EnvironmentListing(m.environmentsLocked(workspace, EnvironmentListQuery{}), report.GitHubEnvironment); listed {
		return false, nil
	}
	key := memoryScopedKey{workspace: workspace, id: EnvironmentFold(report.GitHubEnvironment)}
	stored, exists := m.environmentState.suggestions[key]
	if exists && (stored.Ignored || !report.ReportedAt.After(stored.LastReportedAt)) {
		return false, nil
	}
	m.environmentState.suggestions[key] = environmentSuggestionRecord{EnvironmentSuggestion: EnvironmentSuggestion{
		GitHubEnvironment: report.GitHubEnvironment, Repository: report.Repository, LastReportedAt: report.ReportedAt,
	}}
	return true, nil
}

// ListEnvironmentSuggestions implements EnvironmentStore.
func (m *volatileMemory) ListEnvironmentSuggestions(ctx context.Context) ([]EnvironmentSuggestion, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []EnvironmentSuggestion{}
	for key, record := range m.environmentState.suggestions {
		if key.workspace == workspace && !record.Ignored {
			out = append(out, record.EnvironmentSuggestion)
		}
	}
	SortEnvironmentSuggestions(out)
	return out, nil
}

// IgnoreEnvironmentSuggestion implements EnvironmentStore.
func (m *volatileMemory) IgnoreEnvironmentSuggestion(ctx context.Context, gitHubEnvironment string) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	name, err := NormalizeEnvironmentGitHubName("github_environment", gitHubEnvironment)
	if err != nil {
		return err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := CheckEnvironmentNameUnlisted(m.environmentsLocked(workspace, EnvironmentListQuery{}), name); err != nil {
		return err
	}
	key := memoryScopedKey{workspace: workspace, id: EnvironmentFold(name)}
	stored, exists := m.environmentState.suggestions[key]
	if exists && stored.Ignored {
		return nil
	}
	if !exists {
		stored.GitHubEnvironment = name
	}
	stored.Ignored = true
	m.environmentState.suggestions[key] = stored
	m.appendEventLocked(ctx, core.Event{Kind: EnvironmentSuggestionIgnoredEvent, At: EnvironmentTime(time.Now()), Payload: EnvironmentSuggestionIgnoredPayload(workspace, stored.GitHubEnvironment)})
	return nil
}
