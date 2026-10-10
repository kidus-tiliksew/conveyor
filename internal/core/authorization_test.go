package core

import (
	"slices"
	"testing"
)

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
	// Traverse the served enumeration so a role added to roleCapabilities is
	// covered without editing this test, and pin that the five governed roles
	// are all enumerated (req-accounts-and-membership REQ-2).
	roles := WorkspaceRoles()
	if len(roles) != len(roleCapabilities) {
		t.Fatalf("enumerated roles=%v, table holds %d", roles, len(roleCapabilities))
	}
	for _, governed := range []WorkspaceRole{WorkspaceRoleViewer, WorkspaceRoleExecutor, WorkspaceRoleContributor, WorkspaceRoleMaintainer, WorkspaceRoleOperator} {
		if !slices.Contains(roles, governed) {
			t.Fatalf("enumerated roles=%v lack %q", roles, governed)
		}
	}
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

// TestAgentCapabilityCeilingMatchesContributorPlusCreateTasks pins the agent
// credential ceiling that the MCP surface derives from these bundles: every
// contributor capability plus create_tasks, the one maintainer capability an
// execution credential may exercise. A bundle change that moves the ceiling
// fails here, so the MCP agent surface never widens unnoticed
// (req-accounts-and-membership AC-3.4, AC-5.1; component-mcp-protocol).
func TestAgentCapabilityCeilingMatchesContributorPlusCreateTasks(t *testing.T) {
	ceiling := func(capability Capability) bool {
		return capability == CapabilityCreateTasks || RoleAllows(WorkspaceRoleContributor, capability)
	}
	known := map[Capability]bool{}
	for _, bundle := range roleCapabilities {
		for capability := range bundle {
			known[capability] = true
		}
	}
	got := map[Capability]bool{}
	for capability := range known {
		if ceiling(capability) {
			got[capability] = true
		}
	}
	want := map[Capability]bool{
		CapabilityViewWorkspace:    true,
		CapabilityClaimWork:        true,
		CapabilityRequestChanges:   true,
		CapabilityProposeDocuments: true,
		CapabilityCreateTasks:      true,
	}
	if len(got) != len(want) {
		t.Fatalf("agent ceiling=%v, want %v", got, want)
	}
	for capability := range want {
		if !got[capability] {
			t.Fatalf("agent ceiling=%v lacks %q", got, capability)
		}
	}
	for _, capability := range []Capability{CapabilityConfirmDocuments, CapabilityManageMembership, CapabilitySetAssignee, CapabilityOperateGates, CapabilityRecoverWork, CapabilityManageWorkspace, CapabilityManageReferenceDocuments} {
		if !known[capability] || ceiling(capability) {
			t.Fatalf("capability %q known=%v inside agent ceiling=%v", capability, known[capability], ceiling(capability))
		}
	}
	// An unknown capability is held by no bundle and so fails closed.
	if ceiling(Capability("unknown_capability")) {
		t.Fatal("unknown capability passed the agent ceiling")
	}
}

