package store

import (
	"context"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"time"
)

type workspaceAppRecord struct {
	status core.WorkspaceGitHubAppStatus
	sealed gitHubAppKeyRecord
	// webhook is the sealed webhook secret, absent until one is set
	// (component-identity-membership, The webhook secret).
	webhook      *gitHubAppKeyRecord
	webhookSetAt time.Time
}

func (m *volatileMemory) StoreWorkspaceGitHubApp(ctx context.Context, id string, app core.WorkspaceGitHubAppCredential) (core.WorkspaceGitHubAppStatus, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.WorkspaceGitHubAppStatus{}, err
	}
	if err := ValidateWorkspaceGitHubApp(id, app); err != nil {
		return core.WorkspaceGitHubAppStatus{}, err
	}
	m.lock()
	defer m.unlock()
	if _, ok := m.workspaces[id]; !ok {
		return core.WorkspaceGitHubAppStatus{}, ErrNotFound
	}
	sealed, err := m.sealGitHubAppKey("workspace-app:"+id, app.PrivateKey, app.AppSlug)
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
	record := workspaceAppRecord{status: status, sealed: sealed}
	if app.WebhookSecret != "" {
		// A replacement App without a conversion secret drops the earlier one.
		webhook, sealErr := m.sealGitHubAppKey(string(GitHubWebhookSecretAAD(id)), app.WebhookSecret, app.AppSlug)
		if sealErr != nil {
			return core.WorkspaceGitHubAppStatus{}, sealErr
		}
		record.webhook, record.webhookSetAt = &webhook, status.ConnectedAt
	}
	m.workspaceGitHubApps[id] = record
	m.workspaceEventLocked(ctx, id, core.Event{Kind: kind, Payload: core.JSONPayload(WorkspaceGitHubAppEvent(status))})
	redact.RegisterSecret(app.PrivateKey, time.Time{})
	if app.WebhookSecret != "" {
		redact.RegisterSecret(app.WebhookSecret, time.Time{})
	}
	return status, nil
}
func (m *volatileMemory) RecordWorkspaceGitHubAppInstallation(ctx context.Context, id string, appID, installationID int64, account string) (core.WorkspaceGitHubAppStatus, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.WorkspaceGitHubAppStatus{}, err
	}
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
	pem, err := m.openGitHubAppKey(r.sealed)
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
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
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

func (m *volatileMemory) SetWorkspaceGitHubWebhookSecret(ctx context.Context, id, secret string) (time.Time, error) {
	if _, err := RequireActor(ctx); err != nil {
		return time.Time{}, err
	}
	if err := ValidateGitHubWebhookSecret(id, secret); err != nil {
		return time.Time{}, err
	}
	m.lock()
	defer m.unlock()
	r, ok := m.workspaceGitHubApps[id]
	if !ok {
		return time.Time{}, ErrNotFound
	}
	sealed, err := m.sealGitHubAppKey(string(GitHubWebhookSecretAAD(id)), secret, r.status.AppSlug)
	if err != nil {
		return time.Time{}, err
	}
	at := time.Now().UTC()
	r.webhook, r.webhookSetAt = &sealed, at
	m.workspaceGitHubApps[id] = r
	m.workspaceEventLocked(ctx, id, core.Event{Kind: GitHubWebhookSecretSetEvent, Payload: core.JSONPayload(GitHubWebhookSecretEvent(id, r.status.AppID, at))})
	redact.RegisterSecret(secret, time.Time{})
	return at, nil
}

func (m *volatileMemory) GetWorkspaceGitHubWebhookSecretForUse(_ context.Context, id string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.workspaceGitHubApps[id]
	if !ok || r.webhook == nil {
		return "", ErrNotFound
	}
	secret, err := m.openGitHubAppKey(*r.webhook)
	if err != nil {
		return "", err
	}
	redact.RegisterSecret(secret, time.Time{})
	return secret, nil
}

func (m *volatileMemory) RecordGitHubWebhookDelivery(ctx context.Context, id, deliveryID, event string, receivedAt time.Time) (bool, error) {
	if err := ValidateGitHubWebhookDelivery(id, deliveryID, event); err != nil {
		return false, err
	}
	m.lock()
	defer m.unlock()
	if _, ok := m.workspaces[id]; !ok {
		return false, ErrNotFound
	}
	if m.gitHubWebhookDeliveries == nil {
		m.gitHubWebhookDeliveries = map[string]map[string]time.Time{}
	}
	deliveries := m.gitHubWebhookDeliveries[id]
	if deliveries == nil {
		deliveries = map[string]time.Time{}
		m.gitHubWebhookDeliveries[id] = deliveries
	}
	if _, seen := deliveries[deliveryID]; seen {
		return false, nil
	}
	deliveries[deliveryID] = receivedAt.UTC().Truncate(time.Microsecond)
	return true, nil
}

func (m *volatileMemory) ForgetGitHubWebhookDelivery(_ context.Context, id, deliveryID string) error {
	m.lock()
	defer m.unlock()
	delete(m.gitHubWebhookDeliveries[id], deliveryID)
	return nil
}

func (m *volatileMemory) GitHubWebhookStatus(_ context.Context, id string) (core.GitHubWebhookStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.workspaces[id]; !ok {
		return core.GitHubWebhookStatus{}, ErrNotFound
	}
	var status core.GitHubWebhookStatus
	if r, ok := m.workspaceGitHubApps[id]; ok && r.webhook != nil {
		at := r.webhookSetAt
		status.SecretConfigured, status.SecretSetAt = true, &at
	}
	for _, at := range m.gitHubWebhookDeliveries[id] {
		if status.LastDeliveryAt == nil || at.After(*status.LastDeliveryAt) {
			last := at
			status.LastDeliveryAt = &last
		}
	}
	return status, nil
}
