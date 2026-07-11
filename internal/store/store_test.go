package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestMemoryMutationsAppendAttributedEvents(t *testing.T) {
	t.Parallel()
	ctx := WithActor(context.Background(), Actor{ID: "operator-1", Role: core.ActorHuman})
	st := NewMemory()
	task := core.Task{ID: "task-1", State: core.TaskQueued, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTaskState(ctx, task.ID, core.TaskRunning); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(ctx, core.Intervention{
		TaskID: task.ID, Action: core.InterventionRedirect, ReasonCode: "spec-wrong", Comment: "clarify scope",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	for _, event := range events {
		if event.ActorID != "operator-1" || event.ActorRole != core.ActorHuman {
			t.Fatalf("event actor = %s/%s", event.ActorID, event.ActorRole)
		}
	}
}

func TestMemoryStoreMatchesProductionRelationships(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewMemory()
	for _, task := range []core.Task{
		{ID: "task-a", Branch: "conveyor/shared", State: core.TaskQueued},
		{ID: "task-b", Branch: "conveyor/other", State: core.TaskQueued},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateTask(ctx, core.Task{ID: "task-c", Branch: "conveyor/shared"}); err == nil {
		t.Fatal("duplicate task branch succeeded")
	}
	if err := st.CreateJob(ctx, core.Job{ID: "missing-job", TaskID: "missing"}); err == nil {
		t.Fatal("job for missing task succeeded")
	}
	job := core.Job{ID: "job-a", TaskID: "task-a", Stage: core.StageImplement}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: "task-b", JobID: job.ID, Kind: "wrong"}); err == nil {
		t.Fatal("cross-task job event succeeded")
	}
	if err := st.CreateIntervention(ctx, core.Intervention{
		TaskID: "task-b", JobID: job.ID, Action: core.InterventionApprove, ReasonCode: "approved",
	}); err == nil {
		t.Fatal("cross-task intervention succeeded")
	}
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: "task-a", Action: "invalid"}); err == nil {
		t.Fatal("invalid intervention succeeded")
	}
	if err := st.UpsertTranscript(ctx, core.Transcript{JobID: "missing", URI: "missing"}); err == nil {
		t.Fatal("transcript for missing job succeeded")
	}
	if err := st.UpsertTranscript(ctx, core.Transcript{JobID: job.ID, URI: "events.jsonl"}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Kind != "transcript.persisted" {
		t.Fatalf("transcript event missing: %+v", events)
	}
	if !strings.Contains(string(events[len(events)-1].Payload), "events.jsonl") {
		t.Fatalf("transcript payload = %s", events[len(events)-1].Payload)
	}
	after, err := st.ListEventsAfter(ctx, "task-a", events[len(events)-2].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Kind != "transcript.persisted" {
		t.Fatalf("incremental events = %+v", after)
	}
}
