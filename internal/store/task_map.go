package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

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
	// NextActionHistory, equal to FoldNextActionHistory over the task's full
	// history and orders, without transferring event payloads. Tasks without
	// a folded field are absent.
	ListNextActionHistory(ctx context.Context, taskIDs []string) (map[string]NextActionHistory, error)
	// ListOpenDependents returns, in ID order, the heads of the workspace's
	// tasks outside merged and closed that depend on any requested task.
	ListOpenDependents(ctx context.Context, taskIDs []string) ([]TaskHead, error)
	// ListTaskContextDesignPins returns each requested task's pins of the
	// requested System Designs, folded by ActiveTaskContextReferences from
	// its context attachment events alone. Tasks without one are absent.
	ListTaskContextDesignPins(ctx context.Context, taskIDs, documentIDs []string) (map[string]map[string]int, error)
	// ListDocumentHeads returns each requested requirement or System Design
	// head; a requirement wins over a System Design with the same ID and a
	// missing ID is absent.
	ListDocumentHeads(ctx context.Context, ids []string) (map[string]DocumentHead, error)
}

// MergeConflictEventKinds are the kinds the merge-conflict fold reads.
var MergeConflictEventKinds = []string{
	"merge.blocked", "merge.conflict_cleared", "merge.conflict_fix_dispatched",
	"merge.conflict_dispatch_failed", "merge.conflict_dispatch_exhausted", "merge.conflict_recovery_blocked",
}

// NextActionHistory is the bounded summary of the history the next-action
// assembler folds: the park command, the latest recorded pull request, the
// bounce window, and the merge-conflict summary. No field carries an event
// payload (component-persistence; component-work-orders, Next action).
type NextActionHistory struct {
	ParkCommand   string
	PullRequest   int
	BounceCount   int
	MergeConflict MergeConflictHistory
}

// MergeConflictHistory is the fixed-size input of the merge-conflict fold.
// Open reports that the latest merge.blocked decodes into its heads; nothing
// else is set otherwise. Cleared reports a merge.conflict_cleared after it.
// Exhausted, Failures, and NextRetryAt describe the open episode after it and
// are set only when it is not cleared: Failures counts the same-episode
// failures whose next_retry_at the Go decoder accepts, and NextRetryAt is the
// normalized time of the last effective step when that step is a failure with
// a time, nil otherwise. DispatchedOrderIDs are the task's own orders that a
// same-heads dispatch named while its episode was open, sorted.
type MergeConflictHistory struct {
	Open               bool
	Cleared            bool
	Exhausted          bool
	Failures           int
	NextRetryAt        json.RawMessage
	DispatchedOrderIDs []string
}

// ParkCommandSummaryLength bounds the park command a summary carries. The
// projection compares it only with dispatch.fail_final, which this prefix
// decides exactly.
const ParkCommandSummaryLength = 64

// SummarizeParkCommand keeps the first ParkCommandSummaryLength characters
// of a park command, as every backend transfers it.
func SummarizeParkCommand(command string) string {
	if runes := []rune(command); len(runes) > ParkCommandSummaryLength {
		return string(runes[:ParkCommandSummaryLength])
	}
	return command
}

// NormalizeRetryTime keeps at most nine fraction digits of an accepted JSON
// retry time, the digits the decoder reads, so the value is at most 37 bytes
// and decodes to the same instant.
func NormalizeRetryTime(raw json.RawMessage) json.RawMessage {
	text := string(raw)
	start := strings.IndexAny(text, ".,")
	if start < 0 {
		return raw
	}
	end := start + 1
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end-start <= 10 {
		return raw
	}
	return json.RawMessage(text[:start+10] + text[end:])
}

type mergeConflictHeads struct {
	ApprovedHead string `json:"approved_head"`
	NewHead      string `json:"new_head"`
}

