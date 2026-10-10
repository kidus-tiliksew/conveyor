package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// TaskNeedsAttention is the shared operator-attention truth table. Keeping it
// at the store boundary lets the memory projection match the HTTP activity
// surfaces while PostgreSQL computes the same predicates from narrow columns.
func TaskNeedsAttention(task core.Task, marker ActivityMarker, pendingAuthority, pendingContext bool) bool {
	return task.State == core.TaskAwaiting || task.State == core.TaskParked ||
		marker.ForgeFailure != nil || marker.ReviewRecovery != nil ||
		marker.InterruptedReviewRecovery != nil || marker.Stalled != nil || pendingAuthority || pendingContext || marker.UserChangesRequested
}

// TaskFilter is the one predicate set the Tasks list and the Board both send
// to the server (AC-2.4). It is shared rather than duplicated so the two
// surfaces cannot drift into filtering differently, and every member is
// evaluated by the store in its own query language — never by narrowing a
// fully-loaded workspace in Go (AC-2.3).
//
// UpdatedFrom/UpdatedTo bound the task's last activity: the time of its
// newest event by (at, id), or its creation instant until one arrives. That is
// the LastEventAt activity markers report and the value both surfaces label
// "Updated" (component-persistence). The range is half-open: UpdatedFrom is
// inclusive, UpdatedTo exclusive.
//
// CreatedFrom/CreatedTo bound the task's persisted creation instant. Later
// events never change whether a task matches. They remain a separate,
// compatible predicate rather than an alias for the Updated bounds; when both
// families are set a task must satisfy both. The range is half-open:
// CreatedFrom is inclusive, CreatedTo exclusive.
//
// ServesRequirementIDs and GoverningDesignIDs name documents the task carries
// as attached context. Both fold the same append-only attachment stream the
// read model folds (see ActiveTaskContextReferences), so a detached document
// stops matching exactly when the row stops showing it.
//
// Every list member is a disjunction — a task matches when it satisfies any of
// the listed states, repositories, requirements, or designs — while distinct
// members still intersect. An empty list leaves that member inactive.
type TaskFilter struct {
	States       []core.TaskState
	Repositories []string
	// Assignee is one single-valued selector. Empty leaves the member inactive,
	// "unassigned" selects tasks without an assignee, and every other value is
	// matched as an exact user ID. An unknown ID therefore matches no tasks.
	Assignee string
	// Query narrows on the row's own identifying text — title, ID, source, and
	// assigned branch — case-insensitively.
	Query                string
	UpdatedFrom          time.Time
	UpdatedTo            time.Time
	CreatedFrom          time.Time
	CreatedTo            time.Time
	ServesRequirementIDs []string
	GoverningDesignIDs   []string
}

// Active reports whether any predicate would narrow the workspace. Callers that
// have a cheaper unfiltered read path use it to keep that path byte-identical.
func (f TaskFilter) Active() bool {
	return len(f.States) > 0 || len(f.Repositories) > 0 || f.Assignee != "" || f.Query != "" || !f.UpdatedFrom.IsZero() ||
		!f.UpdatedTo.IsZero() || !f.CreatedFrom.IsZero() || !f.CreatedTo.IsZero() || len(f.ServesRequirementIDs) > 0 ||
		len(f.GoverningDesignIDs) > 0
}

// Validate rejects a range no task can satisfy, so an inverted range fails at
// the edge instead of silently rendering an empty workspace.
func (f TaskFilter) Validate() error {
	if !f.UpdatedFrom.IsZero() && !f.UpdatedTo.IsZero() && !f.UpdatedFrom.Before(f.UpdatedTo) {
		return fmt.Errorf("task operations updated_from must precede updated_to")
	}
	if !f.CreatedFrom.IsZero() && !f.CreatedTo.IsZero() && !f.CreatedFrom.Before(f.CreatedTo) {
		return fmt.Errorf("task operations created_from must precede created_to")
	}
	return nil
}

// TaskOperationsQuery is the bounded input for the daily Tasks projection.
// A zero Limit preserves the historical unpaginated array response; callers
// that opt into pagination use a positive Limit and receive Total alongside
// the selected page.
type TaskOperationsQuery struct {
	TaskFilter
	Limit  int
	Offset int
}

// CallerAttentionQuery is the bounded input for the personal attention read.
// UserID is derived from the authenticated credential at the HTTP boundary;
// clients never select another member's subject.
type CallerAttentionQuery struct {
	UserID string
	Limit  int
	Offset int
}

func (q CallerAttentionQuery) Validate() error {
	if strings.TrimSpace(q.UserID) == "" {
		return fmt.Errorf("caller attention user id is required")
	}
	if q.Limit <= 0 {
		return fmt.Errorf("caller attention limit must be positive")
	}
	return (TaskOperationsQuery{Limit: q.Limit, Offset: q.Offset}).Validate()
}

// MaxTaskOperationsLimit bounds a single page, and MaxTaskOperationsOffset is
// the largest offset the projection can represent: the Postgres page query
// binds LIMIT/OFFSET as int32, so a larger offset silently wraps — to a
// negative bound Postgres rejects, or worse to a small positive one that pages
// from an unrelated position — where the memory store returns an empty page.
// Both stores reject the out-of-range query instead, so pagination stays
// equivalent across them.
const (
	MaxTaskOperationsLimit  = 200
	MaxTaskOperationsOffset = math.MaxInt32
)

// Validate rejects pagination that the stores cannot represent identically. It
// runs at the HTTP edge and again inside each store, so no caller can reach a
// backend with an unrepresentable bound.
func (q TaskOperationsQuery) Validate() error {
	switch {
	case q.Limit < 0 || q.Limit > MaxTaskOperationsLimit:
		return fmt.Errorf("task operations limit must be between 1 and %d", MaxTaskOperationsLimit)
	case q.Offset < 0 || q.Offset > MaxTaskOperationsOffset:
		return fmt.Errorf("task operations offset must be between 0 and %d", MaxTaskOperationsOffset)
	}
	return q.TaskFilter.Validate()
}

// TaskOperationsPage batches the task rows with only the event and plan data
// consumed by the Tasks HTTP projection. It deliberately does not carry full
// task histories.
type TaskOperationsPage struct {
	Tasks  []core.Task
	Events map[string][]core.Event
	Plans  map[string]core.SpecVersion
	Total  int
}

// TaskPage is the bounded task-only projection used by activity. It omits the
// context events and latest plans that the richer Tasks surface consumes.
type TaskPage struct {
	Tasks []core.Task
	Total int
}

// RecoveryRefreeze re-freezes a recovered order's pipeline policy and stage
// timeout. It carries no harness, model, or effort: queue re-entry clears every
// execution pin (req-worker AC-2.2; DEC-56).
type RecoveryRefreeze struct {
	Setup                config.ExecutionSetup
	ExecutionTimeoutText string
}

type DependencyBlockers struct {
	BlockingTaskIDs      []string
	UnsatisfiableTaskIDs []string
}

type DependencyRemovalRequest struct {
	TaskID          string `json:"task_id"`
	DependsOnTaskID string `json:"depends_on_task_id"`
	Reason          string `json:"reason"`
	RequestID       string `json:"request_id"`
}

