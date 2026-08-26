package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// IdentityProvisioner is the deployment-administration account boundary.
// It must only be mounted behind a human operator-scoped credential; workspace
// capabilities do not authorize account creation (REQ-1/AC-1.2-AC-1.3).
type IdentityProvisioner interface {
	ProvisionIdentityUser(context.Context, string, string) (core.IdentityUser, error)
}

// CallerIdentityStore reads only the authenticated caller. The caller user ID
// comes from credential context; workspaceID is empty unless an authorized
// optional workspace context was supplied (REQ-2/AC-2.1, REQ-3/AC-3.1).
type CallerIdentityStore interface {
	GetCallerIdentity(context.Context, string, string) (core.CallerIdentity, error)
}

// OwnProfileStore mutates only the authenticated dashboard-session owner. Both
// identifiers come from the verified cookie, so another user's profile cannot
// be addressed through this boundary (REQ-10/AC-10.8).
type OwnProfileStore interface {
	SetOwnDisplayName(context.Context, string, string, string) (core.CallerIdentity, error)
}

// MembershipStore is the only workspace authorization boundary. Callers name
// capabilities and never inspect persisted roles directly (REQ-8/AC-8.1).
type MembershipStore interface {
	AuthorizeDeployment(context.Context, string, core.Capability) (bool, error)
	AuthorizeWorkspace(context.Context, string, string, core.Capability) (bool, error)
	ListWorkspacesForUser(context.Context, string) ([]core.Workspace, error)
	ListWorkspaceMembers(context.Context, string, string) ([]core.WorkspaceMembership, error)
	ListWorkspaceInvitations(context.Context, string) ([]core.WorkspaceInvitation, error)
	GrantWorkspaceRole(context.Context, string, string, core.WorkspaceRole) (core.MembershipGrant, error)
	RevokeWorkspaceInvitation(context.Context, string, string) error
	RevokeWorkspaceRole(context.Context, string, string) error
}

// InvitationSessionStore owns the opaque, hashed browser bootstrap
// credentials. Issuance is restricted to an existing invitation or account;
// there is deliberately no registration operation.
type InvitationSessionStore interface {
	IssueSignInLink(context.Context, string) (core.IssuedSignInLink, error)
	RedeemSignInLink(context.Context, string) (core.DashboardSession, core.IdentityUser, error)
	SignInWithPassword(context.Context, string, string) (core.DashboardSession, core.IdentityUser, error)
	SetOwnPassword(context.Context, string, string, string, string) error
	VerifyDashboardSession(context.Context, string) (core.AuthenticatedCredential, error)
	RevokeDashboardSession(context.Context, string, string) error
	RecordInvitationDelivery(context.Context, string, string) error
}

var (
	ErrInvalidCurrentPassword  = errors.New("invalid current password")
	ErrInvalidPassword         = errors.New("password must contain between 12 and 1024 bytes")
	ErrForgeTokenKey           = errors.New("forge token encryption key unavailable")
	ErrForgeTokenDecrypt       = errors.New("forge token decryption failed")
	ErrForgeTokenOwnerInactive = errors.New("forge token owner is inactive")
	ErrForgeTokenRequired      = errors.New(ForgeTokenRequiredMessage)
)

const (
	ForgeTokenRequiredCode    = "forge_token_required"
	ForgeTokenRequiredMessage = "stored forge token is required; add one in account settings before claiming work"
)

// RequireForgeTokenPresence is the metadata-only eligibility check shared by
// read projections and non-durable claim paths. Durable claims repeat this
// check transactionally without decrypting the token or contacting the forge.
func RequireForgeTokenPresence(ctx context.Context, tokens ForgeTokenStore, ownerUserID string) error {
	if tokens == nil || ownerUserID == "" {
		return ErrForgeTokenRequired
	}
	status, err := tokens.GetForgeTokenStatus(ctx, ownerUserID)
	if errors.Is(err, ErrNotFound) || err == nil && !status.Configured {
		return ErrForgeTokenRequired
	}
	if err != nil {
		return fmt.Errorf("check stored forge token presence: %w", err)
	}
	return nil
}

