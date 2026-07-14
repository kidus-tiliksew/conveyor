// Package store holds event-sourced control-plane state behind an interface.
// The memory implementation is for unit tests and explicit local development;
// Postgres is the durable Phase 2 implementation (spec §16, §19).
package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type Store interface {
	CreateTask(ctx context.Context, t core.Task) error
	GetTask(ctx context.Context, id string) (core.Task, error)
	GetTaskByIntakeKey(ctx context.Context, key string) (core.Task, bool, error)
	ListTasks(ctx context.Context) ([]core.Task, error)
	UpdateTaskState(ctx context.Context, id string, s core.TaskState) error
	SetTaskTransition(ctx context.Context, id string, state core.TaskState, nextStage, recoveryStage core.Stage) error
	UpdateTaskClassification(ctx context.Context, id, class string) error
	EnsureTaskEnqueued(ctx context.Context, id string) error

	CreateJob(ctx context.Context, j core.Job) error
	UpdateJob(ctx context.Context, j core.Job) error
	ListJobs(ctx context.Context, taskID string) ([]core.Job, error)
	GetLatestJob(ctx context.Context, taskID string) (core.Job, bool, error)

	AppendEvent(ctx context.Context, event core.Event) error
	ListEvents(ctx context.Context, taskID string) ([]core.Event, error)
	ListEventsAfter(ctx context.Context, taskID string, afterID int64) ([]core.Event, error)
	CountEvents(ctx context.Context, taskID, kind string) (int, error)
	ListActivityMarkers(ctx context.Context) ([]ActivityMarker, error)
	CreateIntervention(ctx context.Context, intervention core.Intervention) error
	ListInterventions(ctx context.Context, taskID string) ([]core.Intervention, error)
	UpsertTranscript(ctx context.Context, transcript core.Transcript) error
	GetTranscript(ctx context.Context, jobID string) (core.Transcript, error)
	CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error)
	GetLatestSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error)
	ApproveSpecVersion(ctx context.Context, taskID string, version int) error

	CreateWorkOrder(ctx context.Context, order core.WorkOrder) error
	GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error)
	ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error)
	ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error)
	ClaimWorkOrder(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error)
	UpdateWorkOrder(ctx context.Context, order core.WorkOrder) error
	QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error
	GetReviewPublication(ctx context.Context, reviewWorkOrderID string) (core.ReviewPublication, error)
	UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error
	ReconcileReviewPublications(ctx context.Context) (int, error)

	CreateFeature(ctx context.Context, feature core.Feature) error
	ListFeatures(ctx context.Context) ([]core.Feature, error)
	AssignTaskFeature(ctx context.Context, taskID, featureID string) error
	CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error)
	GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error)
	ListArtifacts(ctx context.Context) ([]core.Artifact, error)
}

// ActivityMarker contains only the changing fields needed by the activity
// index. Full job and event histories are loaded for one selected task, not
// once per task on every dashboard refresh.
type ActivityMarker struct {
	TaskID      string
	LatestStage core.Stage
	LastEventAt time.Time
}

func NewMemory() Store {
	return &memory{
		tasks:         map[string]core.Task{},
		jobs:          map[string][]core.Job{},
		events:        map[string][]core.Event{},
		interventions: map[string][]core.Intervention{},
		transcripts:   map[string]core.Transcript{},
		specs:         map[string][]core.SpecVersion{},
		workOrders:    map[string]core.WorkOrder{},
		publications:  map[string]core.ReviewPublication{},
		features:      map[string]core.Feature{},
		artifacts:     map[string]memoryArtifact{},
	}
}

type memoryArtifact struct {
	meta    core.Artifact
	content []byte
}

type memory struct {
	mu            sync.RWMutex
	tasks         map[string]core.Task
	jobs          map[string][]core.Job
	events        map[string][]core.Event
	interventions map[string][]core.Intervention
	transcripts   map[string]core.Transcript
	specs         map[string][]core.SpecVersion
	workOrders    map[string]core.WorkOrder
	publications  map[string]core.ReviewPublication
	features      map[string]core.Feature
	artifacts     map[string]memoryArtifact
	nextEventID   int64
	nextReviewID  int64
}