type DependencyAdditionRequest struct {
	TaskID          string `json:"task_id"`
	DependsOnTaskID string `json:"depends_on_task_id"`
	Reason          string `json:"reason"`
	RequestID       string `json:"request_id"`
}

type DependencyAdditionResult struct {
	Task      core.Task `json:"task"`
	RequestID string    `json:"request_id"`
	Added     bool      `json:"added"`
}

type DependencyRemovalResult struct {
	Task      core.Task `json:"task"`
	RequestID string    `json:"request_id"`
	Removed   bool      `json:"removed"`
}

// StalledState is derived presentation data for operator-actionable work that
// cannot make forward progress on its own.
type StalledState struct {
	Needed            bool           `json:"needed"`
	Reason            string         `json:"reason"`
	WorkOrder         core.WorkOrder `json:"work_order"`
	LastFailure       string         `json:"last_failure,omitempty"`
	BlockingTaskIDs   []string       `json:"blocking_task_ids,omitempty"`
	UnsatisfiableEdge bool           `json:"unsatisfiable_edge,omitempty"`
}

func StalledTask(orders []core.WorkOrder) *StalledState {
	for i := len(orders) - 1; i >= 0; i-- {
		order := orders[i]
		switch order.State {
		case core.WorkOrderQueued, core.WorkOrderStale, core.WorkOrderTimedOut:
		default:
			continue
		}
		reason := ""
		switch {
		case len(order.UnsatisfiableTaskIDs) > 0:
			reason = "dependency reached a terminal state without merging"
		case order.RetrySuppressed:
			reason = "automatic retry is suppressed"
		case order.State == core.WorkOrderStale:
			reason = "queue deadline expired"
		case order.AutomaticRetryCount >= 2 && strings.TrimSpace(order.LastFailureMessage) != "":
			reason = "dispatch is failing repeatedly"
		}
		if reason != "" {
			failure := strings.TrimSpace(order.LastFailureMessage)
			if failure == "" {
				failure = strings.TrimSpace(order.LastFailureDetail)
			}
			return &StalledState{
				Needed: true, Reason: reason, WorkOrder: order, LastFailure: failure,
				BlockingTaskIDs:   append([]string(nil), order.UnsatisfiableTaskIDs...),
				UnsatisfiableEdge: len(order.UnsatisfiableTaskIDs) > 0,
			}
		}
	}
	return nil
}

type memoryDependencyRemoval struct {
	Request DependencyRemovalRequest
	Actor   Actor
}

type memoryDependencyAddition struct {
	Request DependencyAdditionRequest
	Actor   Actor
	Added   bool
}

func (m *memory) ApproveSpecVersionAndMaterialize(ctx context.Context, taskID string, version int) ([]core.Task, error) {
	if _, err := RequireActor(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.specs[taskID]
	if len(versions) == 0 || versions[len(versions)-1].Version != version {
		return nil, fmt.Errorf("spec version %d for task %s not found or superseded", version, taskID)
	}
	parent, ok := m.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}
	var decomposition []core.BlueprintDecompositionItem
	if len(versions[len(versions)-1].Decomposition) > 0 {
		if err := json.Unmarshal(versions[len(versions)-1].Decomposition, &decomposition); err != nil {
			return nil, fmt.Errorf("decode blueprint decomposition: %w", err)
		}
	}
	if len(decomposition) == 0 {
		versions[len(versions)-1].Approved = true
		versions[len(versions)-1].ApprovedAt = time.Now().UTC()
		m.specs[taskID] = versions
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})})
		return nil, nil
	}
	if err := core.ValidateBlueprintDecomposition(decomposition); err != nil {
		return nil, err
	}
	baseBranches := make(map[string]string, len(decomposition))
	for _, item := range decomposition {
		repo := strings.TrimSpace(item.Repo)
		baseBranch, exists := m.repositories[parent.Workspace][repo]
		if !exists {
			return nil, fmt.Errorf("blueprint %s repository %q is not configured in workspace %s", item.ID, item.Repo, parent.Workspace)
		}
		baseBranches[item.ID] = baseBranch
	}
	createdAt := time.Now().UTC()
	childrenBySub := map[string]core.Task{}
	for _, existing := range m.tasks {
		if existing.ParentTaskID != taskID || existing.OriginSubID == "" {
			continue
		}
		current, exists := childrenBySub[existing.OriginSubID]
		if !exists || blueprintChildPrecedes(existing, current) {
			childrenBySub[existing.OriginSubID] = existing
		}
	}
	createdSubs := make(map[string]struct{}, len(decomposition))
	for _, item := range decomposition {
		if _, exists := childrenBySub[item.ID]; exists {
			continue
		}
		id := core.NewTaskID()
		for {
			if _, exists := m.tasks[id]; !exists {
				break
			}
			id = core.NewTaskID()
		}
		title := strings.TrimSpace(item.Summary)
		title = core.TruncateUTF8Bytes(title, 200)
		child := core.Task{
			ID: id, Workspace: parent.Workspace,
			Source: fmt.Sprintf("blueprint:%s@v%d#%s", parent.ID, version, item.ID),
			Title:  title,
			Body:   fmt.Sprintf("%s\n\nDefined by blueprint task %s, spec version %d (%s).", strings.TrimSpace(item.Summary), parent.ID, version, item.ID),
			Class:  parent.Class, Level: parent.Level, Hold: parent.Hold,
			SpecApproval: parent.SpecApproval, MergeApproval: parent.MergeApproval,
			PolicyVersion: parent.PolicyVersion,
			SetupContract: parent.SetupContract, Repo: strings.TrimSpace(item.Repo),
			BaseBranch: baseBranches[item.ID], Branch: gitx.BranchName(id),
			State: core.TaskQueued, NextStage: core.StageImplement,
			ParentTaskID: parent.ID, OriginSpecVersion: version, OriginSubID: item.ID,
			CreatedAt: createdAt,
		}
		m.tasks[id] = child
		childrenBySub[item.ID] = child
		createdSubs[item.ID] = struct{}{}
		m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "task.created", Payload: core.JSONPayload(child), At: createdAt})
	}
	for _, item := range decomposition {
		if _, created := createdSubs[item.ID]; !created {
			continue
		}
		child := childrenBySub[item.ID]
		if m.dependencies[child.ID] == nil {
			m.dependencies[child.ID] = map[string]struct{}{}
		}
		for _, dependency := range item.DependsOn {
			dependencyID := childrenBySub[dependency].ID
			m.dependencies[child.ID][dependencyID] = struct{}{}
			m.appendEventLocked(ctx, core.Event{TaskID: child.ID, Kind: "task.dependency_added", At: createdAt,
				Payload: core.JSONPayload(map[string]string{"task_id": child.ID, "depends_on_task_id": dependencyID})})
		}
	}
	versions[len(versions)-1].Approved = true
	versions[len(versions)-1].ApprovedAt = createdAt
	m.specs[taskID] = versions
	m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})})
	if len(createdSubs) > 0 {
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "blueprint.materialized", Payload: core.JSONPayload(map[string]any{
			"version": version, "children_created": len(createdSubs), "children_total": len(decomposition),
		})})
	}
	result := make([]core.Task, 0, len(childrenBySub))
	for _, item := range decomposition {
		result = append(result, m.hydrateTaskLocked(childrenBySub[item.ID]))
	}
	return result, nil
}

