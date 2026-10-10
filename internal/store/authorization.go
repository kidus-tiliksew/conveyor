package store

import (
	"context"
	"encoding/json"
	"errors"
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
//
// IssueSignInLink admits any active account or any pending invitation and
// serves only the host-local `conveyor user issue-link` act
// (req-invitations-and-sign-in REQ-3). The HTTP membership routes call
// IssueInvitationSignInLink, which requires a pending invitation for exactly
// the named workspace and email under the same email serialization that
// rotates and stores the link (req-accounts-and-membership AC-2.1, AC-4.3;
// component-identity-membership).
type InvitationSessionStore interface {
	IssueSignInLink(context.Context, string) (core.IssuedSignInLink, error)
	IssueInvitationSignInLink(ctx context.Context, workspaceID, email string, purpose SignInLinkPurpose) (core.IssuedSignInLink, error)
	RedeemSignInLink(context.Context, string) (core.DashboardSession, core.IdentityUser, error)
	SignInWithPassword(context.Context, string, string) (core.DashboardSession, core.IdentityUser, error)
	SetOwnPassword(context.Context, string, string, string, string) error
	VerifyDashboardSession(context.Context, string) (core.AuthenticatedCredential, error)
	RevokeDashboardSession(context.Context, string, string) error
	RecordInvitationDelivery(context.Context, string, string) error
}

// SignInLinkPurpose is selected by the server, never by request input. The
// resend purpose appends workspace.invitation_resent in the issuance
// transaction; the invitation purpose follows a grant and appends no resend.
type SignInLinkPurpose string

const (
	SignInLinkInvitation SignInLinkPurpose = "invitation"
	SignInLinkResend     SignInLinkPurpose = "resend"
)

// Valid reports whether purpose is one of the two server-selected purposes.
func (p SignInLinkPurpose) Valid() bool {
	return p == SignInLinkInvitation || p == SignInLinkResend
}

// Identity test-hook stages. Each backend calls the context hook at these
// points while holding its email serialization, so shared and native tests
// can hold one side of a race with channels or inject a failure at the resend
// audit append (component-identity-membership, Verification).
const (
	IdentityHookInvitationLinkLocked   = "invitation_link_locked"
	IdentityHookInvitationResentAudit  = "invitation_resent_audit"
	IdentityHookInvitationRevokeLocked = "invitation_revoke_locked"
	IdentityHookSignInRedeemLocked     = "signin_redeem_locked"
)

type identityTestHookKey struct{}

// WithIdentityTestHook attaches a test-only hook that identity stores call at
// the named stages. A non-nil error aborts the surrounding transaction. No
// production caller sets it.
func WithIdentityTestHook(ctx context.Context, hook func(stage string) error) context.Context {
	return context.WithValue(ctx, identityTestHookKey{}, hook)
}

// RunIdentityTestHook calls the context's identity test hook for stage, if
// any, and returns its error.
func RunIdentityTestHook(ctx context.Context, stage string) error {
	hook, _ := ctx.Value(identityTestHookKey{}).(func(string) error)
	if hook == nil {
		return nil
	}
	return hook(stage)
}

var (
	ErrInvalidCurrentPassword = errors.New("invalid current password")
	ErrInvalidPassword        = errors.New("password must contain between 12 and 1024 bytes")
	ErrGitHubAppKey           = errors.New("GitHub App key encryption key unavailable")
	ErrGitHubAppKeyDecrypt    = errors.New("GitHub App key decryption failed")
)

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
