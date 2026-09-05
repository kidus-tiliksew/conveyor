package store

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// workspaceRecord is the workspace entity: identity plus its versioned
// policy document, stored as the canonical YAML the API serves.
type workspaceRecord struct {
	core.Workspace
	ConfigYAML string `json:"config_yaml"`
}

// ListWorkspaces implements WorkspaceControlStore.
func (m *volatileMemory) ListWorkspaces(context.Context) ([]core.Workspace, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]core.Workspace, 0, len(m.workspaces))
	for _, record := range m.workspaces {
		result = append(result, record.Workspace)
	}
	sortWorkspaces(result)
	return result, nil
}

// GetWorkspace implements WorkspaceControlStore.
func (m *volatileMemory) GetWorkspace(_ context.Context, id string) (core.Workspace, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.workspaces[id]
	if !ok {
		return core.Workspace{}, fmt.Errorf("%w: workspace %s", ErrNotFound, id)
	}
	return record.Workspace, nil
}

// CreateWorkspace implements WorkspaceControlStore: the record, its repos,
// the creator's operator binding, and the audit event land together.
func (m *volatileMemory) CreateWorkspace(ctx context.Context, id, name string, cfg *config.Config) (core.Workspace, error) {
	data, err := config.MarshalPolicyDocument(cfg)
	if err != nil {
		return core.Workspace{}, err
	}
	m.lock()
	defer m.unlock()
	if _, exists := m.workspaces[id]; exists {
		return core.Workspace{}, ErrWorkspaceConflict
	}
	for _, record := range m.workspaces {
		if strings.EqualFold(record.Name, name) {
			return core.Workspace{}, ErrWorkspaceConflict
		}
	}
	record := workspaceRecord{Workspace: core.Workspace{ID: id, Name: name, ConfigVersion: 1, CreatedAt: time.Now().UTC()}, ConfigYAML: string(data)}
	m.workspaces[id] = record
	m.upsertReposLocked(id, cfg.Repos)
	if credential, ok := CredentialFromContext(ctx); ok {
		m.memberships[memoryScopedKey{workspace: id, id: credential.OwnerUserID}] = workspaceBinding{Role: core.WorkspaceRoleOperator, CreatedAt: record.CreatedAt}
	}
	m.workspaceEventLocked(ctx, id, core.Event{Kind: "workspace.created", Payload: core.JSONPayload(map[string]any{"id": id, "name": name, "config_version": record.ConfigVersion})})
	return record.Workspace, nil
}

func (m *volatileMemory) upsertReposLocked(workspaceID string, repos []config.Repo) {
	bases := m.repositories[workspaceID]
	if bases == nil {
		bases = map[string]string{}
		m.repositories[workspaceID] = bases
	}
	for _, repo := range repos {
		bases[repo.Name] = repo.Base
	}
}

// BootstrapConfig implements WorkspaceConfigStore.
func (m *volatileMemory) BootstrapConfig(ctx context.Context, cfg *config.Config) error {
	_, err := m.BootstrapWorkspaceConfig(ctx, cfg)
	return err
}

// BootstrapWorkspaceConfig imports the configured workspace once. Later
// starts canonicalize a legacy stored document and report seeded=false so
// the caller can say the file's workspace sections were ignored.
func (m *volatileMemory) BootstrapWorkspaceConfig(_ context.Context, cfg *config.Config) (bool, error) {
	configYAML, err := config.MarshalPolicyDocument(cfg)
	if err != nil {
		return false, err
	}
	m.lock()
	defer m.unlock()
	if record, exists := m.workspaces[cfg.Workspace]; exists {
		stored, legacy, parseErr := config.ParseStoredWorkspaceDocument([]byte(record.ConfigYAML), cfg, "stored workspace config")
		if parseErr != nil {
			return false, parseErr
		}
		if legacy {
			canonical, marshalErr := config.MarshalPolicyDocument(stored)
			if marshalErr != nil {
				return false, marshalErr
			}
			record.ConfigYAML = string(canonical)
			m.workspaces[cfg.Workspace] = record
		}
		return false, nil
	}
	now := time.Now().UTC()
	m.workspaces[cfg.Workspace] = workspaceRecord{Workspace: core.Workspace{ID: cfg.Workspace, Name: cfg.Workspace, ConfigVersion: 1, CreatedAt: now}, ConfigYAML: string(configYAML)}
	m.upsertReposLocked(cfg.Workspace, cfg.Repos)
	// Identity bootstraps before the configured workspace; the earliest
	// account becomes its operator so the deployment token stays zero-config.
	if first, ok := m.earliestUserLocked(); ok {
		key := memoryScopedKey{workspace: cfg.Workspace, id: first.ID}
		if _, bound := m.memberships[key]; !bound {
			m.memberships[key] = workspaceBinding{Role: core.WorkspaceRoleOperator, CreatedAt: now}
		}
	}
	return true, nil
}

func (m *volatileMemory) earliestUserLocked() (identityUser, bool) {
	var users []identityUser
	for _, user := range m.users {
		users = append(users, user)
	}
	if len(users) == 0 {
		return identityUser{}, false
	}
	sort.Slice(users, func(i, j int) bool {
		if !users[i].CreatedAt.Equal(users[j].CreatedAt) {
			return users[i].CreatedAt.Before(users[j].CreatedAt)
		}
		return users[i].ID < users[j].ID
	})
	return users[0], true
}

