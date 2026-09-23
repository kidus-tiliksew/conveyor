package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runEmptyProjections(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	empty := func(value any, err error) {
		t.Helper()
		requireOK(t, err)
		if reflect.ValueOf(value).Len() != 0 {
			t.Fatalf("fresh fixture returned nonempty %T", value)
		}
	}
	missing := func(_ any, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("missing resource returned success")
		}
	}
	empty(st.ListActivityMarkers(ctx))
	empty(st.ListActivityMarkersForTasks(ctx, []string{"absent"}))
	empty(st.ListCheckpointContextCandidates(ctx, "absent"))
	empty(st.ListDependentTaskIDs(ctx, "absent"))
	empty(st.ListFeatures(ctx))
	empty(st.ListGovernanceDesigns(ctx, "conveyor"))
	empty(st.ListJobs(ctx, "absent"))
	empty(st.ListPlanningBundles(ctx))
	empty(st.ListRequirementDeliveryEventsForTasks(ctx, []string{"absent"}))
	empty(st.ListRequirementDeliveryLineageByRequirement(ctx, nil, core.LineageTraversalBudget{Workspace: x.Workspace, MaxDepth: 5, MaxNodes: 32}))
	empty(st.ListRequirementEventsByRequirement(ctx))
	empty(st.ListRequirementVersionsByRequirement(ctx))
	empty(st.ListSystemDesignEventsByDocument(ctx))
	empty(st.ListSystemDesignVersionsByDocument(ctx))
	empty(st.ListActiveSystemDesignDriftCounts(ctx))
	empty(st.ListWorkOrders(ctx))
	empty(st.ListWorkOrdersForTasks(ctx, []string{"absent"}))
	empty(st.ListLineageNodeRecords(ctx, nil))
	records, err := st.ListLineageContextRecords(ctx, nil)
	requireOK(t, err)
	if len(records.Tasks) != 0 || len(records.Requirements) != 0 {
		t.Fatal("empty lineage request returned unrelated records")
	}
	missing(st.GetReferenceDocument(ctx, "absent"))
	missing(st.GetReferenceDocumentVersion(ctx, "absent", 1))
	missing(st.GetSystemDesignVersion(ctx, "absent", 1))
	missing(st.GetTranscript(ctx, "absent"))
	if _, found, err := st.GetLatestJob(ctx, "absent"); err != nil || found {
		t.Fatalf("missing latest job found=%v err=%v", found, err)
	}
	if _, found, err := st.GetSpecVersion(ctx, "absent", 1); err != nil || found {
		t.Fatalf("missing spec found=%v err=%v", found, err)
	}
	if _, found, err := st.GetTaskByIntakeKey(ctx, "absent"); err != nil || found {
		t.Fatalf("missing intake found=%v err=%v", found, err)
	}
	exists, err := st.RequirementExists(ctx, "absent")
	requireOK(t, err)
	if exists {
		t.Fatal("missing requirement exists")
	}
	page, err := st.ListTaskPage(ctx, store.TaskOperationsQuery{Limit: 10})
	requireOK(t, err)
	if page.Total != 0 || len(page.Tasks) != 0 {
		t.Fatal("fresh task page is nonempty")
	}
	attention, err := st.ListCallerAttentionTaskPage(ctx, store.CallerAttentionQuery{UserID: "absent", Limit: 10})
	requireOK(t, err)
	if attention.Total != 0 || len(attention.Tasks) != 0 {
		t.Fatal("unknown caller has attention tasks")
	}
}