// ForgeTokenStore is the sole recoverable-credential boundary. Management
// methods take a credential-derived owner; presence is metadata-only for later
// claim/preflight consumers; use and redaction lookups are the only plaintext
// exits and must fail closed for inactive owners or cipher failures.
type ForgeTokenStore interface {
	StoreForgeToken(context.Context, string, string, string) (core.ForgeTokenStatus, error)
	DeleteForgeToken(context.Context, string) error
	GetForgeTokenStatus(context.Context, string) (core.ForgeTokenStatus, error)
	GetForgeTokenForUse(context.Context, string) (core.ForgeTokenCredential, error)
	ListForgeTokensForRedaction(context.Context) ([]string, error)
}

// WorkspaceForgeTokenStore owns the single recoverable forge credential for a
// workspace. Management callers supply a workspace resolved by the HTTP
// capability boundary; only use and redaction reads may return plaintext.
type WorkspaceForgeTokenStore interface {
	StoreWorkspaceForgeToken(context.Context, string, string, string) (core.ForgeTokenStatus, error)
	DeleteWorkspaceForgeToken(context.Context, string) error
	GetWorkspaceForgeTokenStatus(context.Context, string) (core.ForgeTokenStatus, error)
	GetWorkspaceForgeTokenForUse(context.Context, string) (core.WorkspaceForgeTokenCredential, error)
}

// PersonalAccessTokenStore is the self-service human-credential boundary. Every
// method takes the owning user resolved from the presented credential, so a
// caller cannot name another user's tokens: cross-user reads and revocations
// are unrepresentable here rather than merely rejected (REQ-2/AC-2.1).
// Administrative revocation by token ID alone stays outside this interface.
type PersonalAccessTokenStore interface {
	ListOwnPersonalAccessTokens(context.Context, string) ([]core.PersonalAccessToken, error)
	IssueOwnPersonalAccessToken(context.Context, string, string) (core.IssuedPersonalAccessToken, error)
	RevokeOwnPersonalAccessToken(context.Context, string, string) (core.PersonalAccessToken, error)
}

// AgentCredentialStore is the execution-only credential boundary used by the
// attended run parent. The HTTP layer derives the owner from the authenticated
// user and validates the exact live work-order session before calling it
// (req-security-boundaries REQ-1/AC-1.1, REQ-2/AC-2.2-AC-2.3).
type AgentCredentialStore interface {
	IssueAgentCredential(context.Context, string, string) (IssuedAgentCredential, error)
	RevokeRunAgentCredential(context.Context, string, string, RunAgentCredentialBinding) error
}

type IssuedAgentCredential struct {
	ID     string `json:"credential_id"`
	UserID string `json:"-"`
	Label  string `json:"-"`
	Value  string `json:"credential"`
}

const runAgentCredentialLabelPrefix = "conveyor-run:"

type RunAgentCredentialBinding struct {
	WorkspaceID string `json:"workspace_id"`
	WorkOrderID string `json:"work_order_id"`
	SessionID   string `json:"session_id"`
}

var ErrRunAgentCredentialBindingMismatch = errors.New("agent credential does not match the task run binding")

func RunAgentCredentialLabel(binding RunAgentCredentialBinding) (string, error) {
	if strings.TrimSpace(binding.WorkspaceID) == "" || strings.TrimSpace(binding.WorkOrderID) == "" || strings.TrimSpace(binding.SessionID) == "" {
		return "", errors.New("run agent credential binding is incomplete")
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	return runAgentCredentialLabelPrefix + string(encoded), nil
}

func ParseRunAgentCredentialLabel(label string) (RunAgentCredentialBinding, bool) {
	if !strings.HasPrefix(label, runAgentCredentialLabelPrefix) {
		return RunAgentCredentialBinding{}, false
	}
	var binding RunAgentCredentialBinding
	if err := json.Unmarshal([]byte(strings.TrimPrefix(label, runAgentCredentialLabelPrefix)), &binding); err != nil ||
		strings.TrimSpace(binding.WorkspaceID) == "" || strings.TrimSpace(binding.WorkOrderID) == "" || strings.TrimSpace(binding.SessionID) == "" {
		return RunAgentCredentialBinding{}, false
	}
	return binding, true
}
