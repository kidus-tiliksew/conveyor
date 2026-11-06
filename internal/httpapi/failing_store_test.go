package httpapi

import (
	"context"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// failingStoreDriverText reads like an untranslated driver error with a
// credential in it. A test asserts that no response carries it and that the log
// line carries it only redacted.
const failingStoreDriverText = `pq: relation "tasks" does not exist: dial postgres://conveyor:ghp_abcdefghijklmnopqrstuvwxyz0123456789@db.internal/prod`

// failingStoreCredential is the credential inside failingStoreDriverText.
const failingStoreCredential = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

// failingStore fails the named store reads with err and passes every other call
// to the wrapped store, so a REST test reaches a route's failure branch through
// the route itself. A test that needs another method adds its override here
// (DEC-105(1)).
type failingStore struct {
	store.Store
	err     error
	methods map[string]bool
}

// newFailingStore wraps base and fails each named method with an untranslated
// driver error. A test sets err to inject a sentinel or typed store error.
func newFailingStore(base store.Store, methods ...string) *failingStore {
	failing := &failingStore{Store: base, err: errors.New(failingStoreDriverText), methods: map[string]bool{}}
	for _, method := range methods {
		failing.methods[method] = true
	}
	return failing
}

func (s *failingStore) failure(method string) error {
	if s.methods[method] {
		return s.err
	}
	return nil
}

func (s *failingStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	if err := s.failure("GetTask"); err != nil {
		return core.Task{}, err
	}
	return s.Store.GetTask(ctx, id)
}

func (s *failingStore) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	if err := s.failure("ListTasksFiltered"); err != nil {
		return nil, err
	}
	return s.Store.ListTasksFiltered(ctx, filter)
}

func (s *failingStore) ListTaskOperations(ctx context.Context, query store.TaskOperationsQuery) (store.TaskOperationsPage, error) {
	if err := s.failure("ListTaskOperations"); err != nil {
		return store.TaskOperationsPage{}, err
	}
	return s.Store.ListTaskOperations(ctx, query)
}

func (s *failingStore) ListCallerAttentionTaskPage(ctx context.Context, query store.CallerAttentionQuery) (store.TaskPage, error) {
	if err := s.failure("ListCallerAttentionTaskPage"); err != nil {
		return store.TaskPage{}, err
	}
	return s.Store.ListCallerAttentionTaskPage(ctx, query)
}

func (s *failingStore) ListJobPage(ctx context.Context, taskID string, page store.PageQuery) (store.JobPage, error) {
	if err := s.failure("ListJobPage"); err != nil {
		return store.JobPage{}, err
	}
	return s.Store.ListJobPage(ctx, taskID, page)
}

func (s *failingStore) ListEventPage(ctx context.Context, taskID string, page store.PageQuery) (store.EventPage, error) {
	if err := s.failure("ListEventPage"); err != nil {
		return store.EventPage{}, err
	}
	return s.Store.ListEventPage(ctx, taskID, page)
}

func (s *failingStore) ListRecoveryActPage(ctx context.Context, query store.RecoveryActQuery) (store.RecoveryActPage, error) {
	if err := s.failure("ListRecoveryActPage"); err != nil {
		return store.RecoveryActPage{}, err
	}
	return s.Store.ListRecoveryActPage(ctx, query)
}

func (s *failingStore) ListFactoryActs(ctx context.Context, since time.Time, limit int) ([]nextaction.Act, error) {
	if err := s.failure("ListFactoryActs"); err != nil {
		return nil, err
	}
	return s.Store.ListFactoryActs(ctx, since, limit)
}

func (s *failingStore) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	if err := s.failure("GetWorkOrder"); err != nil {
		return core.WorkOrder{}, err
	}
	return s.Store.GetWorkOrder(ctx, id)
}

func (s *failingStore) GetWorkOrderWithoutSnapshots(ctx context.Context, id string) (core.WorkOrder, error) {
	if err := s.failure("GetWorkOrderWithoutSnapshots"); err != nil {
		return core.WorkOrder{}, err
	}
	return s.Store.GetWorkOrderWithoutSnapshots(ctx, id)
}

