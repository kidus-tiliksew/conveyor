package store

import (
	"context"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"time"
)

type workspaceAppRecord struct {
	status core.WorkspaceGitHubAppStatus
	sealed forgeTokenRecord
}

func (m *volatileMemory) StoreWorkspaceGitHubApp(ctx context.Context, id string, app core.WorkspaceGitHubAppCredential) (core.WorkspaceGitHubAppStatus, error) {
	if err := ValidateWorkspaceGitHubApp(id, app); err != nil {
		return core.WorkspaceGitHubAppStatus{}, err
	}
	m.lock()
	defer m.unlock()
	if _, ok := m.workspaces[id]; !ok {
		return core.WorkspaceGitHubAppStatus{}, ErrNotFound
	}
	sealed, err := m.sealForgeToken("workspace-app:"+id, app.PrivateKey, app.AppSlug)
	if err != nil {
		return core.WorkspaceGitHubAppStatus{}, err
	}
	status := core.WorkspaceGitHubAppStatus{Connected: true, WorkspaceID: id, AppID: app.AppID, AppSlug: app.AppSlug, ClientID: app.ClientID, ConnectedBy: ActorFromContext(ctx).ID, ConnectedAt: time.Now().UTC()}
	kind := "workspace.github_app_connected"
	if _, ok := m.workspaceGitHubApps[id]; ok {
		kind = "workspace.github_app_replaced"
	}
	if m.workspaceGitHubApps == nil {
		m.workspaceGitHubApps = map[string]workspaceAppRecord{}
	}
	m.workspaceGitHubApps[id] = workspaceAppRecord{status, sealed}
	m.workspaceEventLocked(ctx, id, core.Event{Kind: kind, Payload: core.JSONPayload(WorkspaceGitHubAppEvent(status))})
	redact.RegisterSecret(app.PrivateKey, time.Time{})
	return status, nil
}
func (m *volatileMemory) RecordWorkspaceGitHubAppInstallation(ctx context.Context, id string, appID, installationID int64, account string) (core.WorkspaceGitHubAppStatus, error) {
	m.lock()
	defer m.unlock()
	r, ok := m.workspaceGitHubApps[id]
	if !ok {
		return core.WorkspaceGitHubAppStatus{}, ErrNotFound
	}
	if r.status.AppID != appID {
		return core.WorkspaceGitHubAppStatus{}, ErrGitHubAppChanged
	}
	if installationID <= 0 || account == "" {
		return core.WorkspaceGitHubAppStatus{}, ErrNotFound
	}
	now := time.Now().UTC()
	r.status.InstallationID = installationID
	r.status.InstallationAccount = account
	r.status.InstallationRecordedAt = &now
	m.workspaceGitHubApps[id] = r
	m.workspaceEventLocked(ctx, id, core.Event{Kind: "workspace.github_app_installation_recorded", Payload: core.JSONPayload(WorkspaceGitHubAppEvent(r.status))})
	result := r.status
	at := *result.InstallationRecordedAt
	result.InstallationRecordedAt = &at
	return result, nil
}
func (m *volatileMemory) GetWorkspaceGitHubAppStatus(_ context.Context, id string) (core.WorkspaceGitHubAppStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.workspaces[id]; !ok {
		return core.WorkspaceGitHubAppStatus{}, ErrNotFound
	}
	r := m.workspaceGitHubApps[id]
	result := r.status
	if result.InstallationRecordedAt != nil {
		at := *result.InstallationRecordedAt
		result.InstallationRecordedAt = &at
	}
	return result, nil
}
func (m *volatileMemory) GetWorkspaceGitHubAppForUse(_ context.Context, id string) (core.WorkspaceGitHubAppCredential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.workspaceGitHubApps[id]
	if !ok {
		return core.WorkspaceGitHubAppCredential{}, ErrNotFound
	}
	pem, err := m.openForgeToken(r.sealed)
	if err != nil {
		return core.WorkspaceGitHubAppCredential{}, err
	}
	redact.RegisterSecret(pem, time.Time{})
	status := r.status
	if status.InstallationRecordedAt != nil {
		at := *status.InstallationRecordedAt
		status.InstallationRecordedAt = &at
	}
	return core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: status, PrivateKey: pem}, nil
}
func (m *volatileMemory) DeleteWorkspaceGitHubApp(ctx context.Context, id string) error {
	m.lock()
	defer m.unlock()
	if id == "" {
		return ErrNotFound
	}
	r, ok := m.workspaceGitHubApps[id]
	if !ok {
		return nil
	}
	delete(m.workspaceGitHubApps, id)
	m.workspaceEventLocked(ctx, id, core.Event{Kind: "workspace.github_app_disconnected", Payload: core.JSONPayload(WorkspaceGitHubAppEvent(r.status))})
	return nil
}
