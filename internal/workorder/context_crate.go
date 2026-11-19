package workorder

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// MaxCrateSiblingTasks bounds the other member tasks one order context lists
// (component-crates invariant 20).
const MaxCrateSiblingTasks = 50

// CrateContext is the crate field of a member task's order context. It holds
// only the crate's ID and name, its other member tasks, and the truncation
// flag. It carries no target date, no declared requirement, and no crate
// state, and every role prompt names it as untrusted background that is not
// authority (component-crates invariant 20; req-crates AC-4.3; DEC-141(6)).
type CrateContext struct {
	ID                    string             `json:"id"`
	Name                  string             `json:"name"`
	SiblingTasks          []CrateSiblingTask `json:"sibling_tasks"`
	SiblingTasksTruncated bool               `json:"sibling_tasks_truncated"`
}

// CrateSiblingTask is one other member task of the order's crate.
type CrateSiblingTask struct {
	ID    string         `json:"id"`
	Title string         `json:"title"`
	State core.TaskState `json:"state"`
}

// crateContextForTask reads the crate field for one assembly from the records
// as they stand: the crate's name and its member tasks, read on every
// assembly and kept on no order. A task in no crate reads nothing and gets no
// field (req-crates AC-4.5). The two reads are the lean CrateStore reads, not
// the crate page read, so no next action or task context is read here, and
// the member read is bounded. A failed read fails the assembly: an order
// never shows a member task as a task in no crate (component-crates
// invariants 1 and 20).
func crateContextForTask(ctx context.Context, st store.Store, task core.Task) (*CrateContext, error) {
	crateID := task.TaskCrateID()
	if crateID == "" {
		return nil, nil
	}
	crates, ok := st.(store.CrateStore)
	if !ok {
		return nil, fmt.Errorf("read crate %s of task %s: the store keeps no crates", crateID, task.ID)
	}
	names, err := crates.CrateNames(ctx, []string{crateID})
	if err != nil {
		return nil, fmt.Errorf("read crate %s of task %s: %w", crateID, task.ID, err)
	}
	name, found := names[crateID]
	if !found {
		return nil, fmt.Errorf("read crate %s of task %s: %w", crateID, task.ID, &store.CrateNotFoundError{ID: crateID})
	}
	// The store leaves the task itself out before the limit, so the cap counts
	// only other members, and it reads at most MaxCrateSiblingTasks plus one
	// rows whatever the crate's size (DEC-105(4)).
	members, more, err := crates.ListCrateMembersLimited(ctx, crateID, task.ID, MaxCrateSiblingTasks)
	if err != nil {
		return nil, fmt.Errorf("read members of crate %s for task %s: %w", crateID, task.ID, err)
	}
	result := &CrateContext{ID: crateID, Name: name, SiblingTasks: make([]CrateSiblingTask, 0, len(members)), SiblingTasksTruncated: more}
	for _, member := range members {
		result.SiblingTasks = append(result.SiblingTasks, CrateSiblingTask{ID: member.ID, Title: member.Title, State: member.State})
	}
	return result, nil
}