func (m *memory) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.publications[publication.ReviewWorkOrderID]; ok {
		return nil
	}
	now := time.Now().UTC()
	publication.State, publication.CreatedAt, publication.UpdatedAt = core.ReviewPublicationQueued, now, now
	m.publications[publication.ReviewWorkOrderID] = publication
	m.appendEventLocked(ctx, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: "review.publication_queued", Payload: core.JSONPayload(publication)})
	return nil
}

func (m *memory) GetReviewPublication(_ context.Context, id string) (core.ReviewPublication, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	publication, ok := m.publications[id]
	if !ok {
		return core.ReviewPublication{}, fmt.Errorf("review publication %s not found", id)
	}
	return publication, nil
}

func (m *memory) UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.publications[publication.ReviewWorkOrderID]; !ok {
		return fmt.Errorf("review publication %s not found", publication.ReviewWorkOrderID)
	}
	publication.UpdatedAt = time.Now().UTC()
	m.publications[publication.ReviewWorkOrderID] = publication
	kind := "review.publication_retry"
	if publication.State == core.ReviewPublicationPublished {
		kind = "review.publication_published"
	} else if publication.State == core.ReviewPublicationFailed {
		kind = "review.publication_failed"
	}
	m.appendEventLocked(ctx, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: kind, Payload: core.JSONPayload(publication)})
	return nil
}

func (m *memory) ReconcileReviewPublications(ctx context.Context) (int, error) {
	m.mu.RLock()
	var missing []core.ReviewPublication
	seen := map[string]bool{}
	for taskID, events := range m.events {
		for _, event := range events {
			if event.Kind != "review.completed" {
				continue
			}
			publication, ok := reviewPublicationFromEvent(taskID, event.JobID, event.Payload)
			if ok && !seen[publication.ReviewWorkOrderID] {
				if _, exists := m.publications[publication.ReviewWorkOrderID]; !exists {
					missing = append(missing, publication)
					seen[publication.ReviewWorkOrderID] = true
				}
			}
		}
	}
	m.mu.RUnlock()
	created := 0
	for _, publication := range missing {
		if err := m.QueueReviewPublication(ctx, publication); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

func reviewPublicationFromEvent(taskID, jobID string, payload []byte) (core.ReviewPublication, bool) {
	var publication core.ReviewPublication
	if json.Unmarshal(payload, &publication) != nil || publication.ReviewWorkOrderID == "" {
		return core.ReviewPublication{}, false
	}
	publication.TaskID = taskID
	publication.JobID = jobID
	publication.State = core.ReviewPublicationQueued
	return publication, true
}

func (m *memory) CreateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[order.TaskID]; !ok {
		return fmt.Errorf("task %s not found", order.TaskID)
	}
	if _, exists := m.workOrders[order.ID]; exists {
		return fmt.Errorf("work order %s already exists", order.ID)
	}
	now := time.Now().UTC()
	if order.CreatedAt.IsZero() {
		order.CreatedAt = now
	}
	order.UpdatedAt = now
	if order.State == "" {
		order.State = core.WorkOrderQueued
	}
	m.workOrders[order.ID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.created", Payload: core.JSONPayload(order)})
	return nil
}

func (m *memory) GetWorkOrder(_ context.Context, id string) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	if order.State == core.WorkOrderClaimed && !order.LeaseExpiresAt.After(time.Now()) {
		order.State, order.ClaimantID, order.SessionID, order.Agent, order.Model = core.WorkOrderQueued, "", "", "", ""
		order.ClientTokenHash = ""
		order.LeaseExpiresAt = time.Time{}
		m.workOrders[id] = order
	}
	return order, nil
}

func (m *memory) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	orders := make([]core.WorkOrder, 0, len(m.workOrders))
	for id, order := range m.workOrders {
		if order.State == core.WorkOrderClaimed && !order.LeaseExpiresAt.After(now) {
			order.State, order.ClaimantID, order.SessionID, order.Agent, order.Model = core.WorkOrderQueued, "", "", "", ""
			order.ClientTokenHash = ""
			order.LeaseExpiresAt = time.Time{}
			m.workOrders[id] = order
		}
		orders = append(orders, order)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt.Before(orders[j].CreatedAt) })
	return orders, nil
}

