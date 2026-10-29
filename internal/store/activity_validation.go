package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"reflect"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ActivityValidation is the fixed-size state the Board's conditional activity
// read compares before it projects a page (component-persistence, Board
// activity validation reads). It carries no row, payload, or content.
type ActivityValidation struct {
	// EventCount and EventMaxID are the count and maximum ID of the
	// workspace's events. Events are append-only, so the count rises when any
	// event becomes visible, including a lower ID committed after a higher one.
	EventCount int64
	EventMaxID int64
	// Signature is FoldActivityValidationTables over one row per table of
	// ActivityValidationTables.
	Signature [32]byte
	// PassedDeadlines counts the workspace's work-order timestamp values at or
	// before the read's instant.
	PassedDeadlines int64
}

// ActivityValidationReader reads the Board validation state of the bound
// workspace at one instant.
type ActivityValidationReader interface {
	ReadActivityValidation(ctx context.Context, now time.Time) (ActivityValidation, error)
}

// ActivityInputs is the Board 200 path's one read of markers and orders.
type ActivityInputs struct {
	Markers []ActivityMarker
	// Orders is the same selection ListWorkOrdersForTasks returns for the
	// tasks, projected at the read's instant; the markers fold the same
	// selection.
	Orders []core.WorkOrder
	// ValidUntil is the earliest review-lease time after the instant in the
	// lifecycle events the marker folds read, or zero when there is none.
	ValidUntil time.Time
}

// ActivityInputReader reads one task set's markers and orders from one order
// selection at one instant.
type ActivityInputReader interface {
	ReadActivityInputs(ctx context.Context, taskIDs []string, now time.Time) (ActivityInputs, error)
}

// ActivityValidationScope says which rows of a table belong to a workspace.
type ActivityValidationScope int

const (
	// ActivityScopeWorkspace selects rows whose workspace_id is the workspace.
	ActivityScopeWorkspace ActivityValidationScope = iota
	// ActivityScopeTask selects rows whose task_id is a task of the workspace.
	ActivityScopeTask
	// ActivityScopeMember selects users bound to the workspace or assigned to
	// one of its tasks.
	ActivityScopeMember
)

// ActivityValidationTable is one table the Board's 200 path reads. Exclude
// names the columns no activity read selects; every other column is hashed.
type ActivityValidationTable struct {
	Name    string
	Scope   ActivityValidationScope
	Exclude []string
}

// ActivityValidationTables is the fixed, ordered table list every durable
// backend fingerprints (component-persistence, Board activity validation
// reads).
var ActivityValidationTables = []ActivityValidationTable{
	{Name: "tasks", Scope: ActivityScopeWorkspace, Exclude: []string{"body"}},
	{Name: "task_dependencies", Scope: ActivityScopeWorkspace},
	{Name: "task_dependency_additions", Scope: ActivityScopeWorkspace},
	{Name: "jobs", Scope: ActivityScopeTask},
	{Name: "work_orders", Scope: ActivityScopeWorkspace, Exclude: []string{"served_requirement_snapshot", "governance_snapshot"}},
	{Name: "task_specs", Scope: ActivityScopeTask, Exclude: []string{"content", "acceptance", "decomposition"}},
	{Name: "requirements", Scope: ActivityScopeWorkspace},
	{Name: "requirement_versions", Scope: ActivityScopeWorkspace, Exclude: []string{"content", "statements_json", "derived_from"}},
	{Name: "system_designs", Scope: ActivityScopeWorkspace},
	{Name: "system_design_versions", Scope: ActivityScopeWorkspace, Exclude: []string{"content", "governs"}},
	{Name: "decisions", Scope: ActivityScopeWorkspace, Exclude: []string{"context", "alternatives_rejected"}},
	{Name: "task_context_proposals", Scope: ActivityScopeWorkspace},
	{Name: "monitor_observations", Scope: ActivityScopeWorkspace},
	{Name: "workspace_role_bindings", Scope: ActivityScopeWorkspace},
	{Name: "users", Scope: ActivityScopeMember, Exclude: []string{"password_hash"}},
}

// ActivityTableSignature is one table's transferred row: its scoped row count
// and the decimal sum of its rows' 64-bit hashes.
type ActivityTableSignature struct {
	Table string
	Rows  int64
	Sum   string
}

// FoldActivityValidationTables folds the per-table rows into the 32-byte
// signature, in the order the backend returned them.
func FoldActivityValidationTables(tables []ActivityTableSignature) [32]byte {
	hash := sha256.New()
	var count [8]byte
	for _, table := range tables {
		hash.Write([]byte(table.Table))
		hash.Write([]byte{0})
		binary.BigEndian.PutUint64(count[:], uint64(table.Rows))
		hash.Write(count[:])
		hash.Write([]byte(table.Sum))
		hash.Write([]byte{0})
	}
	var out [32]byte
	copy(out[:], hash.Sum(nil))
	return out
}