func blueprintChildPrecedes(candidate, current core.Task) bool {
	if candidate.OriginSpecVersion != current.OriginSpecVersion {
		return candidate.OriginSpecVersion < current.OriginSpecVersion
	}
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.Before(current.CreatedAt)
	}
	return candidate.ID < current.ID
}

func (m *memory) ListBlockingTaskIDs(_ context.Context, taskID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.tasks[taskID]; !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}
	var result []string
	for dependencyID := range m.dependencies[taskID] {
		if dependency, ok := m.tasks[dependencyID]; ok && dependency.State != core.TaskMerged {
			result = append(result, dependencyID)
		}
	}
	sort.Strings(result)
	return result, nil
}

func (m *memory) ValidateTaskDependencies(ctx context.Context, dependencyIDs []string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, _ := WorkspaceFromContext(ctx)
	seen := map[string]bool{}
	for _, dependencyID := range dependencyIDs {
		dependencyID = strings.TrimSpace(dependencyID)
		if dependencyID == "" || seen[dependencyID] {
			return fmt.Errorf("depends_on contains an empty or duplicate task id")
		}
		seen[dependencyID] = true
		dependency, ok := m.tasks[dependencyID]
		if !ok {
			return fmt.Errorf("dependency task %s not found", dependencyID)
		}
		if workspace != "" && dependency.Workspace != workspace {
			return fmt.Errorf("dependency task %s belongs to another workspace", dependencyID)
		}
		if core.TaskTerminal(dependency.State) {
			return fmt.Errorf("dependency task %s is not open", dependencyID)
		}
	}
	return nil
}

func (m *memory) ListDependentTaskIDs(_ context.Context, taskID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []string
	for dependentID, dependencies := range m.dependencies {
		if _, ok := dependencies[taskID]; ok {
			result = append(result, dependentID)
		}
	}
	sort.Strings(result)
	return result, nil
}

