package store

import (
	"context"
	"slices"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TaskMapQuery selects the task nodes of the workspace task map
// (req-board-map AC-1.7, AC-3.1, AC-3.4; component-http-api). Repositories
// narrows the returned nodes only. IncludeFinished returns every matching
// task; otherwise the map returns the open tasks and the merged or closed
// tasks those open tasks depend on.
type TaskMapQuery struct {
	Repositories    []string
	IncludeFinished bool
}

// TaskMapRequirementRef names one requirement a task actively serves.
type TaskMapRequirementRef struct {
	ID    string
	Title string
}

// TaskMapTask is one returned map node. Task carries the map columns and the
// internal next-action projection inputs; the backend never reads the task
// body into it, and HTTP responses copy only the map fields out of it.
type TaskMapTask struct {
	Task         core.Task
	DependsOn    []string
	Requirements []TaskMapRequirementRef
}

// TaskMapRequirement is one workspace-wide served-requirement aggregate.
// Open counts nonterminal tasks, merged counts merged tasks, and total is
// their sum: closed tasks count in neither (req-board-map AC-1.3).
type TaskMapRequirement struct {
	ID              string
	Title           string
	OpenTaskCount   int
	MergedTaskCount int
	TotalTaskCount  int
}

// TaskMap is one bounded map read. Tasks are the selected nodes ordered by
// ID. OpenTasks are every open task in the workspace, whatever the
// repository selection, so the shared next-action dependency view can name
// dependents outside the selection without a second task read.
// Requirements are ordered by ID and never change with the query.
type TaskMap struct {
	Tasks        []TaskMapTask
	OpenTasks    []core.Task
	Requirements []TaskMapRequirement
}

// TaskMapAttachment is one active task/requirement serves attachment: the
// latest requirement add/remove event for the pair is an add, and the
// requirement has a confirmed version in the workspace. Backends read the
// workspace's attachments in one set-based query; FoldTaskMapAttachments
// derives both the per-task references and the aggregates from them.
type TaskMapAttachment struct {
	TaskID           string
	TaskState        core.TaskState
	RequirementID    string
	RequirementTitle string
}

// FoldTaskMapAttachments returns each task's served requirements, ordered by
// requirement ID, and the workspace aggregates ordered by requirement ID.
// Duplicate attachment rows for one pair count once.
func FoldTaskMapAttachments(attachments []TaskMapAttachment) (map[string][]TaskMapRequirementRef, []TaskMapRequirement) {
	type pair struct{ task, requirement string }
	seen := map[pair]bool{}
	byTask := map[string][]TaskMapRequirementRef{}
	byRequirement := map[string]*TaskMapRequirement{}
	for _, attachment := range attachments {
		key := pair{attachment.TaskID, attachment.RequirementID}
		if seen[key] {
			continue
		}
		seen[key] = true
		byTask[attachment.TaskID] = append(byTask[attachment.TaskID], TaskMapRequirementRef{ID: attachment.RequirementID, Title: attachment.RequirementTitle})
		aggregate := byRequirement[attachment.RequirementID]
		if aggregate == nil {
			aggregate = &TaskMapRequirement{ID: attachment.RequirementID, Title: attachment.RequirementTitle}
			byRequirement[attachment.RequirementID] = aggregate
		}
		switch {
		case attachment.TaskState == core.TaskMerged:
			aggregate.MergedTaskCount++
		case !core.TaskTerminal(attachment.TaskState):
			aggregate.OpenTaskCount++
		}
		aggregate.TotalTaskCount = aggregate.OpenTaskCount + aggregate.MergedTaskCount
	}
	for id := range byTask {
		refs := byTask[id]
		sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	}
	requirements := make([]TaskMapRequirement, 0, len(byRequirement))
	for _, aggregate := range byRequirement {
		requirements = append(requirements, *aggregate)
	}
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].ID < requirements[j].ID })
	return byTask, requirements
}

// TaskMapSelects reports whether a loaded task is a returned map node. A
// finished task enters the default scope only when an open task inside the
// repository selection depends on it.
func TaskMapSelects(query TaskMapQuery, task core.Task, dependedOnByOpen bool) bool {
	if len(query.Repositories) > 0 && !slices.Contains(query.Repositories, task.Repo) {
		return false
	}
	return query.IncludeFinished || !core.TaskTerminal(task.State) || dependedOnByOpen
}

// AssembleTaskMap partitions loaded rows into the map read. Loaded holds
// every open workspace task plus any finished candidate; edges maps each
// loaded task to its dependency IDs.
func AssembleTaskMap(query TaskMapQuery, loaded []core.Task, edges map[string][]string, attachments []TaskMapAttachment) TaskMap {
	served, requirements := FoldTaskMapAttachments(attachments)
	finishedDependency := map[string]bool{}
	result := TaskMap{Tasks: []TaskMapTask{}, OpenTasks: []core.Task{}, Requirements: requirements}
	for _, task := range loaded {
		if core.TaskTerminal(task.State) {
			continue
		}
		result.OpenTasks = append(result.OpenTasks, task)
		if len(query.Repositories) > 0 && !slices.Contains(query.Repositories, task.Repo) {
			continue
		}
		for _, dependency := range edges[task.ID] {
			finishedDependency[dependency] = true
		}
	}
	for _, task := range loaded {
		if !TaskMapSelects(query, task, finishedDependency[task.ID]) {
			continue
		}
		dependsOn := append([]string{}, edges[task.ID]...)
		sort.Strings(dependsOn)
		refs := append([]TaskMapRequirementRef{}, served[task.ID]...)
		result.Tasks = append(result.Tasks, TaskMapTask{Task: task, DependsOn: dependsOn, Requirements: refs})
	}
	sort.Slice(result.Tasks, func(i, j int) bool { return result.Tasks[i].Task.ID < result.Tasks[j].Task.ID })
	sort.Slice(result.OpenTasks, func(i, j int) bool { return result.OpenTasks[i].ID < result.OpenTasks[j].ID })
	return result
}

