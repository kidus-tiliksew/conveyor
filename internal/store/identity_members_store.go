package store

import (
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// SetMemoryWorkspaceMember configures the volatile backend's membership
// projection. Tests and embedded callers use it to exercise the same active
// membership invariant as PostgreSQL without weakening task assignment.
func SetMemoryWorkspaceMember(st Store, workspaceID, userID string, active bool) error {
	memoryStore, ok := st.(*memory)
	if !ok {
		return fmt.Errorf("workspace membership setup requires the memory store")
	}
	workspaceID, userID = strings.TrimSpace(workspaceID), strings.TrimSpace(userID)
	if workspaceID == "" || userID == "" {
		return fmt.Errorf("workspace and user ids are required")
	}
	memoryStore.mu.Lock()
	defer memoryStore.mu.Unlock()
	memoryStore.workspaceMembers[memoryScopedKey{workspace: workspaceID, id: userID}] = active
	if active {
		memoryStore.workspaceMemberRoles[memoryScopedKey{workspace: workspaceID, id: userID}] = core.WorkspaceRoleContributor
	}
	return nil
}

// SetMemoryWorkspaceMemberRole configures an active volatile membership with
// its fixed role so assignment conformance can exercise capability eligibility.
func SetMemoryWorkspaceMemberRole(st Store, workspaceID, userID string, role core.WorkspaceRole) error {
	if err := SetMemoryWorkspaceMember(st, workspaceID, userID, true); err != nil {
		return err
	}
	memoryStore := st.(*memory)
	memoryStore.mu.Lock()
	defer memoryStore.mu.Unlock()
	memoryStore.workspaceMemberRoles[memoryScopedKey{workspace: strings.TrimSpace(workspaceID), id: strings.TrimSpace(userID)}] = role
	return nil
}