func runProjectionReads(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := newAggregateOrder(t, x)
	jobs, err := st.ListJobs(ctx, order.TaskID)
	requireOK(t, err)
	if len(jobs) != 1 || jobs[0].ID != order.JobID {
		t.Fatal("job list lost created job")
	}
	job, found, err := st.GetLatestJob(ctx, order.TaskID)
	requireOK(t, err)
	if !found || job.ID != order.JobID {
		t.Fatal("latest job differs")
	}
	job.TokensIn = 42
	requireOK(t, st.UpdateJob(ctx, job))
	job, _, err = st.GetLatestJob(ctx, order.TaskID)
	requireOK(t, err)
	if job.TokensIn != 42 {
		t.Fatal("job update missing")
	}
	requireOK(t, st.UpsertTranscript(ctx, core.Transcript{JobID: job.ID, URI: "https://example.test/transcript"}))
	transcript, err := st.GetTranscript(ctx, job.ID)
	requireOK(t, err)
	if transcript.URI != "https://example.test/transcript" {
		t.Fatal("transcript round trip differs")
	}
	events, err := st.ListEvents(ctx, order.TaskID)
	requireOK(t, err)
	if len(events) == 0 {
		t.Fatal("task has no events")
	}
	tail, err := st.ListEventsAfter(ctx, order.TaskID, events[len(events)-1].ID)
	requireOK(t, err)
	if len(tail) != 0 {
		t.Fatal("event cursor repeated its boundary")
	}
	count, err := st.CountEventsSinceHumanIntervention(ctx, order.TaskID, "task.created")
	requireOK(t, err)
	if count != 1 {
		t.Fatalf("creation count=%d", count)
	}
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: order.TaskID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"commit_sha": "fixture-head"})}))
	delivery, err := st.ListRequirementDeliveryEventsForTasks(ctx, []string{order.TaskID})
	requireOK(t, err)
	if len(delivery[order.TaskID]) != 1 {
		t.Fatal("delivery event missing from batch")
	}
	foreign, err := st.ListRequirementDeliveryEventsForTasks(store.WithWorkspace(ctx, x.Workspace+"-foreign"), []string{order.TaskID})
	requireOK(t, err)
	if len(foreign) != 0 {
		t.Fatal("delivery events crossed workspace boundary")
	}
	nodes := []core.LineageNode{{Type: core.LineageTask, ID: order.TaskID}}
	labels, err := st.ListLineageNodeRecords(ctx, nodes)
	requireOK(t, err)
	if len(labels) != 1 {
		t.Fatal("selected task label missing")
	}
	contextRecords, err := st.ListLineageContextRecords(ctx, nodes)
	requireOK(t, err)
	if len(contextRecords.Tasks) != 1 {
		t.Fatal("selected task context missing")
	}
	feature := core.Feature{ID: "feature", Workspace: x.Workspace, Name: "Historical feature"}
	requireOK(t, st.CreateFeature(ctx, feature))
	requireOK(t, st.AssignTaskFeature(ctx, order.TaskID, feature.ID))
	features, err := st.ListFeatures(ctx)
	requireOK(t, err)
	if len(features) != 1 || features[0].ID != feature.ID {
		t.Fatal("historical feature missing")
	}
	callbackError := errors.New("fixture callback")
	for range 2 {
		if err := st.WithTaskSideEffectLock(ctx, order.TaskID, func(context.Context) error { return callbackError }); !errors.Is(err, callbackError) {
			t.Fatal("side-effect lock did not return callback error or release")
		}
	}
	requireOK(t, st.ValidateTaskDependencies(ctx, []string{order.TaskID}))
	if err := st.ValidateTaskDependencies(ctx, []string{"absent"}); err == nil {
		t.Fatal("missing dependency accepted")
	}
}

func runWorkspaceControl(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	workspaces, err := st.ListWorkspaces(ctx)
	requireOK(t, err)
	if len(workspaces) != 1 || workspaces[0].ID != x.Workspace {
		t.Fatal("workspace isolation failed")
	}
	workspace, err := st.GetWorkspace(ctx, x.Workspace)
	requireOK(t, err)
	if workspace.ID != x.Workspace {
		t.Fatal("workspace read differs")
	}
	seeded, err := st.BootstrapWorkspaceConfig(ctx, x.Config)
	requireOK(t, err)
	if seeded {
		t.Fatal("workspace bootstrap was not idempotent")
	}
	version, err := st.WorkspaceConfig(ctx)
	requireOK(t, err)
	next := *x.Config
	next.MaxBounces = 7
	receipt, err := st.UpdateWorkspaceConfig(ctx, version.Version, &next)
	requireOK(t, err)
	if receipt.Version != version.Version+1 {
		t.Fatal("configuration version did not advance")
	}
	if _, err := st.UpdateWorkspaceConfig(ctx, version.Version, &next); !errors.Is(err, config.ErrVersionConflict) {
		t.Fatalf("stale configuration error=%v", err)
	}
	runtime, err := st.RuntimeConfig(ctx, x.Config)
	requireOK(t, err)
	if runtime.MaxBounces != 7 {
		t.Fatal("runtime missed stored workspace configuration")
	}
	if st.Log() == nil {
		t.Fatal("backend has no event-log handle")
	}
	// Log behavior belongs to logtest; these reconciliation reads verify
	// that an empty store creates no lifecycle or queue work on startup.
	for name, run := range map[string]func(context.Context) (int, error){"queued": st.ReconcileQueuedTasks, "blueprints": st.ReconcileBlueprintClosures, "forge": st.ReconcileGitHubLifecycles} {
		count, err := run(ctx)
		requireOK(t, err)
		if count != 0 {
			t.Fatalf("empty %s reconciliation=%d", name, count)
		}
	}
}

