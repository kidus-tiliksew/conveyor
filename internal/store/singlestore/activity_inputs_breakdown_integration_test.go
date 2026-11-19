package singlestore

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// TestActivityInputsBreakdownDiagnostic is an opt-in diagnostic for the
// task poll read cost (internal/httpapi TestTaskPollReadCostIntegration). It
// opens the database that test seeded and kept, named by
// CONVEYOR_TASK_POLL_DATABASE, and reads the activity inputs of the task-list
// page and of the inbox's open tasks once through ReadActivityInputs. It then
// reads the same inputs step by step, each step under its own read label:
// the task rows, the latest-summary read, the order read, the dependency
// read, the first fold read, the claim backfill fold read, and the wide reads. It reports the
// bytes, rows, and statements of each step. It changes no production label
// and skips unless the database is named.
func TestActivityInputsBreakdownDiagnostic(t *testing.T) {
	name := strings.TrimSpace(os.Getenv("CONVEYOR_TASK_POLL_DATABASE"))
	raw := strings.TrimSpace(os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL"))
	if name == "" || raw == "" {
		t.Skip("set CONVEYOR_TASK_POLL_DATABASE and CONVEYOR_TEST_SINGLESTORE_URL to break down a seeded task poll database")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{2,62}_test$`).MatchString(name) {
		t.Fatalf("%q is not an owned test database name", name)
	}
	cfg, err := connectionConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "task-poll")
	st, err := Open(ctx, cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2030, 1, 1, 0, 0, 5, 0, time.UTC)
	page, err := st.ListTaskPage(ctx, store.TaskOperationsQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	open, err := st.ListTasksFiltered(ctx, store.TaskFilter{Open: true})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(tasks []core.Task) []string {
		out := make([]string, len(tasks))
		for i, task := range tasks {
			out[i] = task.ID
		}
		return out
	}
	for _, set := range []struct {
		name string
		ids  []string
	}{{"task-list page", ids(page.Tasks)}, {"inbox open tasks", ids(open)}} {
		// Warm the store's one-time schema reads and plan compiles first.
		if _, err := st.ReadActivityInputs(ctx, set.ids, now); err != nil {
			t.Fatal(err)
		}
		st.ReadStats(0)
		if _, err := st.ReadActivityInputs(ctx, set.ids, now); err != nil {
			t.Fatal(err)
		}
		whole := st.ReadStats(64)
		step := func(label string) context.Context { return dbmetrics.WithReadLabel(ctx, "step:"+label) }
		rows, err := st.markerTaskRows(step("task rows"), set.ids)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.markerLatestSummaries(step("latest summaries"), rows); err != nil {
			t.Fatal(err)
		}
		orders, err := st.activityWorkOrdersAt(step("orders"), set.ids, now)
		if err != nil {
			t.Fatal(err)
		}
		var dependencyIDs []string
		seen := map[string]bool{}
		for _, order := range orders {
			if (order.Stage == core.StageImplement || order.Stage == core.StageVerify) && !seen[order.TaskID] {
				dependencyIDs = append(dependencyIDs, order.TaskID)
				seen[order.TaskID] = true
			}
		}
		if len(dependencyIDs) > 0 {
			if _, err := st.ListDependencyBlockers(step("dependencies"), dependencyIDs); err != nil {
				t.Fatal(err)
			}
		}
		in := breakdownFoldInput(documentWorkspace(ctx), rows, orders, now)
		backfills := 0
		for _, chunk := range markerTaskBatches(in.Page) {
			part := breakdownChunk(in, chunk)
			query, args := markerFoldQuery(part)
			folded, err := st.markerFoldScan(step("fold rows"), query, args)
			if err != nil {
				t.Fatal(err)
			}
			if len(store.MarkerClaimBackfill(folded, part.OpenReviewOrders)) > 0 {
				backfills++
				part.KeepAllClaims = true
				query, args = markerFoldQuery(part)
				if folded, err = st.markerFoldScan(step("claim backfill"), query, args); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.markerWideRows(step("wide rows"), part, folded); err != nil {
				t.Fatal(err)
			}
		}
		steps := st.ReadStats(64)
		report, err := json.Marshal(map[string]any{"backend": "singlestore", "set": set.name, "tasks": len(set.ids), "fold_chunks": len(markerTaskBatches(in.Page)), "backfilled_chunks": backfills, "read_activity_inputs": whole, "steps": steps})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("activity inputs breakdown: %s", report)
	}
}

// breakdownFoldInput derives the fold input listActivityMarkers derives
// from the task rows and the order selection.
func breakdownFoldInput(workspace string, rows []markerRow, orders []core.WorkOrder, now time.Time) store.MarkerFoldInput {
	in := store.MarkerFoldInput{Workspace: workspace, Now: now, ReviewTasks: []string{}}
	review, live := map[string]bool{}, map[string]bool{}
	for _, order := range orders {
		if order.Stage == core.StageReview && !review[order.TaskID] {
			in.ReviewTasks = append(in.ReviewTasks, order.TaskID)
			review[order.TaskID] = true
		}
		if order.Stage == core.StageReview && (order.State == core.WorkOrderClaimed || order.State == core.WorkOrderQueued) {
			in.OpenReviewOrders = append(in.OpenReviewOrders, store.MarkerReviewOrder{TaskID: order.TaskID, ID: order.ID, JobID: order.JobID})
			if !live[order.TaskID] {
				in.LiveReviewTasks = append(in.LiveReviewTasks, order.TaskID)
				live[order.TaskID] = true
			}
		}
	}
	for _, row := range rows {
		in.Page = append(in.Page, row.taskID)
	}
	return in
}

// breakdownChunk is the part of in that markerFoldRows reads for one chunk.
func breakdownChunk(in store.MarkerFoldInput, chunk []string) store.MarkerFoldInput {
	set := make(map[string]bool, len(chunk))
	for _, id := range chunk {
		set[id] = true
	}
	within := func(ids []string) []string {
		kept := make([]string, 0, len(ids))
		for _, id := range ids {
			if set[id] {
				kept = append(kept, id)
			}
		}
		return kept
	}
	part := store.MarkerFoldInput{Workspace: in.Workspace, Page: chunk, ReviewTasks: within(in.ReviewTasks), LiveReviewTasks: within(in.LiveReviewTasks), Now: in.Now}
	for _, order := range in.OpenReviewOrders {
		if set[order.TaskID] {
			part.OpenReviewOrders = append(part.OpenReviewOrders, order)
		}
	}
	return part
}