// WorkspaceConfig implements WorkspaceConfigStore.
func (m *volatileMemory) WorkspaceConfig(ctx context.Context) (config.VersionedDocument, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id := workspaceOrDefault(ctx, "")
	record, ok := m.workspaces[id]
	if !ok {
		return config.VersionedDocument{}, fmt.Errorf("%w: workspace %s", ErrNotFound, id)
	}
	var document config.WorkspaceDocument
	decoder := yaml.NewDecoder(strings.NewReader(record.ConfigYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return config.VersionedDocument{}, fmt.Errorf("decode stored workspace config: %w", err)
	}
	if document.Harnesses == nil {
		document.Harnesses = []config.Harness{}
	}
	if document.Repos == nil {
		document.Repos = []config.Repo{}
	}
	return config.VersionedDocument{Document: document, Version: record.ConfigVersion}, nil
}

// RuntimeConfig overlays the stored document onto the immutable deployment
// settings. Callers take one value per dispatch so a running job never
// observes a mid-flight policy change.
func (m *volatileMemory) RuntimeConfig(ctx context.Context, deployment *config.Config) (*config.Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id := workspaceOrDefault(ctx, "")
	record, ok := m.workspaces[id]
	if !ok {
		return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, id)
	}
	base := *deployment
	base.Workspace = id
	cfg, _, err := config.ParseStoredWorkspaceDocument([]byte(record.ConfigYAML), &base, "stored workspace config")
	return cfg, err
}

// UpdateWorkspaceConfig implements WorkspaceConfigStore as a compare-and-set
// on the document version.
func (m *volatileMemory) UpdateWorkspaceConfig(ctx context.Context, expectedVersion int64, next *config.Config) (config.UpdateReceipt, error) {
	data, err := config.MarshalPolicyDocument(next)
	if err != nil {
		return config.UpdateReceipt{}, err
	}
	m.lock()
	defer m.unlock()
	id := workspaceOrDefault(ctx, "")
	record, ok := m.workspaces[id]
	if !ok {
		return config.UpdateReceipt{}, fmt.Errorf("%w: workspace %s", ErrNotFound, id)
	}
	if record.ConfigVersion != expectedVersion {
		return config.UpdateReceipt{}, config.ErrVersionConflict
	}
	var previous config.WorkspaceDocument
	if err := yaml.Unmarshal([]byte(record.ConfigYAML), &previous); err != nil {
		return config.UpdateReceipt{}, fmt.Errorf("decode previous workspace config: %w", err)
	}
	from := record.ConfigVersion
	record.ConfigYAML, record.ConfigVersion = string(data), record.ConfigVersion+1
	m.workspaces[id] = record
	m.upsertReposLocked(id, next.Repos)
	sections := configDiff(previous, next.PolicyDocument())
	actor := ActorFromContext(ctx)
	event := m.workspaceEventLocked(ctx, id, core.Event{Kind: "config.updated", Payload: core.JSONPayload(map[string]any{
		"from_version": from, "to_version": record.ConfigVersion, "sections": sections,
	})})
	return config.UpdateReceipt{
		VersionedDocument: config.VersionedDocument{Document: next.PolicyDocument(), Version: record.ConfigVersion},
		EventID:           event.ID, ActorID: actor.ID, Sections: sections,
	}, nil
}

func configDiff(before, after config.WorkspaceDocument) []string {
	sections := make([]string, 0, 9)
	if before.Workspace != after.Workspace || before.MaxBounces != after.MaxBounces ||
		before.WorkOrderQueueTimeoutText != after.WorkOrderQueueTimeoutText {
		sections = append(sections, "workspace")
	}
	if !reflect.DeepEqual(before.Routing, after.Routing) {
		sections = append(sections, "routing")
	}
	if !reflect.DeepEqual(before.ExecutionSettings, after.ExecutionSettings) {
		sections = append(sections, "execution_settings")
	}
	if !reflect.DeepEqual(before.Repos, after.Repos) {
		sections = append(sections, "repos")
	}
	if !reflect.DeepEqual(before.Monitor, after.Monitor) {
		sections = append(sections, "monitor")
	}
	if !reflect.DeepEqual(before.Harnesses, after.Harnesses) {
		sections = append(sections, "harnesses")
	}
	if !reflect.DeepEqual(before.Review, after.Review) {
		sections = append(sections, "review")
	}
	if !reflect.DeepEqual(before.Setups, after.Setups) || before.DefaultSetup != after.DefaultSetup {
		sections = append(sections, "setups")
	}
	if !reflect.DeepEqual(before.Execution, after.Execution) {
		sections = append(sections, "execution")
	}
	return sections
}

// ReconcileBlueprintClosures implements BlueprintClosureReconciler: a queued
// parent whose children have all finished is closed.
func (m *volatileMemory) ReconcileBlueprintClosures(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	var parents []core.Task
	for _, task := range m.tasks {
		if task.Workspace == workspace && task.State == core.TaskQueued {
			parents = append(parents, task)
		}
	}
	sort.Slice(parents, func(i, j int) bool {
		if !parents[i].CreatedAt.Equal(parents[j].CreatedAt) {
			return parents[i].CreatedAt.Before(parents[j].CreatedAt)
		}
		return parents[i].ID < parents[j].ID
	})
	closed := 0
	for _, parent := range parents {
		if m.closeBlueprintParentLocked(ctx, parent.ID) {
			closed++
		}
	}
	return closed, nil
}

// Volatile dispatch uses the in-process queue and has no durable jobs to repair.
func (m *volatileMemory) ReconcileQueuedTasks(context.Context) (int, error) { return 0, nil }