func (m *memory) ListDependencyBlockers(ctx context.Context, taskIDs []string) (map[string]DependencyBlockers, error) {
	if len(taskIDs) == 0 {
		return map[string]DependencyBlockers{}, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, _ := WorkspaceFromContext(ctx)
	requested := make(map[string]struct{}, len(taskIDs))
	for _, taskID := range taskIDs {
		requested[taskID] = struct{}{}
	}
	result := map[string]DependencyBlockers{}
	for dependentID, dependencies := range m.dependencies {
		if _, ok := requested[dependentID]; !ok {
			continue
		}
		task, ok := m.tasks[dependentID]
		if !ok || (workspace != "" && task.Workspace != workspace) {
			continue
		}
		blockers := result[dependentID]
		for dependencyID := range dependencies {
			dependency, exists := m.tasks[dependencyID]
			if !exists || dependency.State == core.TaskMerged {
				continue
			}
			blockers.BlockingTaskIDs = append(blockers.BlockingTaskIDs, dependencyID)
			if core.TaskTerminal(dependency.State) {
				blockers.UnsatisfiableTaskIDs = append(blockers.UnsatisfiableTaskIDs, dependencyID)
			}
		}
		sort.Strings(blockers.BlockingTaskIDs)
		sort.Strings(blockers.UnsatisfiableTaskIDs)
		if len(blockers.BlockingTaskIDs) > 0 {
			result[dependentID] = blockers
		}
	}
	return result, nil
}

func (m *memory) AddTaskDependency(ctx context.Context, request DependencyAdditionRequest) (DependencyAdditionResult, error) {
	if _, err := RequireActor(ctx); err != nil {
		return DependencyAdditionResult{}, err
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.DependsOnTaskID = strings.TrimSpace(request.DependsOnTaskID)
	request.Reason = strings.TrimSpace(request.Reason)
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.TaskID == "" || request.DependsOnTaskID == "" || request.Reason == "" || request.RequestID == "" {
		return DependencyAdditionResult{}, fmt.Errorf("task_id, depends_on_task_id, reason, and request_id are required")
	}
	if request.TaskID == request.DependsOnTaskID {
		return DependencyAdditionResult{}, fmt.Errorf("%w: task cannot depend on itself", ErrTaskDependencyConflict)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace, _ := WorkspaceFromContext(ctx)
	task, ok := m.tasks[request.TaskID]
	if !ok || (workspace != "" && task.Workspace != workspace) {
		return DependencyAdditionResult{}, fmt.Errorf("task %s: %w", request.TaskID, ErrNotFound)
	}
	dependency, ok := m.tasks[request.DependsOnTaskID]
	if !ok || dependency.Workspace != task.Workspace || (workspace != "" && dependency.Workspace != workspace) {
		return DependencyAdditionResult{}, fmt.Errorf("dependency task %s: %w", request.DependsOnTaskID, ErrNotFound)
	}
	actor := ActorFromContext(ctx)
	key := task.Workspace + "\x00" + request.RequestID
	if prior, exists := m.dependencyAdditions[key]; exists {
		if prior.Request != request || prior.Actor != actor {
			return DependencyAdditionResult{}, fmt.Errorf("%w: request_id %s was already used for different dependency addition inputs", ErrTaskDependencyConflict, request.RequestID)
		}
		return DependencyAdditionResult{Task: m.hydrateTaskLocked(task), RequestID: request.RequestID, Added: prior.Added}, nil
	}
	if core.TaskTerminal(task.State) {
		return DependencyAdditionResult{}, fmt.Errorf("task %s is not open: %w", request.TaskID, ErrTaskTerminal)
	}
	if core.TaskTerminal(dependency.State) {
		return DependencyAdditionResult{}, fmt.Errorf("dependency task %s is not open: %w", request.DependsOnTaskID, ErrTaskTerminal)
	}
	if dependencies := m.dependencies[request.TaskID]; dependencies != nil {
		if _, exists := dependencies[request.DependsOnTaskID]; exists {
			m.dependencyAdditions[key] = memoryDependencyAddition{Request: request, Actor: actor}
			return DependencyAdditionResult{Task: m.hydrateTaskLocked(task), RequestID: request.RequestID}, nil
		}
	}
	if m.dependencyPathExistsLocked(request.DependsOnTaskID, request.TaskID) {
		return DependencyAdditionResult{}, fmt.Errorf("%w: %s already reaches %s", ErrTaskDependencyCycle, request.DependsOnTaskID, request.TaskID)
	}
	if m.dependencies[request.TaskID] == nil {
		m.dependencies[request.TaskID] = map[string]struct{}{}
	}
	m.dependencies[request.TaskID][request.DependsOnTaskID] = struct{}{}
	m.dependencyAdditions[key] = memoryDependencyAddition{Request: request, Actor: actor, Added: true}
	now := time.Now().UTC()
	m.appendEventLocked(ctx, core.Event{
		TaskID: request.TaskID, Kind: "task.dependency_added", ActorID: actor.ID, ActorRole: actor.Role, At: now,
		Payload: core.JSONPayload(map[string]any{
			"task_id": request.TaskID, "depends_on_task_id": request.DependsOnTaskID,
			"reason": request.Reason, "request_id": request.RequestID,
		}),
	})
	for id, order := range m.workOrders {
		if order.TaskID != request.TaskID || (order.Stage != core.StageImplement && order.Stage != core.StageVerify) ||
			order.State != core.WorkOrderQueued || !order.QueueBlockedAt.IsZero() {
			continue
		}
		order.QueueBlockedAt = now
		order.Claimable = false
		order.UpdatedAt = now
		m.workOrders[id] = order
	}
	return DependencyAdditionResult{Task: m.hydrateTaskLocked(task), RequestID: request.RequestID, Added: true}, nil
}

func (m *memory) dependencyPathExistsLocked(from, target string) bool {
	seen := map[string]struct{}{}
	pending := []string{from}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == target {
			return true
		}
		if _, exists := seen[current]; exists {
			continue
		}
		seen[current] = struct{}{}
		for dependencyID := range m.dependencies[current] {
			pending = append(pending, dependencyID)
		}
	}
	return false
}

func (m *memory) RemoveTaskDependency(ctx context.Context, request DependencyRemovalRequest) (DependencyRemovalResult, error) {
	if _, err := RequireActor(ctx); err != nil {
		return DependencyRemovalResult{}, err
	}
	request.TaskID = strings.TrimSpace(request.TaskID)
	request.DependsOnTaskID = strings.TrimSpace(request.DependsOnTaskID)
	request.Reason = strings.TrimSpace(request.Reason)
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.TaskID == "" || request.DependsOnTaskID == "" || request.Reason == "" || request.RequestID == "" {
		return DependencyRemovalResult{}, fmt.Errorf("task_id, depends_on_task_id, reason, and request_id are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[request.TaskID]
	workspace, _ := WorkspaceFromContext(ctx)
	if !ok || (workspace != "" && task.Workspace != workspace) {
		return DependencyRemovalResult{}, fmt.Errorf("task %s not found", request.TaskID)
	}
	actor := ActorFromContext(ctx)
	key := task.Workspace + "\x00" + request.RequestID
	if prior, exists := m.dependencyRemovals[key]; exists {
		if prior.Request != request || prior.Actor != actor {
			return DependencyRemovalResult{}, fmt.Errorf("request_id %s was already used for different dependency removal inputs", request.RequestID)
		}
		return DependencyRemovalResult{Task: m.hydrateTaskLocked(task), RequestID: request.RequestID}, nil
	}
	dependencies := m.dependencies[request.TaskID]
	if dependencies == nil {
		return DependencyRemovalResult{}, fmt.Errorf("dependency edge %s -> %s not found", request.TaskID, request.DependsOnTaskID)
	}
	if _, exists := dependencies[request.DependsOnTaskID]; !exists {
		return DependencyRemovalResult{}, fmt.Errorf("dependency edge %s -> %s not found", request.TaskID, request.DependsOnTaskID)
	}
	delete(dependencies, request.DependsOnTaskID)
	m.dependencyRemovals[key] = memoryDependencyRemoval{Request: request, Actor: actor}
	now := time.Now().UTC()
	m.appendEventLocked(ctx, core.Event{
		TaskID: request.TaskID, Kind: "task.dependency_removed", ActorID: actor.ID, ActorRole: actor.Role, At: now,
		Payload: core.JSONPayload(map[string]any{
			"task_id": request.TaskID, "depends_on_task_id": request.DependsOnTaskID,
			"actor": actor.ID, "reason": request.Reason, "request_id": request.RequestID,
		}),
	})
	m.resumeDependencyQueueClocksLocked(request.TaskID, now)
	return DependencyRemovalResult{Task: m.hydrateTaskLocked(task), RequestID: request.RequestID, Removed: true}, nil
}

func (m *memory) CreateTask(ctx context.Context, t core.Task) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	return m.CreateTaskWithDependencies(ctx, t, nil)
}

func (m *memory) CreateTaskWithDependencies(ctx context.Context, t core.Task, dependencyIDs []string) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	return m.CreateTaskWithDependenciesAndContext(ctx, t, dependencyIDs, TaskContextInput{})
}

func (m *memory) CreateTaskWithDependenciesAndContext(ctx context.Context, t core.Task, dependencyIDs []string, attached TaskContextInput) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createTaskWithContextLocked(ctx, t, dependencyIDs, attached, nil)
}

func (m *memory) createTaskWithContextLocked(ctx context.Context, t core.Task, dependencyIDs []string, attached TaskContextInput, pinned map[string]int) error {
	attached, err := NormalizeTaskContextInput(attached)
	if err != nil {
		return err
	}
	if _, exists := m.tasks[t.ID]; exists {
		return fmt.Errorf("task %s already exists", t.ID)
	}
	// The workspace-scoped intake key is unique, as PostgreSQL's
	// tasks_workspace_intake_key_idx and SingleStore's in-transaction check
	// enforce. A concurrent retry that loses the race therefore resolves to
	// the winner's task and provenance on every backend (DEC-39).
	if t.IntakeKey != "" {
		for _, existing := range m.tasks {
			if existing.Workspace == t.Workspace && existing.IntakeKey == t.IntakeKey {
				return fmt.Errorf("task intake key already exists")
			}
		}
	}
	if other := openTaskHoldingBranch(m.tasks, t.Workspace, t.Repo, t.Branch, t.ID); other != "" {
		return TaskBranchInUseError(t.Branch, other)
	}
	seen := map[string]bool{}
	for _, dependencyID := range dependencyIDs {
		dependencyID = strings.TrimSpace(dependencyID)
		if dependencyID == "" || seen[dependencyID] {
			return fmt.Errorf("depends_on contains an empty or duplicate task id")
		}
		seen[dependencyID] = true
		dependency, ok := m.tasks[dependencyID]
		if !ok {
			return fmt.Errorf("dependency task %s not found", dependencyID)
		}
		if dependency.Workspace != t.Workspace {
			return fmt.Errorf("dependency task %s belongs to another workspace", dependencyID)
		}
		if core.TaskTerminal(dependency.State) {
			return fmt.Errorf("dependency task %s is not open", dependencyID)
		}
	}
	designVersions, err := m.validateTaskContextLocked(t.Workspace, attached)
	if err != nil {
		return err
	}
	if pinned != nil {
		designVersions = pinned
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.NextStage == "" && (t.State == core.TaskQueued || t.State == core.TaskClaiming) {
		t.NextStage = core.InitialStage(t.Level)
	}
	if err := ValidateRepositoryInstallTask(t); err != nil {
		return err
	}
	if t.RepositoryInstallAttempt > 0 {
		key := memoryScopedKey{workspace: t.Workspace, id: t.Repo}
		for _, existing := range m.repositoryInstalls[key] {
			if existing.Attempt == t.RepositoryInstallAttempt {
				return fmt.Errorf("repository install attempt already exists")
			}
		}
		if m.repositoryInstalls == nil {
			m.repositoryInstalls = map[memoryScopedKey][]core.RepositoryInstallTask{}
		}
		m.repositoryInstalls[key] = append(m.repositoryInstalls[key], core.RepositoryInstallTask{TaskID: t.ID, Attempt: t.RepositoryInstallAttempt})
	}
	m.tasks[t.ID] = t
	if len(seen) > 0 {
		m.dependencies[t.ID] = map[string]struct{}{}
		for dependencyID := range seen {
			m.dependencies[t.ID][dependencyID] = struct{}{}
		}
	}
	m.appendEventLocked(ctx, core.Event{TaskID: t.ID, Kind: "task.created", Payload: TaskCreatedPayload(ctx, t), At: t.CreatedAt})
	for dependencyID := range seen {
		m.appendEventLocked(ctx, core.Event{TaskID: t.ID, Kind: "task.dependency_added", At: t.CreatedAt,
			Payload: core.JSONPayload(map[string]string{"task_id": t.ID, "depends_on_task_id": dependencyID})})
	}
	for _, id := range attached.RequirementIDs {
		m.appendEventLocked(ctx, core.Event{TaskID: t.ID, Kind: TaskContextRequirementAdded, At: t.CreatedAt,
			Payload: core.JSONPayload(map[string]any{"id": id})})
	}
	for _, id := range attached.DesignIDs {
		m.appendEventLocked(ctx, core.Event{TaskID: t.ID, Kind: TaskContextDesignAdded, At: t.CreatedAt,
			Payload: core.JSONPayload(map[string]any{"id": id, "version": designVersions[id]})})
	}
	return nil
}

func (m *memory) validateTaskContextLocked(workspace string, input TaskContextInput) (map[string]int, error) {
	for _, id := range input.RequirementIDs {
		document, ok := m.requirements[memoryScopedKey{workspace: workspace, id: id}]
		if !ok {
			return nil, &TaskContextReferenceError{Kind: "requirement", ID: id, Reason: "was not found in this workspace"}
		}
		if err := ValidateContextDocument("requirement", id, document.CurrentVersion, document.Archived); err != nil {
			return nil, err
		}
	}
	versions := map[string]int{}
	for _, id := range input.DesignIDs {
		document, ok := m.systemDesigns[memoryScopedKey{workspace: workspace, id: id}]
		if !ok {
			return nil, &TaskContextReferenceError{Kind: "system design", ID: id, Reason: "was not found in this workspace"}
		}
		if err := ValidateContextDocument("system design", id, document.CurrentVersion, document.Archived); err != nil {
			return nil, err
		}
		versions[id] = document.CurrentVersion
	}
	return versions, nil
}

func (m *memory) UpdateTaskContext(ctx context.Context, taskID string, change TaskContextChange) (core.TaskContext, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.TaskContext{}, err
	}
	add, err := NormalizeTaskContextInput(change.Add)
	if err != nil {
		return core.TaskContext{}, err
	}
	remove, err := NormalizeTaskContextInput(change.Remove)
	if err != nil {
		return core.TaskContext{}, err
	}
	m.mu.Lock()
	task, ok := m.tasks[taskID]
	if !ok || task.Workspace != workspaceOrDefault(ctx, "") {
		m.mu.Unlock()
		return core.TaskContext{}, fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	if core.TaskTerminal(task.State) {
		m.mu.Unlock()
		return core.TaskContext{}, ErrTaskTerminal
	}
	activeRequirements, activeDesigns := ActiveTaskContextReferences(m.events[taskID])
	versions, err := m.validateTaskContextLocked(task.Workspace, add)
	if err != nil {
		m.mu.Unlock()
		return core.TaskContext{}, err
	}
	for _, id := range remove.RequirementIDs {
		if !activeRequirements[id] {
			m.mu.Unlock()
			return core.TaskContext{}, &TaskContextReferenceError{Kind: "requirement", ID: id, Reason: "is not attached to this task"}
		}
	}
	for _, id := range remove.DesignIDs {
		if activeDesigns[id] == 0 {
			m.mu.Unlock()
			return core.TaskContext{}, &TaskContextReferenceError{Kind: "system design", ID: id, Reason: "is not attached to this task"}
		}
	}
	now := time.Now().UTC()
	for _, id := range add.RequirementIDs {
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextRequirementAdded, At: now, Payload: core.JSONPayload(map[string]any{"id": id})})
	}
	for _, id := range remove.RequirementIDs {
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextRequirementRemoved, At: now, Payload: core.JSONPayload(map[string]any{"id": id})})
	}
	for _, id := range add.DesignIDs {
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextDesignAdded, At: now, Payload: core.JSONPayload(map[string]any{"id": id, "version": versions[id]})})
		activeDesigns[id] = versions[id]
	}
	for _, id := range remove.DesignIDs {
		version := activeDesigns[id]
		if version == 0 {
			version = versions[id]
		}
		m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: TaskContextDesignRemoved, At: now, Payload: core.JSONPayload(map[string]any{"id": id, "version": version})})
	}
	m.mu.Unlock()
	return TaskContextForTask(ctx, m, taskID)
}

