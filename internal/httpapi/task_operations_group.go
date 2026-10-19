package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// taskOperationsGroup is one Board list group of GET /v1/task-operations: the
// tasks whose stated next action names one actor family
// (req-operator-next-action AC-6.5; component-http-api, Grouped
// task-operations read).
type taskOperationsGroup string

const (
	taskOperationsGroupOperator taskOperationsGroup = "operator"
	taskOperationsGroupAgent    taskOperationsGroup = "agent"
	taskOperationsGroupWaiting  taskOperationsGroup = "waiting"
	taskOperationsGroupTerminal taskOperationsGroup = "terminal"
)

// taskOperationsGroups is the Board's group order.
var taskOperationsGroups = []taskOperationsGroup{taskOperationsGroupOperator, taskOperationsGroupAgent, taskOperationsGroupWaiting, taskOperationsGroupTerminal}

// taskOperationsGroupHeaders names each group's count header.
var taskOperationsGroupHeaders = map[taskOperationsGroup]string{
	taskOperationsGroupOperator: "X-Conveyor-Group-Operator-Total",
	taskOperationsGroupAgent:    "X-Conveyor-Group-Agent-Total",
	taskOperationsGroupWaiting:  "X-Conveyor-Group-Waiting-Total",
	taskOperationsGroupTerminal: "X-Conveyor-Group-Terminal-Total",
}

var errTaskOperationsGroup = errors.New("group must be supplied once as operator, agent, waiting, or terminal")

// parseTaskOperationsGroup reads the optional group selector. Only
// GET /v1/task-operations calls it; an absent key leaves the read ungrouped.
func parseTaskOperationsGroup(values url.Values) (taskOperationsGroup, bool, error) {
	raw, present := values["group"]
	if !present {
		return "", false, nil
	}
	if len(raw) != 1 {
		return "", false, errTaskOperationsGroup
	}
	for _, group := range taskOperationsGroups {
		if raw[0] == string(group) {
			return group, true, nil
		}
	}
	return "", false, errTaskOperationsGroup
}

// taskOperationsGroupForActor maps a stated next-action actor to its group.
// Any other actor has no group.
func taskOperationsGroupForActor(actor string) (taskOperationsGroup, bool) {
	switch actor {
	case nextaction.ActorOperator:
		return taskOperationsGroupOperator, true
	case nextaction.ActorAgent:
		return taskOperationsGroupAgent, true
	case nextaction.ActorFactory, nextaction.ActorDependency:
		return taskOperationsGroupWaiting, true
	case nextaction.ActorNone:
		return taskOperationsGroupTerminal, true
	default:
		return "", false
	}
}

// taskOperationsPartition is the filtered set split by stated actor: every
// group's count and the requested group's members in read order.
type taskOperationsPartition struct {
	counts   map[taskOperationsGroup]int
	selected []core.Task
}

// partitionTaskOperations assigns each candidate to the group of its projected
// actor and counts every group before selecting one. A candidate without a
// projection, or with an actor no group names, fails the read: no task is
// grouped by default.
func partitionTaskOperations(candidates []core.Task, projected map[string]nextaction.NextAction, group taskOperationsGroup) (taskOperationsPartition, error) {
	partition := taskOperationsPartition{counts: map[taskOperationsGroup]int{}}
	for _, candidate := range candidates {
		action, ok := projected[candidate.ID]
		if !ok {
			return taskOperationsPartition{}, fmt.Errorf("task operations group: task %s has no projected next action", candidate.ID)
		}
		member, ok := taskOperationsGroupForActor(action.Actor)
		if !ok {
			return taskOperationsPartition{}, fmt.Errorf("task operations group: task %s has unexpected next-action actor %q", candidate.ID, action.Actor)
		}
		partition.counts[member]++
		if member == group {
			partition.selected = append(partition.selected, candidate)
		}
	}
	return partition, nil
}

