package core

import "time"

// WorkspaceGitHubAppStatus is the secret-free workspace projection (DEC-41).
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
// req-security-boundaries AC-6.2: the PEM is encrypted at rest and never serialized.
type WorkspaceGitHubAppCredential struct {
	WorkspaceGitHubAppStatus
	PrivateKey string `json:"-"`
}