func (m *memory) hydrateTaskLocked(task core.Task) core.Task {
	if p, ok := m.pullRequestCloses[task.ID]; ok {
		task.PullRequestClose = &p
		task.PullRequestCloseState = p.State
	}
	task.Dependencies = nil
	task.BlockingTaskIDs = nil
	task.Children = nil
	for dependencyID := range m.dependencies[task.ID] {
		dependency, ok := m.tasks[dependencyID]
		if !ok {
			continue
		}
		task.Dependencies = append(task.Dependencies, core.TaskRelation{
			ID: dependency.ID, Title: dependency.Title, State: dependency.State,
			OriginSpecVersion: dependency.OriginSpecVersion, OriginSubID: dependency.OriginSubID,
		})
		if dependency.State != core.TaskMerged {
			task.BlockingTaskIDs = append(task.BlockingTaskIDs, dependency.ID)
		}
	}
	for _, child := range m.tasks {
		if child.ParentTaskID == task.ID {
			task.Children = append(task.Children, core.TaskRelation{
				ID: child.ID, Title: child.Title, State: child.State,
				OriginSpecVersion: child.OriginSpecVersion, OriginSubID: child.OriginSubID,
			})
		}
	}
	sort.Slice(task.Dependencies, func(i, j int) bool { return task.Dependencies[i].ID < task.Dependencies[j].ID })
	sort.Strings(task.BlockingTaskIDs)
	sort.Slice(task.Children, func(i, j int) bool {
		if task.Children[i].OriginSpecVersion != task.Children[j].OriginSpecVersion {
			return task.Children[i].OriginSpecVersion < task.Children[j].OriginSpecVersion
		}
		return task.Children[i].OriginSubID < task.Children[j].OriginSubID
	})
	return task
}

func (m *memory) GetTask(_ context.Context, id string) (core.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return core.Task{}, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	if lifecycle, exists := m.github[id]; exists {
		copy := lifecycle
		t.GitHub = &copy
	}
	return m.hydrateTaskLocked(t), nil
}

