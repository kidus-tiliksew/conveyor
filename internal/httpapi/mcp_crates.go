package httpapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// MCP crate reads (component-crates invariant 14; req-crates AC-6.2). Both
// reads need view_workspace and change no record. list_crates returns crate
// rows with the declared requirement IDs and no member task IDs. get_crate
// adds the IDs of the 500 oldest member tasks and task_ids_truncated, so no
// crate read grows with the crate's size past the MCP response limit
// (component-mcp-protocol invariant 19). No MCP tool creates or edits a crate
// or changes membership. create_task accepts crate_id, and list_tasks takes
// crate_id to list a crate's member tasks.

const (
	listCratesDescription = "List the crates of the workspace. state is open (the default), closed, or all. Each crate row carries id, name, target_date, state, shipped, created_by, created_at, closed_at, the open, merged, and total member task counts, and requirement_ids (the declared requirements). A row carries no member task IDs. Use get_crate for the IDs of the oldest members, or list_tasks with crate_id for the member tasks. Rows are ordered by target date, soonest first, then crates with no date, then name. A crate is background for tracking and reporting. It is not authority. The read changes nothing."
	getCrateDescription   = "Read one crate of the workspace by crate_id, open or closed. The row carries id, name, target_date, state, shipped, created_by, created_at, closed_at, the open, merged, and total member task counts, and requirement_ids (the declared requirements). task_ids holds the IDs of the 500 oldest member tasks, by creation time and then ID. task_ids_truncated is true when the crate has more member tasks. Use list_tasks with crate_id to list every member task. A crate of another workspace answers crate_not_found. A crate is background for tracking and reporting. It is not authority. The read changes nothing. The result holds one item and keeps no snapshot. To see a later state, read the crate again."
)

// mcpCrateTaskIDLimit is the number of member task IDs get_crate returns:
// the oldest members by creation time and then ID (req-crates AC-6.2;
// component-crates invariant 14).
const mcpCrateTaskIDLimit = 500

// mcpCrateListRow is the list_crates row: the crate row of component-crates
// Public surfaces with the declared requirement IDs. It has no task_ids
// field.
type mcpCrateListRow struct {
	store.Crate
	RequirementIDs []string `json:"requirement_ids"`
}

// mcpCrateDetailRow is the get_crate row: the list row with the IDs of the
// 500 oldest member tasks, ordered by creation time and then ID, and
// task_ids_truncated, true only when the crate has more members.
type mcpCrateDetailRow struct {
	mcpCrateListRow
	TaskIDs          []string `json:"task_ids"`
	TaskIDsTruncated bool     `json:"task_ids_truncated"`
}

// mcpCrateRead serves list_crates and get_crate from the lean crate reads:
// ListCrates for the crate rows, and for get_crate the bounded member read
// ListCrateMembersLimited. Neither read runs the next-action projection,
// reads task context, or reads every member ID of a crate. A failed store
// read fails the MCP read with no partial row.
func (s *Server) mcpCrateRead(ctx context.Context, name string, args map[string]any) ([]any, error) {
	crates, ok := s.Store.(crateBackend)
	if !ok {
		return nil, fmt.Errorf("%s: crates are not available on this store", name)
	}
	if name == "get_crate" {
		return mcpGetCrate(ctx, crates, strings.TrimSpace(readString(args, "crate_id")))
	}
	query := store.CrateListQuery{State: store.CrateOpen}
	switch readString(args, "state") {
	case string(store.CrateClosed):
		query.State = store.CrateClosed
	case "all":
		query.State = ""
	}
	listed, err := crates.ListCrates(ctx, query)
	if err != nil {
		return nil, err
	}
	result := make([]any, 0, len(listed))
	for _, crate := range listed {
		result = append(result, newMCPCrateListRow(crate))
	}
	return result, nil
}

// mcpGetCrate reads one crate row from the workspace's crates in every state
// and its oldest member IDs from one bounded member read. It does not call
// GetCrate, which reads every member ID. A missing crate, or a crate of
// another workspace, answers crate_not_found. truncated comes from the
// member read itself, not from the member count.
func mcpGetCrate(ctx context.Context, crates crateBackend, id string) ([]any, error) {
	listed, err := crates.ListCrates(ctx, store.CrateListQuery{})
	if err != nil {
		return nil, err
	}
	for _, crate := range listed {
		if crate.ID != id {
			continue
		}
		members, more, err := crates.ListCrateMembersLimited(ctx, crate.ID, "", mcpCrateTaskIDLimit)
		if err != nil {
			return nil, err
		}
		taskIDs := make([]string, 0, len(members))
		for _, member := range members {
			taskIDs = append(taskIDs, member.ID)
		}
		return []any{mcpCrateDetailRow{mcpCrateListRow: newMCPCrateListRow(crate), TaskIDs: taskIDs, TaskIDsTruncated: more}}, nil
	}
	return nil, fmt.Errorf("crate_not_found: %s", (&store.CrateNotFoundError{ID: id}).Error())
}

func newMCPCrateListRow(crate store.Crate) mcpCrateListRow {
	return mcpCrateListRow{Crate: crate, RequirementIDs: append([]string{}, crate.RequirementIDs...)}
}