func (m *memory) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	orders, _ := m.ListWorkOrders(ctx)
	out := orders[:0]
	for _, order := range orders {
		if order.TaskID == taskID {
			out = append(out, order)
		}
	}
	return out, nil
}

func (m *memory) ClaimWorkOrder(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.workOrders[id]
	if !ok {
		return core.WorkOrder{}, fmt.Errorf("work order %s not found", id)
	}
	now := time.Now().UTC()
	if order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(now) {
		return core.WorkOrder{}, fmt.Errorf("work order %s is already claimed", id)
	}
	if order.State != core.WorkOrderQueued && order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not claimable", id)
	}
	if order.Stage == core.StageReview {
		for _, candidate := range m.workOrders {
			if candidate.TaskID == order.TaskID && candidate.Stage == core.StageImplement &&
				((claim.SessionID != "" && candidate.SessionID == claim.SessionID) || (claim.ClientToken != "" && candidate.ClientTokenHash == tokenHash(claim.ClientToken))) {
				return core.WorkOrder{}, fmt.Errorf("self-review forbidden: use a fresh agent session")
			}
		}
	}
	lease := claim.Lease
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	order.State, order.ClaimantID, order.SessionID = core.WorkOrderClaimed, claim.ClaimantID, claim.SessionID
	order.Agent, order.Model, order.LeaseExpiresAt, order.UpdatedAt = claim.Agent, claim.Model, now.Add(lease), now
	if claim.ClientToken != "" {
		order.ClientTokenHash = tokenHash(claim.ClientToken)
	}
	m.workOrders[id] = order
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(order)})
	return order, nil
}

func tokenHash(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }

func (m *memory) UpdateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.workOrders[order.ID]; !ok {
		return fmt.Errorf("work order %s not found", order.ID)
	}
	order.UpdatedAt = time.Now().UTC()
	m.workOrders[order.ID] = order
	m.appendEventLocked(ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(order)})
	return nil
}

func (m *memory) CreateFeature(ctx context.Context, feature core.Feature) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.features[feature.ID]; ok {
		return fmt.Errorf("feature %s already exists", feature.ID)
	}
	if feature.ParentID != "" {
		if _, ok := m.features[feature.ParentID]; !ok {
			return fmt.Errorf("parent feature %s not found", feature.ParentID)
		}
	}
	if feature.CreatedAt.IsZero() {
		feature.CreatedAt = time.Now().UTC()
	}
	m.features[feature.ID] = feature
	return nil
}

func (m *memory) ListFeatures(_ context.Context) ([]core.Feature, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]core.Feature, 0, len(m.features))
	for _, feature := range m.features {
		out = append(out, feature)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memory) AssignTaskFeature(ctx context.Context, taskID, featureID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	if featureID != "" {
		if _, ok := m.features[featureID]; !ok {
			return fmt.Errorf("feature %s not found", featureID)
		}
	}
	task.FeatureID = featureID
	m.tasks[taskID] = task
	m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "task.feature_assigned", Payload: core.JSONPayload(map[string]string{"feature_id": featureID})})
	return nil
}

func (m *memory) CreateArtifact(_ context.Context, artifact core.Artifact, content []byte) (core.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	artifact.ID = fmt.Sprintf("%x", sha256.Sum256(content))
	artifact.SizeBytes = int64(len(content))
	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}
	if existing, ok := m.artifacts[artifact.ID]; ok {
		return existing.meta, nil
	}
	m.artifacts[artifact.ID] = memoryArtifact{meta: artifact, content: append([]byte(nil), content...)}
	return artifact, nil
}

func (m *memory) GetArtifact(_ context.Context, id string) (core.Artifact, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	artifact, ok := m.artifacts[id]
	if !ok {
		return core.Artifact{}, nil, fmt.Errorf("artifact %s not found", id)
	}
	return artifact.meta, append([]byte(nil), artifact.content...), nil
}

