package store

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
)

const (
	PlanningBundleFinalized = "planning_bundle.finalized"
	PlanningBundleRevised   = "planning_bundle.revised"
	PlanningBundleApproved  = "planning_bundle.approved"
	PlanningBundleRejected  = "planning_bundle.rejected"
)

// PlanningBundleConflictError classifies lifecycle and identity conflicts so
// HTTP callers can return a stable 409 without exposing adapter errors.
type PlanningBundleConflictError struct{ Message string }

func (e *PlanningBundleConflictError) Error() string { return e.Message }

type bundleContextVersions struct {
	Designs             map[string]int
	PendingRequirements map[string]int
	PendingDesigns      map[string]int
}

func pendingBundleDocuments(bundle core.PlanningBundle) (map[string]int, map[string]int) {
	requirements, designs := map[string]int{}, map[string]int{}
	for _, document := range bundle.Documents {
		switch document.Kind {
		case core.PlanningBundleRequirement:
			requirements[document.ID] = document.Version
		case core.PlanningBundleSystemDesign:
			designs[document.ID] = document.Version
		}
	}
	return requirements, designs
}

// PreparePlanningBundleRevision preserves generated task identities across an
// idempotent retry or an unapproved preview revision. The row is replaceable
// only while pending; planning_bundle.revised retains the append-only audit.
func PreparePlanningBundleRevision(existing core.PlanningBundle, incoming *core.PlanningBundle) {
	ids := map[string]string{}
	for _, member := range existing.Tasks {
		ids[member.MemberID] = member.CreatedTaskID
	}
	for i := range incoming.Tasks {
		if id := ids[strings.TrimSpace(incoming.Tasks[i].MemberID)]; id != "" {
			// A preview revision may change the task content or membership, but
			// an existing member keeps the durable identity already presented to
			// the operator. Never accept a caller-supplied replacement identity.
			incoming.Tasks[i].CreatedTaskID = id
		}
	}
}

func PlanningBundleContentEqual(left, right core.PlanningBundle) bool {
	strip := func(bundle core.PlanningBundle) core.PlanningBundle {
		bundle.Workspace, bundle.Status, bundle.CreatedBy, bundle.DecidedBy = "", "", "", ""
		bundle.CreatedAt, bundle.DecidedAt = time.Time{}, time.Time{}
		for i := range bundle.Documents {
			bundle.Documents[i].Title, bundle.Documents[i].Status = "", ""
		}
		return bundle
	}
	return reflect.DeepEqual(strip(left), strip(right))
}