// runMonitorPullRequestEvents holds all backends to the monitor's narrow read
// contract (component-monitor-drift; component-verification-strategy).
func runMonitorPullRequestEvents(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	first, second, excluded := newAggregateTask(t, x), newAggregateTask(t, x), newAggregateTask(t, x)
	at := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	for _, event := range []core.Event{
		{TaskID: first.ID, Kind: "pull_request.opened", At: at.Add(time.Second), Payload: core.JSONPayload(map[string]any{"number": 2})},
		{TaskID: first.ID, Kind: "pull_request.opened", At: at, Payload: core.JSONPayload(map[string]any{"number": 1})},
		{TaskID: first.ID, Kind: "pull_request.opened", At: at, Payload: core.JSONPayload(map[string]any{"number": 3})},
		{TaskID: first.ID, Kind: "merge.confirmed", At: at, Payload: core.JSONPayload(map[string]any{})},
		{TaskID: second.ID, Kind: "pull_request.opened", At: at, Payload: core.JSONPayload(map[string]any{"number": 4})},
		{TaskID: excluded.ID, Kind: "pull_request.opened", At: at, Payload: core.JSONPayload(map[string]any{"number": 5})},
	} {
		requireOK(t, st.AppendEvent(ctx, event))
	}
	ids := []string{second.ID, first.ID, first.ID, "absent"}
	got, err := st.ListMonitorPullRequestEventsForTasks(ctx, ids)
	requireOK(t, err)
	if len(got) != 2 || len(got[first.ID]) != 3 || len(got[second.ID]) != 1 {
		t.Fatalf("candidate/kind filter returned %v", got)
	}
	for _, id := range []string{first.ID, second.ID} {
		all, err := st.ListEvents(ctx, id)
		requireOK(t, err)
		var want []core.Event
		for _, event := range all {
			if event.Kind == "pull_request.opened" {
				want = append(want, event)
			}
		}
		if !reflect.DeepEqual(got[id], want) {
			t.Fatalf("batch events differ from ordered ledger: got=%v want=%v", got[id], want)
		}
	}
	for _, foreign := range []bool{false, true} {
		readCtx := ctx
		if foreign {
			readCtx = store.WithWorkspace(ctx, x.Workspace+"-foreign")
		}
		candidates := ids
		if !foreign {
			candidates = nil
		}
		empty, err := st.ListMonitorPullRequestEventsForTasks(readCtx, candidates)
		requireOK(t, err)
		if len(empty) != 0 {
			t.Fatalf("empty or foreign read returned %v", empty)
		}
	}
}