func (m *memory) ListArtifacts(_ context.Context) ([]core.Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]core.Artifact, 0, len(m.artifacts))
	for _, artifact := range m.artifacts {
		out = append(out, artifact.meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *memory) CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[spec.TaskID]; !ok {
		return core.SpecVersion{}, fmt.Errorf("task %s not found", spec.TaskID)
	}
	spec.Version = len(m.specs[spec.TaskID]) + 1
	// Approval is a separate exact-version gate; callers cannot smuggle an
	// approved artifact through creation. This matches the Postgres contract.
	spec.Approved = false
	spec.ApprovedAt = time.Time{}
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	m.specs[spec.TaskID] = append(m.specs[spec.TaskID], spec)
	m.appendEventLocked(ctx, core.Event{TaskID: spec.TaskID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": spec.Version, "acceptance_count": spec.AcceptanceCount})})
	return spec, nil
}

func (m *memory) GetLatestSpecVersion(_ context.Context, taskID string) (core.SpecVersion, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	versions := m.specs[taskID]
	if len(versions) == 0 {
		return core.SpecVersion{}, false, nil
	}
	return versions[len(versions)-1], true, nil
}

func (m *memory) ApproveSpecVersion(ctx context.Context, taskID string, version int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := m.specs[taskID]
	if len(versions) == 0 || versions[len(versions)-1].Version != version {
		return fmt.Errorf("spec version %d for task %s not found or superseded", version, taskID)
	}
	versions[len(versions)-1].Approved = true
	versions[len(versions)-1].ApprovedAt = time.Now().UTC()
	m.specs[taskID] = versions
	m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})})
	return nil
}

func (m *memory) CreateTask(ctx context.Context, t core.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tasks[t.ID]; exists {
		return fmt.Errorf("task %s already exists", t.ID)
	}
	for _, existing := range m.tasks {
		if existing.Branch != "" && existing.Branch == t.Branch {
			return fmt.Errorf("branch %s already belongs to task %s", t.Branch, existing.ID)
		}
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.NextStage == "" && (t.State == core.TaskQueued || t.State == core.TaskClaiming) {
		t.NextStage = core.InitialStage(t.Level)
	}
	m.tasks[t.ID] = t
	m.appendEventLocked(ctx, core.Event{TaskID: t.ID, Kind: "task.created", Payload: core.JSONPayload(t), At: t.CreatedAt})
	return nil
}

func (m *memory) GetTask(_ context.Context, id string) (core.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return core.Task{}, fmt.Errorf("task %s not found", id)
	}
	return t, nil
}

func (m *memory) GetTaskByIntakeKey(_ context.Context, key string) (core.Task, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, task := range m.tasks {
		if key != "" && task.IntakeKey == key {
			return task, true, nil
		}
	}
	return core.Task{}, false, nil
}

func (m *memory) ListTasks(_ context.Context) ([]core.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]core.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (m *memory) UpdateTaskState(ctx context.Context, id string, s core.TaskState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	from := t.State
	t.State = s
	m.tasks[id] = t
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": from, "to": s})})
	return nil
}

func (m *memory) SetTaskTransition(ctx context.Context, id string, state core.TaskState, nextStage, recoveryStage core.Stage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	fromState, fromStage := task.State, task.NextStage
	task.State = state
	task.NextStage = nextStage
	task.RecoveryStage = recoveryStage
	m.tasks[id] = task
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state})})
	m.appendEventLocked(ctx, core.Event{TaskID: id, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{"from_stage": fromStage, "next_stage": nextStage, "recovery_stage": recoveryStage, "state": state})})
	return nil
}

func (m *memory) UpdateTaskClassification(ctx context.Context, id, class string) error {
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[j.TaskID]; !ok {
		return fmt.Errorf("task %s not found", j.TaskID)
	}
	if _, _, ok := m.findJobLocked(j.ID); ok {
		return fmt.Errorf("job %s already exists", j.ID)
	}
	if j.StartedAt.IsZero() {
		j.StartedAt = time.Now().UTC()
	}
	m.jobs[j.TaskID] = append(m.jobs[j.TaskID], j)
	m.appendEventLocked(ctx, core.Event{TaskID: j.TaskID, JobID: j.ID, Kind: "job.created", Payload: core.JSONPayload(j), At: j.StartedAt})
	return nil
}

