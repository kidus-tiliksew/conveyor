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
	if !RoleAllows(WorkspaceRoleMaintainer, CapabilityOperateGates) || !RoleAllows(WorkspaceRoleMaintainer, CapabilityCreateTasks) || !RoleAllows(WorkspaceRoleMaintainer, CapabilitySetAssignee) || RoleAllows(WorkspaceRoleMaintainer, CapabilityConfirmDocuments) || RoleAllows(WorkspaceRoleMaintainer, CapabilityManageMembership) || RoleAllows(WorkspaceRoleMaintainer, CapabilityManageWorkspace) {
		t.Fatal("maintainer capability bundle is incorrect")
	}
	// Reference documents are informative: maintainers manage them without
	// gaining normative confirmation (req-accounts-and-membership AC-2.6, AC-2.7).
	for role, allowed := range map[WorkspaceRole]bool{
		WorkspaceRoleViewer:      false,
		WorkspaceRoleExecutor:    false,
		WorkspaceRoleContributor: false,
		WorkspaceRoleMaintainer:  true,
		WorkspaceRoleOperator:    true,
	} {
		if RoleAllows(role, CapabilityManageReferenceDocuments) != allowed {
			t.Fatalf("role %q manage_reference_documents = %v, want %v", role, !allowed, allowed)
		}
	}
	if !RoleAllows(WorkspaceRoleOperator, CapabilityClaimWork) || !RoleAllows(WorkspaceRoleOperator, CapabilityConfirmDocuments) || !RoleAllows(WorkspaceRoleOperator, CapabilityManageMembership) || !RoleAllows(WorkspaceRoleOperator, CapabilityManageWorkspace) {
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

// TestRoleCreateTasksCapabilityMatrix pins task intake to exactly the roles
// that filed tasks under operate_gates, so human intake is unchanged while
// create_tasks stays a separately named capability an execution credential may
// exercise (req-accounts-and-membership AC-2.5, AC-3.4; DEC-60).
func TestRoleCreateTasksCapabilityMatrix(t *testing.T) {
	want := map[WorkspaceRole]bool{
		WorkspaceRoleViewer:      false,
		WorkspaceRoleExecutor:    false,
		WorkspaceRoleContributor: false,
		WorkspaceRoleMaintainer:  true,
		WorkspaceRoleOperator:    true,
	}
	for role, allowed := range want {
		if got := RoleAllows(role, CapabilityCreateTasks); got != allowed {
			t.Fatalf("role %q create_tasks = %v, want %v", role, got, allowed)
		}
		if RoleAllows(role, CapabilityCreateTasks) != RoleAllows(role, CapabilityOperateGates) {
			t.Fatalf("role %q create_tasks diverges from the roles that file tasks today", role)
		}
	}
	if RoleAllows(WorkspaceRole("unknown"), CapabilityCreateTasks) {
		t.Fatal("unknown role holds create_tasks")
	}
	if CapabilityCreateTasks != "create_tasks" {
		t.Fatalf("create_tasks wire name = %q", CapabilityCreateTasks)
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
