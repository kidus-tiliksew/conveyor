package storetest

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runListPages pins the page reads behind the REST list routes: each backend
// serves the unpaged read's order one page at a time with a task-wide or
// query-wide total, states the total past the end, refuses an unrepresentable
// page, and keeps another workspace's rows out (DEC-105(4); DEC-38;
// component-http-api invariants 13, 19).
func runListPages(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC().Truncate(time.Millisecond)
	task := core.Task{ID: "list-pages-" + core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", Branch: "list-pages-" + core.NewTaskID(), State: core.TaskRunning, CreatedAt: now}
	requireOK(t, st.CreateTask(ctx, task))
	other := core.Task{ID: "list-pages-" + core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", Branch: "list-pages-" + core.NewTaskID(), State: core.TaskRunning, CreatedAt: now}
	requireOK(t, st.CreateTask(ctx, other))

	t.Run("jobs", func(t *testing.T) {
		for i := range 3 {
			requireOK(t, st.CreateJob(ctx, core.Job{ID: fmt.Sprintf("%s-job-%d", task.ID, i), TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: now.Add(time.Duration(i) * time.Second)}))
		}
		requireOK(t, st.CreateJob(ctx, core.Job{ID: other.ID + "-job", TaskID: other.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: now}))
		whole, err := st.ListJobs(ctx, task.ID)
		requireOK(t, err)
		if len(whole) != 3 {
			t.Fatalf("whole jobs = %+v", whole)
		}
		var paged []core.Job
		for offset := range 3 {
			page, err := st.ListJobPage(ctx, task.ID, store.PageQuery{Limit: 1, Offset: offset})
			requireOK(t, err)
			if page.Total != 3 || len(page.Jobs) != 1 {
				t.Fatalf("offset %d page = %+v", offset, page)
			}
			paged = append(paged, page.Jobs...)
		}
		if !reflect.DeepEqual(jobIDs(paged), jobIDs(whole)) {
			t.Fatalf("paged order %v, whole order %v", jobIDs(paged), jobIDs(whole))
		}
		past, err := st.ListJobPage(ctx, task.ID, store.PageQuery{Limit: 2, Offset: store.MaxTaskOperationsOffset})
		requireOK(t, err)
		if past.Total != 3 || len(past.Jobs) != 0 {
			t.Fatalf("past-the-end page = %+v", past)
		}
		absent, err := st.ListJobPage(ctx, "absent-"+task.ID, store.PageQuery{Limit: 2})
		requireOK(t, err)
		if absent.Total != 0 || len(absent.Jobs) != 0 {
			t.Fatalf("absent task page = %+v", absent)
		}
	})

	t.Run("events", func(t *testing.T) {
		for i := range 3 {
			requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "list.page", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now.Add(time.Duration(i) * time.Second)}))
		}
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: other.ID, Kind: "list.page", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now}))
		whole, err := st.ListEvents(ctx, task.ID)
		requireOK(t, err)
		if len(whole) < 3 {
			t.Fatalf("whole events = %+v", whole)
		}
		var paged []core.Event
		for offset := 0; offset < len(whole); offset += 2 {
			page, err := st.ListEventPage(ctx, task.ID, store.PageQuery{Limit: 2, Offset: offset})
			requireOK(t, err)
			if page.Total != len(whole) {
				t.Fatalf("offset %d total = %d, want %d", offset, page.Total, len(whole))
			}
			paged = append(paged, page.Events...)
		}
		if !reflect.DeepEqual(eventIDs(paged), eventIDs(whole)) {
			t.Fatalf("paged order %v, whole order %v", eventIDs(paged), eventIDs(whole))
		}
		past, err := st.ListEventPage(ctx, task.ID, store.PageQuery{Limit: 2, Offset: store.MaxTaskOperationsOffset})
		requireOK(t, err)
		if past.Total != len(whole) || len(past.Events) != 0 {
			t.Fatalf("past-the-end page = %+v", past)
		}
	})

	t.Run("recovery acts", func(t *testing.T) {
		since := now.Add(-time.Hour)
		for _, event := range []core.Event{
			{TaskID: task.ID, Kind: "work_order.recovered", ActorID: "clock", ActorRole: core.ActorSystem, At: now},
			{TaskID: other.ID, Kind: "merge.conflict_fix_dispatched", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now.Add(time.Second)},
			{TaskID: task.ID, Kind: "task.retry_accepted", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now.Add(2 * time.Second)},
			// Each of these three fails one predicate of the query.
			{TaskID: task.ID, Kind: "work_order.recovered", ActorID: "agent", ActorRole: core.ActorAgent, At: now.Add(3 * time.Second)},
			{TaskID: task.ID, Kind: "work_order.expired", ActorID: "clock", ActorRole: core.ActorSystem, At: now.Add(4 * time.Second)},
			{TaskID: task.ID, Kind: "work_order.recovery_scheduled", ActorID: "clock", ActorRole: core.ActorSystem, At: since.Add(-time.Second)},
		} {
			requireOK(t, st.AppendEvent(ctx, event))
		}
		first, err := st.ListRecoveryActPage(ctx, store.RecoveryActQuery{Since: since, Page: store.PageQuery{Limit: 2}})
		requireOK(t, err)
		if first.Total != 3 || len(first.Acts) != 2 {
			t.Fatalf("first page = %+v", first)
		}
		if act := first.Acts[0]; act.Kind != "retry_accepted" || act.TaskID != task.ID || !act.Automatic || act.ID == 0 || !act.At.Equal(now.Add(2*time.Second)) {
			t.Fatalf("newest act = %+v", act)
		}
		if act := first.Acts[1]; act.Kind != "conflict_fix_dispatched" || act.TaskID != other.ID {
			t.Fatalf("second act = %+v", act)
		}
		second, err := st.ListRecoveryActPage(ctx, store.RecoveryActQuery{Since: since, Page: store.PageQuery{Limit: 2, Offset: 2}})
		requireOK(t, err)
		if second.Total != 3 || len(second.Acts) != 1 || second.Acts[0].Kind != "recovered" {
			t.Fatalf("second page = %+v", second)
		}
		past, err := st.ListRecoveryActPage(ctx, store.RecoveryActQuery{Since: since, Page: store.PageQuery{Limit: 2, Offset: store.MaxTaskOperationsOffset}})
		requireOK(t, err)
		if past.Total != 3 || len(past.Acts) != 0 {
			t.Fatalf("past-the-end page = %+v", past)
		}
	})

	t.Run("refuses an unrepresentable page", func(t *testing.T) {
		for _, page := range []store.PageQuery{{Limit: 0}, {Limit: store.MaxTaskOperationsLimit + 1}, {Limit: 1, Offset: -1}, {Limit: 1, Offset: store.MaxTaskOperationsOffset + 1}} {
			if _, err := st.ListJobPage(ctx, task.ID, page); err == nil {
				t.Fatalf("ListJobPage accepted %+v", page)
			}
			if _, err := st.ListEventPage(ctx, task.ID, page); err == nil {
				t.Fatalf("ListEventPage accepted %+v", page)
			}
			if _, err := st.ListRecoveryActPage(ctx, store.RecoveryActQuery{Page: page}); err == nil {
				t.Fatalf("ListRecoveryActPage accepted %+v", page)
			}
		}
	})

	t.Run("another workspace reads nothing", func(t *testing.T) {
		isolated := x.Workspace + "-pages-" + core.NewTaskID()
		iso := store.WithWorkspace(ctx, isolated)
		jobs, err := st.ListJobPage(iso, task.ID, store.PageQuery{Limit: 10})
		requireOK(t, err)
		if jobs.Total != 0 || len(jobs.Jobs) != 0 {
			t.Fatalf("jobs across workspaces = %+v", jobs)
		}
		events, err := st.ListEventPage(iso, task.ID, store.PageQuery{Limit: 10})
		requireOK(t, err)
		if events.Total != 0 || len(events.Events) != 0 {
			t.Fatalf("events across workspaces = %+v", events)
		}
		acts, err := st.ListRecoveryActPage(iso, store.RecoveryActQuery{Since: now.Add(-time.Hour), Page: store.PageQuery{Limit: 10}})
		requireOK(t, err)
		if acts.Total != 0 || len(acts.Acts) != 0 {
			t.Fatalf("acts across workspaces = %+v", acts)
		}
	})
}

func jobIDs(jobs []core.Job) []string {
	ids := make([]string, len(jobs))
	for i, job := range jobs {
		ids[i] = job.ID
	}
	return ids
}