// TestRoleCapabilitiesEnumeratesEnabledBundle proves the served capability
// list is the table's enabled bundle, decided by RoleAllows, and that callers
// receive copies (req-accounts-and-membership AC-5.1).
func TestRoleCapabilitiesEnumeratesEnabledBundle(t *testing.T) {
	known := map[Capability]bool{}
	for _, bundle := range roleCapabilities {
		for capability := range bundle {
			known[capability] = true
		}
	}
	for role := range roleCapabilities {
		got := RoleCapabilities(role)
		if !slices.IsSorted(got) {
			t.Fatalf("role %q capabilities %v are not sorted", role, got)
		}
		if len(slices.Compact(slices.Clone(got))) != len(got) {
			t.Fatalf("role %q capabilities %v contain duplicates", role, got)
		}
		for capability := range known {
			if slices.Contains(got, capability) != RoleAllows(role, capability) {
				t.Fatalf("role %q capability %q listed=%v allowed=%v", role, capability, slices.Contains(got, capability), RoleAllows(role, capability))
			}
		}
	}
	if got := RoleCapabilities(WorkspaceRole("unknown")); len(got) != 0 {
		t.Fatalf("unknown role capabilities=%v", got)
	}
	if got := RoleCapabilities(WorkspaceRoleViewer); !slices.Equal(got, []Capability{CapabilityViewWorkspace}) {
		t.Fatalf("viewer capabilities=%v", got)
	}

	// A disabled entry is not part of the bundle.
	bundles := map[WorkspaceRole]map[Capability]bool{
		"reader": {CapabilityViewWorkspace: true, CapabilityClaimWork: false},
	}
	if got := roleCapabilityList(bundles, "reader"); !slices.Equal(got, []Capability{CapabilityViewWorkspace}) {
		t.Fatalf("disabled entry listed: %v", got)
	}

	// Mutating a returned slice cannot change policy or a later result.
	operator := RoleCapabilities(WorkspaceRoleOperator)
	want := slices.Clone(operator)
	for index := range operator {
		operator[index] = "tampered"
	}
	if got := RoleCapabilities(WorkspaceRoleOperator); !slices.Equal(got, want) {
		t.Fatalf("operator capabilities after mutation=%v, want %v", got, want)
	}
	if !RoleAllows(WorkspaceRoleOperator, CapabilityManageWorkspace) || RoleAllows(WorkspaceRoleOperator, "tampered") {
		t.Fatal("mutating a returned list changed policy")
	}
}

// TestWorkspaceRolesDerivesStrictChain proves the served role chain comes from
// the table's keys in ascending bundle order, and that a future role and
// capability join the chain with no enumeration-list edit.
func TestWorkspaceRolesDerivesStrictChain(t *testing.T) {
	want := []WorkspaceRole{WorkspaceRoleViewer, WorkspaceRoleExecutor, WorkspaceRoleContributor, WorkspaceRoleMaintainer, WorkspaceRoleOperator}
	got := WorkspaceRoles()
	if !slices.Equal(got, want) {
		t.Fatalf("roles=%v, want %v", got, want)
	}
	got[0] = "tampered"
	if again := WorkspaceRoles(); !slices.Equal(again, want) {
		t.Fatalf("roles after mutation=%v, want %v", again, want)
	}
	for range 10 {
		if repeated := WorkspaceRoles(); !slices.Equal(repeated, want) {
			t.Fatalf("repeated roles=%v, want %v", repeated, want)
		}
	}

	// A synthetic strict chain with an added role above operator and an added
	// capability. Only the bundle table changes.
	synthetic := map[WorkspaceRole]map[Capability]bool{}
	for role, bundle := range roleCapabilities {
		synthetic[role] = map[Capability]bool{}
		for capability, allowed := range bundle {
			synthetic[role][capability] = allowed
		}
	}
	synthetic["owner"] = map[Capability]bool{"transfer_workspace": true}
	for capability, allowed := range roleCapabilities[WorkspaceRoleOperator] {
		synthetic["owner"][capability] = allowed
	}
	chain := workspaceRoleChain(synthetic)
	if !slices.Equal(chain, append(slices.Clone(want), "owner")) {
		t.Fatalf("synthetic chain=%v", chain)
	}
	if list := roleCapabilityList(synthetic, "owner"); !slices.Contains(list, "transfer_workspace") || len(list) != len(RoleCapabilities(WorkspaceRoleOperator))+1 {
		t.Fatalf("synthetic owner capabilities=%v", list)
	}
	// Equal-size bundles fall back to lexical order, so the result never
	// depends on map iteration.
	tied := map[WorkspaceRole]map[Capability]bool{
		"zeta":  {CapabilityViewWorkspace: true},
		"alpha": {CapabilityClaimWork: true},
	}
	for range 10 {
		if got := workspaceRoleChain(tied); !slices.Equal(got, []WorkspaceRole{"alpha", "zeta"}) {
			t.Fatalf("tied chain=%v", got)
		}
	}
}
