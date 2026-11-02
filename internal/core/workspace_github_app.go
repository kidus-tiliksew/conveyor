package core

import "time"

// WorkspaceGitHubAppStatus is the secret-free workspace projection (DEC-59).
type WorkspaceGitHubAppStatus struct {
	Connected              bool       `json:"connected"`
	WorkspaceID            string     `json:"workspace_id"`
	AppID                  int64      `json:"app_id"`
	AppSlug                string     `json:"app_slug"`
	ClientID               string     `json:"client_id"`
	InstallationID         int64      `json:"installation_id,omitempty"`
	InstallationAccount    string     `json:"installation_account,omitempty"`
	ConnectedBy            string     `json:"connected_by"`
	ConnectedAt            time.Time  `json:"connected_at"`
	InstallationRecordedAt *time.Time `json:"installation_recorded_at,omitempty"`
}

// WorkspaceGitHubAppCredential crosses only the immediate in-process use boundary.
// req-delivery-and-forge AC-1.11 and AC-1.5: the PEM is encrypted at rest and never serialized.
type WorkspaceGitHubAppCredential struct {
	WorkspaceGitHubAppStatus
	PrivateKey string `json:"-"`
	// WebhookSecret is the secret GitHub's manifest conversion returned, when
	// it returned one. It is sealed like the private key and never serialized
	// (req-quiet-idle-runtime AC-3.2).
	WebhookSecret string `json:"-"`
}

// GitHubWebhookStatus is the secret-free webhook projection of a workspace:
// whether a webhook secret is set and when the last delivery was accepted. A
// workspace has webhook delivery from its first accepted delivery
// (req-quiet-idle-runtime AC-3.6; component-identity-membership, The webhook
// secret).
type GitHubWebhookStatus struct {
	SecretConfigured bool       `json:"secret_configured"`
	SecretSetAt      *time.Time `json:"secret_set_at"`
	LastDeliveryAt   *time.Time `json:"last_delivery_at"`
}

// HasWebhookDelivery reports whether the workspace has accepted a delivery.
func (s GitHubWebhookStatus) HasWebhookDelivery() bool { return s.LastDeliveryAt != nil }
