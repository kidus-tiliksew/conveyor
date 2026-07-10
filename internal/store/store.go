// Package store holds task/job state behind an interface. Phase 1 is
// in-memory ("logs only", spec §19); Phase 2 replaces the implementation
// with event-sourced Postgres (pgx + sqlc + River) without changing
// callers.
package store

import (
	"fmt"
	"sort"
	"sync"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type Store interface {
	CreateTask(t core.Task) error
	GetTask(id string) (core.Task, error)
	ListTasks() ([]core.Task, error)
	UpdateTaskState(id string, s core.TaskState) error

	CreateJob(j core.Job) error
	ListJobs(taskID string) ([]core.Job, error)
}

func NewMemory() Store {
	return &memory{tasks: map[string]core.Task{}, jobs: map[string][]core.Job{}}
}

type memory struct {
	mu    sync.RWMutex
	tasks map[string]core.Task
	jobs  map[string][]core.Job
}

func (m *memory) CreateTask(t core.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tasks[t.ID]; exists {
		return fmt.Errorf("task %s already exists", t.ID)
	}
	m.tasks[t.ID] = t
	return nil
}

func (m *memory) GetTask(id string) (core.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return core.Task{}, fmt.Errorf("task %s not found", id)
	}
	return t, nil
}

func (m *memory) ListTasks() ([]core.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]core.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *memory) UpdateTaskState(id string, s core.TaskState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.State = s
	m.tasks[id] = t
	return nil
}

func (m *memory) CreateJob(j core.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.TaskID] = append(m.jobs[j.TaskID], j)
	return nil
}

func (m *memory) ListJobs(taskID string) ([]core.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]core.Job(nil), m.jobs[taskID]...), nil
}
