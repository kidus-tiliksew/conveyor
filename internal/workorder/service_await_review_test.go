package workorder

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// Pushed await_review (component-work-orders, await_review;
// feature-quiet-idle-runtime, "Pushed task streams and `await_review`";
// req-quiet-idle-runtime AC-1.2, AC-2.3). The volatile backend publishes a
// task notice after each event; awaitReadStore counts the ListEvents reads
// that latestReviewResult makes and signals each one, so no case sleeps in
// place of a poll.

type awaitReadStore struct {
	store.Store
	mu        sync.Mutex
	reads     int
	readDone  chan int
	afterRead func(read int)
}

func (s *awaitReadStore) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	events, err := s.Store.ListEvents(ctx, taskID)
	s.mu.Lock()
	s.reads++
	read, hook := s.reads, s.afterRead
	s.mu.Unlock()
	if hook != nil {
		hook(read)
	}
	if s.readDone != nil {
		s.readDone <- read
	}
	return events, err
}

func (s *awaitReadStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

type awaitFixture struct {
	base  store.Store
	reads *awaitReadStore
	svc   *Service
	ctx   context.Context
	order core.WorkOrder
}

func newAwaitFixture(t *testing.T, taskID string) *awaitFixture {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "await-ws")
	base := store.NewMemory()
	for _, id := range []string{taskID, taskID + "-other"} {
		if err := base.CreateTask(ctx, core.Task{ID: id, Workspace: "await-ws", State: core.TaskRunning, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	job := core.Job{ID: taskID + "-implement", TaskID: taskID, Stage: core.StageImplement, State: core.JobDone, StartedAt: time.Now()}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(base).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	order, err := storetest.For(base).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "await-owner", ClientToken: "secret", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	order.State = core.WorkOrderSubmitted
	if err = storetest.For(base).UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	reads := &awaitReadStore{Store: base, readDone: make(chan int, 16)}
	svc := &Service{Store: reads}
	if svc.changeHub() != nil {
		t.Fatal("a wrapper without Changes() must not resolve a hub")
	}
	source, ok := base.(interface{ Changes() *changes.Hub })
	if !ok {
		t.Fatal("memory store exposes no change hub")
	}
	svc.Changes = source.Changes()
	return &awaitFixture{base: base, reads: reads, svc: svc, ctx: ctx, order: order}
}

func (f *awaitFixture) commit(t *testing.T, taskID, kind string) {
	t.Helper()
	payload := core.JSONPayload(map[string]any{})
	if kind == "review.round_completed" {
		payload = core.JSONPayload(map[string]any{"verdict": "approved", "review_round": 1})
	}
	if err := f.base.AppendEvent(f.ctx, core.Event{TaskID: taskID, Kind: kind, Payload: payload, At: time.Now().Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
}

type awaitOutcome struct {
	result map[string]any
	err    error
}

func (f *awaitFixture) start(ctx context.Context, wait time.Duration) chan awaitOutcome {
	done := make(chan awaitOutcome, 1)
	go func() {
		result, err := f.svc.AwaitReview(ctx, f.order.ID, "await-owner", wait)
		done <- awaitOutcome{result, err}
	}()
	return done
}

func waitRead(t *testing.T, f *awaitFixture, want int) {
	t.Helper()
	select {
	case got := <-f.reads.readDone:
		if got != want {
			t.Fatalf("read %d, want %d", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("read %d never happened", want)
	}
}

func waitOutcome(t *testing.T, done chan awaitOutcome) awaitOutcome {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("await_review did not return")
		return awaitOutcome{}
	}
}

func TestAwaitReviewReadsOnlyOnTaskNotices(t *testing.T) {
	f := newAwaitFixture(t, "await-push")
	done := f.start(f.ctx, time.Minute)
	waitRead(t, f, 1)
	// Another task's commit raises a notice the subscription does not match;
	// the next read is the one this task's unsettled commit wakes.
	f.commit(t, "await-push-other", "review.round_completed")
	f.commit(t, "await-push", "work_order.lease_renewed")
	waitRead(t, f, 2)
	f.commit(t, "await-push", "review.round_completed")
	waitRead(t, f, 3)
	outcome := waitOutcome(t, done)
	if outcome.err != nil || outcome.result["verdict"] != "approved" {
		t.Fatalf("result=%v err=%v", outcome.result, outcome.err)
	}
	if got := f.reads.count(); got != 3 {
		t.Fatalf("reads=%d, want 3", got)
	}
}

// A round that settles after the first read's snapshot, before the first
// wait, is the lost-wake window. Several commits in it coalesce into one
// notice; one more read sees the settled round.
func TestAwaitReviewSettleBetweenFirstReadAndWait(t *testing.T) {
	f := newAwaitFixture(t, "await-window")
	f.reads.afterRead = func(read int) {
		if read == 1 {
			for range 3 {
				f.commit(t, "await-window", "work_order.lease_renewed")
			}
			f.commit(t, "await-window", "review.round_completed")
		}
	}
	outcome := waitOutcome(t, f.start(f.ctx, time.Minute))
	if outcome.err != nil || outcome.result["verdict"] != "approved" {
		t.Fatalf("result=%v err=%v", outcome.result, outcome.err)
	}
	if got := f.reads.count(); got != 2 {
		t.Fatalf("reads=%d, want the first read and one coalesced wake", got)
	}
}

// Without a notice nothing reads until the wait elapses, which returns the
// pending-progress answer. The wait outlasts the retired one-second ticker.
func TestAwaitReviewIdleUntilDeadline(t *testing.T) {
	f := newAwaitFixture(t, "await-idle")
	f.reads.readDone = nil
	outcome := waitOutcome(t, f.start(f.ctx, 1200*time.Millisecond))
	if outcome.err != nil || outcome.result["status"] != "pending" {
		t.Fatalf("result=%v err=%v", outcome.result, outcome.err)
	}
	if got := f.reads.count(); got != 1 {
		t.Fatalf("an idle await_review read events %d times", got)
	}
}

func TestAwaitReviewWakesOnPeerTail(t *testing.T) {
	f := newAwaitFixture(t, "await-peer")
	f.svc.Changes = nil // a peer on a backend without notification hears no commit
	f.svc.PeerTail = time.Millisecond
	ctx, cancel := context.WithCancel(f.ctx)
	done := f.start(ctx, time.Minute)
	waitRead(t, f, 1)
	waitRead(t, f, 2)
	waitRead(t, f, 3)
	cancel()
	for {
		select {
		case <-f.reads.readDone:
		case outcome := <-done:
			if !errors.Is(outcome.err, context.Canceled) {
				t.Fatalf("err=%v", outcome.err)
			}
			return
		}
	}
}