func (m *memory) GetTaskByIntakeKey(_ context.Context, key string) (core.Task, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, task := range m.tasks {
		if key != "" && task.IntakeKey == key {
			if lifecycle, exists := m.github[task.ID]; exists {
				copy := lifecycle
				task.GitHub = &copy
			}
			return m.hydrateTaskLocked(task), true, nil
		}
	}
	return core.Task{}, false, nil
}

func (m *memory) ListTasks(_ context.Context) ([]core.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]core.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		if lifecycle, exists := m.github[t.ID]; exists {
			copy := lifecycle
			t.GitHub = &copy
		}
		out = append(out, m.hydrateTaskLocked(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (m *memory) ListTasksFiltered(ctx context.Context, filter TaskFilter) ([]core.Task, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	if !filter.Active() {
		return m.ListTasks(ctx)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]core.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		if !m.taskMatchesFilterLocked(t, filter) {
			continue
		}
		if lifecycle, exists := m.github[t.ID]; exists {
			copy := lifecycle
			t.GitHub = &copy
		}
		out = append(out, m.hydrateTaskLocked(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// taskMatchesFilterLocked evaluates the shared surface predicate. The Postgres
// store expresses the same members in SQL; the two must agree, so the fixtures
// in storetest exercise both through one table.
func (m *memory) taskMatchesFilterLocked(task core.Task, filter TaskFilter) bool {
	if len(filter.States) > 0 && !slices.Contains(filter.States, task.State) {
		return false
	}
	if len(filter.Repositories) > 0 && !slices.Contains(filter.Repositories, task.Repo) {
		return false
	}
	if filter.Assignee != "" {
		if filter.Assignee == "unassigned" {
			if task.Assignee != nil {
				return false
			}
		} else if task.Assignee == nil || task.Assignee.UserID != filter.Assignee {
			return false
		}
	}
	if needle := strings.ToLower(filter.Query); needle != "" {
		matched := false
		for _, field := range []string{task.Title, task.ID, task.Source, task.Branch} {
			matched = matched || strings.Contains(strings.ToLower(field), needle)
		}
		if !matched {
			return false
		}
	}
	if !filter.UpdatedFrom.IsZero() || !filter.UpdatedTo.IsZero() {
		updated := m.taskLastActivityLocked(task)
		if !filter.UpdatedFrom.IsZero() && updated.Before(filter.UpdatedFrom) {
			return false
		}
		if !filter.UpdatedTo.IsZero() && !updated.Before(filter.UpdatedTo) {
			return false
		}
	}
	if !filter.CreatedFrom.IsZero() || !filter.CreatedTo.IsZero() {
		if !filter.CreatedFrom.IsZero() && task.CreatedAt.Before(filter.CreatedFrom) {
			return false
		}
		if !filter.CreatedTo.IsZero() && !task.CreatedAt.Before(filter.CreatedTo) {
			return false
		}
	}
	if len(filter.ServesRequirementIDs) > 0 || len(filter.GoverningDesignIDs) > 0 {
		requirements, designs := ActiveTaskContextReferences(ChronologicalTaskEvents(m.events[task.ID]))
		if len(filter.ServesRequirementIDs) > 0 {
			matched := false
			for _, id := range filter.ServesRequirementIDs {
				matched = matched || requirements[id]
			}
			if !matched {
				return false
			}
		}
		if len(filter.GoverningDesignIDs) > 0 {
			matched := false
			for _, id := range filter.GoverningDesignIDs {
				_, attached := designs[id]
				matched = matched || attached
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

// taskLastActivityLocked is the instant the surfaces label "Updated": the time
// of the task's newest event by (at, id), or its creation time until one
// arrives. It is the same instant ListActivityMarkers reports as LastEventAt.
func (m *memory) taskLastActivityLocked(task core.Task) time.Time {
	if latest, ok := LatestTaskEvent(m.events[task.ID], ""); ok {
		return latest.At
	}
	return task.CreatedAt
}

func (m *memory) ListTaskOperations(ctx context.Context, query TaskOperationsQuery) (TaskOperationsPage, error) {
	if err := query.Validate(); err != nil {
		return TaskOperationsPage{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	tasks := make([]core.Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		if task.Workspace != workspace || !m.taskMatchesFilterLocked(task, query.TaskFilter) {
			continue
		}
		if lifecycle, exists := m.github[task.ID]; exists {
			copy := lifecycle
			task.GitHub = &copy
		}
		tasks = append(tasks, m.hydrateTaskLocked(task))
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].ID < tasks[j].ID
		}
		return tasks[i].CreatedAt.After(tasks[j].CreatedAt)
	})
	page := TaskOperationsPage{Total: len(tasks), Events: map[string][]core.Event{}, Plans: map[string]core.SpecVersion{}}
	start := query.Offset
	if start > len(tasks) {
		start = len(tasks)
	}
	end := len(tasks)
	if query.Limit > 0 && start+query.Limit < end {
		end = start + query.Limit
	}
	page.Tasks = append([]core.Task(nil), tasks[start:end]...)
	for _, task := range page.Tasks {
		for _, event := range m.events[task.ID] {
			if taskOperationsEventKind(event.Kind) {
				page.Events[task.ID] = append(page.Events[task.ID], event)
			}
		}
		if versions := m.specs[task.ID]; len(versions) > 0 {
			page.Plans[task.ID] = versions[len(versions)-1]
		}
	}
	return page, nil
}

func (m *memory) ListTaskPage(ctx context.Context, query TaskOperationsQuery) (TaskPage, error) {
	page, err := m.ListTaskOperations(ctx, query)
	return TaskPage{Tasks: page.Tasks, Total: page.Total}, err
}

func (m *memory) ListCallerAttentionTaskPage(ctx context.Context, query CallerAttentionQuery) (TaskPage, error) {
	if err := query.Validate(); err != nil {
		return TaskPage{}, err
	}
	tasks, err := m.ListTasksFiltered(ctx, TaskFilter{Assignee: query.UserID})
	if err != nil {
		return TaskPage{}, err
	}
	markers, err := m.ListActivityMarkers(ctx)
	if err != nil {
		return TaskPage{}, err
	}
	proposals, err := m.ListPendingProposals(ctx)
	if err != nil {
		return TaskPage{}, err
	}
	orders, err := m.ListWorkOrders(ctx)
	if err != nil {
		return TaskPage{}, err
	}
	markerByTask := make(map[string]ActivityMarker, len(markers))
	for _, marker := range markers {
		markerByTask[marker.TaskID] = marker
	}
	pendingAuthority := pendingAuthorityTaskIDs(orders, proposals)
	pendingContext := pendingTaskContextIDs(proposals)
	attention := make([]core.Task, 0, len(tasks))
	for _, task := range tasks {
		if core.TaskTerminal(task.State) || core.BlueprintAnchor(task) {
			continue
		}
		if TaskNeedsAttention(task, markerByTask[task.ID], pendingAuthority[task.ID], pendingContext[task.ID]) {
			attention = append(attention, task)
		}
	}
	page := TaskPage{Total: len(attention)}
	start := min(query.Offset, len(attention))
	end := min(start+query.Limit, len(attention))
	page.Tasks = append([]core.Task(nil), attention[start:end]...)
	return page, nil
}

func taskOperationsEventKind(kind string) bool {
	switch kind {
	case TaskContextRequirementAdded, TaskContextRequirementRemoved, TaskContextDesignAdded, TaskContextDesignRemoved, "task.state_changed":
		return true
	default:
		return false
	}
}

func (m *memory) SetTaskHold(ctx context.Context, id string, hold bool) (core.Task, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Task{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return core.Task{}, fmt.Errorf("task %s not found", id)
	}
	if t.Hold == hold {
		return t, nil
	}
	// PostgreSQL rolls back the projection when its audit text cannot be
	// stored. Validate before changing the memory projection (DEC-38;
	// component-persistence; component-verification-strategy).
	actor := ActorFromContext(ctx)
	if !utf8.ValidString(actor.ID) || !utf8.ValidString(string(actor.Role)) || strings.ContainsRune(actor.ID, '\x00') || strings.ContainsRune(string(actor.Role), '\x00') {
		return core.Task{}, fmt.Errorf("audit actor must be valid UTF-8 without NUL characters")
	}
	t.Hold = hold
	m.tasks[id] = t
	kind := "task.hold.set"
	if !hold {
		kind = "task.hold.cleared"
	}
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: kind, Payload: core.JSONPayload(map[string]any{"hold": hold})})
	return t, nil
}

func (m *memory) SetTaskAssigneeCommand(ctx context.Context, lease taskops.TaskLease, id, assigneeUserID string) (core.Task, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Task{}, err
	}
	if !lease.ValidForCommand(id, taskops.SetAssigneeCommand) {
		return core.Task{}, fmt.Errorf("task assignment requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return core.Task{}, fmt.Errorf("task %s not found", id)
	}
	assigneeUserID = strings.TrimSpace(assigneeUserID)
	if assigneeUserID != "" {
		key := memoryScopedKey{workspace: task.Workspace, id: assigneeUserID}
		if !m.workspaceMembers[key] {
			return core.Task{}, fmt.Errorf("assignee %s is not an active member of workspace %s", assigneeUserID, task.Workspace)
		}
		role := m.workspaceMemberRoles[key]
		if role == "" {
			role = core.WorkspaceRoleContributor
		}
		if !core.RoleAllows(role, core.CapabilityClaimWork) {
			return core.Task{}, fmt.Errorf("assignee %s role %s lacks %s capability in workspace %s", assigneeUserID, role, core.CapabilityClaimWork, task.Workspace)
		}
	}
	if (task.Assignee != nil && task.Assignee.UserID == assigneeUserID) || (task.Assignee == nil && assigneeUserID == "") {
		return task, nil
	}
	kind := "task.assignee.set"
	if assigneeUserID == "" {
		task.Assignee = nil
		kind = "task.assignee.cleared"
	} else {
		task.Assignee = &core.TaskAssignee{UserID: assigneeUserID}
	}
	m.tasks[id] = task
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: kind, Payload: core.JSONPayload(map[string]any{"assignee_user_id": assigneeUserID})})
	return task, nil
}

func (m *memory) UpdateTaskClassification(ctx context.Context, id, class string) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	task.Class = class
	m.tasks[id] = task
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "task.classified", Payload: core.JSONPayload(map[string]any{"class": class})})
	return nil
}

func (m *memory) EnsureTaskEnqueued(_ context.Context, id string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	if task.State != core.TaskQueued {
		return fmt.Errorf("task %s is not queued", id)
	}
	return nil
}

func (m *memory) CreateJob(ctx context.Context, j core.Job) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[j.TaskID]; !ok {
		return fmt.Errorf("task %s not found", j.TaskID)
	}
	if _, _, ok := m.findJobLocked(j.ID); ok {
		return fmt.Errorf("%w: job %s already exists", ErrDispatchJobConflict, j.ID)
	}
	m.jobs[j.TaskID] = append(m.jobs[j.TaskID], j)
	m.appendEventLocked(ctx, core.Event{TaskID: j.TaskID, JobID: j.ID, Kind: "job.created", Payload: core.JSONPayload(j)})
	return nil
}