func (s *failingStore) GetLatestJob(ctx context.Context, taskID string) (core.Job, bool, error) {
	if err := s.failure("GetLatestJob"); err != nil {
		return core.Job{}, false, err
	}
	return s.Store.GetLatestJob(ctx, taskID)
}

func (s *failingStore) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	if err := s.failure("ListEvents"); err != nil {
		return nil, err
	}
	return s.Store.ListEvents(ctx, taskID)
}

func (s *failingStore) CountEvents(ctx context.Context, taskID, kind string) (int, error) {
	if err := s.failure("CountEvents"); err != nil {
		return 0, err
	}
	return s.Store.CountEvents(ctx, taskID, kind)
}

func (s *failingStore) AppendEvent(ctx context.Context, event core.Event) error {
	if err := s.failure("AppendEvent"); err != nil {
		return err
	}
	return s.Store.AppendEvent(ctx, event)
}

func (s *failingStore) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	if err := s.failure("ListCompactEvents"); err != nil {
		return nil, err
	}
	return s.Store.ListCompactEvents(ctx, taskID)
}

func (s *failingStore) SetTaskAssigneeCommand(ctx context.Context, lease taskops.TaskLease, id, assignee string) (core.Task, error) {
	if err := s.failure("SetTaskAssigneeCommand"); err != nil {
		return core.Task{}, err
	}
	return s.Store.SetTaskAssigneeCommand(ctx, lease, id, assignee)
}

func (s *failingStore) AddTaskDependency(ctx context.Context, request store.DependencyAdditionRequest) (store.DependencyAdditionResult, error) {
	if err := s.failure("AddTaskDependency"); err != nil {
		return store.DependencyAdditionResult{}, err
	}
	return s.Store.AddTaskDependency(ctx, request)
}

func (s *failingStore) RemoveTaskDependency(ctx context.Context, request store.DependencyRemovalRequest) (store.DependencyRemovalResult, error) {
	if err := s.failure("RemoveTaskDependency"); err != nil {
		return store.DependencyRemovalResult{}, err
	}
	return s.Store.RemoveTaskDependency(ctx, request)
}

func (s *failingStore) RebuildLineage(ctx context.Context, request core.LineageRebuildRequest) (core.LineageRebuildResult, error) {
	if err := s.failure("RebuildLineage"); err != nil {
		return core.LineageRebuildResult{}, err
	}
	return s.Store.RebuildLineage(ctx, request)
}

func (s *failingStore) ApplyTaskCommand(ctx context.Context, lease taskops.TaskLease, id string, command taskops.Command) (core.Task, error) {
	if err := s.failure("ApplyTaskCommand"); err != nil {
		return core.Task{}, err
	}
	return s.Store.ApplyTaskCommand(ctx, lease, id, command)
}

func (s *failingStore) RequestChangesCommand(ctx context.Context, lease taskops.TaskLease, request taskops.RequestChanges) (core.Task, error) {
	if err := s.failure("RequestChangesCommand"); err != nil {
		return core.Task{}, err
	}
	return s.Store.RequestChangesCommand(ctx, lease, request)
}

func (s *failingStore) ChangeTaskPolicyCommand(ctx context.Context, lease taskops.TaskLease, request store.SetupChangeRequest) (store.SetupChangeResult, error) {
	if err := s.failure("ChangeTaskPolicyCommand"); err != nil {
		return store.SetupChangeResult{}, err
	}
	return s.Store.ChangeTaskPolicyCommand(ctx, lease, request)
}

func (s *failingStore) CreateTaskWithDependenciesAndContext(ctx context.Context, task core.Task, dependencyIDs []string, attached store.TaskContextInput) error {
	if err := s.failure("CreateTaskWithDependenciesAndContext"); err != nil {
		return err
	}
	return s.Store.CreateTaskWithDependenciesAndContext(ctx, task, dependencyIDs, attached)
}

// compile-time check that a test can drop the fixture in for a store.
var _ store.Store = (*failingStore)(nil)
