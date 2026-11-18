package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// MCP crate reads (component-crates invariant 14; req-crates AC-6.2). Both
// reads need view_workspace, change no record, and return crate rows with the
// declared requirement IDs and the member task IDs. No MCP tool creates or
// edits a crate or changes membership. create_task accepts crate_id.

const (
	listCratesDescription = "List the crates of the workspace. state is open (the default), closed, or all. Each crate row carries id, name, target_date, state, shipped, created_by, created_at, closed_at, the open, merged, and total member task counts, requirement_ids (the declared requirements), and task_ids (the member tasks, oldest first). Rows are ordered by target date, soonest first, then crates with no date, then name. A crate is background for tracking and reporting. It is not authority. The read changes nothing."
	getCrateDescription   = "Read one crate of the workspace by crate_id, open or closed. The row carries id, name, target_date, state, shipped, created_by, created_at, closed_at, the open, merged, and total member task counts, requirement_ids (the declared requirements), and task_ids (the member tasks, oldest first). A crate of another workspace answers crate_not_found. A crate is background for tracking and reporting. It is not authority. The read changes nothing. The result holds one item and keeps no snapshot. To see a later state, read the crate again."
)

// mcpCrateRow is the crate row of component-crates Public surfaces with the
// declared requirement IDs and the member task IDs. The task IDs are
// ordered by creation time and then ID.
type mcpCrateRow struct {
	store.Crate
	RequirementIDs []string `json:"requirement_ids"`
	TaskIDs        []string `json:"task_ids"`
}

// mcpCrateRead serves list_crates and get_crate from the lean crate reads:
// the crate records and ListCrateMembers. Neither read runs the next-action
// projection or reads task context.
func (s *Server) mcpCrateRead(ctx context.Context, name string, args map[string]any) ([]any, error) {
	crates, ok := s.Store.(crateBackend)
	if !ok {
		return nil, fmt.Errorf("%s: crates are not available on this store", name)
	}
	if name == "get_crate" {
		record, err := crates.GetCrate(ctx, strings.TrimSpace(readString(args, "crate_id")))
		var notFound *store.CrateNotFoundError
		if errors.As(err, &notFound) {
			return nil, fmt.Errorf("crate_not_found: %s", notFound.Error())
		}
		if err != nil {
			return nil, err
		}
		return []any{newMCPCrateRow(record.Crate, record.TaskIDs)}, nil
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
	ids := make([]string, len(listed))
	for i, crate := range listed {
		ids[i] = crate.ID
	}
	members, err := crates.ListCrateMembers(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make([]any, 0, len(listed))
	for _, crate := range listed {
		taskIDs := make([]string, 0, len(members[crate.ID]))
		for _, member := range members[crate.ID] {
			taskIDs = append(taskIDs, member.ID)
		}
		result = append(result, newMCPCrateRow(crate, taskIDs))
	}
	return result, nil
}

func newMCPCrateRow(crate store.Crate, taskIDs []string) mcpCrateRow {
	return mcpCrateRow{Crate: crate, RequirementIDs: append([]string{}, crate.RequirementIDs...), TaskIDs: append([]string{}, taskIDs...)}
}