func (m *memory) UpdateJob(ctx context.Context, j core.Job) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs := m.jobs[j.TaskID]
	for i := range jobs {
		if jobs[i].ID == j.ID {
			if err := ValidateJobTransition(jobs[i].State, j.State); err != nil {
				return err
			}
			jobs[i] = j
			m.jobs[j.TaskID] = jobs
			m.appendEventLocked(ctx, core.Event{TaskID: j.TaskID, JobID: j.ID, Kind: "job.updated", Payload: core.JSONPayload(j)})
			return nil
		}
	}
	return fmt.Errorf("job %s not found", j.ID)
}

func (m *memory) ListJobs(_ context.Context, taskID string) ([]core.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := append([]core.Job(nil), m.jobs[taskID]...)
	sortJobs(jobs)
	return jobs, nil
}

func (m *memory) GetLatestJob(_ context.Context, taskID string) (core.Job, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := append([]core.Job(nil), m.jobs[taskID]...)
	if len(jobs) == 0 {
		return core.Job{}, false, nil
	}
	sortJobs(jobs)
	return jobs[len(jobs)-1], true, nil
}

func (m *memory) CreateIntervention(ctx context.Context, intervention core.Intervention) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.tasks[intervention.TaskID]
	if !ok {
		return fmt.Errorf("task %s not found", intervention.TaskID)
	}
	if !intervention.Action.Valid() {
		return fmt.Errorf("invalid intervention action %q", intervention.Action)
	}
	if intervention.JobID != "" {
		job, _, ok := m.findJobLocked(intervention.JobID)
		if !ok || job.TaskID != intervention.TaskID {
			return fmt.Errorf("job %s does not belong to task %s", intervention.JobID, intervention.TaskID)
		}
	}
	actor := ActorFromContext(ctx)
	if intervention.ActorID == "" {
		intervention.ActorID = actor.ID
	}
	if intervention.ActorRole == "" {
		intervention.ActorRole = actor.Role
	}
	if intervention.At.IsZero() {
		intervention.At = time.Now().UTC()
	}
	m.nextReviewID++
	intervention.ID = m.nextReviewID
	m.interventions[intervention.TaskID] = append(m.interventions[intervention.TaskID], intervention)
	m.appendEventLocked(ctx, core.Event{
		TaskID:    intervention.TaskID,
		JobID:     intervention.JobID,
		Kind:      "intervention." + string(intervention.Action),
		ActorID:   intervention.ActorID,
		ActorRole: intervention.ActorRole,
		Payload: core.JSONPayload(map[string]any{
			"reason_code": intervention.ReasonCode,
			"comment":     intervention.Comment,
		}),
		At: intervention.At,
	})
	return nil
}

