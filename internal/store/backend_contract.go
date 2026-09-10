package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// Backend is the complete store contract required by daemon and host CLI
// wiring. PostgreSQL owns its relational schema and transactions (DEC-36;
// component-persistence). Optional capability discovery is not deployment
// validation.
type Backend interface {
	Store
	monitor.Store
	IdentityProvisioner
	CallerIdentityStore
	OwnProfileStore
	MembershipStore
	InvitationSessionStore
	redact.SecretSource
	WorkspaceGitHubAppStore
	PersonalAccessTokenStore
	AgentCredentialStore
	WorkspaceControlStore
	Close()
	ConfigureForgeTokenEncryptionKey([]byte)
	BootstrapIdentity(context.Context, config.FirstOperatorIdentity, string) (bool, error)
	BootstrapWorkspaceConfig(context.Context, *config.Config) (bool, error)
	RuntimeConfig(context.Context, *config.Config) (*config.Config, error)
	WorkspaceConfig(context.Context) (config.VersionedDocument, error)
	UpdateWorkspaceConfig(context.Context, int64, *config.Config) (config.UpdateReceipt, error)
	VerifyPersonalAccessToken(context.Context, string) (core.IdentityUser, error)
	VerifyCredential(context.Context, string) (core.AuthenticatedCredential, error)
	ReconcileQueuedTasks(context.Context) (int, error)
	ReconcileBlueprintClosures(context.Context) (int, error)
	Log() eventlog.Store
}

// WorkspaceGitHubAppStore owns encrypted app identity and secret-free status.
// Only GetWorkspaceGitHubAppForUse returns the private key (DEC-41).
type WorkspaceGitHubAppStore interface {
	StoreWorkspaceGitHubApp(context.Context, string, core.WorkspaceGitHubAppCredential) (core.WorkspaceGitHubAppStatus, error)
	RecordWorkspaceGitHubAppInstallation(context.Context, string, int64, int64, string) (core.WorkspaceGitHubAppStatus, error)
	GetWorkspaceGitHubAppStatus(context.Context, string) (core.WorkspaceGitHubAppStatus, error)
	GetWorkspaceGitHubAppForUse(context.Context, string) (core.WorkspaceGitHubAppCredential, error)
	DeleteWorkspaceGitHubApp(context.Context, string) error
}