func ValidatePlanningBundleShape(bundle *core.PlanningBundle) error {
	bundle.ID, bundle.SessionID, bundle.Title = strings.TrimSpace(bundle.ID), strings.TrimSpace(bundle.SessionID), strings.TrimSpace(bundle.Title)
	if bundle.ID == "" || bundle.SessionID == "" || bundle.Title == "" {
		return fmt.Errorf("planning bundle id, session_id, and title are required")
	}
	if len(bundle.Documents) == 0 || len(bundle.Tasks) == 0 {
		return fmt.Errorf("planning bundle requires document revisions and tasks")
	}
	docSeen := map[string]bool{}
	for i := range bundle.Documents {
		doc := &bundle.Documents[i]
		doc.ID = strings.TrimSpace(doc.ID)
		key := string(doc.Kind) + "\x00" + doc.ID + fmt.Sprintf("\x00%d", doc.Version)
		if doc.ID == "" || docSeen[key] {
			return fmt.Errorf("planning bundle contains an empty or duplicate document reference")
		}
		docSeen[key] = true
		switch doc.Kind {
		case core.PlanningBundleRequirement, core.PlanningBundleSystemDesign:
			if doc.Version <= 0 {
				return fmt.Errorf("%s %s requires a positive version", doc.Kind, doc.ID)
			}
		case core.PlanningBundleDecision:
			if doc.Version != 0 {
				return fmt.Errorf("decision %s does not use a version", doc.ID)
			}
		default:
			return fmt.Errorf("unknown planning bundle document kind %q", doc.Kind)
		}
	}
	members := map[string]int{}
	for i := range bundle.Tasks {
		member := &bundle.Tasks[i]
		member.MemberID, member.Title, member.Body, member.Repo = strings.TrimSpace(member.MemberID), strings.TrimSpace(member.Title), strings.TrimSpace(member.Body), strings.TrimSpace(member.Repo)
		if member.MemberID == "" || member.Title == "" || member.Body == "" || member.Repo == "" {
			return fmt.Errorf("bundle task member_id, title, body, and repo are required")
		}
		if _, exists := members[member.MemberID]; exists {
			return fmt.Errorf("duplicate bundle task member %s", member.MemberID)
		}
		members[member.MemberID] = i
		if member.CreatedTaskID == "" {
			member.CreatedTaskID = core.NewTaskID()
		}
		if member.BaseBranch == "" {
			member.BaseBranch = "main"
		}
		var err error
		member.Context.RequirementIDs, err = normalizeStrings("requirement", member.Context.RequirementIDs)
		if err != nil {
			return err
		}
		member.Context.DesignIDs, err = normalizeStrings("system design", member.Context.DesignIDs)
		if err != nil {
			return err
		}
	}
	indegree := map[string]int{}
	children := map[string][]string{}
	for i := range bundle.Tasks {
		member := &bundle.Tasks[i]
		deps, err := normalizeStrings("dependency member", member.DependsOn)
		if err != nil {
			return err
		}
		member.DependsOn = deps
		for _, dep := range deps {
			if dep == member.MemberID {
				return fmt.Errorf("bundle task %s cannot depend on itself", member.MemberID)
			}
			if _, exists := members[dep]; !exists {
				return fmt.Errorf("bundle task %s depends on unknown member %s", member.MemberID, dep)
			}
			indegree[member.MemberID]++
			children[dep] = append(children[dep], member.MemberID)
		}
	}
	queue := []string{}
	for id := range members {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, child := range children[id] {
			indegree[child]--
			if indegree[child] == 0 {
				queue = append(queue, child)
			}
		}
		delete(members, id)
	}
	if len(members) != 0 {
		return fmt.Errorf("planning bundle task dependencies contain a cycle")
	}
	return nil
}

