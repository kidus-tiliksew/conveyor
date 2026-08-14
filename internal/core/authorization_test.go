package core

import "testing"

func TestRoleCapabilitiesAreBundles(t *testing.T) {
	if !RoleAllows(WorkspaceRoleViewer, CapabilityViewWorkspace) || RoleAllows(WorkspaceRoleViewer, CapabilityClaimWork) || RoleAllows(WorkspaceRoleViewer, CapabilityProposeDocuments) {
		t.Fatal("viewer must hold only the workspace view capability")
	}
	if !RoleAllows(WorkspaceRoleExecutor, CapabilityClaimWork) || !RoleAllows(WorkspaceRoleExecutor, CapabilityRequestChanges) || RoleAllows(WorkspaceRoleExecutor, CapabilityProposeDocuments) {
		t.Fatal("executor capability bundle is incorrect")
	}
	if !RoleAllows(WorkspaceRoleContributor, CapabilityProposeDocuments) || RoleAllows(WorkspaceRoleContributor, CapabilityOperateGates) {
		t.Fatal("contributor capability bundle is incorrect")
	}
	if !RoleAllows(WorkspaceRoleMaintainer, CapabilityOperateGates) || !RoleAllows(WorkspaceRoleMaintainer, CapabilitySetAssignee) || RoleAllows(WorkspaceRoleMaintainer, CapabilityConfirmDocuments) || RoleAllows(WorkspaceRoleMaintainer, CapabilityManageMembership) {
		t.Fatal("maintainer capability bundle is incorrect")
	}
	if !RoleAllows(WorkspaceRoleOperator, CapabilityClaimWork) || !RoleAllows(WorkspaceRoleOperator, CapabilityManageMembership) {
		t.Fatal("operator must subsume contributor and operator capabilities")
	}
	// A role policy change is a bundle edit: the decision function and callers
	// continue naming the same capability.
	changedBundles := map[WorkspaceRole]map[Capability]bool{
		WorkspaceRoleContributor: {CapabilityManageWorkspace: true},
	}
	if !roleAllows(changedBundles, WorkspaceRoleContributor, CapabilityManageWorkspace) {
		t.Fatal("bundle edit did not alter the centralized decision")
	}
}

func TestRoleCapabilityOrdering(t *testing.T) {
	roles := []WorkspaceRole{WorkspaceRoleViewer, WorkspaceRoleExecutor, WorkspaceRoleContributor, WorkspaceRoleMaintainer, WorkspaceRoleOperator}
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
		if lowerIndex+1 < len(roles) {
			higher := roles[lowerIndex+1]
			strict := false
			for capability, allowed := range roleCapabilities[higher] {
				if allowed && !RoleAllows(lower, capability) {
					strict = true
					break
				}
			}
			if !strict {
				t.Fatalf("role %q does not strictly contain %q", higher, lower)
			}
		}
	}
}
