package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// ActivityMarkerEgressBaselineRevision is the commit whose activity-marker
// read the activity-markers baseline variants copy inline: origin/main
// 3062eb520, the read PR #1679 widened.
const ActivityMarkerEgressBaselineRevision = "inline-queries@3062eb520fab76502cd86507fd0342a9dfb57c3c"

// ActivityMarkerEgressOutcome digests what an activity-marker read served:
// every page task's markers and the page's review lease horizon.
func ActivityMarkerEgressOutcome(markers []store.ActivityMarker, horizon time.Time) (ReadEgressOutcome, error) {
	digest, err := ReadEgressDigest(struct {
		Markers []store.ActivityMarker `json:"markers"`
		Horizon time.Time              `json:"horizon"`
	}{markers, horizon.UTC()})
	return ReadEgressOutcome{Digest: digest, Successes: len(markers)}, err
}

// SeedActivityMarkerEgress stores a page of tasks whose implement claims,
// updates, and bounces have long histories with padded fields no fold reads.
// Half of the tasks have an open review order claimed as often; the others'
// review order is retired. Each task ends with a raised change request, a
// submitted implement head, a failed merge, and, for an open review order, a
// lease after now. Sizes come from CONVEYOR_READ_EGRESS_MARKER_TASKS,
// CONVEYOR_READ_EGRESS_MARKER_HISTORY, and CONVEYOR_READ_EGRESS_MARKER_PAD.
// Only public store commands write the fixture.
func SeedActivityMarkerEgress(t testing.TB, ctx context.Context, st store.Backend, workspace string, now time.Time) ([]string, map[string]int) {
	t.Helper()
	tasks, err := ReadEgressCount("CONVEYOR_READ_EGRESS_MARKER_TASKS", 12)
	if err != nil {
		t.Fatal(err)
	}
	history, err := ReadEgressCount("CONVEYOR_READ_EGRESS_MARKER_HISTORY", 40)
	if err != nil {
		t.Fatal(err)
	}
	padBytes, err := ReadEgressCount("CONVEYOR_READ_EGRESS_MARKER_PAD", 1024)
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("p", padBytes)
	ctx = store.WithActor(ctx, store.SystemActor())
	ids := make([]string, 0, tasks)
	events := 0
	for index := range tasks {
		taskID := core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{
			ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main",
			Branch: "conveyor/task-" + taskID, Title: fmt.Sprintf("Marker egress %d", index), Body: "marker egress",
			State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now.Add(-6 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		live := index%2 == 0
		newOrder := func(suffix string, stage core.Stage, retire bool) core.WorkOrder {
			id := taskID + "-" + suffix
			if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}); err != nil {
				t.Fatal(err)
			}
			order := core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: stage, QueueEnteredAt: now.Add(-5 * time.Hour), QueueDeadline: now.Add(48 * time.Hour)}
			if err := CreateWorkOrder(ctx, st, order); err != nil {
				t.Fatal(err)
			}
			if retire {
				order.State = core.WorkOrderTimedOut
				if err := UpdateWorkOrder(ctx, st, order, core.WorkOrderCmdTimeout); err != nil {
					t.Fatal(err)
				}
			}
			return order
		}
		implement := newOrder("implement-1", core.StageImplement, false)
		review := newOrder("review-1-seat-1", core.StageReview, !live)
		at := now.Add(-5 * time.Hour)
		appendEvent := func(jobID, kind string, payload any) {
			at = at.Add(time.Second)
			raw, ok := payload.(json.RawMessage)
			if !ok {
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				raw = encoded
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, JobID: jobID, Kind: kind, At: at.UTC(), Payload: raw}); err != nil {
				t.Fatal(err)
			}
			events++
		}
		claim := func(order core.WorkOrder, lease time.Time) json.RawMessage {
			order.State, order.LeaseExpiresAt, order.Progress = core.WorkOrderClaimed, lease, pad
			return store.WorkOrderEventPayload(order)
		}
		for step := range history {
			lease := now.Add(-4*time.Hour + time.Duration(step)*time.Second)
			appendEvent(implement.JobID, "work_order.claimed", claim(implement, lease))
			appendEvent("", "work_order.updated", map[string]any{"id": implement.ID, "stage": "implement", "state": "claimed", "head_sha": fmt.Sprintf("head-%d", step), "detail": pad})
			appendEvent("", "work_order.updated", map[string]any{"id": review.ID, "stage": "review", "state": "submitted", "head_sha": fmt.Sprintf("head-%d", step), "detail": pad})
			appendEvent("", "pipeline.bounced", map[string]any{"source": "operator", "feedback": pad})
			appendEvent(review.JobID, "work_order.claimed", claim(review, lease))
		}
		appendEvent(implement.JobID, "work_order.claimed", claim(implement, now.Add(-2*time.Hour)))
		appendEvent("", "work_order.updated", map[string]any{"id": implement.ID, "stage": "implement", "state": "submitted", "head_sha": "head-final", "detail": pad})
		reviewLease := now.Add(-time.Hour)
		if live {
			reviewLease = now.Add(time.Duration(30+index) * time.Minute)
		}
		appendEvent(review.JobID, "work_order.claimed", claim(review, reviewLease))
		appendEvent("", "merge.failed", map[string]any{"forge_error_category": "merge_conflict", "error": "merge conflict", "head_sha": "head-final", "detail": pad})
		appendEvent("", "pipeline.bounced", map[string]any{"source": store.UserRequestChangesSource, "feedback": pad})
		ids = append(ids, taskID)
	}
	return ids, map[string]int{"tasks": tasks, "history_steps": history, "events": events, "pad_bytes": padBytes, "open_review_tasks": (tasks + 1) / 2}
}
