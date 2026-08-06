package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type PlanningBundleFactory func(*testing.T) (store.Store, context.Context, string)

// RunPlanningBundleConformance asserts the shared memory/Postgres contract for
// pending document context, audited preview revision, enqueue, and rebuild.
func RunPlanningBundleConformance(t *testing.T, factory PlanningBundleFactory) {
	t.Helper()
	st, ctx, workspace := factory(t)
	requirement, pending, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Pending bundle intent"}, core.RequirementVersion{
		Content: "Pending intent", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Create the task set while this revision stays pending."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Goal: core.PlanningGoalBundle})
	if err != nil {
		t.Fatal(err)
	}
	base := core.PlanningBundle{ID: "bundle-" + core.NewTaskID(), SessionID: session.ID, Title: "Pending bundle", Documents: []core.PlanningBundleDocument{{Kind: core.PlanningBundleRequirement, ID: requirement.ID, Version: pending.Version}}, Tasks: []core.PlanningBundleTask{{
		MemberID: "one", Title: "One", Body: "Initial body", Repo: "conveyor",
		Context: core.PlanningBundleTaskContext{RequirementIDs: []string{requirement.ID}},
	}}}
	invalid := base
	invalid.ID = "bundle-" + core.NewTaskID()
	invalid.Tasks = append([]core.PlanningBundleTask(nil), base.Tasks...)
	invalid.Tasks[0].Context.RequirementIDs = []string{"req-missing"}
	if _, err = st.CreatePlanningBundle(ctx, invalid); err == nil {
		t.Fatal("unknown task context finalized")
	} else {
		var referenceErr *store.TaskContextReferenceError
		if !errors.As(err, &referenceErr) || referenceErr.ID != "req-missing" {
			t.Fatalf("unknown context error=%v", err)
		}
	}
	if _, err = st.GetPlanningBundle(ctx, invalid.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalid bundle persisted: %v", err)
	}

	created, err := st.CreatePlanningBundle(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := st.CreatePlanningBundle(ctx, base)
	if err != nil || retried.Tasks[0].CreatedTaskID != created.Tasks[0].CreatedTaskID {
		t.Fatalf("idempotent retry=%+v err=%v", retried, err)
	}
	revisedInput := base
	revisedInput.Tasks = append([]core.PlanningBundleTask(nil), base.Tasks...)
	revisedInput.Tasks[0].Body = "Revised body"
	revised, err := st.CreatePlanningBundle(ctx, revisedInput)
	if err != nil || revised.Tasks[0].CreatedTaskID != created.Tasks[0].CreatedTaskID || revised.Tasks[0].Body != "Revised body" {
		t.Fatalf("revised=%+v err=%v", revised, err)
	}
	if _, err = st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{SessionID: session.ID, BundleID: revised.ID, Title: revised.Title}); err != nil {
		t.Fatal(err)
	}
	approved, err := st.ApprovePlanningBundle(ctx, revised.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskID := approved.Tasks[0].CreatedTaskID
	if err = st.EnsureTaskEnqueued(ctx, taskID); err != nil {
		t.Fatalf("approved task was not enqueued: %v", err)
	}
	version, err := st.GetRequirementVersion(ctx, requirement.ID, pending.Version)
	if err != nil || version.Confirmed {
		t.Fatalf("bundle approval confirmed pending document: %+v err=%v", version, err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	marked := false
	for _, event := range events {
		if event.Kind != store.TaskContextRequirementAdded {
			continue
		}
		var payload struct {
			ID             string `json:"id"`
			PendingVersion int    `json:"pending_version"`
			Unconfirmed    bool   `json:"unconfirmed"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.ID == requirement.ID && payload.PendingVersion == pending.Version && payload.Unconfirmed {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("pending task context marker missing: %+v", events)
	}
	assertBundleEdges(t, st, ctx, workspace, session.ID, revised.ID, requirement.ID, pending.Version, taskID)
	rebuild, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "planning bundle conformance", RequestID: core.NewTaskID()})
	if err != nil {
		t.Fatal(err)
	}
	if rebuild.PreservedUnregenerable != 0 {
		t.Fatalf("bundle rebuild preserved event-derived edges: %+v", rebuild)
	}
	assertBundleEdges(t, st, ctx, workspace, session.ID, revised.ID, requirement.ID, pending.Version, taskID)

	if _, err = st.UpdateTaskContext(ctx, taskID, store.TaskContextChange{Remove: store.TaskContextInput{RequirementIDs: []string{"req-missing"}}}); err == nil {
		t.Fatal("unknown removal appended a phantom event")
	}
	if _, err = st.UpdateTaskContext(ctx, taskID, store.TaskContextChange{Remove: store.TaskContextInput{RequirementIDs: []string{requirement.ID}}}); err != nil {
		t.Fatalf("pending attachment could not be removed: %v", err)
	}
}

func assertBundleEdges(t *testing.T, st store.Store, ctx context.Context, workspace, sessionID, bundleID, requirementID string, version int, taskID string) {
	t.Helper()
	want := map[string]bool{
		string(core.LineagePlanningSession) + "\x00" + sessionID + "\x00" + string(core.LineagePlanningBundle) + "\x00" + bundleID + "\x00produced_bundle":                                            false,
		string(core.LineagePlanningBundle) + "\x00" + bundleID + "\x00" + string(core.LineageRequirementVersion) + "\x00" + core.RequirementVersionLineageID(requirementID, version) + "\x00proposes": false,
		string(core.LineagePlanningBundle) + "\x00" + bundleID + "\x00" + string(core.LineageTask) + "\x00" + taskID + "\x00creates":                                                                  false,
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		key := string(link.SrcType) + "\x00" + link.SrcID + "\x00" + string(link.DstType) + "\x00" + link.DstID + "\x00" + link.Kind
		if _, ok := want[key]; ok && link.Workspace == workspace && link.CreatedByEventID > 0 {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Fatalf("bundle edge %q missing full identity from %+v", key, links)
		}
	}
}
