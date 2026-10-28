package store

import (
	"context"
	"errors"
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
	// ListNextActionHistory returns each requested task's bounded
	// NextActionHistory, built with FoldNextActionHistory from only the rows
	// that fold consumes. Tasks without one are absent.
	ListNextActionHistory(ctx context.Context, taskIDs []string) (map[string]NextActionHistory, error)
	// ListOpenDependents returns, in ID order, the heads of the workspace's
	// tasks outside merged and closed that depend on any requested task.
	ListOpenDependents(ctx context.Context, taskIDs []string) ([]TaskHead, error)
	// ListTaskContextDesignPins returns each requested task's System Design
	// pins, folded by ActiveTaskContextReferences from its context
	// attachment events alone. Tasks without a pin are absent.
	ListTaskContextDesignPins(ctx context.Context, taskIDs []string) (map[string]map[string]int, error)
	// ListDocumentHeads returns each requested requirement or System Design
	// head; a requirement wins over a System Design with the same ID and a
	// missing ID is absent.
	ListDocumentHeads(ctx context.Context, ids []string) (map[string]DocumentHead, error)
}

// MergeConflictEventKinds are the kinds the merge-conflict fold reads. Every
// one of them is kept in NextActionHistory.MergeEvents, because the fold
// needs each episode boundary and each dispatched fix order.
var MergeConflictEventKinds = []string{
	"merge.blocked", "merge.conflict_cleared", "merge.conflict_fix_dispatched",
	"merge.conflict_dispatch_failed", "merge.conflict_dispatch_exhausted", "merge.conflict_recovery_blocked",
}

// NextActionHistory is the bounded summary of the history the next-action
// assembler folds: the park command, the latest recorded pull request, the
// bounce window, and the merge-conflict events in (at, id) order
// (component-persistence; component-work-orders, Next action).
type NextActionHistory struct {
	ParkCommand string
	PullRequest int
	BounceCount int
	MergeEvents []core.Event
}

// FoldNextActionHistory is the one fold every backend applies to build a
// NextActionHistory from a task's events in (at, id) order. It applies the
// assembler's park-command, pull-request, and bounce-count folds unchanged,
// so a durable backend that passes only the latest row each of those folds
// can consume yields the summary of the task's full history.
func FoldNextActionHistory(events []core.Event) NextActionHistory {
	history := NextActionHistory{
		ParkCommand: parkCommand(events),
		PullRequest: recordedPullRequest(events),
		BounceCount: recordedBounceCount(events),
	}
	for _, event := range events {
		if slices.Contains(MergeConflictEventKinds, event.Kind) {
			history.MergeEvents = append(history.MergeEvents, event)
		}
	}
	return history
}

// FoldNextActionHistories groups rows in (at, id) order by task and folds
// each task's rows. A task without a folded field or merge event is absent.
func FoldNextActionHistories(rows []core.Event) map[string]NextActionHistory {
	byTask := map[string][]core.Event{}
	var order []string
	for _, row := range rows {
		if _, ok := byTask[row.TaskID]; !ok {
			order = append(order, row.TaskID)
		}
		byTask[row.TaskID] = append(byTask[row.TaskID], row)
	}
	out := map[string]NextActionHistory{}
	for _, id := range order {
		events := byTask[id]
		sort.SliceStable(events, func(i, j int) bool {
			if events[i].At.Equal(events[j].At) {
				return events[i].ID < events[j].ID
			}
			return events[i].At.Before(events[j].At)
		})
		if history := FoldNextActionHistory(events); !history.empty() {
			out[id] = history
		}
	}
	return out
}

func (h NextActionHistory) empty() bool {
	return h.ParkCommand == "" && h.PullRequest == 0 && h.BounceCount == 0 && len(h.MergeEvents) == 0
}

// DocumentHead is one cited document's tier, current confirmed version
// (zero when none), and archived flag.
type DocumentHead struct {
	ID             string
	Tier           string
	CurrentVersion int
	Archived       bool
}

