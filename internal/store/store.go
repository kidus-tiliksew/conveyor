// Package store holds event-sourced control-plane state behind an interface.
// The memory implementation is for unit tests and explicit local development;
// Postgres is the durable Phase 2 implementation (spec §16, §19).
package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type Store interface {
	CreateTask(ctx context.Context, t core.Task) error
	GetTask(ctx context.Context, id string) (core.Task, error)
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
	}
}

type memory struct {
	mu            sync.RWMutex
	tasks         map[string]core.Task
	jobs          map[string][]core.Job
	events        map[string][]core.Event
	interventions map[string][]core.Intervention
	transcripts   map[string]core.Transcript
	specs         map[string][]core.SpecVersion
	nextEventID   int64
	nextReviewID  int64
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
