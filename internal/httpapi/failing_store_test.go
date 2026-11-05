package httpapi

import (
	"context"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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

// compile-time check that a test can drop the fixture in for a store.
var _ store.Store = (*failingStore)(nil)