// listGroupedTaskOperations serves GET /v1/task-operations with a group
// selector. It projects the actor over the whole filtered set before paging
// and reuses that one projection for membership, every count, and every row
// (req-operator-next-action AC-1.7, AC-6.5; component-http-api, Grouped
// task-operations read).
func (s *Server) listGroupedTaskOperations(w http.ResponseWriter, r *http.Request, query store.TaskOperationsQuery, paginated bool, group taskOperationsGroup) {
	ctx := r.Context()
	fail := func(err error) {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
	candidates, err := s.Store.ListTaskOperationsCandidates(ctx, query.TaskFilter)
	if err != nil {
		fail(err)
		return
	}
	partition := taskOperationsPartition{counts: map[taskOperationsGroup]int{}}
	items := []taskOperationsItem{}
	if len(candidates) > 0 {
		open, err := s.taskOperationsGroupOpenTasks(ctx, query.TaskFilter, candidates)
		if err != nil {
			fail(err)
			return
		}
		ids := make([]string, len(candidates))
		for i := range candidates {
			ids[i] = candidates[i].ID
		}
		markers, err := s.Store.ListActivityMarkersForTasks(ctx, ids)
		if err != nil {
			fail(err)
			return
		}
		opts, err := s.nextActionOptions(r)
		if err != nil {
			fail(err)
			return
		}
		opts.MarkersLoaded, opts.Markers = true, markers
		opts.OpenTasksLoaded, opts.OpenTasks = true, open
		opts.CompactOnly = true
		projected, err := store.ReadNextActions(ctx, s.Store, candidates, opts)
		if err != nil {
			fail(err)
			return
		}
		if partition, err = partitionTaskOperations(candidates, projected, group); err != nil {
			fail(err)
			return
		}
		start, end := min(query.Offset, len(partition.selected)), len(partition.selected)
		if paginated && start+query.Limit < end {
			end = start + query.Limit
		}
		if page := partition.selected[start:end]; len(page) > 0 {
			pageIDs := make([]string, len(page))
			for i := range page {
				pageIDs[i] = page[i].ID
			}
			details, err := s.readTaskOperationsDetails(ctx, pageIDs)
			if err != nil {
				fail(err)
				return
			}
			blockers, err := s.Store.ListDependencyBlockers(ctx, pageIDs)
			if err != nil {
				fail(err)
				return
			}
			rows := make([]core.Task, len(page))
			for i, candidate := range page {
				rows[i] = taskOperationsRowTask(candidate, details.Tasks[i])
			}
			markerByTask := make(map[string]store.ActivityMarker, len(markers))
			for _, marker := range markers {
				markerByTask[marker.TaskID] = marker
			}
			if items, err = s.taskOperationsItems(ctx, rows, details.Events, details.Plans, markerByTask, projected, blockers); err != nil {
				fail(err)
				return
			}
		}
	}
	for _, member := range taskOperationsGroups {
		w.Header().Set(taskOperationsGroupHeaders[member], strconv.Itoa(partition.counts[member]))
	}
	if paginated {
		w.Header().Set("X-Conveyor-Total", strconv.Itoa(len(partition.selected)))
		w.Header().Set("X-Conveyor-Limit", strconv.Itoa(query.Limit))
		w.Header().Set("X-Conveyor-Offset", strconv.Itoa(query.Offset))
	}
	writeJSON(w, http.StatusOK, items)
}

// taskOperationsGroupOpenTasks is the workspace's open set for the dependency
// view, including blockers and dependents the active filter hides. An
// inactive filter already selected every task, so its nonterminal candidates
// are the open set and no second read is issued.
func (s *Server) taskOperationsGroupOpenTasks(ctx context.Context, filter store.TaskFilter, candidates []core.Task) ([]core.Task, error) {
	if !filter.Active() {
		open := make([]core.Task, 0, len(candidates))
		for _, candidate := range candidates {
			if !core.TaskTerminal(candidate.State) {
				open = append(open, candidate)
			}
		}
		return open, nil
	}
	var states []core.TaskState
	for _, state := range core.TaskStates() {
		if !core.TaskTerminal(state) {
			states = append(states, state)
		}
	}
	return s.Store.ListTaskOperationsCandidates(ctx, store.TaskFilter{States: states})
}

// readTaskOperationsDetails hydrates the selected IDs in consecutive chunks of
// at most store.MaxTaskOperationsLimit and returns their rows in selected
// order. A selected ID that the detail read does not return fails the read.
// Chunk totals are never used: the partition owns every count.
func (s *Server) readTaskOperationsDetails(ctx context.Context, ids []string) (store.TaskOperationsPage, error) {
	merged := store.TaskOperationsPage{Tasks: make([]core.Task, 0, len(ids)), Events: map[string][]core.Event{}, Plans: map[string]core.SpecVersion{}}
	byID := make(map[string]core.Task, len(ids))
	for start := 0; start < len(ids); start += store.MaxTaskOperationsLimit {
		chunk := ids[start:min(start+store.MaxTaskOperationsLimit, len(ids))]
		page, err := s.Store.ListTaskOperationsForIDs(ctx, chunk)
		if err != nil {
			return store.TaskOperationsPage{}, err
		}
		for _, task := range page.Tasks {
			byID[task.ID] = task
		}
		for id, events := range page.Events {
			merged.Events[id] = events
		}
		for id, plan := range page.Plans {
			merged.Plans[id] = plan
		}
	}
	for _, id := range ids {
		task, ok := byID[id]
		if !ok {
			return store.TaskOperationsPage{}, fmt.Errorf("task operations group: task %s missing from the detail read", id)
		}
		merged.Tasks = append(merged.Tasks, task)
	}
	merged.Total = len(merged.Tasks)
	return merged, nil
}

// taskOperationsRowTask builds a row's task from the candidate the actor was
// projected from. Only the body, relations, pull-request close projection,
// and forge lifecycle come from the detail row, so a transition committed
// between the two reads cannot pair a row with another observation's actor.
func taskOperationsRowTask(candidate, detail core.Task) core.Task {
	row := candidate
	row.Body = detail.Body
	row.Dependencies = detail.Dependencies
	row.BlockingTaskIDs = detail.BlockingTaskIDs
	row.Children = detail.Children
	row.PullRequestClose = detail.PullRequestClose
	row.PullRequestCloseState = detail.PullRequestCloseState
	row.GitHub = detail.GitHub
	return row
}
