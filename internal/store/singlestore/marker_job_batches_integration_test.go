package singlestore

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestMarkerLifecycleJobBatchesIntegration proves the lifecycle read of a
// chunk whose open review orders have more jobs than one statement binds
// (markerReviewJobBatches). With the bound lists two wide, one task holds
// five open review orders and another two, so their seven jobs take four
// executions, each of the claims and renewals of its own jobs. The read must
// return every claim once and the kept renewal of every job, and the page's
// markers and review lease horizon must equal the Go folds over each task's
// complete event list. The earliest future lease is a later claim of the
// fifth seat's job, which only the third execution reads, and a renewal of
// the last job only the last execution reads.
func TestMarkerLifecycleJobBatchesIntegration(t *testing.T) {
	st := openOwnedStore(t, false)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "marker-jobs")
	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Marker jobs", Email: "owner@example.test", DisplayName: "Owner"}, "marker-jobs-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "marker-jobs", Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	width := markerTaskChunk
	markerTaskChunk = 2
	t.Cleanup(func() { markerTaskChunk = width })

	now := time.Now().UTC().Truncate(time.Second).Add(123456789 * time.Nanosecond)
	type seeded struct {
		taskID string
		orders []store.MarkerReviewOrder
	}
	// seed stores one task with seats open review orders, each on its own
	// job, each claimed once and renewed once. lease gives the renewal's
	// lease of each seat; the claim's lease is always later. reclaim, when
	// set, gives a seat a second claim after its renewal with that lease.
	seed := func(name string, seats int, lease func(seat int) time.Time, reclaim map[int]time.Time) seeded {
		t.Helper()
		taskID := core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{
			ID: taskID, Workspace: "marker-jobs", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID,
			Title: "Marker job batches " + name, Body: "marker job batches", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now.Add(-4 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		out := seeded{taskID: taskID}
		at := now.Add(-3 * time.Hour)
		for seat := range seats {
			id := fmt.Sprintf("%s-review-1-seat-%d", taskID, seat+1)
			if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobPending}); err != nil {
				t.Fatal(err)
			}
			order := core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: seat + 1, QueueEnteredAt: at, QueueDeadline: now.Add(24 * time.Hour)}
			if err := storetest.CreateWorkOrder(ctx, st, order); err != nil {
				t.Fatal(err)
			}
			order.State, order.LeaseExpiresAt = core.WorkOrderClaimed, now.Add(3*time.Hour)
			events := []core.Event{
				{Kind: "work_order.claimed", Payload: store.WorkOrderEventPayload(order)},
				{Kind: "work_order.lease_renewed", Payload: core.JSONPayload(map[string]any{"attempt_id": "attempt-" + id, "lease_expires_at": lease(seat)})},
			}
			if again, ok := reclaim[seat]; ok {
				order.LeaseExpiresAt = again
				events = append(events, core.Event{Kind: "work_order.claimed", Payload: store.WorkOrderEventPayload(order)})
			}
			for _, event := range events {
				at = at.Add(time.Second)
				event.TaskID, event.JobID, event.At = taskID, id, at
				if err := st.AppendEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			out.orders = append(out.orders, store.MarkerReviewOrder{TaskID: taskID, ID: id, JobID: id})
		}
		return out
	}
	// The later the seat, the earlier its renewal's lease, and the fifth
	// seat's second claim is earlier still: the horizon is that claim, on
	// the fifth job of seven, which the third job list holds.
	five := seed("five seats", 5, func(seat int) time.Time { return now.Add(time.Duration(60-seat) * time.Minute) }, map[int]time.Time{4: now.Add(50 * time.Minute)})
	// Leases already past: each order is an expired claim.
	two := seed("two seats", 2, func(seat int) time.Time { return now.Add(-time.Duration(10+seat) * time.Minute) }, nil)
	horizon := now.Add(50 * time.Minute)

	// The lifecycle read of the one chunk that holds both tasks.
	in := store.MarkerFoldInput{
		Workspace: "marker-jobs", Page: []string{five.taskID, two.taskID}, ReviewTasks: []string{five.taskID, two.taskID},
		LiveReviewTasks: []string{five.taskID, two.taskID}, OpenReviewOrders: append(append([]store.MarkerReviewOrder(nil), five.orders...), two.orders...), Now: now,
	}
	if batches := markerReviewJobBatches(in.OpenReviewOrders); len(batches) != 4 {
		t.Fatalf("seven jobs give %d job lists of width %d, want 4", len(batches), markerTaskChunk)
	}
	collector := dbmetrics.NewCollector()
	rows, err := st.markerLifecycleRows(dbmetrics.WithCollector(ctx, collector), in)
	if err != nil {
		t.Fatal(err)
	}
	if got := collector.Snapshot().Queries; got != 4 {
		t.Fatalf("the lifecycle read issued %d statements, want one per job list, 4", got)
	}
	claims, renewals := map[string]int{}, map[string]int{}
	seen := map[int64]bool{}
	for _, row := range rows {
		if seen[row.Event.ID] {
			t.Fatalf("event %d (%s of job %s) is returned twice", row.Event.ID, row.Event.Kind, row.Event.JobID)
		}
		seen[row.Event.ID] = true
		switch row.Event.Kind {
		case "work_order.claimed":
			claims[row.Event.JobID]++
		case "work_order.lease_renewed":
			renewals[row.Event.JobID]++
		default:
			t.Fatalf("the lifecycle read returned a %s event", row.Event.Kind)
		}
	}
	for _, order := range in.OpenReviewOrders {
		wantClaims := 1
		if order.JobID == five.orders[4].JobID {
			wantClaims = 2
		}
		if claims[order.JobID] != wantClaims || renewals[order.JobID] != 1 {
			t.Fatalf("job %s: %d claims and %d renewals, want %d claims and one renewal", order.JobID, claims[order.JobID], renewals[order.JobID], wantClaims)
		}
	}
	if len(rows) != 15 {
		t.Fatalf("the lifecycle read returned %d rows, want 8 claims and 7 renewals", len(rows))
	}

	// The whole read, with a third task that has no review order so the
	// page spans two chunks.
	plain := seed("no seats", 0, nil, nil)
	page := []string{five.taskID, two.taskID, plain.taskID}
	inputs, err := st.ReadActivityInputs(ctx, page, now)
	if err != nil {
		t.Fatal(err)
	}
	if !inputs.ValidUntil.Equal(horizon) {
		t.Fatalf("review lease horizon = %s, want the fifth seat's second claim %s", inputs.ValidUntil, horizon)
	}
	if len(inputs.Markers) != len(page) {
		t.Fatalf("ReadActivityInputs returned %d markers, want %d", len(inputs.Markers), len(page))
	}
	ordersByTask := map[string][]core.WorkOrder{}
	for _, order := range inputs.Orders {
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
	}
	var wantHorizon time.Time
	for _, marker := range inputs.Markers {
		events, err := st.ListEvents(ctx, marker.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		want := store.ReviewVerdictDiagnostics(ordersByTask[marker.TaskID], events, now)
		gotJSON, _ := json.Marshal(marker.ReviewDiagnostics)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("task %s review diagnostics = %s, want %s", marker.TaskID, gotJSON, wantJSON)
		}
		if lease := store.ActivityReviewLeaseHorizon(ordersByTask[marker.TaskID], events, now); !lease.IsZero() && (wantHorizon.IsZero() || lease.Before(wantHorizon)) {
			wantHorizon = lease
		}
		// The orders are still queued in the store, so an order whose
		// renewed lease is past is an expired claim and one whose lease
		// still runs is none.
		wantDiagnostics := 0
		if marker.TaskID == two.taskID {
			wantDiagnostics = 2
		}
		if len(want) != wantDiagnostics {
			t.Fatalf("task %s: the Go fold reads %d review diagnostics, want %d: %s", marker.TaskID, len(want), wantDiagnostics, wantJSON)
		}
	}
	if !wantHorizon.Equal(horizon) {
		t.Fatalf("the Go folds read the horizon %s, want %s", wantHorizon, horizon)
	}
}
