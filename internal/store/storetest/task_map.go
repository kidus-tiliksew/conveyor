package storetest

import (
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
