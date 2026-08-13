package core

import "time"

// WorkspaceRole is a persisted membership label. Authorization call sites use
// Capability instead, so changing a bundle never requires editing enforcement.
type WorkspaceRole string

const (
	WorkspaceRoleViewer   WorkspaceRole = "viewer"
	WorkspaceRoleUser     WorkspaceRole = "user"
	WorkspaceRoleOperator WorkspaceRole = "operator"
)

type Capability string

const (
	CapabilityViewWorkspace    Capability = "view_workspace"
	CapabilityClaimWork        Capability = "claim_work"
	CapabilityRequestChanges   Capability = "request_changes"
	CapabilityProposeDocuments Capability = "propose_documents"
	CapabilityConfirmDocuments Capability = "confirm_documents"
	CapabilityManageMembership Capability = "manage_membership"
	CapabilitySetAssignee      Capability = "set_assignee"
	CapabilityOperateGates     Capability = "operate_gates"
	CapabilityRecoverWork      Capability = "recover_work"
	CapabilityManageWorkspace  Capability = "manage_workspace"
)

// roleCapabilities is the single role-to-capability decision table (REQ-8,
// AC-8.1). Operator explicitly subsumes the user bundle.
var roleCapabilities = map[WorkspaceRole]map[Capability]bool{
	WorkspaceRoleViewer: {
		CapabilityViewWorkspace: true,
	},
	WorkspaceRoleUser: {
		CapabilityViewWorkspace:    true,
		CapabilityClaimWork:        true,
		CapabilityRequestChanges:   true,
		CapabilityProposeDocuments: true,
	},
	WorkspaceRoleOperator: {
		CapabilityViewWorkspace:    true,
		CapabilityClaimWork:        true,
		CapabilityRequestChanges:   true,
		CapabilityProposeDocuments: true,
		CapabilityConfirmDocuments: true,
		CapabilityManageMembership: true,
		CapabilitySetAssignee:      true,
		CapabilityOperateGates:     true,
		CapabilityRecoverWork:      true,
		CapabilityManageWorkspace:  true,
	},
}

func RoleAllows(role WorkspaceRole, capability Capability) bool {
	return roleAllows(roleCapabilities, role, capability)
}

func roleAllows(bundles map[WorkspaceRole]map[Capability]bool, role WorkspaceRole, capability Capability) bool {
	return bundles[role][capability]
}

type WorkspaceMembership struct {
	WorkspaceID string        `json:"workspace_id"`
	UserID      string        `json:"user_id"`
	Email       string        `json:"email,omitempty"`
	DisplayName string        `json:"display_name,omitempty"`
	Role        WorkspaceRole `json:"role"`
	CreatedAt   time.Time     `json:"created_at"`
}

// IdentityUser is a deployment-scoped account. Account provisioning is an
// instance-administration act and is deliberately separate from workspace
// membership capabilities (REQ-1/AC-1.2-AC-1.3).
type IdentityUser struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// CallerIdentity is the deliberately narrow self-identity projection. Role is
// present only when the caller supplied an authorized workspace context.
type CallerIdentity struct {
	ID          string        `json:"id"`
	Email       string        `json:"email"`
	DisplayName string        `json:"display_name"`
	Role        WorkspaceRole `json:"role,omitempty"`
}

type MembershipGrant struct {
	Email     string        `json:"email"`
	Role      WorkspaceRole `json:"role"`
	SignInURL string        `json:"sign_in_url,omitempty"`
	Delivery  string        `json:"delivery,omitempty"`
}

// IssuedSignInLink is secret-bearing and may only cross the operator response
// or outbound email boundary. Only TokenHash is persisted.
type IssuedSignInLink struct {
	Email     string    `json:"email"`
	Value     string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
}

type DashboardSession struct {
	ID        string
	UserID    string
	Value     string
	ExpiresAt time.Time
}

// WorkspaceInvitation is an unredeemed membership grant addressed to an email.
// Redemption and revocation both delete the row, so an invitation record is
// pending by construction and carries no account reference: inviting addresses
// an email and never resolves the deployment's user directory (REQ-3/AC-3.2).
type WorkspaceInvitation struct {
	WorkspaceID string        `json:"workspace_id"`
	Email       string        `json:"email"`
	Role        WorkspaceRole `json:"role"`
	// InvitedBy and its rendered name describe a workspace member the reader
	// already sees in the member list, so naming the inviter discloses nothing
	// the caller could not read from that list.
	InvitedBy            string    `json:"invited_by"`
	InvitedByDisplayName string    `json:"invited_by_display_name,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// PersonalAccessToken is the non-secret view of a human credential. The bearer
// value is stored only as a hash and is returned exactly once from issuance, so
// no read path can reconstruct it (REQ-2, req-security-boundaries AC-2.1).
type PersonalAccessToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Label      string     `json:"label"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// IssuedPersonalAccessToken carries the one-time bearer value. Issuance is the
// only operation that produces it; listing a token never does.
type IssuedPersonalAccessToken struct {
	PersonalAccessToken
	Value string `json:"value"`
}
