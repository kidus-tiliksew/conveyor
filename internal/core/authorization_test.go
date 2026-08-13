package core

import "testing"

func TestRoleCapabilitiesAreBundles(t *testing.T) {
	if !RoleAllows(WorkspaceRoleViewer, CapabilityViewWorkspace) || RoleAllows(WorkspaceRoleViewer, CapabilityClaimWork) || RoleAllows(WorkspaceRoleViewer, CapabilityProposeDocuments) {
		t.Fatal("viewer must hold only the workspace view capability")
	}
	if !RoleAllows(WorkspaceRoleUser, CapabilityClaimWork) || !RoleAllows(WorkspaceRoleUser, CapabilityRequestChanges) || RoleAllows(WorkspaceRoleUser, CapabilityManageMembership) {
		t.Fatal("user capability bundle is incorrect")
	}
	if !RoleAllows(WorkspaceRoleOperator, CapabilityClaimWork) || !RoleAllows(WorkspaceRoleOperator, CapabilityManageMembership) {
		t.Fatal("operator must subsume user and operator capabilities")
	}
	// A role policy change is a bundle edit: the decision function and callers
	// continue naming the same capability.
	changedBundles := map[WorkspaceRole]map[Capability]bool{
		WorkspaceRoleUser: {CapabilityManageWorkspace: true},
	}
	if !roleAllows(changedBundles, WorkspaceRoleUser, CapabilityManageWorkspace) {
		t.Fatal("bundle edit did not alter the centralized decision")
	}
}

func TestRoleCapabilityOrdering(t *testing.T) {
	roles := []WorkspaceRole{WorkspaceRoleViewer, WorkspaceRoleUser, WorkspaceRoleOperator}
	for lowerIndex, lower := range roles {
		for capability, allowed := range roleCapabilities[lower] {
			if !allowed {
				continue
			}
			for _, higher := range roles[lowerIndex:] {
				if !RoleAllows(higher, capability) {
					t.Fatalf("role %q does not subsume %q capability %q", higher, lower, capability)
				}
			}
		}
	}
}
