package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"

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
