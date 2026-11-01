package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

var ErrGitHubAppChanged = errors.New("workspace GitHub App changed; restart connection in workspace settings")

func ValidateWorkspaceGitHubApp(workspace string, app core.WorkspaceGitHubAppCredential) error {
	if strings.TrimSpace(workspace) == "" || app.AppID <= 0 || strings.TrimSpace(app.AppSlug) == "" || strings.TrimSpace(app.ClientID) == "" || strings.TrimSpace(app.PrivateKey) == "" {
		return errors.New("workspace GitHub App identity is incomplete")
	}
	return nil
}

func WorkspaceGitHubAppEvent(app core.WorkspaceGitHubAppStatus) map[string]any {
	return map[string]any{"workspace_id": app.WorkspaceID, "app_id": app.AppID, "app_slug": app.AppSlug, "installation_id": app.InstallationID, "installation_account": app.InstallationAccount}
}

// GitHubWebhookSecretSetEvent is the metadata-only workspace event a webhook
// secret write appends. Its payload never carries the secret.
const GitHubWebhookSecretSetEvent = "workspace.github_webhook_secret_set"

// GitHubWebhookStore owns the workspace's webhook secret and its accepted
// deliveries (req-quiet-idle-runtime AC-3.2, AC-3.3, AC-3.6;
// component-identity-membership, The webhook secret). Only
// GetWorkspaceGitHubWebhookSecretForUse returns the secret.
type GitHubWebhookStore interface {
	// SetWorkspaceGitHubWebhookSecret seals and stores the secret for a
	// workspace with a connected App; ErrNotFound without one.
	SetWorkspaceGitHubWebhookSecret(ctx context.Context, workspace, secret string) (time.Time, error)
	// GetWorkspaceGitHubWebhookSecretForUse returns the plaintext secret;
	// ErrNotFound when the workspace has none.
	GetWorkspaceGitHubWebhookSecretForUse(ctx context.Context, workspace string) (string, error)
	// RecordGitHubWebhookDelivery records one accepted delivery and reports
	// false, writing nothing, when the ID is already recorded.
	RecordGitHubWebhookDelivery(ctx context.Context, workspace, deliveryID, event string, receivedAt time.Time) (bool, error)
	// ForgetGitHubWebhookDelivery removes one delivery so a redelivery of a
	// delivery whose application failed is accepted again.
	ForgetGitHubWebhookDelivery(ctx context.Context, workspace, deliveryID string) error
	// GitHubWebhookStatus reports the secret's presence and the last
	// accepted delivery time.
	GitHubWebhookStatus(ctx context.Context, workspace string) (core.GitHubWebhookStatus, error)
}

// ValidateGitHubWebhookSecret refuses an empty or oversized secret.
func ValidateGitHubWebhookSecret(workspace, secret string) error {
	if strings.TrimSpace(workspace) == "" || strings.TrimSpace(secret) == "" || len(secret) > 1024 {
		return errors.New("GitHub webhook secret is incomplete")
	}
	return nil
}

// ValidateGitHubWebhookDelivery refuses a delivery without an ID or event.
func ValidateGitHubWebhookDelivery(workspace, deliveryID, event string) error {
	if strings.TrimSpace(workspace) == "" || strings.TrimSpace(deliveryID) == "" || strings.TrimSpace(event) == "" || len(deliveryID) > 255 || len(event) > 255 {
		return errors.New("GitHub webhook delivery is incomplete")
	}
	return nil
}

// GitHubWebhookSecretAAD is the additional data that binds a sealed webhook
// secret to its workspace, distinct from the App private key's.
func GitHubWebhookSecretAAD(workspace string) []byte {
	return []byte("workspace-webhook-secret:" + workspace)
}

// GitHubWebhookSecretEvent is the metadata-only payload of the secret event.
func GitHubWebhookSecretEvent(workspace string, appID int64, at time.Time) map[string]any {
	return map[string]any{"workspace_id": workspace, "app_id": appID, "set_at": at.UTC().Format(time.RFC3339Nano)}
}