// Document head tiers.
const (
	DocumentHeadRequirement  = "requirement"
	DocumentHeadSystemDesign = "system_design"
)

// TaskContextEventKinds are the context attachment kinds
// ActiveTaskContextReferences folds.
var TaskContextEventKinds = []string{TaskContextRequirementAdded, TaskContextRequirementRemoved, TaskContextDesignAdded, TaskContextDesignRemoved}

// TaskContextDesignPins folds rows in (at, id) order into each task's design
// pins. A task without one is absent.
func TaskContextDesignPins(rows []core.Event) map[string]map[string]int {
	byTask := map[string][]core.Event{}
	for _, row := range rows {
		byTask[row.TaskID] = append(byTask[row.TaskID], row)
	}
	out := map[string]map[string]int{}
	for id, events := range byTask {
		if _, pins := ActiveTaskContextReferences(events); len(pins) > 0 {
			out[id] = pins
		}
	}
	return out
}

// VerificationReviewRef names one (task, order) pair of a batched
// verification review read.
type VerificationReviewRef struct {
	TaskID  string
	OrderID string
}

// VerificationReviewBatchReader reads ReadVerificationReview for several
// pairs at once, keyed by order ID, with the same per-pair result and errors.
type VerificationReviewBatchReader interface {
	ReadVerificationReviews(ctx context.Context, refs []VerificationReviewRef) (map[string]VerificationReviewState, error)
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

func (m *memory) ListNextActionHistory(ctx context.Context, taskIDs []string) (map[string]NextActionHistory, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	var rows []core.Event
	for _, id := range taskIDs {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		for _, event := range m.sortedEventsLocked(id) {
			if NextActionEventKind(event.Kind) {
				rows = append(rows, event)
			}
		}
	}
	return FoldNextActionHistories(rows), nil
}

func (m *memory) ListOpenDependents(ctx context.Context, taskIDs []string) ([]TaskHead, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	out := []TaskHead{}
	for dependentID, dependencies := range m.dependencies {
		task, ok := m.tasks[dependentID]
		if !ok || core.TaskTerminal(task.State) || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		for _, id := range taskIDs {
			if _, edge := dependencies[id]; edge {
				out = append(out, TaskHead{ID: task.ID, Title: task.Title, State: task.State})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memory) ListTaskContextDesignPins(ctx context.Context, taskIDs []string) (map[string]map[string]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	var rows []core.Event
	for _, id := range taskIDs {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		for _, event := range m.sortedEventsLocked(id) {
			if slices.Contains(TaskContextEventKinds, event.Kind) {
				rows = append(rows, event)
			}
		}
	}
	return TaskContextDesignPins(rows), nil
}

func (m *memory) ListDocumentHeads(ctx context.Context, ids []string) (map[string]DocumentHead, error) {
	out := map[string]DocumentHead{}
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		requirement, err := m.GetRequirement(ctx, id)
		if err == nil {
			out[id] = DocumentHead{ID: id, Tier: DocumentHeadRequirement, CurrentVersion: requirement.CurrentVersion, Archived: requirement.Archived}
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		design, err := m.GetSystemDesign(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = DocumentHead{ID: id, Tier: DocumentHeadSystemDesign, CurrentVersion: design.CurrentVersion, Archived: design.Archived}
	}
	return out, nil
}

func (m *memory) ReadVerificationReviews(ctx context.Context, refs []VerificationReviewRef) (map[string]VerificationReviewState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws, _ := WorkspaceFromContext(ctx)
	out := make(map[string]VerificationReviewState, len(refs))
	for _, ref := range refs {
		task, ok := m.tasks[ref.TaskID]
		if !ok || task.Workspace != ws {
			return nil, ErrVerificationAccess
		}
		out[ref.OrderID] = m.verificationReviewStateLocked(task, ref.OrderID)
	}
	return out, nil
}