// ActivityReviewLeaseHorizon returns the earliest lease_expires_at after now
// in the claim and renewal payloads of the review lifecycle events of open
// review orders, decoded as ReviewVerdictDiagnostics decodes them, or zero.
// ReviewVerdictDiagnostics turns a queued review order into an expired one at
// that instant without a row or event change, so the Board's validator must
// not outlive it.
func ActivityReviewLeaseHorizon(orders []core.WorkOrder, events []core.Event, now time.Time) time.Time {
	var horizon time.Time
	for _, order := range orders {
		if order.Stage != core.StageReview || (order.State != core.WorkOrderClaimed && order.State != core.WorkOrderQueued) {
			continue
		}
		for _, event := range events {
			if !reviewLifecycleEventMatches(order, event) {
				continue
			}
			var lease time.Time
			switch event.Kind {
			case "work_order.claimed":
				var claimed core.WorkOrder
				if json.Unmarshal(event.Payload, &claimed) == nil {
					lease = claimed.LeaseExpiresAt
				}
			case "work_order.lease_renewed":
				var payload struct {
					LeaseExpiresAt time.Time `json:"lease_expires_at"`
				}
				if json.Unmarshal(event.Payload, &payload) == nil {
					lease = payload.LeaseExpiresAt
				}
			}
			if lease.After(now) && (horizon.IsZero() || lease.Before(horizon)) {
				horizon = lease
			}
		}
	}
	return horizon
}