func (m *memory) RequestChangesCommand(ctx context.Context, lease taskops.TaskLease, request taskops.RequestChanges) (core.Task, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Task{}, err
	}
	if !lease.ValidForCommand(request.TaskID, taskops.RequestChangesCommand) {
		return core.Task{}, fmt.Errorf("request changes requires a valid taskops lease")
	}
	if strings.TrimSpace(request.Feedback) == "" {
		return core.Task{}, fmt.Errorf("feedback is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[request.TaskID]
	if !ok {
		return core.Task{}, fmt.Errorf("task %s not found", request.TaskID)
	}
	if !AtMergeGate(task, m.events[task.ID]) {
		return core.Task{}, fmt.Errorf("task %s is not at the merge gate", task.ID)
	}
	if request.JobID != "" {
		job, _, found := m.findJobLocked(request.JobID)
		if !found || job.TaskID != task.ID || job.Stage != core.StageReview {
			return core.Task{}, fmt.Errorf("review job %s does not belong to task %s", request.JobID, task.ID)
		}
	}
	actor := ActorFromContext(ctx)
	if actor.Role != core.ActorUser && actor.Role != core.ActorHuman {
		return core.Task{}, fmt.Errorf("request changes requires a user actor")
	}
	count := 1
	for _, event := range m.events[task.ID] {
		if event.Kind == "pipeline.bounced" {
			count++
		}
	}
	plan := PlanChangesRequested(ChangesRequestedInput{
		TaskID: task.ID, JobID: request.JobID, ActorID: actor.ID, ActorRole: actor.Role,
		ReasonCode: UserRequestChangesReason, Feedback: request.Feedback, Source: UserRequestChangesSource,
		Count: count, Window: 1, MaxBounces: request.MaxBounces, Requeue: core.TaskInterventionRedirect,
	})
	state, err := core.TransitionTask(task.State, plan.Command)
	if err != nil {
		return core.Task{}, err
	}
	m.nextReviewID++
	plan.Intervention.ID = m.nextReviewID
	m.interventions[task.ID] = append(m.interventions[task.ID], plan.Intervention)
	for _, event := range plan.Events {
		m.appendEventLocked(ctx, event)
	}
	fromState, fromStage := task.State, task.NextStage
	task.State, task.NextStage, task.RecoveryStage = state, plan.NextStage, plan.Recovery
	if request.Hold {
		task.Hold = true
	}
	m.tasks[task.ID] = task
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.state_changed", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state, "command": plan.Command})})
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "pipeline.transition_decided", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"from_stage": fromStage, "next_stage": plan.NextStage, "recovery_stage": plan.Recovery, "state": state})})
	if request.Hold {
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.hold_changed", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"hold": true, "reason_code": UserRequestChangesReason})})
	}
	return task, nil
}

func (m *memory) CancelTaskCommand(ctx context.Context, lease taskops.TaskLease, intervention core.Intervention) (core.Task, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Task{}, err
	}
	if !lease.ValidFor(intervention.TaskID) {
		return core.Task{}, fmt.Errorf("task cancellation requires a valid taskops lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelTaskLocked(ctx, intervention)
}

func (m *memory) cancelTaskLocked(ctx context.Context, intervention core.Intervention) (core.Task, error) {
	task, ok := m.tasks[intervention.TaskID]
	if !ok {
		return core.Task{}, fmt.Errorf("task %s not found", intervention.TaskID)
	}
	if core.TaskTerminal(task.State) {
		return core.Task{}, ErrTaskTerminal
	}
	if strings.TrimSpace(intervention.ReasonCode) == "" || intervention.Action != core.InterventionCancel {
		return core.Task{}, fmt.Errorf("cancel intervention requires a reason")
	}
	taskState, err := core.TransitionTask(task.State, core.TaskCancel)
	if err != nil {
		return core.Task{}, err
	}
	actor := ActorFromContext(ctx)
	if intervention.ActorID == "" {
		intervention.ActorID = actor.ID
	}
	if intervention.ActorRole == "" {
		intervention.ActorRole = actor.Role
	}
	if intervention.At.IsZero() {
		intervention.At = time.Now().UTC()
	}
	m.nextReviewID++
	intervention.ID = m.nextReviewID
	m.interventions[task.ID] = append(m.interventions[task.ID], intervention)
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: intervention.JobID, Kind: "intervention.cancel", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"reason_code": intervention.ReasonCode, "comment": intervention.Comment}), At: intervention.At})

	cancelled := make([]string, 0)
	for id, order := range m.workOrders {
		if order.TaskID != task.ID || order.State == core.WorkOrderCompleted || order.State == core.WorkOrderCancelled {
			continue
		}
		attemptID, sessionID := order.AttemptID, order.SessionID
		if attemptID != "" {
			order.LastAttemptID = attemptID
			clearActiveAttempt(&order)
		}
		if order.State == core.WorkOrderClaimed {
			order.LastAttemptOutcome = core.WorkOrderOutcomeCancelled
		}
		orderState, transitionErr := core.TransitionWorkOrder(order.State, core.WorkOrderCmdCancel)
		if transitionErr != nil {
			return core.Task{}, transitionErr
		}
		order.State, order.Claimable, order.UpdatedAt = orderState, false, intervention.At
		m.workOrders[id] = order
		eventOrder := order
		eventOrder.AttemptID = attemptID
		eventOrder.SessionID = sessionID
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "work_order.cancelled", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(eventOrder), At: intervention.At})
		cancelled = append(cancelled, id)
		if jobs := m.jobs[task.ID]; len(jobs) != 0 {
			for i := range jobs {
				if jobs[i].ID == order.JobID && jobs[i].State != core.JobDone {
					jobs[i].State, jobs[i].EndedAt = core.JobFailed, intervention.At
				}
			}
			m.jobs[task.ID] = jobs
		}
	}
	from := task.State
	task.State, task.NextStage, task.RecoveryStage = taskState, "", ""
	m.tasks[task.ID] = task
	m.deleteProposedTaskContextLocked(task.ID)
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.state_changed", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"from": from, "to": taskState, "command": core.TaskCancel}), At: intervention.At})
	m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.cancelled", ActorID: intervention.ActorID, ActorRole: intervention.ActorRole, Payload: core.JSONPayload(map[string]any{"actor": intervention.ActorID, "reason": intervention.ReasonCode, "comment": intervention.Comment, "from": from, "cancelled_work_orders": cancelled}), At: intervention.At})
	m.recordDependencyOutcomeLocked(ctx, task.ID, taskState, intervention.At)
	return task, nil
}

func (m *memory) ListInterventions(_ context.Context, taskID string) ([]core.Intervention, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := append([]core.Intervention(nil), m.interventions[taskID]...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].At.Equal(items[j].At) {
			return items[i].ID < items[j].ID
		}
		return items[i].At.Before(items[j].At)
	})
	return items, nil
}

func (m *memory) findJobLocked(id string) (core.Job, int, bool) {
	for _, jobs := range m.jobs {
		for i, job := range jobs {
			if job.ID == id {
				return job, i, true
			}
		}
	}
	return core.Job{}, 0, false
}

func sortJobs(jobs []core.Job) {
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].StartedAt.IsZero() != jobs[j].StartedAt.IsZero() {
			return !jobs[i].StartedAt.IsZero()
		}
		if jobs[i].StartedAt.IsZero() {
			return false
		}
		if jobs[i].StartedAt.Equal(jobs[j].StartedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].StartedAt.Before(jobs[j].StartedAt)
	})
}