// runTaskEventOrdering holds every backend to the per-task ledger order owned
// by component-persistence and exercised through the two filtered batch reads.
func runTaskEventOrdering(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task, excluded := newAggregateTask(t, x), newAggregateTask(t, x)
	at := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	events := []core.Event{
		{TaskID: task.ID, Kind: "merge.confirmed", At: at.Add(time.Second), Payload: core.JSONPayload(map[string]any{"marker": "delivery-late"})},
		{TaskID: task.ID, Kind: "pull_request.opened", At: at.Add(time.Second), Payload: core.JSONPayload(map[string]any{"marker": "monitor-late"})},
		{TaskID: task.ID, Kind: "merge.confirmed", At: at, Payload: core.JSONPayload(map[string]any{"marker": "delivery-first"})},
		{TaskID: task.ID, Kind: "pull_request.opened", At: at, Payload: core.JSONPayload(map[string]any{"marker": "monitor-first"})},
		{TaskID: task.ID, Kind: "merge.confirmed", At: at, Payload: core.JSONPayload(map[string]any{"marker": "delivery-second"})},
		{TaskID: task.ID, Kind: "pull_request.opened", At: at, Payload: core.JSONPayload(map[string]any{"marker": "monitor-second"})},
		{TaskID: excluded.ID, Kind: "merge.confirmed", At: at, Payload: core.JSONPayload(map[string]any{"marker": "excluded-delivery"})},
		{TaskID: excluded.ID, Kind: "pull_request.opened", At: at, Payload: core.JSONPayload(map[string]any{"marker": "excluded-monitor"})},
	}
	for _, event := range events {
		requireOK(t, st.AppendEvent(ctx, event))
	}

	assertOrdered := func(name string, got []core.Event, wantKinds []string) {
		t.Helper()
		if len(got) != len(wantKinds) {
			t.Fatalf("%s event count=%d want=%d: %v", name, len(got), len(wantKinds), got)
		}
		for i, event := range got {
			if event.Kind != wantKinds[i] {
				t.Fatalf("%s event %d kind=%q want=%q: %v", name, i, event.Kind, wantKinds[i], got)
			}
			if i == 0 {
				continue
			}
			previous := got[i-1]
			if event.At.Before(previous.At) || (event.At.Equal(previous.At) && event.ID <= previous.ID) {
				t.Fatalf("%s is not ordered by at,id: %v", name, got)
			}
		}
	}

	ledger, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	orderedFixture := make([]core.Event, 0, 6)
	for _, event := range ledger {
		if event.Kind == "merge.confirmed" || event.Kind == "pull_request.opened" {
			orderedFixture = append(orderedFixture, event)
		}
	}
	assertOrdered("ListEvents", orderedFixture, []string{"merge.confirmed", "pull_request.opened", "merge.confirmed", "pull_request.opened", "merge.confirmed", "pull_request.opened"})

	cursorEvents, err := st.ListEventsAfter(ctx, task.ID, 0)
	requireOK(t, err)
	cursorMarkers := make([]string, 0, 6)
	var previousID int64
	for _, event := range cursorEvents {
		var payload struct {
			Marker string `json:"marker"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("ListEventsAfter payload: %v", err)
		}
		if payload.Marker == "" {
			continue
		}
		if event.ID <= previousID {
			t.Fatalf("ListEventsAfter is not ordered by id: %v", cursorEvents)
		}
		previousID = event.ID
		cursorMarkers = append(cursorMarkers, payload.Marker)
	}
	wantCursorMarkers := []string{"delivery-late", "monitor-late", "delivery-first", "monitor-first", "delivery-second", "monitor-second"}
	if !reflect.DeepEqual(cursorMarkers, wantCursorMarkers) {
		t.Fatalf("ListEventsAfter markers=%v want=%v", cursorMarkers, wantCursorMarkers)
	}

	ids := []string{task.ID, task.ID, "absent"}
	delivery, err := st.ListRequirementDeliveryEventsForTasks(ctx, ids)
	requireOK(t, err)
	if len(delivery) != 1 {
		t.Fatalf("requirement-delivery task filter returned %v", delivery)
	}
	assertOrdered("ListRequirementDeliveryEventsForTasks", delivery[task.ID], []string{"merge.confirmed", "merge.confirmed", "merge.confirmed"})

	monitor, err := st.ListMonitorPullRequestEventsForTasks(ctx, ids)
	requireOK(t, err)
	if len(monitor) != 1 {
		t.Fatalf("monitor task filter returned %v", monitor)
	}
	assertOrdered("ListMonitorPullRequestEventsForTasks", monitor[task.ID], []string{"pull_request.opened", "pull_request.opened", "pull_request.opened"})

	foreignCtx := store.WithWorkspace(ctx, x.Workspace+"-foreign")
	for name, got := range map[string]map[string][]core.Event{
		"foreign requirement-delivery": mustRequirementDeliveryEvents(t, st, foreignCtx, []string{task.ID}),
		"foreign monitor":              mustMonitorPullRequestEvents(t, st, foreignCtx, []string{task.ID}),
		"absent requirement-delivery":  mustRequirementDeliveryEvents(t, st, ctx, []string{"absent"}),
		"absent monitor":               mustMonitorPullRequestEvents(t, st, ctx, []string{"absent"}),
	} {
		if len(got) != 0 {
			t.Fatalf("%s returned %v", name, got)
		}
	}
}

func mustRequirementDeliveryEvents(t *testing.T, st store.Store, ctx context.Context, taskIDs []string) map[string][]core.Event {
	t.Helper()
	events, err := st.ListRequirementDeliveryEventsForTasks(ctx, taskIDs)
	requireOK(t, err)
	return events
}

func mustMonitorPullRequestEvents(t *testing.T, st store.Store, ctx context.Context, taskIDs []string) map[string][]core.Event {
	t.Helper()
	events, err := st.ListMonitorPullRequestEventsForTasks(ctx, taskIDs)
	requireOK(t, err)
	return events
}