// SummarizeMergeConflict builds the bounded merge-conflict summary from a
// task's full history in (at, id) order and its work orders, with the fold's
// own decoders; the volatile backend reads it, and every durable backend
// computes the same summary in SQL (component-persistence).
func SummarizeMergeConflict(events []core.Event, orders []core.WorkOrder) MergeConflictHistory {
	latest := -1
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == "merge.blocked" {
			latest = i
			break
		}
	}
	if latest < 0 {
		return MergeConflictHistory{}
	}
	var heads mergeConflictHeads
	if json.Unmarshal(events[latest].Payload, &heads) != nil {
		return MergeConflictHistory{}
	}
	same := func(payload json.RawMessage) bool {
		var candidate mergeConflictHeads
		return json.Unmarshal(payload, &candidate) == nil && candidate == heads
	}
	own := map[string]bool{}
	for _, order := range orders {
		own[order.ID] = true
	}
	out := MergeConflictHistory{Open: true}
	inside := false
	dispatched := map[string]bool{}
	for i, event := range events {
		switch event.Kind {
		case "merge.conflict_cleared":
			inside = false
			if i > latest {
				out.Cleared = true
			}
		case "merge.blocked":
			inside = same(event.Payload)
		case "merge.conflict_fix_dispatched":
			if !inside || !same(event.Payload) {
				continue
			}
			var payload struct {
				WorkOrderID string `json:"work_order_id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.WorkOrderID != "" && own[payload.WorkOrderID] {
				dispatched[payload.WorkOrderID] = true
			}
			if i > latest {
				out.NextRetryAt = nil
			}
		case "merge.conflict_dispatch_failed":
			if i <= latest || !inside || !same(event.Payload) {
				continue
			}
			var failed struct {
				NextRetryAt time.Time `json:"next_retry_at"`
			}
			if json.Unmarshal(event.Payload, &failed) != nil {
				continue
			}
			out.Failures++
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(event.Payload, &fields)
			if raw := fields["next_retry_at"]; raw != nil && string(raw) != "null" {
				out.NextRetryAt = NormalizeRetryTime(raw)
			} else {
				out.NextRetryAt = nil
			}
		case "merge.conflict_dispatch_exhausted":
			if i > latest && inside && same(event.Payload) {
				out.Exhausted = true
			}
		}
	}
	if out.Cleared {
		out.Exhausted, out.Failures, out.NextRetryAt = false, 0, nil
	}
	for id := range dispatched {
		out.DispatchedOrderIDs = append(out.DispatchedOrderIDs, id)
	}
	sort.Strings(out.DispatchedOrderIDs)
	return out
}

// FoldNextActionHistory builds a task's summary from its full history in
// (at, id) order and its work orders, applying the assembler's park-command,
// pull-request, bounce-count, and merge-conflict folds unchanged. The volatile
// backend reads it, and tests use it as the full-history oracle.
func FoldNextActionHistory(events []core.Event, orders []core.WorkOrder) NextActionHistory {
	return NextActionHistory{
		ParkCommand:   SummarizeParkCommand(parkCommand(events)),
		PullRequest:   recordedPullRequest(events),
		BounceCount:   recordedBounceCount(events),
		MergeConflict: SummarizeMergeConflict(events, orders),
	}
}

// ApplyLatestHistoryValue sets the summary field a durable backend's typed
// latest value fills: the park command, the pull-request number, or the
// bounce window.
func ApplyLatestHistoryValue(history *NextActionHistory, kind, value string) error {
	switch kind {
	case "task.state_changed":
		history.ParkCommand = value
	case "pull_request.opened", "pipeline.bounce_limit":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("next-action history %s value %q: %w", kind, value, err)
		}
		if kind == "pull_request.opened" {
			history.PullRequest = n
		} else {
			history.BounceCount = n
		}
	}
	return nil
}

// CompactNextActionHistories drops summaries without a folded field, so a
// task without one is absent on every backend.
func CompactNextActionHistories(histories map[string]NextActionHistory) map[string]NextActionHistory {
	for id, history := range histories {
		if history.empty() {
			delete(histories, id)
		}
	}
	return histories
}

func (h NextActionHistory) empty() bool {
	return h.ParkCommand == "" && h.PullRequest == 0 && h.BounceCount == 0 && !h.MergeConflict.Open
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

// TaskContextDesignPins folds rows in (at, id) order into each task's pins
// of the requested documents. A task without one is absent.
func TaskContextDesignPins(rows []core.Event, documentIDs []string) map[string]map[string]int {
	byTask := map[string][]core.Event{}
	for _, row := range rows {
		byTask[row.TaskID] = append(byTask[row.TaskID], row)
	}
	out := map[string]map[string]int{}
	for id, events := range byTask {
		_, pins := ActiveTaskContextReferences(events)
		for document, version := range pins {
			if !slices.Contains(documentIDs, document) {
				continue
			}
			if out[id] == nil {
				out[id] = map[string]int{}
			}
			out[id][document] = version
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
	out := map[string]NextActionHistory{}
	for _, id := range taskIDs {
		task, ok := m.tasks[id]
		if !ok || (scoped && workspace != "" && task.Workspace != workspace) {
			continue
		}
		var events []core.Event
		for _, event := range m.sortedEventsLocked(id) {
			if NextActionEventKind(event.Kind) {
				events = append(events, event)
			}
		}
		var orders []core.WorkOrder
		for _, order := range m.workOrders {
			if order.TaskID == id {
				orders = append(orders, order)
			}
		}
		out[id] = FoldNextActionHistory(events, orders)
	}
	return CompactNextActionHistories(out), nil
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

func (m *memory) ListTaskContextDesignPins(ctx context.Context, taskIDs, documentIDs []string) (map[string]map[string]int, error) {
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
	return TaskContextDesignPins(rows, documentIDs), nil
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