func normalizeStrings(kind string, values []string) ([]string, error) {
	seen, out := map[string]bool{}, make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return nil, fmt.Errorf("%s references contain an empty or duplicate id", kind)
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func (m *memory) CreatePlanningBundle(ctx context.Context, bundle core.PlanningBundle) (core.PlanningBundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bundle.Workspace = workspaceOrDefault(ctx, bundle.Workspace)
	key := memoryScopedKey{workspace: bundle.Workspace, id: strings.TrimSpace(bundle.ID)}
	existing, exists := m.planningBundles[key]
	if exists {
		PreparePlanningBundleRevision(existing, &bundle)
	}
	if err := ValidatePlanningBundleShape(&bundle); err != nil {
		return core.PlanningBundle{}, err
	}
	if exists && PlanningBundleContentEqual(existing, bundle) {
		return existing, nil
	}
	if exists && existing.Status != core.PlanningBundlePending {
		return core.PlanningBundle{}, &PlanningBundleConflictError{Message: fmt.Sprintf("planning bundle %s is %s", bundle.ID, existing.Status)}
	}
	for i := range bundle.Documents {
		doc := &bundle.Documents[i]
		switch doc.Kind {
		case core.PlanningBundleRequirement:
			versions := m.requirementVersions[memoryScopedKey{workspace: bundle.Workspace, id: doc.ID}]
			if doc.Version > len(versions) || versions[doc.Version-1].Confirmed {
				return core.PlanningBundle{}, fmt.Errorf("requirement %s version %d is not pending", doc.ID, doc.Version)
			}
			doc.Title = m.requirements[memoryScopedKey{workspace: bundle.Workspace, id: doc.ID}].Title
			doc.Status = "pending"
		case core.PlanningBundleSystemDesign:
			versions := m.systemDesignVersions[memoryScopedKey{workspace: bundle.Workspace, id: doc.ID}]
			if doc.Version > len(versions) || versions[doc.Version-1].Confirmed || versions[doc.Version-1].Dismissed {
				return core.PlanningBundle{}, fmt.Errorf("system design %s version %d is not pending", doc.ID, doc.Version)
			}
			doc.Title = m.systemDesigns[memoryScopedKey{workspace: bundle.Workspace, id: doc.ID}].Title
			doc.Status = "pending"
		case core.PlanningBundleDecision:
			decision, ok := m.decisions[memoryScopedKey{workspace: bundle.Workspace, id: doc.ID}]
			if !ok || decision.Status != core.DecisionProposed {
				return core.PlanningBundle{}, fmt.Errorf("decision %s is not pending", doc.ID)
			}
			doc.Title, doc.Status = decision.Statement, "pending"
		}
	}
	if _, err := m.validatePlanningBundleContextsLocked(bundle); err != nil {
		return core.PlanningBundle{}, err
	}
	bundle.Status = core.PlanningBundlePending
	bundle.CreatedBy = ActorFromContext(ctx).ID
	if exists {
		bundle.CreatedAt, bundle.CreatedBy = existing.CreatedAt, existing.CreatedBy
	} else if bundle.CreatedAt.IsZero() {
		bundle.CreatedAt = time.Now().UTC()
	}
	m.planningBundles[key] = bundle
	kind, at := PlanningBundleFinalized, bundle.CreatedAt
	payload := map[string]any{"workspace_id": bundle.Workspace, "bundle_id": bundle.ID, "session_id": bundle.SessionID, "documents": bundle.Documents}
	if exists {
		kind, at = PlanningBundleRevised, time.Now().UTC()
		payload["previous_documents"] = existing.Documents
	}
	m.appendEventLocked(ctx, core.Event{Kind: kind, At: at, Payload: core.JSONPayload(payload)})
	return bundle, nil
}

func (m *memory) validatePlanningBundleContextsLocked(bundle core.PlanningBundle) (map[string]bundleContextVersions, error) {
	pendingRequirements, pendingDesigns := pendingBundleDocuments(bundle)
	result := map[string]bundleContextVersions{}
	for _, member := range bundle.Tasks {
		versions := bundleContextVersions{Designs: map[string]int{}, PendingRequirements: map[string]int{}, PendingDesigns: map[string]int{}}
		for _, id := range member.Context.RequirementIDs {
			if version := pendingRequirements[id]; version > 0 {
				versions.PendingRequirements[id] = version
				continue
			}
			if _, err := m.validateTaskContextLocked(bundle.Workspace, TaskContextInput{RequirementIDs: []string{id}}); err != nil {
				return nil, err
			}
		}
		for _, id := range member.Context.DesignIDs {
			if version := pendingDesigns[id]; version > 0 {
				versions.Designs[id], versions.PendingDesigns[id] = version, version
				continue
			}
			confirmed, err := m.validateTaskContextLocked(bundle.Workspace, TaskContextInput{DesignIDs: []string{id}})
			if err != nil {
				return nil, err
			}
			versions.Designs[id] = confirmed[id]
		}
		result[member.MemberID] = versions
	}
	return result, nil
}

func (m *memory) GetPlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	bundle, ok := m.planningBundles[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: id}]
	if !ok {
		return core.PlanningBundle{}, fmt.Errorf("planning bundle %s: %w", id, ErrNotFound)
	}
	return bundle, nil
}

