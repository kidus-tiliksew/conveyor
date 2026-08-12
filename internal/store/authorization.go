package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// IdentityProvisioner is the deployment-administration account boundary.
// It must only be mounted behind a human operator-scoped credential; workspace
// capabilities do not authorize account creation (REQ-1/AC-1.2-AC-1.3).
type IdentityProvisioner interface {
	ProvisionIdentityUser(context.Context, string, string) (core.IdentityUser, error)
}

// MembershipStore is the only workspace authorization boundary. Callers name
// capabilities and never inspect persisted roles directly (REQ-8/AC-8.1).
type MembershipStore interface {
	AuthorizeDeployment(context.Context, string, core.Capability) (bool, error)
	AuthorizeWorkspace(context.Context, string, string, core.Capability) (bool, error)
	ListWorkspacesForUser(context.Context, string) ([]core.Workspace, error)
	ListWorkspaceMembers(context.Context, string, string) ([]core.WorkspaceMembership, error)
	GrantWorkspaceRole(context.Context, string, string, core.WorkspaceRole) (core.MembershipGrant, error)
	RevokeWorkspaceInvitation(context.Context, string, string) error
	RevokeWorkspaceRole(context.Context, string, string) error
}
