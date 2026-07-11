package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/routing"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestCapacityWaitSnoozesWithoutParking(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "task-1", Workspace: "test", State: core.TaskQueued}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	worker := &dispatchTaskWorker{dispatcher: &Dispatcher{Store: st}}
	job := &river.Job[queueargs.DispatchTaskArgs]{
		JobRow: &rivertype.JobRow{ID: 1, Attempt: 3, MaxAttempts: 3},
		Args:   queueargs.DispatchTaskArgs{TaskID: task.ID},
	}
	err := worker.handleFailure(ctx, job, fmt.Errorf("route implement: %w", routing.ErrNoCapacity))
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != routing.RateLimitCooldown {
		t.Fatalf("error = %#v, want %s snooze", err, routing.RateLimitCooldown)
	}
	persisted, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != core.TaskQueued {
		t.Fatalf("task state = %s, want queued", persisted.State)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "dispatch.failed" {
			t.Fatalf("capacity wait recorded as failure: %+v", event)
		}
	}
	if events[len(events)-1].Kind != "dispatch.capacity_wait" {
		t.Fatalf("events = %+v", events)
	}
}