func (m *memory) ListPlanningBundles(ctx context.Context) ([]core.PlanningBundle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := []core.PlanningBundle{}
	for key, bundle := range m.planningBundles {
		if key.workspace == workspace {
			out = append(out, bundle)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *memory) ApprovePlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: id}
	bundle, ok := m.planningBundles[key]
	if !ok {
		return core.PlanningBundle{}, fmt.Errorf("planning bundle %s: %w", id, ErrNotFound)
	}
	if bundle.Status == core.PlanningBundleApproved {
		return bundle, nil
	}
	if bundle.Status != core.PlanningBundlePending {
		return core.PlanningBundle{}, &PlanningBundleConflictError{Message: fmt.Sprintf("planning bundle %s is %s", id, bundle.Status)}
	}
	byMember := map[string]core.PlanningBundleTask{}
	for _, member := range bundle.Tasks {
		byMember[member.MemberID] = member
		if _, exists := m.tasks[member.CreatedTaskID]; exists {
			return core.PlanningBundle{}, &PlanningBundleConflictError{Message: fmt.Sprintf("bundle task identity %s already exists", member.CreatedTaskID)}
		}
	}
	contextVersions, err := m.validatePlanningBundleContextsLocked(bundle)
	if err != nil {
		return core.PlanningBundle{}, err
	}
	now := time.Now().UTC()
	for _, member := range bundle.Tasks {
		task := core.Task{ID: member.CreatedTaskID, Workspace: bundle.Workspace, Source: "planning_bundle:" + bundle.ID + ":" + member.MemberID, IntakeKey: "planning-bundle:" + bundle.ID + ":" + member.MemberID, Title: member.Title, Body: member.Body, Level: core.L2, SpecApproval: member.SpecApproval, MergeApproval: member.MergeApproval, PolicyVersion: 1, SetupName: member.SetupName, SetupContract: member.SetupContract, Repo: member.Repo, BaseBranch: member.BaseBranch, Branch: gitx.BranchName(member.CreatedTaskID), State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: now}
		m.tasks[task.ID] = task
		m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.created", At: now, Payload: core.JSONPayload(task)})
		versions := contextVersions[member.MemberID]
		for _, dep := range member.DependsOn {
			dependencyID := byMember[dep].CreatedTaskID
			if m.dependencies[task.ID] == nil {
				m.dependencies[task.ID] = map[string]struct{}{}
			}
			m.dependencies[task.ID][dependencyID] = struct{}{}
			m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: "task.dependency_added", At: now, Payload: core.JSONPayload(map[string]string{"task_id": task.ID, "depends_on_task_id": dependencyID})})
		}
		for _, reqID := range member.Context.RequirementIDs {
			payload := map[string]any{"id": reqID}
			if pending := versions.PendingRequirements[reqID]; pending > 0 {
				payload["pending_version"], payload["unconfirmed"] = pending, true
			}
			m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: TaskContextRequirementAdded, At: now, Payload: core.JSONPayload(payload)})
		}
		for _, designID := range member.Context.DesignIDs {
			payload := map[string]any{"id": designID, "version": versions.Designs[designID]}
			if versions.PendingDesigns[designID] > 0 {
				payload["unconfirmed"] = true
			}
			m.appendEventLocked(ctx, core.Event{TaskID: task.ID, Kind: TaskContextDesignAdded, At: now, Payload: core.JSONPayload(payload)})
		}
	}
	bundle.Status, bundle.DecidedAt, bundle.DecidedBy = core.PlanningBundleApproved, now, ActorFromContext(ctx).ID
	m.planningBundles[key] = bundle
	m.appendEventLocked(ctx, core.Event{Kind: PlanningBundleApproved, At: now, Payload: core.JSONPayload(map[string]any{"workspace_id": bundle.Workspace, "bundle_id": bundle.ID, "created_task_ids": createdBundleTaskIDs(bundle)})})
	return bundle, nil
}

func (m *memory) RejectPlanningBundle(ctx context.Context, id string) (core.PlanningBundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: id}
	bundle, ok := m.planningBundles[key]
	if !ok {
		return core.PlanningBundle{}, fmt.Errorf("planning bundle %s: %w", id, ErrNotFound)
	}
	if bundle.Status == core.PlanningBundleRejected {
		return bundle, nil
	}
	if bundle.Status != core.PlanningBundlePending {
		return core.PlanningBundle{}, &PlanningBundleConflictError{Message: fmt.Sprintf("planning bundle %s is %s", id, bundle.Status)}
	}
	bundle.Status, bundle.DecidedAt, bundle.DecidedBy = core.PlanningBundleRejected, time.Now().UTC(), ActorFromContext(ctx).ID
	m.planningBundles[key] = bundle
	m.appendEventLocked(ctx, core.Event{Kind: PlanningBundleRejected, At: bundle.DecidedAt, Payload: core.JSONPayload(map[string]any{"workspace_id": bundle.Workspace, "bundle_id": bundle.ID})})
	return bundle, nil
}

func createdBundleTaskIDs(bundle core.PlanningBundle) []string {
	ids := make([]string, len(bundle.Tasks))
	for i := range bundle.Tasks {
		ids[i] = bundle.Tasks[i].CreatedTaskID
	}
	return ids
}