// ReadActivityInputs folds the volatile markers and orders from one order
// projection at now.
func (m *memory) ReadActivityInputs(ctx context.Context, taskIDs []string, now time.Time) (ActivityInputs, error) {
	if taskIDs != nil && len(taskIDs) == 0 {
		return ActivityInputs{Markers: []ActivityMarker{}, Orders: []core.WorkOrder{}}, nil
	}
	markers, orders, err := m.activityMarkersAt(ctx, now)
	if err != nil {
		return ActivityInputs{}, err
	}
	wanted := make(map[string]bool, len(taskIDs))
	for _, taskID := range taskIDs {
		wanted[taskID] = true
	}
	if taskIDs == nil {
		for _, marker := range markers {
			wanted[marker.TaskID] = true
		}
	}
	out := ActivityInputs{Markers: make([]ActivityMarker, 0, len(taskIDs)), Orders: []core.WorkOrder{}}
	for _, marker := range markers {
		if wanted[marker.TaskID] {
			out.Markers = append(out.Markers, marker)
		}
	}
	for _, order := range orders {
		if wanted[order.TaskID] {
			out.Orders = append(out.Orders, order)
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for taskID := range wanted {
		if lease := ActivityReviewLeaseHorizon(out.Orders, m.events[taskID], now); !lease.IsZero() && (out.ValidUntil.IsZero() || lease.Before(out.ValidUntil)) {
			out.ValidUntil = lease
		}
	}
	return out, nil
}

// workOrdersAt is ListWorkOrders projected at one instant.
func (m *memory) workOrdersAt(now time.Time) []core.WorkOrder {
	m.mu.RLock()
	defer m.mu.RUnlock()
	orders := make([]core.WorkOrder, 0, len(m.workOrders))
	for _, order := range m.workOrders {
		order.Assignee = m.tasks[order.TaskID].Assignee
		orders = append(orders, ProjectWorkOrderAt(order, now))
	}
	sort.Slice(orders, func(i, j int) bool {
		if orders[i].CreatedAt.Equal(orders[j].CreatedAt) {
			return orders[i].ID < orders[j].ID
		}
		return orders[i].CreatedAt.Before(orders[j].CreatedAt)
	})
	return orders
}

// ReadActivityValidation computes the volatile validation state under one
// read lock: the workspace's event count and maximum, a SHA-256 over the JSON
// of each map that backs a table of ActivityValidationTables (task bodies,
// plan and document content, and work-order snapshots cleared), and the
// passed work-order deadlines.
func (m *memory) ReadActivityValidation(ctx context.Context, now time.Time) (ActivityValidation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, scoped := WorkspaceFromContext(ctx)
	scoped = scoped && workspace != ""
	inWorkspace := func(ws string) bool { return !scoped || ws == workspace }
	taskIDs := make([]string, 0, len(m.tasks))
	for id, task := range m.tasks {
		if inWorkspace(task.Workspace) {
			taskIDs = append(taskIDs, id)
		}
	}
	sort.Strings(taskIDs)
	ownTask := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		ownTask[id] = true
	}
	var out ActivityValidation
	for taskID, events := range m.events {
		if taskID != "" && !ownTask[taskID] {
			continue
		}
		if taskID == "" && scoped {
			continue
		}
		for _, event := range events {
			out.EventCount++
			out.EventMaxID = max(out.EventMaxID, event.ID)
		}
	}
	hash := sha256.New()
	section := func(name string, value any) {
		body, _ := json.Marshal(value)
		hash.Write([]byte(name))
		hash.Write([]byte{0})
		hash.Write(body)
		hash.Write([]byte{0})
	}
	tasks := make([]core.Task, 0, len(taskIDs))
	dependencies := make([][]string, 0)
	jobs := make([][]core.Job, 0)
	specs := make([]core.SpecVersion, 0)
	for _, id := range taskIDs {
		task := m.tasks[id]
		task.Body = ""
		tasks = append(tasks, task)
		edges := make([]string, 0, len(m.dependencies[id]))
		for dependency := range m.dependencies[id] {
			edges = append(edges, dependency)
		}
		sort.Strings(edges)
		dependencies = append(dependencies, append([]string{id}, edges...))
		jobs = append(jobs, m.jobs[id])
		for _, spec := range m.specs[id] {
			spec.Content, spec.Acceptance, spec.Decomposition = "", nil, nil
			specs = append(specs, spec)
		}
	}
	section("tasks", tasks)
	section("task_dependencies", dependencies)
	section("jobs", jobs)
	section("task_specs", specs)
	orders := make([]core.WorkOrder, 0)
	for _, order := range m.workOrders {
		if !ownTask[order.TaskID] {
			continue
		}
		order.ServedRequirementSnapshot, order.GovernanceSnapshot = nil, nil
		orders = append(orders, order)
		out.PassedDeadlines += passedWorkOrderDeadlines(order, now)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].ID < orders[j].ID })
	section("work_orders", orders)
	section("requirements", scopedValues(m.requirements, inWorkspace, func(value core.Requirement) core.Requirement { return value }))
	section("requirement_versions", scopedValues(m.requirementVersions, inWorkspace, func(versions []core.RequirementVersion) []core.RequirementVersion {
		out := make([]core.RequirementVersion, len(versions))
		for i, version := range versions {
			version.Content, version.Statements, version.DerivedFrom = "", nil, nil
			out[i] = version
		}
		return out
	}))
	section("system_designs", scopedValues(m.systemDesigns, inWorkspace, func(value core.SystemDesign) core.SystemDesign { return value }))
	section("system_design_versions", scopedValues(m.systemDesignVersions, inWorkspace, func(versions []core.SystemDesignVersion) []core.SystemDesignVersion {
		out := make([]core.SystemDesignVersion, len(versions))
		for i, version := range versions {
			version.Content, version.Governs = "", nil
			out[i] = version
		}
		return out
	}))
	section("decisions", scopedValues(m.decisions, inWorkspace, func(value core.Decision) core.Decision {
		value.Context, value.AlternativesRejected = "", ""
		return value
	}))
	proposals := make([]core.TaskContextProposal, 0)
	for _, proposal := range m.taskContextProposals {
		if ownTask[proposal.TaskID] {
			proposals = append(proposals, proposal)
		}
	}
	sort.Slice(proposals, func(i, j int) bool {
		left, _ := json.Marshal(proposals[i])
		right, _ := json.Marshal(proposals[j])
		return string(left) < string(right)
	})
	section("task_context_proposals", proposals)
	observations := make([]string, 0)
	for key, record := range m.monitorObservations {
		if record.TaskID != "" && ownTask[record.TaskID] {
			body, _ := json.Marshal(record)
			observations = append(observations, key+"\x00"+string(body))
		}
	}
	sort.Strings(observations)
	section("monitor_observations", observations)
	section("workspace_members", scopedValues(m.workspaceMembers, inWorkspace, func(value bool) bool { return value }))
	section("workspace_role_bindings", scopedValues(m.workspaceMemberRoles, inWorkspace, func(value core.WorkspaceRole) core.WorkspaceRole { return value }))
	copy(out.Signature[:], hash.Sum(nil))
	return out, nil
}

// scopedValues returns a workspace's map entries ordered by key.
func scopedValues[V, W any](values map[memoryScopedKey]V, inWorkspace func(string) bool, clear func(V) W) []any {
	keys := make([]memoryScopedKey, 0, len(values))
	for key := range values {
		if inWorkspace(key.workspace) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].workspace != keys[j].workspace {
			return keys[i].workspace < keys[j].workspace
		}
		return keys[i].id < keys[j].id
	})
	out := make([]any, 0, 2*len(keys))
	for _, key := range keys {
		out = append(out, key.workspace+"\x00"+key.id, clear(values[key]))
	}
	return out
}

var timeType = reflect.TypeOf(time.Time{})

// passedWorkOrderDeadlines counts the order's non-zero time.Time fields at or
// before now, the volatile twin of the durable deadline row.
func passedWorkOrderDeadlines(order core.WorkOrder, now time.Time) int64 {
	value := reflect.ValueOf(order)
	var passed int64
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if field.Type() != timeType {
			continue
		}
		at := field.Interface().(time.Time)
		if !at.IsZero() && !at.After(now) {
			passed++
		}
	}
	return passed
}
