package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// countingAttentionStore counts the attention counts a server performs.
type countingAttentionStore struct {
	store.Store
	mu     sync.Mutex
	counts int
}

func (s *countingAttentionStore) Changes() *changes.Hub {
	return s.Store.(interface{ Changes() *changes.Hub }).Changes()
}

func (s *countingAttentionStore) CountOperatorAttention(ctx context.Context) (int, error) {
	s.mu.Lock()
	s.counts++
	s.mu.Unlock()
	return s.Store.CountOperatorAttention(ctx)
}

func (s *countingAttentionStore) counted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts
}

// TestAttentionCountServedFromCache proves the badge total is served from the
// cache inside attentionCountReuse, counted again after a change notice for
// its workspace, and counted again once the window passes
// (req-operator-next-action AC-2.7).
func TestAttentionCountServedFromCache(t *testing.T) {
	counted := &countingAttentionStore{Store: store.NewMemory()}
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if err := counted.Store.CreateTask(ctx, core.Task{ID: "attention-task", Workspace: "demo", Repo: "conveyor", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(counted)
	server.Workspace = "demo"
	hub := counted.Changes()
	now := time.Now().UTC()
	server.attention.clock = func() time.Time { return now }
	read := func() {
		t.Helper()
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/pending-proposals?workspace_id=demo", nil)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("pending proposals: status %d: %s", recorder.Code, recorder.Body.String())
		}
	}

	read()
	now = now.Add(4 * time.Second)
	read()
	if counted.counted() != 1 {
		t.Fatalf("counts inside the window = %d, want 1 served from the cache", counted.counted())
	}

	// A notice for another workspace leaves this workspace's count cached.
	hub.Publish(changes.Notice{Workspace: "other", Kind: changes.Task, TaskID: "task-other"})
	read()
	if counted.counted() != 1 {
		t.Fatalf("a notice for another workspace dropped the cached count: counts=%d", counted.counted())
	}

	// A notice for this workspace drops it, so the next read counts again.
	hub.Publish(changes.Notice{Workspace: "demo", Kind: changes.Task, TaskID: "task-demo"})
	read()
	if counted.counted() != 2 {
		t.Fatalf("a change notice did not invalidate the count: counts=%d, want 2", counted.counted())
	}

	// The window bounds a count that outlives its notice.
	now = now.Add(attentionCountReuse)
	read()
	if counted.counted() != 3 {
		t.Fatalf("counts after the window = %d, want 3", counted.counted())
	}
}
