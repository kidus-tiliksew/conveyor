package core

import "time"

// WorkspaceRole is a persisted membership label. Authorization call sites use
// Capability instead, so changing a bundle never requires editing enforcement.
type WorkspaceRole string

const (
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

type MembershipGrant struct {
	Email string        `json:"email"`
	Role  WorkspaceRole `json:"role"`
}
