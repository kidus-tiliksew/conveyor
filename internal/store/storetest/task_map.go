package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// taskMapBodySentinel must never appear in a map read.
const taskMapBodySentinel = "TASK-MAP-BODY-SENTINEL"

// RunTaskMapConformance runs unchanged against memory, PostgreSQL, and
// SingleStore (DEC-38, DEC-39). It covers the map's finished-task scopes,
// repository selection, served-requirement fold and aggregates, body
// exclusion, and the batched next-action input reads against their per-task
// equivalents (req-board-map AC-1.3, AC-1.7, AC-3.1, AC-3.4;
// component-persistence).
func RunTaskMapConformance(t *testing.T, x Fixture) {
	st, ws := x.Backend, x.Workspace
	ctx := store.WithActor(x.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
	plane := taskops.New(st)
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	newTask := func(title, repo string, state core.TaskState, dependencies ...string) core.Task {
		t.Helper()
		id := core.NewTaskID()
		created = created.Add(time.Second)
		task := core.Task{ID: id, Workspace: ws, Repo: repo, BaseBranch: "main", Branch: "conveyor/task-" + id, Title: title, Body: taskMapBodySentinel + " " + title, State: state, NextStage: core.StageImplement, CreatedAt: created}
		if err := st.CreateTaskWithDependencies(ctx, task, dependencies); err != nil {
			t.Fatal(err)
		}
		return task
	}
	perform := func(id string, command core.TaskCommand) {
		t.Helper()
		if _, err := plane.Perform(ctx, id, taskops.Command{Kind: command}); err != nil {
			t.Fatalf("%s %s: %v", id, command, err)
		}
	}
	requirement := func(title string, confirm bool) string {
		t.Helper()
		id := "req-map-" + core.NewTaskID()
		_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: title}, core.RequirementVersion{Content: "# " + title, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The map counts " + title + "."}}})
		if err != nil {
			t.Fatal(err)
		}
		if confirm {
			if _, _, err = st.ConfirmRequirementVersion(ctx, id, version.Version); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	attach := func(taskID string, add, remove []string) {
		t.Helper()
		if _, err := st.UpdateTaskContext(ctx, taskID, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: add}, Remove: store.TaskContextInput{RequirementIDs: remove}}); err != nil {
			t.Fatalf("attach %s add=%v remove=%v: %v", taskID, add, remove, err)
		}
	}

	alpha := requirement("Alpha", true)
	beta := requirement("Beta", true)
	proposed := requirement("Proposed only", true)

	depMerged := newTask("merged dependency", "app", core.TaskQueued)
	depClosed := newTask("closed dependency", "conveyor", core.TaskQueued)
	openConveyor := newTask("open conveyor", "conveyor", core.TaskQueued, depMerged.ID, depClosed.ID)
	openApp := newTask("open app", "app", core.TaskRunning)
	mergedUnrelated := newTask("merged unrelated", "conveyor", core.TaskQueued)
	closedUnrelated := newTask("closed unrelated", "conveyor", core.TaskQueued)

	// Remove and re-add count once, as does a repeated add; a removed
	// attachment and a pending context proposal are not served.
	attach(openConveyor.ID, []string{alpha, beta}, nil)
	attach(openConveyor.ID, nil, []string{alpha})
	attach(openConveyor.ID, []string{alpha}, nil)
	attach(openConveyor.ID, []string{alpha}, nil)
	attach(openApp.ID, []string{alpha, beta}, nil)
	attach(openApp.ID, nil, []string{beta})
	attach(depMerged.ID, []string{alpha}, nil)
	attach(mergedUnrelated.ID, []string{alpha}, nil)
	attach(closedUnrelated.ID, []string{alpha}, nil)
	if _, _, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: openApp.ID, TargetKind: core.TaskContextProposalRequirement, TargetID: proposed, Source: core.TaskContextProposalPlanning, Justification: "Pending only."}); err != nil {
		t.Fatal(err)
	}
	// An archived requirement stays a served attachment, as in attached
	// task context.
	if err := st.ArchiveRequirement(ctx, beta, requirementConformanceActor, nil); err != nil {
		t.Fatal(err)
	}
	perform(depMerged.ID, core.TaskMergeRecover)
	perform(mergedUnrelated.ID, core.TaskMergeRecover)
	perform(depClosed.ID, core.TaskCancel)
	perform(closedUnrelated.ID, core.TaskCancel)

	wantRequirements := []store.TaskMapRequirement{
		{ID: alpha, Title: "Alpha", OpenTaskCount: 2, MergedTaskCount: 2, TotalTaskCount: 4},
		{ID: beta, Title: "Beta", OpenTaskCount: 1, MergedTaskCount: 0, TotalTaskCount: 1},
	}
	sort.Slice(wantRequirements, func(i, j int) bool { return wantRequirements[i].ID < wantRequirements[j].ID })
	wantServed := map[string][]string{openConveyor.ID: {alpha, beta}, openApp.ID: {alpha}, depMerged.ID: {alpha}, mergedUnrelated.ID: {alpha}, closedUnrelated.ID: {alpha}}
	for id := range wantServed {
		sort.Strings(wantServed[id])
	}
	wantDeps := []string{depMerged.ID, depClosed.ID}
	sort.Strings(wantDeps)

	read := func(query store.TaskMapQuery, want ...string) store.TaskMap {
		t.Helper()
		got, err := st.ListTaskMap(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, node := range got.Tasks {
			ids = append(ids, node.Task.ID)
			if node.Task.Body != "" {
				t.Fatalf("map read carried body for %s: %q", node.Task.ID, node.Task.Body)
			}
			wantRefs := wantServed[node.Task.ID]
			var refs []string
			for _, ref := range node.Requirements {
				refs = append(refs, ref.ID)
				if (ref.ID == alpha && ref.Title != "Alpha") || (ref.ID == beta && ref.Title != "Beta") {
					t.Fatalf("requirement title %+v", ref)
				}
			}
			if !slices.Equal(refs, wantRefs) {
				t.Fatalf("task %s served=%v want %v", node.Task.ID, refs, wantRefs)
			}
			wantEdges := []string{}
			if node.Task.ID == openConveyor.ID {
				wantEdges = wantDeps
			}
			if !slices.Equal(node.DependsOn, wantEdges) {
				t.Fatalf("task %s depends_on=%v want %v", node.Task.ID, node.DependsOn, wantEdges)
			}
		}
		sort.Strings(want)
		if !slices.Equal(ids, want) {
			t.Fatalf("query %+v tasks=%v want %v", query, ids, want)
		}
		if !reflect.DeepEqual(got.Requirements, wantRequirements) {
			t.Fatalf("query %+v requirements=%+v want %+v", query, got.Requirements, wantRequirements)
		}
		var open []string
		for _, task := range got.OpenTasks {
			open = append(open, task.ID)
			if task.Body != "" {
				t.Fatalf("open set carried body for %s", task.ID)
			}
		}
		wantOpen := []string{openConveyor.ID, openApp.ID}
		sort.Strings(wantOpen)
		if !slices.Equal(open, wantOpen) {
			t.Fatalf("query %+v open=%v want %v", query, open, wantOpen)
		}
		return got
	}

	t.Run("default scope returns open tasks and their finished dependencies", func(t *testing.T) {
		read(store.TaskMapQuery{}, openConveyor.ID, openApp.ID, depMerged.ID, depClosed.ID)
	})
	t.Run("repository selection narrows nodes, not counts or edges", func(t *testing.T) {
		read(store.TaskMapQuery{Repositories: []string{"conveyor"}}, openConveyor.ID, depClosed.ID)
		read(store.TaskMapQuery{Repositories: []string{"app"}}, openApp.ID)
		read(store.TaskMapQuery{Repositories: []string{"app", "conveyor"}}, openConveyor.ID, openApp.ID, depMerged.ID, depClosed.ID)
		read(store.TaskMapQuery{Repositories: []string{"absent"}})
	})
	t.Run("include finished returns every selected task", func(t *testing.T) {
		read(store.TaskMapQuery{IncludeFinished: true}, openConveyor.ID, openApp.ID, depMerged.ID, depClosed.ID, mergedUnrelated.ID, closedUnrelated.ID)
		read(store.TaskMapQuery{IncludeFinished: true, Repositories: []string{"conveyor"}}, openConveyor.ID, depClosed.ID, mergedUnrelated.ID, closedUnrelated.ID)
	})
	t.Run("batched next-action inputs equal per-task reads", func(t *testing.T) {
		at := time.Now().UTC().Truncate(time.Second)
		for _, event := range []core.Event{
			{TaskID: openApp.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7})},
			{TaskID: openApp.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"command": "triage.park"})},
			{TaskID: openConveyor.ID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"window": 2})},
		} {
			event.ActorRole, event.At = core.ActorSystem, at
			if err := st.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 2; i++ {
			if _, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: openApp.ID, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})}); err != nil {
				t.Fatal(err)
			}
		}
		version := core.RequirementVersion{RequirementID: alpha, Content: "# Alpha", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The map counts Alpha twice."}}, Origin: core.RequirementOriginImplementation, OriginTaskID: openApp.ID}
		if _, err := st.ProposeRequirementVersion(ctx, version); err != nil {
			t.Fatal(err)
		}
		ids := []string{openConveyor.ID, openApp.ID, depMerged.ID, depClosed.ID, "missing-" + core.NewTaskID()}
		events, err := st.ListNextActionEvents(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		specs, err := st.ListLatestSpecVersionNumbers(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		blocking, err := st.ListClaimBlockingProposalsForTasks(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		heads, err := st.ListTaskHeads(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids[:4] {
			all, err := st.ListEvents(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var want []core.Event
			for _, event := range all {
				if store.NextActionEventKind(event.Kind) {
					want = append(want, event)
				}
			}
			if !sameEvents(events[id], want) {
				t.Fatalf("task %s batched events=%+v want %+v", id, events[id], want)
			}
			spec, found, err := st.GetLatestSpecVersion(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := specs[id]; ok != found || (found && got != spec.Version) {
				t.Fatalf("task %s latest spec=%d/%t want %d/%t", id, got, ok, spec.Version, found)
			}
			one, err := st.ListClaimBlockingProposalsForTask(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(one) == 0 {
				one = nil
			}
			if !reflect.DeepEqual(blocking[id], one) {
				t.Fatalf("task %s batched claim blockers=%+v want %+v", id, blocking[id], one)
			}
			task, err := st.GetTask(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if heads[id] != (store.TaskHead{ID: task.ID, Title: task.Title, State: task.State}) {
				t.Fatalf("task %s head=%+v", id, heads[id])
			}
		}
		if len(blocking[openApp.ID]) != 1 || specs[openApp.ID] != 2 || len(events[openApp.ID]) != 2 {
			t.Fatalf("fixture did not exercise batched reads: blocking=%+v specs=%v events=%d", blocking, specs, len(events[openApp.ID]))
		}
		if _, ok := heads[ids[4]]; ok {
			t.Fatal("missing task returned a head")
		}
	})
	t.Run("map projection equals the single-task next action", func(t *testing.T) {
		got := read(store.TaskMapQuery{IncludeFinished: true}, openConveyor.ID, openApp.ID, depMerged.ID, depClosed.ID, mergedUnrelated.ID, closedUnrelated.ID)
		now := time.Now().UTC().Truncate(time.Second)
		tasks := make([]core.Task, 0, len(got.Tasks))
		for _, node := range got.Tasks {
			tasks = append(tasks, node.Task)
		}
		batch, err := store.ReadNextActions(ctx, st, tasks, store.NextActionOptions{Now: now, OpenTasksLoaded: true, OpenTasks: got.OpenTasks, CompactOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range tasks {
			one, err := store.ReadNextAction(ctx, st, task.ID, store.NextActionOptions{Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(nextaction.CompactFrom(batch[task.ID]), nextaction.CompactFrom(one)) {
				t.Fatalf("task %s map compact=%+v single=%+v", task.ID, nextaction.CompactFrom(batch[task.ID]), nextaction.CompactFrom(one))
			}
		}
	})
	t.Run("next-action batch reads equal their per-task and full-history reads", func(t *testing.T) {
		runNextActionBatchReads(t, x, ctx, newTask, requirement)
	})
	t.Run("empty workspace reads empty arrays", func(t *testing.T) {
		other := x.Workspace + "-map-empty"
		emptyCtx := store.WithWorkspace(ctx, other)
		got, err := st.ListTaskMap(emptyCtx, store.TaskMapQuery{IncludeFinished: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Tasks) != 0 || len(got.OpenTasks) != 0 || len(got.Requirements) != 0 {
			t.Fatalf("foreign workspace map=%+v", got)
		}
	})
}

func sameEvents(got, want []core.Event) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].ID != want[i].ID || got[i].Kind != want[i].Kind || got[i].TaskID != want[i].TaskID || !got[i].At.Equal(want[i].At) || string(got[i].Payload) != string(want[i].Payload) {
			return false
		}
	}
	return true
}

// runNextActionBatchReads compares the next-action assembler's batch reads
// with the per-task or full-history reads they replace on every backend
// (component-persistence, Task map and next-action batch reads).
func runNextActionBatchReads(t *testing.T, x Fixture, ctx context.Context, newTask func(string, string, core.TaskState, ...string) core.Task, requirement func(string, bool) string) {
	st := x.Backend
	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	t.Run("history equals the full-history fold", func(t *testing.T) {
		at := base
		next := func() time.Time { at = at.Add(time.Second); return at }
		raw := func(taskID, kind string, payload string) {
			t.Helper()
			requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind, ActorRole: core.ActorSystem, At: next(), Payload: json.RawMessage(payload)}))
		}
		order := func(task core.Task, attempt int, terminal bool) string {
			t.Helper()
			id := fmt.Sprintf("%s-implement-%d", task.ID, attempt)
			requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}))
			wo := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: base, QueueDeadline: base.Add(time.Hour)}
			if terminal {
				wo.LastAttemptOutcome = core.WorkOrderOutcomeReleased
			}
			requireOK(t, CreateWorkOrder(ctx, st, wo))
			return id
		}

		history := newTask("history", "conveyor", core.TaskParked)
		raw(history.ID, "task.state_changed", `{"command":"triage.park"}`)
		for i := 0; i < 25; i++ {
			raw(history.ID, "task.state_changed", `{"from":"queued","to":"running"}`)
			raw(history.ID, "task.created", `{"body":"unrelated"}`)
		}
		raw(history.ID, "task.state_changed", `{"command":5}`)
		raw(history.ID, "task.state_changed", `{"command":""}`)
		raw(history.ID, "pipeline.bounce_limit", `{"window":2}`)
		raw(history.ID, "pipeline.bounce_limit", `{"window":0}`)
		// Pull-request numbers the Go int decoder rejects never displace an
		// older accepted one.
		overflow := newTask("pr overflow", "conveyor", core.TaskParked)
		raw(overflow.ID, "pull_request.opened", `{"number":42}`)
		raw(overflow.ID, "pull_request.opened", `{"number":9223372036854775808}`)
		boundary := newTask("pr boundary", "conveyor", core.TaskParked)
		raw(boundary.ID, "pull_request.opened", `{"number":42}`)
		raw(boundary.ID, "pull_request.opened", `{"number":9223372036854775807}`)
		invalid := newTask("pr invalid", "conveyor", core.TaskParked)
		raw(invalid.ID, "pull_request.opened", `{"number":3}`)
		for _, payload := range []string{`{"number":0}`, `{"number":-3}`, `{"number":null}`, `{}`, `{"number":"12"}`, `{"number":12.5}`, `{"number":99999999999999999999}`, `{"number":-9223372036854775809}`, `{"number":true}`, `[1]`, `null`} {
			raw(invalid.ID, "pull_request.opened", payload)
		}

		// An open episode with failures, an exhaustion, and dispatches inside
		// and outside episodes, of other heads, and of another task's order.
		episode := newTask("conflict episodes", "conveyor", core.TaskApproved)
		stranger := newTask("conflict stranger", "conveyor", core.TaskRunning)
		strangerOrder := order(stranger, 1, true)
		first, second, live, early := order(episode, 1, true), order(episode, 2, true), order(episode, 3, false), order(episode, 4, true)
		raw(episode.ID, "merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":"`+early+`"}`)
		for i := 0; i < 3; i++ {
			raw(episode.ID, "merge.blocked", `{"approved_head":"a","new_head":"b"}`)
			raw(episode.ID, "merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"b","next_retry_at":"2026-01-01T00:00:00Z"}`)
			raw(episode.ID, "merge.conflict_cleared", `{}`)
		}
		raw(episode.ID, "merge.blocked", `{"approved_head":"a","new_head":"b"}`)
		raw(episode.ID, "merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":"`+first+`"}`)
		raw(episode.ID, "merge.conflict_cleared", `{}`)
		raw(episode.ID, "merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":"`+early+`"}`)
		raw(episode.ID, "merge.blocked", `{"approved_head":"other","new_head":"b"}`)
		raw(episode.ID, "merge.blocked", `{"approved_head":"a","new_head":"b"}`)
		raw(episode.ID, "merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":"`+second+`"}`)
		raw(episode.ID, "merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":"`+strangerOrder+`"}`)
		raw(episode.ID, "merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"other","work_order_id":"`+live+`"}`)
		raw(episode.ID, "merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":7}`)
		raw(episode.ID, "merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"b","next_retry_at":"2026-02-01T00:00:00Z"}`)
		raw(episode.ID, "merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"b","next_retry_at":"not a time"}`)
		raw(episode.ID, "merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"b","next_retry_at":null}`)
		raw(episode.ID, "merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"other","next_retry_at":"2026-03-01T00:00:00Z"}`)
		raw(episode.ID, "merge.conflict_dispatch_exhausted", `{"approved_head":"a","new_head":"b"}`)
		raw(episode.ID, "merge.conflict_recovery_blocked", `{"approved_head":"a","new_head":"b"}`)
		// A cleared latest episode keeps only its dispatched orders.
		cleared := newTask("conflict cleared", "conveyor", core.TaskApproved)
		clearedOrder := order(cleared, 1, true)
		raw(cleared.ID, "merge.blocked", `{"approved_head":"a","new_head":"b"}`)
		raw(cleared.ID, "merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":"`+clearedOrder+`"}`)
		raw(cleared.ID, "merge.conflict_dispatch_exhausted", `{"approved_head":"a","new_head":"b"}`)
		raw(cleared.ID, "merge.conflict_cleared", `{}`)
		// An undecodable latest block is no conflict; a null block reads
		// empty heads.
		undecodable := newTask("conflict undecodable", "conveyor", core.TaskApproved)
		raw(undecodable.ID, "merge.blocked", `{"approved_head":"a","new_head":"b"}`)
		raw(undecodable.ID, "merge.blocked", `{"approved_head":5,"new_head":"b"}`)
		nullHeads := newTask("conflict null heads", "conveyor", core.TaskApproved)
		raw(nullHeads.ID, "merge.blocked", `null`)
		raw(nullHeads.ID, "merge.conflict_dispatch_failed", `{"approved_head":null,"new_head":""}`)
		raw(nullHeads.ID, "merge.conflict_dispatch_failed", `[1]`)
		tiedAt := next()
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: nullHeads.ID, Kind: "merge.conflict_dispatch_failed", ActorRole: core.ActorSystem, At: tiedAt, Payload: json.RawMessage(`{"next_retry_at":"2026-04-01T00:00:00Z"}`)}))
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: nullHeads.ID, Kind: "merge.conflict_fix_dispatched", ActorRole: core.ActorSystem, At: tiedAt, Payload: json.RawMessage(`{}`)}))
		quiet := newTask("quiet history", "conveyor", core.TaskParked)
		raw(quiet.ID, "task.state_changed", `{"from":"queued","to":"parked"}`)

		tasks := []core.Task{history, overflow, boundary, invalid, episode, cleared, undecodable, nullHeads, quiet}
		ids := []string{"missing-" + core.NewTaskID()}
		for _, task := range tasks {
			ids = append(ids, task.ID)
		}
		got, err := st.ListNextActionHistory(ctx, ids)
		requireOK(t, err)
		for _, task := range tasks {
			all, err := st.ListEvents(ctx, task.ID)
			requireOK(t, err)
			orders, err := st.ListWorkOrdersForTasks(ctx, []string{task.ID})
			requireOK(t, err)
			want := store.FoldNextActionHistory(all, orders)
			if !sameHistory(got[task.ID], want) {
				t.Fatalf("task %s history=%+v want full fold %+v", task.Title, got[task.ID], want)
			}
			if gotConflict, oracle := store.MergeConflictFromHistory(got[task.ID].MergeConflict, orders), previousMergeConflict(all, orders); gotConflict != oracle {
				t.Fatalf("task %s merge conflict=%+v previous fold %+v", task.Title, gotConflict, oracle)
			}
		}
		if h := got[history.ID]; h.ParkCommand != "triage.park" || h.BounceCount != 0 || h.MergeConflict.Open {
			t.Fatalf("history fixture=%+v", h)
		}
		if got[overflow.ID].PullRequest != 42 || got[boundary.ID].PullRequest != 9223372036854775807 {
			t.Fatalf("pull requests overflow=%d boundary=%d, want 42 and the int64 maximum", got[overflow.ID].PullRequest, got[boundary.ID].PullRequest)
		}
		if n := got[invalid.ID].PullRequest; n != 3 && n != 100 {
			t.Fatalf("invalid pull request numbers displaced the valid one: %d", n)
		}
		e := got[episode.ID].MergeConflict
		if !e.Open || e.Cleared || !e.Exhausted || len(e.Steps) != 6 || !reflect.DeepEqual(e.DispatchedOrderIDs, sortedStrings(first, second)) {
			t.Fatalf("episode summary=%+v want dispatched %s and %s", e, first, second)
		}
		if c := got[cleared.ID].MergeConflict; !c.Open || !c.Cleared || c.Exhausted || len(c.Steps) != 0 || !reflect.DeepEqual(c.DispatchedOrderIDs, []string{clearedOrder}) {
			t.Fatalf("cleared summary=%+v", c)
		}
		if got[undecodable.ID].MergeConflict.Open || !got[nullHeads.ID].MergeConflict.Open {
			t.Fatalf("undecodable=%+v null heads=%+v", got[undecodable.ID].MergeConflict, got[nullHeads.ID].MergeConflict)
		}
		for _, id := range []string{quiet.ID, undecodable.ID, ids[0]} {
			if _, ok := got[id]; ok {
				t.Fatalf("task %s without a folded field returned %+v", id, got[id])
			}
		}
		empty, err := st.ListNextActionHistory(ctx, nil)
		requireOK(t, err)
		foreign, err := st.ListNextActionHistory(store.WithWorkspace(ctx, x.Workspace+"-history-foreign"), ids)
		requireOK(t, err)
		if len(empty) != 0 || len(foreign) != 0 {
			t.Fatalf("empty=%+v foreign=%+v", empty, foreign)
		}
	})
	t.Run("open dependents read only the neighborhood", func(t *testing.T) {
		blocker := newTask("dependents blocker", "conveyor", core.TaskRunning)
		other := newTask("dependents other blocker", "conveyor", core.TaskRunning)
		open := newTask("dependents open", "conveyor", core.TaskQueued, blocker.ID)
		both := newTask("dependents both", "app", core.TaskAwaiting, blocker.ID, other.ID)
		merged := newTask("dependents merged", "conveyor", core.TaskMerged, blocker.ID)
		closed := newTask("dependents closed", "conveyor", core.TaskClosed, blocker.ID)
		elsewhere := newTask("dependents elsewhere", "conveyor", core.TaskQueued, other.ID)
		head := func(task core.Task) store.TaskHead {
			return store.TaskHead{ID: task.ID, Title: task.Title, State: task.State}
		}
		sorted := func(heads ...store.TaskHead) []store.TaskHead {
			sort.Slice(heads, func(i, j int) bool { return heads[i].ID < heads[j].ID })
			return heads
		}
		got, err := st.ListOpenDependents(ctx, []string{blocker.ID})
		requireOK(t, err)
		if want := sorted(head(open), head(both)); !reflect.DeepEqual(got, want) {
			t.Fatalf("dependents of blocker=%+v want %+v (merged %s, closed %s excluded)", got, want, merged.ID, closed.ID)
		}
		got, err = st.ListOpenDependents(ctx, []string{other.ID, blocker.ID, blocker.ID})
		requireOK(t, err)
		if want := sorted(head(open), head(both), head(elsewhere)); !reflect.DeepEqual(got, want) {
			t.Fatalf("dependents of both=%+v want %+v", got, want)
		}
		got, err = st.ListOpenDependents(ctx, nil)
		requireOK(t, err)
		foreign, err := st.ListOpenDependents(store.WithWorkspace(ctx, x.Workspace+"-dependents-foreign"), []string{blocker.ID})
		requireOK(t, err)
		if len(got) != 0 || len(foreign) != 0 {
			t.Fatalf("empty=%+v foreign=%+v", got, foreign)
		}
	})
	t.Run("design pins equal the full context fold", func(t *testing.T) {
		pinned := newTask("pins", "conveyor", core.TaskQueued)
		unpinned := newTask("pins none", "conveyor", core.TaskQueued)
		at := base.Add(10 * time.Minute)
		attach := func(taskID, kind, payload string) {
			t.Helper()
			at = at.Add(time.Second)
			requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: kind, ActorRole: core.ActorSystem, At: at, Payload: json.RawMessage(payload)}))
		}
		for round := 1; round <= 3; round++ {
			attach(pinned.ID, store.TaskContextDesignAdded, fmt.Sprintf(`{"id":"design-pin-x","version":%d}`, round))
			attach(pinned.ID, store.TaskContextDesignAdded, `{"id":"design-pin-y","version":1}`)
			attach(pinned.ID, store.TaskContextDesignRemoved, `{"id":"design-pin-y"}`)
			attach(pinned.ID, store.TaskContextDesignRemoved, `{"id":"design-pin-x"}`)
		}
		attach(pinned.ID, store.TaskContextDesignAdded, `{"id":"design-pin-x","version":4}`)
		attach(pinned.ID, "task.state_changed", `{"from":"queued","to":"running"}`)
		attach(pinned.ID, store.TaskContextDesignAdded, `{"id":"design-pin-z","version":1}`)
		attach(pinned.ID, store.TaskContextDesignAdded, `{"id":"design-pin-z","version":"two"}`)
		attach(pinned.ID, store.TaskContextDesignRemoved, `{"id":"design-pin-z","version":2.5}`)
		attach(pinned.ID, store.TaskContextDesignRemoved, `{"id":5}`)
		attach(pinned.ID, store.TaskContextDesignAdded, `{"id":"design-pin-w","version":9223372036854775808}`)
		attach(pinned.ID, store.TaskContextDesignAdded, `{"id":"design-pin-n","version":-9223372036854775808}`)
		attach(pinned.ID, store.TaskContextDesignAdded, `{"id":"design-pin-v","version":null}`)
		attach(pinned.ID, store.TaskContextDesignAdded, `[1]`)
		attach(pinned.ID, store.TaskContextRequirementAdded, `{"id":"req-pin"}`)
		attach(unpinned.ID, store.TaskContextRequirementAdded, `{"id":"req-pin"}`)
		attach(unpinned.ID, store.TaskContextDesignAdded, `{"id":"design-pin-gone","version":1}`)
		attach(unpinned.ID, store.TaskContextDesignRemoved, `{"id":"design-pin-gone"}`)
		got, err := st.ListTaskContextDesignPins(ctx, []string{pinned.ID, unpinned.ID})
		requireOK(t, err)
		all, err := st.ListEvents(ctx, pinned.ID)
		requireOK(t, err)
		_, want := store.ActiveTaskContextReferences(all)
		if !reflect.DeepEqual(got[pinned.ID], want) || want["design-pin-x"] != 4 || want["design-pin-z"] != 1 || len(want) != 4 {
			t.Fatalf("pins=%+v want %+v", got[pinned.ID], want)
		}
		if _, ok := got[unpinned.ID]; ok {
			t.Fatalf("task without a design pin returned %+v", got[unpinned.ID])
		}
	})
	t.Run("document heads equal the per-document reads", func(t *testing.T) {
		confirmed := requirement("heads confirmed", true)
		unconfirmed := requirement("heads unconfirmed", false)
		archived := requirement("heads archived", true)
		requireOK(t, st.ArchiveRequirement(ctx, archived, requirementConformanceActor, nil))
		designID := "design-heads-" + core.NewTaskID()
		design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: designID, Title: "Heads design", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("heads"), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version)
		requireOK(t, err)
		pendingDesign := "design-heads-pending-" + core.NewTaskID()
		_, _, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: pendingDesign, Title: "Heads pending design", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("pending"), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		missing := "missing-doc-" + core.NewTaskID()
		ids := []string{confirmed, unconfirmed, archived, designID, pendingDesign, missing, confirmed}
		got, err := st.ListDocumentHeads(ctx, ids)
		requireOK(t, err)
		for _, id := range ids {
			var want store.DocumentHead
			found := true
			if requirement, err := st.GetRequirement(ctx, id); err == nil {
				want = store.DocumentHead{ID: id, Tier: store.DocumentHeadRequirement, CurrentVersion: requirement.CurrentVersion, Archived: requirement.Archived}
			} else if design, err := st.GetSystemDesign(ctx, id); err == nil {
				want = store.DocumentHead{ID: id, Tier: store.DocumentHeadSystemDesign, CurrentVersion: design.CurrentVersion, Archived: design.Archived}
			} else {
				found = false
			}
			head, ok := got[id]
			if ok != found || head != want {
				t.Fatalf("document %s head=%+v/%t want %+v/%t", id, head, ok, want, found)
			}
		}
		if !got[archived].Archived || got[unconfirmed].CurrentVersion != 0 || got[designID].Tier != store.DocumentHeadSystemDesign || got[designID].CurrentVersion != version.Version {
			t.Fatalf("fixture did not exercise the heads: %+v", got)
		}
	})
	t.Run("verification reviews equal the per-order reads", func(t *testing.T) {
		reader, ok := st.(store.VerificationReviewReader)
		batch, batched := st.(store.VerificationReviewBatchReader)
		if !ok || !batched {
			t.Fatalf("backend reads verification reviews=%t batched=%t", ok, batched)
		}
		now := time.Now().UTC().Truncate(time.Second)
		order := func(task core.Task) string {
			t.Helper()
			id := task.ID + "-verify-1"
			requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}))
			requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageVerify, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}))
			return id
		}
		first, second := newTask("reviews first", "conveyor", core.TaskRunning), newTask("reviews second", "conveyor", core.TaskRunning)
		refs := []store.VerificationReviewRef{{TaskID: first.ID, OrderID: order(first)}, {TaskID: second.ID, OrderID: order(second)}}
		got, err := batch.ReadVerificationReviews(ctx, refs)
		requireOK(t, err)
		for _, ref := range refs {
			want, err := reader.ReadVerificationReview(ctx, ref.TaskID, ref.OrderID)
			requireOK(t, err)
			if !reflect.DeepEqual(got[ref.OrderID].Rows, want.Rows) || got[ref.OrderID].Order.ID != want.Order.ID || got[ref.OrderID].Order.TaskID != ref.TaskID {
				t.Fatalf("order %s batch=%+v single=%+v", ref.OrderID, got[ref.OrderID], want)
			}
		}
		empty, err := batch.ReadVerificationReviews(ctx, nil)
		requireOK(t, err)
		if len(empty) != 0 {
			t.Fatalf("empty input=%+v", empty)
		}
		// An order of another task gets the per-order read's outcome: the
		// durable backends refuse it, and the volatile read returns the order
		// for the caller's own refusal.
		crossed := store.VerificationReviewRef{TaskID: first.ID, OrderID: refs[1].OrderID}
		single, singleErr := reader.ReadVerificationReview(ctx, crossed.TaskID, crossed.OrderID)
		crossedBatch, batchErr := batch.ReadVerificationReviews(ctx, []store.VerificationReviewRef{crossed})
		if (singleErr == nil) != (batchErr == nil) || errors.Is(singleErr, store.ErrVerificationAccess) != errors.Is(batchErr, store.ErrVerificationAccess) {
			t.Fatalf("another task's order batch err=%v single err=%v", batchErr, singleErr)
		}
		if singleErr == nil && (crossedBatch[crossed.OrderID].Order.ID != single.Order.ID || !reflect.DeepEqual(crossedBatch[crossed.OrderID].Rows, single.Rows)) {
			t.Fatalf("another task's order batch=%+v single=%+v", crossedBatch[crossed.OrderID], single)
		}
	})
}

// sameHistory compares two summaries field by field; a step's retry value
// compares by its decoded JSON, because PostgreSQL normalizes JSON text.
func sameHistory(got, want store.NextActionHistory) bool {
	g, w := got.MergeConflict, want.MergeConflict
	if got.ParkCommand != want.ParkCommand || got.PullRequest != want.PullRequest || got.BounceCount != want.BounceCount ||
		g.Open != w.Open || g.Cleared != w.Cleared || g.Exhausted != w.Exhausted || len(g.Steps) != len(w.Steps) || !reflect.DeepEqual(g.DispatchedOrderIDs, w.DispatchedOrderIDs) {
		return false
	}
	for i := range g.Steps {
		if g.Steps[i].Dispatched != w.Steps[i].Dispatched || (g.Steps[i].NextRetryAt == nil) != (w.Steps[i].NextRetryAt == nil) {
			return false
		}
		if g.Steps[i].NextRetryAt == nil {
			continue
		}
		var a, b any
		if json.Unmarshal(g.Steps[i].NextRetryAt, &a) != nil || json.Unmarshal(w.Steps[i].NextRetryAt, &b) != nil || !reflect.DeepEqual(a, b) {
			return false
		}
	}
	return true
}

// previousMergeConflict is the merge-conflict fold the assembler applied to
// the full history before the bounded summary: the latest merge.blocked
// decides the episode, and MergeConflictDispatchState folds every event.
func previousMergeConflict(events []core.Event, orders []core.WorkOrder) nextaction.MergeConflict {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "merge.blocked" {
			continue
		}
		var heads struct {
			ApprovedHead string `json:"approved_head"`
			NewHead      string `json:"new_head"`
		}
		if json.Unmarshal(events[i].Payload, &heads) != nil {
			return nextaction.MergeConflict{}
		}
		state := store.MergeConflictDispatchState(events, heads.ApprovedHead, heads.NewHead, orders)
		return nextaction.MergeConflict{Open: true, Exhausted: state.Exhausted || state.Failures >= 3, NextRetryAt: state.NextRetryAt}
	}
	return nextaction.MergeConflict{}
}