// ListTaskMap reads the volatile store's map under one lock. It follows the
// same fold as the SQL backends: the latest requirement add/remove event per
// task and requirement, then a confirmed requirement in the workspace.
func (m *memory) ListTaskMap(ctx context.Context, query TaskMapQuery) (TaskMap, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	var loaded []core.Task
	edges := map[string][]string{}
	var attachments []TaskMapAttachment
	for _, task := range m.tasks {
		if task.Workspace != workspace {
			continue
		}
		task.Body = ""
		task.Dependencies, task.BlockingTaskIDs, task.Children = nil, nil, nil
		loaded = append(loaded, task)
		for dependency := range m.dependencies[task.ID] {
			edges[task.ID] = append(edges[task.ID], dependency)
		}
		requirements, _ := ActiveTaskContextReferences(m.sortedEventsLocked(task.ID))
		for id := range requirements {
			requirement, ok := m.requirements[memoryScopedKey{workspace: workspace, id: id}]
			if !ok || requirement.CurrentVersion <= 0 {
				continue
			}
			attachments = append(attachments, TaskMapAttachment{TaskID: task.ID, TaskState: task.State, RequirementID: id, RequirementTitle: requirement.Title})
		}
	}
	return AssembleTaskMap(query, loaded, edges, attachments), nil
}

func (m *memory) sortedEventsLocked(taskID string) []core.Event {
	events := append([]core.Event(nil), m.events[taskID]...)
	sort.Slice(events, func(i, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].ID < events[j].ID
		}
		return events[i].At.Before(events[j].At)
	})
	return events
}

// NextActionEventKinds are the only event kinds the next-action assembler
// folds: the park command, recorded pull request, bounce count, and merge
// conflict episode (feature-operator-next-action NA-3). Batched reads load
// these kinds instead of each task's full history.
var NextActionEventKinds = []string{
	"task.state_changed", "pull_request.opened", "pipeline.bounce_limit",
	"merge.blocked", "merge.conflict_cleared", "merge.conflict_fix_dispatched",
	"merge.conflict_dispatch_failed", "merge.conflict_dispatch_exhausted", "merge.conflict_recovery_blocked",
}

// TaskHead is the identity, title, and state of one task, the fields a
// next-action blocker reference names.
type TaskHead struct {
	ID    string
	Title string
	State core.TaskState
}

// NextActionInputStore batches the per-task reads of the next-action
// assembler so a list projection issues a fixed number of queries
// (feature-operator-next-action, Input loading; component-persistence).
type NextActionInputStore interface {
	// ListNextActionEvents returns each requested task's events of
	// NextActionEventKinds in (at, id) order. Tasks without one are absent.
	ListNextActionEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error)
	// ListLatestSpecVersionNumbers returns each requested task's latest plan
	// version number. Tasks without a plan are absent.
	ListLatestSpecVersionNumbers(ctx context.Context, taskIDs []string) (map[string]int, error)
	// ListClaimBlockingProposalsForTasks applies the claim-blocking
	// predicate of ListClaimBlockingProposalsForTask to every requested task
	// in one read. Tasks without a withholding version are absent.
	ListClaimBlockingProposalsForTasks(ctx context.Context, taskIDs []string) (map[string][]ClaimBlockingProposal, error)
	// ListTaskHeads returns the requested workspace tasks' heads. A missing
	// task is absent; callers that require it report not found.
	ListTaskHeads(ctx context.Context, taskIDs []string) (map[string]TaskHead, error)
}

// NextActionEventKind reports whether the assembler folds an event kind.
func NextActionEventKind(kind string) bool {
	return slices.Contains(NextActionEventKinds, kind)
}

func (m *memory) ListNextActionEvents(ctx context.Context, taskIDs []string) (map[string][]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	out := map[string][]core.Event{}
	for _, id := range taskIDs {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		for _, event := range m.sortedEventsLocked(id) {
			if NextActionEventKind(event.Kind) {
				out[id] = append(out[id], event)
			}
		}
	}
	return out, nil
}

func (m *memory) ListLatestSpecVersionNumbers(ctx context.Context, taskIDs []string) (map[string]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	out := map[string]int{}
	for _, id := range taskIDs {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		if versions := m.specs[id]; len(versions) > 0 {
			out[id] = versions[len(versions)-1].Version
		}
	}
	return out, nil
}

func (m *memory) ListClaimBlockingProposalsForTasks(ctx context.Context, taskIDs []string) (map[string][]ClaimBlockingProposal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := map[string][]ClaimBlockingProposal{}
	for _, id := range taskIDs {
		if items := m.claimBlockingProposalsLocked(workspace, id); len(items) > 0 {
			out[id] = items
		}
	}
	return out, nil
}

func (m *memory) ListTaskHeads(ctx context.Context, taskIDs []string) (map[string]TaskHead, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	out := map[string]TaskHead{}
	for _, id := range taskIDs {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		out[id] = TaskHead{ID: task.ID, Title: task.Title, State: task.State}
	}
	return out, nil
}

// GroupClaimBlockingProposals orders each task's rows with
// SortClaimBlockingProposals, the order every backend returns.
func GroupClaimBlockingProposals(rows map[string][]ClaimBlockingProposal) map[string][]ClaimBlockingProposal {
	for id := range rows {
		SortClaimBlockingProposals(rows[id])
	}
	return rows
}
