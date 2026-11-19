package store

import (
	"container/heap"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

var _ CrateStore = (*memory)(nil)

// CreateCrate implements CrateStore. The store mutex holds the validation,
// the record, and its one crate.created event together, so a refusal records
// nothing (component-crates invariants 4, 5, and 8).
func (m *memory) CreateCrate(ctx context.Context, input CrateInput) (Crate, error) {
	actor, err := RequireActor(ctx)
	if err != nil {
		return Crate{}, err
	}
	input, err = NormalizeCrateInput(input)
	if err != nil {
		return Crate{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.validateTaskContextLocked(workspace, TaskContextInput{RequirementIDs: input.RequirementIDs}); err != nil {
		return Crate{}, err
	}
	crate := Crate{
		ID: NewCrateID(), Workspace: workspace, Name: input.Name, TargetDate: input.TargetDate, State: CrateOpen,
		CreatedBy: actor.ID, CreatedAt: CrateTime(time.Now()), RequirementIDs: append([]string{}, input.RequirementIDs...),
	}
	if m.crates == nil {
		m.crates = map[memoryScopedKey]Crate{}
	}
	key := memoryScopedKey{workspace: workspace, id: crate.ID}
	if _, exists := m.crates[key]; exists {
		return Crate{}, fmt.Errorf("crate %s already exists", crate.ID)
	}
	m.crates[key] = cloneCrate(crate)
	m.appendEventLocked(ctx, core.Event{Kind: CrateCreatedEvent, At: crate.CreatedAt, Payload: CrateCreatedPayload(crate)})
	return m.crateWithCountsLocked(crate), nil
}

// UpdateCrate implements CrateStore. Closing a crate holds the same mutex as
// a join and as intake, so no task joins a crate after it closes
// (component-crates invariant 6).
func (m *memory) UpdateCrate(ctx context.Context, id string, patch CratePatch) (Crate, error) {
	if _, err := RequireActor(ctx); err != nil {
		return Crate{}, err
	}
	patch, err := NormalizeCratePatch(patch)
	if err != nil {
		return Crate{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryScopedKey{workspace: workspace, id: strings.TrimSpace(id)}
	current, ok := m.crates[key]
	if !ok {
		return Crate{}, &CrateNotFoundError{ID: id}
	}
	if patch.RequirementIDs != nil {
		if _, err := m.validateTaskContextLocked(workspace, TaskContextInput{RequirementIDs: *patch.RequirementIDs}); err != nil {
			return Crate{}, err
		}
	}
	now := time.Now()
	next, kind, payload := ApplyCratePatch(cloneCrate(current), patch, now)
	if kind != "" {
		m.crates[key] = cloneCrate(next)
		m.appendEventLocked(ctx, core.Event{Kind: kind, At: CrateTime(now), Payload: payload})
	}
	return m.crateWithCountsLocked(next), nil
}

// GetCrate implements CrateStore.
func (m *memory) GetCrate(ctx context.Context, id string) (CrateRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	crate, ok := m.crates[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: strings.TrimSpace(id)}]
	if !ok {
		return CrateRecord{}, &CrateNotFoundError{ID: id}
	}
	members := m.crateMembersLocked(crate)
	record := CrateRecord{Crate: m.crateWithCountsLocked(crate), TaskIDs: make([]string, len(members))}
	for i, member := range members {
		record.TaskIDs[i] = member.ID
	}
	return record, nil
}

// ListCrates implements CrateStore.
func (m *memory) ListCrates(ctx context.Context, query CrateListQuery) ([]Crate, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Crate{}
	for key, crate := range m.crates {
		if key.workspace != workspace || !CrateListMatches(query, crate.State) {
			continue
		}
		out = append(out, m.crateWithCountsLocked(crate))
	}
	SortCrates(out)
	return out, nil
}

// CrateNames implements CrateStore.
func (m *memory) CrateNames(ctx context.Context, ids []string) (map[string]string, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if crate, ok := m.crates[memoryScopedKey{workspace: workspace, id: id}]; ok {
			out[id] = crate.Name
		}
	}
	return out, nil
}

// ListCrateMembers implements CrateStore.
func (m *memory) ListCrateMembers(ctx context.Context, ids []string) (map[string][]CrateMemberTask, error) {
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]CrateMemberTask, len(ids))
	for _, id := range ids {
		crate, ok := m.crates[memoryScopedKey{workspace: workspace, id: id}]
		if _, listed := out[id]; !ok || listed {
			continue
		}
		for _, member := range m.crateMembersLocked(crate) {
			out[id] = append(out[id], CrateMemberTask{ID: member.ID, Title: member.Title, State: member.State})
		}
	}
	return out, nil
}

// ListCrateMembersLimited implements CrateStore. It scans the tasks under
// the read lock and keeps at most limit plus one candidates in a max-heap on
// (creation time, ID), so a large crate allocates nothing per member.
func (m *memory) ListCrateMembersLimited(ctx context.Context, crateID, excludeTaskID string, limit int) ([]CrateMemberTask, bool, error) {
	if err := CheckCrateMemberLimit(limit); err != nil {
		return nil, false, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.RLock()
	defer m.mu.RUnlock()
	kept := &crateMemberHeap{}
	for _, task := range m.tasks {
		if task.Workspace != workspace || task.TaskCrateID() != crateID || task.ID == excludeTaskID {
			continue
		}
		candidate := crateMemberCandidate{createdAt: task.CreatedAt, member: CrateMemberTask{ID: task.ID, Title: task.Title, State: task.State}}
		if kept.Len() <= limit {
			heap.Push(kept, candidate)
		} else if crateMemberBefore(candidate, (*kept)[0]) {
			(*kept)[0] = candidate
			heap.Fix(kept, 0)
		}
	}
	ordered := []crateMemberCandidate(*kept)
	slices.SortFunc(ordered, func(a, b crateMemberCandidate) int {
		if crateMemberBefore(a, b) {
			return -1
		}
		if crateMemberBefore(b, a) {
			return 1
		}
		return 0
	})
	more := len(ordered) > limit
	if more {
		ordered = ordered[:limit]
	}
	members := make([]CrateMemberTask, len(ordered))
	for i, candidate := range ordered {
		members[i] = candidate.member
	}
	return members, more, nil
}

type crateMemberCandidate struct {
	createdAt time.Time
	member    CrateMemberTask
}

func crateMemberBefore(a, b crateMemberCandidate) bool {
	if n := a.createdAt.Compare(b.createdAt); n != 0 {
		return n < 0
	}
	return a.member.ID < b.member.ID
}

// crateMemberHeap is a max-heap: its root is the latest kept candidate, the
// one a earlier candidate replaces.
type crateMemberHeap []crateMemberCandidate

func (h crateMemberHeap) Len() int           { return len(h) }
func (h crateMemberHeap) Less(i, j int) bool { return crateMemberBefore(h[j], h[i]) }
func (h crateMemberHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *crateMemberHeap) Push(x any)        { *h = append(*h, x.(crateMemberCandidate)) }
func (h *crateMemberHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// ListCrateActivity implements CrateStore: the listed event kinds of the
// crate's current members, newest first (component-crates invariant 13).
func (m *memory) ListCrateActivity(ctx context.Context, id string, query CrateActivityQuery) (CrateActivityPage, error) {
	limit, cursor, err := NormalizeCrateActivityQuery(query)
	if err != nil {
		return CrateActivityPage{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	crate, ok := m.crates[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: strings.TrimSpace(id)}]
	if !ok {
		return CrateActivityPage{}, &CrateNotFoundError{ID: id}
	}
	var entries []CrateActivityEntry
	for _, member := range m.crateMembersLocked(crate) {
		for _, event := range m.events[member.ID] {
			if !slices.Contains(CrateActivityKinds, event.Kind) || !CrateActivityBefore(event, cursor) {
				continue
			}
			event.Payload = append([]byte(nil), event.Payload...)
			entries = append(entries, CrateActivityEntry{Event: event, TaskTitle: member.Title})
		}
	}
	SortCrateActivity(entries)
	if len(entries) > limit+1 {
		entries = entries[:limit+1]
	}
	return PageCrateActivity(entries, limit), nil
}

// SetTaskCrate implements CrateStore. A join of the crate the task already
// holds changes nothing and records nothing.
func (m *memory) SetTaskCrate(ctx context.Context, taskID, crateID string) (core.Task, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Task{}, err
	}
	crateID, err := NormalizeJoinCrateID(crateID)
	if err != nil {
		return core.Task{}, err
	}
	return m.moveTaskCrate(ctx, taskID, crateID)
}

// ClearTaskCrate implements CrateStore. A task in no crate stays in none and
// records nothing.
func (m *memory) ClearTaskCrate(ctx context.Context, taskID string) (core.Task, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Task{}, err
	}
	return m.moveTaskCrate(ctx, taskID, "")
}

// moveTaskCrate changes only the task's crate and appends one task event
// with the actor, the crate joined, and the crate left (component-crates
// invariants 1 and 8; req-crates AC-2.2, AC-2.3, AC-4.1).
func (m *memory) moveTaskCrate(ctx context.Context, taskID, crateID string) (core.Task, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Task{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok || (workspace != "" && task.Workspace != workspace) {
		return core.Task{}, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	if crateID != "" {
		if err := m.crateJoinableLocked(task.Workspace, crateID); err != nil {
			return core.Task{}, err
		}
	}
	left := task.TaskCrateID()
	if left != crateID {
		if crateID == "" {
			task.Crate = nil
		} else {
			task.Crate = &core.TaskCrate{ID: crateID}
		}
		m.tasks[taskID] = task
		m.appendEventLocked(ctx, TaskCrateMoveEvent(taskID, crateID, left, CrateTime(time.Now())))
	}
	if lifecycle, exists := m.github[task.ID]; exists {
		copy := lifecycle
		task.GitHub = &copy
	}
	return m.hydrateTaskLocked(task), nil
}

// crateJoinableLocked refuses a crate that is missing, in another workspace,
// or closed (component-crates invariants 3, 6, and 7).
func (m *memory) crateJoinableLocked(workspace, crateID string) error {
	crate, ok := m.crates[memoryScopedKey{workspace: workspace, id: crateID}]
	if !ok {
		return &CrateNotFoundError{ID: crateID}
	}
	if crate.State != CrateOpen {
		return &CrateClosedError{ID: crateID}
	}
	return nil
}

func (m *memory) crateMembersLocked(crate Crate) []core.Task {
	var members []core.Task
	for _, task := range m.tasks {
		if task.Workspace == crate.Workspace && task.TaskCrateID() == crate.ID {
			members = append(members, task)
		}
	}
	slices.SortFunc(members, func(a, b core.Task) int {
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return members
}

func (m *memory) crateWithCountsLocked(crate Crate) Crate {
	crate = cloneCrate(crate)
	members := m.crateMembersLocked(crate)
	states := make([]core.TaskState, len(members))
	for i, member := range members {
		states[i] = member.State
	}
	ApplyCrateCounts(&crate, states)
	return crate
}

func cloneCrate(crate Crate) Crate {
	if crate.TargetDate != nil {
		date := *crate.TargetDate
		crate.TargetDate = &date
	}
	if crate.ClosedAt != nil {
		closedAt := *crate.ClosedAt
		crate.ClosedAt = &closedAt
	}
	crate.RequirementIDs = append([]string{}, crate.RequirementIDs...)
	return crate
}
