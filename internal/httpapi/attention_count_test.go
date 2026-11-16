package httpapi

import (
	"context"
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
	// The counting wrapper promotes only the store contract, so the server
	// resolves no hub from it; the cache subscribes to the memory hub the
	// backend publishes on.
	hub := memoryHub(t, counted.Store)
	server.attention.subscribe(hub)
	now := time.Now().UTC()
	server.attention.clock = func() time.Time { return now }
	read := func() {
		t.Helper()
		if _, err := server.attentionCount(ctx); err != nil {
			t.Fatal(err)
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