func (m *memory) UpdateJob(ctx context.Context, j core.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs := m.jobs[j.TaskID]
	for i := range jobs {
		if jobs[i].ID == j.ID {
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

func (m *memory) AppendEvent(ctx context.Context, event core.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[event.TaskID]; !ok {
		return fmt.Errorf("task %s not found", event.TaskID)
	}
	if event.JobID != "" {
		job, _, ok := m.findJobLocked(event.JobID)
		if !ok || job.TaskID != event.TaskID {
			return fmt.Errorf("job %s does not belong to task %s", event.JobID, event.TaskID)
		}
	}
	m.appendEventLocked(ctx, event)
	return nil
}

func (m *memory) ListEvents(_ context.Context, taskID string) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	events := append([]core.Event(nil), m.events[taskID]...)
	sort.Slice(events, func(i, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].ID < events[j].ID
		}
		return events[i].At.Before(events[j].At)
	})
	return events, nil
}

func (m *memory) ListEventsAfter(_ context.Context, taskID string, afterID int64) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	events := m.events[taskID]
	first := sort.Search(len(events), func(i int) bool { return events[i].ID > afterID })
	return append([]core.Event(nil), events[first:]...), nil
}

func (m *memory) CountEvents(_ context.Context, taskID, kind string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, event := range m.events[taskID] {
		if event.Kind == kind {
			count++
		}
	}
	return count, nil
}

func (m *memory) ListActivityMarkers(_ context.Context) ([]ActivityMarker, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	markers := make([]ActivityMarker, 0, len(m.tasks))
	for id, task := range m.tasks {
		marker := ActivityMarker{TaskID: id, LastEventAt: task.CreatedAt}
		if jobs := append([]core.Job(nil), m.jobs[id]...); len(jobs) != 0 {
			sortJobs(jobs)
			marker.LatestStage = jobs[len(jobs)-1].Stage
		}
		if events := m.events[id]; len(events) != 0 {
			marker.LastEventAt = events[len(events)-1].At
		}
		markers = append(markers, marker)
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].TaskID < markers[j].TaskID })
	return markers, nil
}

func (m *memory) CreateIntervention(ctx context.Context, intervention core.Intervention) error {
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

func (m *memory) UpsertTranscript(ctx context.Context, transcript core.Transcript) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, _, ok := m.findJobLocked(transcript.JobID)
	if !ok {
		return fmt.Errorf("job %s not found", transcript.JobID)
	}
	if transcript.CreatedAt.IsZero() {
		transcript.CreatedAt = time.Now().UTC()
	}
	m.transcripts[transcript.JobID] = transcript
	m.appendEventLocked(ctx, core.Event{
		TaskID: job.TaskID, JobID: job.ID, Kind: "transcript.persisted",
		Payload: core.JSONPayload(map[string]any{"uri": transcript.URI, "redaction_stats": transcript.RedactionStats}),
	})
	return nil
}

func (m *memory) GetTranscript(_ context.Context, jobID string) (core.Transcript, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	transcript, ok := m.transcripts[jobID]
	if !ok {
		return core.Transcript{}, fmt.Errorf("transcript for job %s not found", jobID)
	}
	return transcript, nil
}

func (m *memory) appendEventLocked(ctx context.Context, event core.Event) {
	actor := ActorFromContext(ctx)
	if event.ActorID == "" {
		event.ActorID = actor.ID
	}
	if event.ActorRole == "" {
		event.ActorRole = actor.Role
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Payload == nil {
		event.Payload = core.JSONPayload(struct{}{})
	}
	m.nextEventID++
	event.ID = m.nextEventID
	m.events[event.TaskID] = append(m.events[event.TaskID], event)
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
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].StartedAt.Equal(jobs[j].StartedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].StartedAt.Before(jobs[j].StartedAt)
	})
}
