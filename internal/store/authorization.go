package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// MembershipStore is the only workspace authorization boundary. Callers name
// capabilities and never inspect persisted roles directly (REQ-8/AC-8.1).
type MembershipStore interface {
	AuthorizeWorkspace(context.Context, string, string, core.Capability) (bool, error)
	ListWorkspacesForUser(context.Context, string) ([]core.Workspace, error)
	ListWorkspaceMembers(context.Context, string, string) ([]core.WorkspaceMembership, error)
	GrantWorkspaceRole(context.Context, string, string, core.WorkspaceRole) (core.MembershipGrant, error)
	RevokeWorkspaceInvitation(context.Context, string, string) error
	RevokeWorkspaceRole(context.Context, string, string) error
}
