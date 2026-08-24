package main

import (
	"encoding/json"
	"os"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func main() {
	roles := []core.WorkspaceRole{
		core.WorkspaceRoleViewer,
		core.WorkspaceRoleExecutor,
		core.WorkspaceRoleContributor,
		core.WorkspaceRoleMaintainer,
		core.WorkspaceRoleOperator,
	}
	capabilities := []core.Capability{
		core.CapabilityViewWorkspace,
		core.CapabilityClaimWork,
		core.CapabilityRequestChanges,
		core.CapabilityProposeDocuments,
		core.CapabilityConfirmDocuments,
		core.CapabilityManageMembership,
		core.CapabilitySetAssignee,
		core.CapabilityOperateGates,
		core.CapabilityRecoverWork,
		core.CapabilityManageWorkspace,
	}
	matrix := make(map[core.WorkspaceRole][]core.Capability, len(roles))
	for _, role := range roles {
		for _, capability := range capabilities {
			if core.RoleAllows(role, capability) {
				matrix[role] = append(matrix[role], capability)
			}
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(matrix); err != nil {
		panic(err)
	}
}
