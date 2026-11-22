package postgres

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// TestActivityInputsBreakdownDiagnostic is an opt-in diagnostic for the task
// poll read cost. It opens an already seeded database named by
// CONVEYOR_TASK_POLL_DATABASE. The retired task poll read-cost test seeded
// and kept that database; the caller now supplies it. The diagnostic reads
// the activity inputs of the task-list page and of the inbox's open tasks
// once through ReadActivityInputs. It then reads the order, dependency,
// first fold, and claim backfill fold steps again, each under its own read
// label. PostgreSQL reads the task rows and their latest summaries in one
// statement inside listActivityMarkers, so that step is the whole read less
// the other steps. It changes no production label and skips unless the
// database is named.
func TestActivityInputsBreakdownDiagnostic(t *testing.T) {
	name := strings.TrimSpace(os.Getenv("CONVEYOR_TASK_POLL_DATABASE"))
	raw := strings.TrimSpace(os.Getenv("CONVEYOR_TEST_DATABASE_URL"))
	if name == "" || raw == "" {
		t.Skip("set CONVEYOR_TASK_POLL_DATABASE and CONVEYOR_TEST_DATABASE_URL to break down a seeded task poll database")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{2,62}_test$`).MatchString(name) {
		t.Fatalf("%q is not an owned test database name", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "task-poll")
	st, err := Open(ctx, u.String())
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
		if _, err := st.ReadActivityInputs(ctx, set.ids, now); err != nil {
			t.Fatal(err)
		}
		st.ReadStats(0)
		inputs, err := st.ReadActivityInputs(ctx, set.ids, now)
		if err != nil {
			t.Fatal(err)
		}
		whole := st.ReadStats(64)
		step := func(label string) context.Context { return dbmetrics.WithReadLabel(ctx, "step:"+label) }
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
		if _, err := st.ListDependencyBlockers(step("dependencies"), dependencyIDs); err != nil {
			t.Fatal(err)
		}
		in := store.MarkerFoldInput{Workspace: workspace(ctx), Now: now, ReviewTasks: []string{}}
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
		for _, marker := range inputs.Markers {
			in.Page = append(in.Page, marker.TaskID)
		}
		query, args := markerFoldQuery(in)
		folded, err := st.markerFoldScan(step("fold rows"), query, args)
		if err != nil {
			t.Fatal(err)
		}
		backfilled := len(store.MarkerClaimBackfill(folded, in.OpenReviewOrders)) > 0
		if backfilled {
			in.KeepAllClaims = true
			query, args = markerFoldQuery(in)
			if _, err := st.markerFoldScan(step("claim backfill"), query, args); err != nil {
				t.Fatal(err)
			}
		}
		steps := st.ReadStats(64)
		report, err := json.Marshal(map[string]any{"backend": "postgres", "set": set.name, "tasks": len(set.ids), "backfilled": backfilled, "read_activity_inputs": whole, "steps": steps})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("activity inputs breakdown: %s", report)
	}
}
