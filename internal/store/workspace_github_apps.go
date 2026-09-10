package store

import (
	"errors"
	"strings"

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
