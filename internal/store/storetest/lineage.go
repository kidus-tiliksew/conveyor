package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type LineageFixture struct {
	Store          store.Store
	Context        context.Context
	ForeignContext context.Context
	Workspace      string
	SeedLegacy     func(*testing.T, string) (int, func(*testing.T))
}

type LineageFactory func(*testing.T, []config.Repo) LineageFixture

// RunLineageConformance drives ordinary store writers and asserts the common
// event projection/rebuild contract for memory and Postgres.
func RunLineageConformance(t *testing.T, factory LineageFactory) {
	t.Helper()
	assertCanonicalVocabulary(t)
	fixture := factory(t, []config.Repo{{Name: "conveyor", Base: "main"}})
	st, ctx := fixture.Store, fixture.Context
	now := time.Now().UTC()
	parent := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	dependency := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning, CreatedAt: now}
	for _, task := range []*core.Task{&parent, &dependency} {
		task.BaseBranch = "main"
		task.Branch = "conveyor/task-" + task.ID
		task.Title = task.ID
	}
	for _, task := range []core.Task{parent, dependency} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	child := core.Task{ID: core.NewTaskID(), Workspace: fixture.Workspace, Repo: "conveyor", State: core.TaskRunning,
		ParentTaskID: parent.ID, OriginSpecVersion: 1, OriginSubID: "SUB-1", CreatedAt: now}
	child.BaseBranch, child.Branch, child.Title = "main", "conveyor/task-"+child.ID, child.ID
	if err := st.CreateTaskWithDependencies(ctx, child, []string{dependency.ID}); err != nil {
		t.Fatal(err)
	}
	// Workspace-scoped reads never fall back to another graph.
	if foreign, err := st.ListLineageLinks(store.WithWorkspace(t.Context(), fixture.Workspace+"-other")); err != nil || len(foreign) != 0 {
		t.Fatalf("cross-workspace lineage leaked: links=%v err=%v", foreign, err)
	}
	assertAbandonedDraftSupersession(t, st, ctx)
	assertEligibleReviewSupport(t, st, ctx, fixture.Workspace, "in-process")
	assertEligibleReviewSupport(t, st, ctx, fixture.Workspace, "external-mcp")
	assertLineageArtifactOrderingAndBounds(t, st, ctx, fixture.Workspace)
	assertReferenceDocumentLineage(t, st, ctx)
	assertSystemDesignEventIsolation(t, st, ctx, fixture.ForeignContext, fixture.Workspace)
	assertSystemDesignAndDecisionLineage(t, st, ctx, parent.ID)
	assertTaskContext := assertTaskContextLineage(t, st, ctx, fixture.Workspace)
	for _, event := range []core.Event{
		{TaskID: child.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": child.ID + "-duplicate"})},
		{TaskID: child.ID, Kind: "work_order.created", Payload: core.JSONPayload(map[string]any{"id": child.ID + "-duplicate"})},
		{TaskID: child.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 99})},
		// Initial projection knows the historical default predecessor. Rebuild
		// cannot prove it from durable requirement versions, so it must retain
		// the event-provenanced row as unregenerable.
		{TaskID: child.ID, Kind: "requirement.version_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": "historical-conformance", "version": 2})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	wantExisting := 0
	var assertLegacy func(*testing.T)
	if fixture.SeedLegacy != nil {
		wantExisting, assertLegacy = fixture.SeedLegacy(t, child.ID)
		assertLegacy(t)
	}
	before := lineageSnapshot(t, st, ctx)
	if !hasLineageKind(before, "depends_on") {
		t.Fatalf("real dependency writer omitted lineage: %v", before)
	}
	if !hasLineageKind(before, "materializes") {
		t.Fatalf("real task writer omitted lineage: %v", before)
	}
	result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "conformance", RequestID: core.NewTaskID()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Existing != wantExisting {
		t.Fatalf("existing=%d want=%d result=%+v", result.Existing, wantExisting, result)
	}
	if result.PreservedUnregenerable != 1 || result.Unsupported != 1 || result.Ambiguous != 1 {
		t.Fatalf("rebuild classification=%+v, want preserved=1 unsupported=1 ambiguous=1", result)
	}
	if assertLegacy != nil {
		assertLegacy(t)
	}
	after := lineageSnapshot(t, st, ctx)
	if result.Projected+result.PreservedUnregenerable != len(after) {
		t.Fatalf("result=%+v links=%v", result, after)
	}
	if len(before) != len(after) {
		t.Fatalf("before=%v after=%v", before, after)
	}
	for key, eventID := range before {
		if after[key] != eventID {
			t.Fatalf("edge %s provenance before=%d after=%d", key, eventID, after[key])
		}
	}
	assertTaskContext(t)
}

func assertTaskContextLineage(t *testing.T, st store.Store, ctx context.Context, workspace string) func(*testing.T) {
	t.Helper()
	requirementIDs := []string{"req-" + core.NewTaskID(), "req-" + core.NewTaskID()}
	for _, id := range requirementIDs {
		document, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Task intent " + id}, core.RequirementVersion{
			Content: "Task intent", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Deliver the attached intent."}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, document.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	designIDs := []string{"design-" + core.NewTaskID(), "design-" + core.NewTaskID()}
	for _, id := range designIDs {
		document, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Task design " + id, Category: "Architecture"}, core.SystemDesignVersion{
			Content: "# Task design\n\n```conveyor:governs\n- repo: other\n  paths:\n    - internal/task/**\n```", Origin: core.SystemDesignOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Title: "Contextual task", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTaskWithDependenciesAndContext(ctx, task, nil, store.TaskContextInput{RequirementIDs: requirementIDs, DesignIDs: designIDs}); err != nil {
		t.Fatal(err)
	}
	active, err := st.UpdateTaskContext(ctx, task.ID, store.TaskContextChange{Remove: store.TaskContextInput{RequirementIDs: requirementIDs[1:], DesignIDs: designIDs[1:]}})
	if err != nil {
		t.Fatal(err)
	}
	if len(active.Requirements) != 1 || active.Requirements[0].ID != requirementIDs[0] || len(active.Designs) != 1 || active.Designs[0].ID != designIDs[0] || active.Designs[0].Version != 1 {
		t.Fatalf("active task context=%+v", active)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var addedDesignEventID int64
	var removedDesignVersion int
	for _, event := range events {
		if event.Kind == store.TaskContextDesignAdded {
			var payload struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ID == designIDs[1] {
				addedDesignEventID = event.ID
			}
		}
		if event.Kind == store.TaskContextDesignRemoved {
			var payload struct {
				Version int `json:"version"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				t.Fatalf("invalid design removal payload: %s", event.Payload)
			}
			removedDesignVersion = payload.Version
		}
	}
	if addedDesignEventID == 0 || removedDesignVersion != 1 {
		t.Fatalf("design audit history add_event=%d removal pinned version=%d want nonzero add and version 1", addedDesignEventID, removedDesignVersion)
	}
	assertActive := func(t *testing.T) {
		t.Helper()
		links, listErr := st.ListLineageLinks(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for index, id := range requirementIDs {
			found := false
			for _, link := range links {
				found = found || (link.SrcType == core.LineageRequirement && link.SrcID == id && link.DstType == core.LineageTask && link.DstID == task.ID && link.Kind == "serves" && link.CreatedByEventID > 0)
			}
			if found != (index == 0) {
				t.Fatalf("active task serves edge for %s found=%t want=%t", id, found, index == 0)
			}
		}
		for index, id := range designIDs {
			found := false
			for _, link := range links {
				found = found || (link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(id, 1) && link.DstType == core.LineageTask && link.DstID == task.ID && link.Kind == "governs" && link.CreatedByEventID > 0)
			}
			if found != (index == 0) {
				t.Fatalf("active task governs edge for %s found=%t want=%t", id, found, index == 0)
			}
		}
		served, servedErr := store.ServedRequirementsForTask(ctx, st, task.ID)
		if servedErr != nil || len(served.Requirements) != 1 || served.Requirements[0].ID != requirementIDs[0] {
			t.Fatalf("served requirements after removal=%+v err=%v", served, servedErr)
		}
		governance, governanceErr := store.GovernanceForTask(ctx, st, task.ID, task.Repo)
		if governanceErr != nil {
			t.Fatalf("governance after removal=%+v err=%v", governance, governanceErr)
		}
		foundDesigns := map[string]int{}
		for _, design := range governance.Designs {
			foundDesigns[design.ID] = design.Version
		}
		if foundDesigns[designIDs[0]] != 1 || foundDesigns[designIDs[1]] != 0 {
			t.Fatalf("task governance after removal=%v want active %s:v1 without %s", foundDesigns, designIDs[0], designIDs[1])
		}
	}
	assertActive(t)
	return assertActive
}

func assertSystemDesignEventIsolation(t *testing.T, st store.Store, ctx, foreignCtx context.Context, workspace string) {
	t.Helper()
	if foreignCtx == nil {
		t.Fatal("lineage conformance fixture must provide a foreign workspace context")
	}
	documentID := "shared-" + core.NewTaskID()
	for _, createCtx := range []context.Context{ctx, foreignCtx} {
		if _, _, err := st.CreateSystemDesign(createCtx,
			core.SystemDesign{ID: documentID, Title: "Workspace-local design", Category: "Architecture"},
			core.SystemDesignVersion{Content: "# Workspace-local design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```", Origin: core.SystemDesignOriginOperator}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.ListSystemDesignEvents(ctx, documentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("system-design timeline returned %d events, want created and proposed only: %+v", len(events), events)
	}
	for _, event := range events {
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["workspace_id"] != workspace || payload["document_id"] != documentID {
			t.Fatalf("cross-workspace system-design event leaked: %+v", event)
		}
	}
}

func assertSystemDesignAndDecisionLineage(t *testing.T, st store.Store, ctx context.Context, originTaskID string) {
	t.Helper()
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Design provenance", Goal: core.PlanningGoalSystemDesign})
	if err != nil {
		t.Fatal(err)
	}
	documentID := "design-" + core.NewTaskID()
	document, first, err := st.CreateSystemDesign(ctx,
		core.SystemDesign{ID: documentID, Title: "Dispatch ownership", Category: "Architecture"},
		core.SystemDesignVersion{Content: "# Dispatch ownership\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: originTaskID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, first.Version, 0); err != nil {
		t.Fatal(err)
	}
	second, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch ownership\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: originTaskID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, second.Version, first.Version); err != nil {
		t.Fatal(err)
	}
	third, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch ownership\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginPlanning, OriginSessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, third.Version, second.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{SessionID: session.ID, SystemDesignID: document.ID, Title: document.Title}); err != nil {
		t.Fatal(err)
	}
	fourth, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch ownership v4\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	fifth, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch ownership v5\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, fifth.Version, third.Version); err != nil {
		t.Fatal(err)
	}
	versions, err := st.ListSystemDesignVersions(ctx, document.ID)
	if err != nil || !versions[fourth.Version-1].Dismissed || versions[fourth.Version-1].Confirmed || !versions[fifth.Version-1].Confirmed {
		t.Fatalf("multi-pending versions=%+v err=%v", versions, err)
	}
	if _, err = st.ProposeDecision(ctx, core.Decision{ID: "DEC-2147483648", Statement: "Reject oversized IDs.", Context: "Sequence storage is int32.", AlternativesRejected: "Overflowing the high-water mark.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID}); err == nil {
		t.Fatal("oversized decision id was accepted")
	}
	firstDecision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Keep dispatch event-derived.", Context: "Lineage must rebuild from durable history.", AlternativesRejected: "Volunteered edges lose provenance.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID})
	if err != nil {
		t.Fatal(err)
	}
	if firstDecision.ID != "DEC-1" {
		t.Fatalf("invalid explicit id advanced sequence: first=%s", firstDecision.ID)
	}
	if _, err = st.ProposeDecision(ctx, core.Decision{ID: firstDecision.ID, Statement: "Duplicate.", Context: "IDs are immutable.", AlternativesRejected: "Overwriting history.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID}); !errors.Is(err, store.ErrDecisionIDConflict) {
		t.Fatalf("duplicate decision error=%v", err)
	}
	if firstDecision, err = st.ConfirmDecision(ctx, firstDecision.ID); err != nil {
		t.Fatal(err)
	}
	secondDecision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Project governance from the same event log.", Context: "All graph edges need stable provenance.", AlternativesRejected: "A second mutable graph would drift.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID, Supersedes: firstDecision.ID})
	if err != nil {
		t.Fatal(err)
	}
	competingDecision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Compete for the same predecessor.", Context: "Proposals must not reserve confirmation slots.", AlternativesRejected: "Proposal-time uniqueness deadlocks recovery.", Origin: core.DecisionOriginImplementation, OriginTaskID: originTaskID, Supersedes: firstDecision.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmDecision(ctx, secondDecision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmDecision(ctx, competingDecision.ID); !errors.Is(err, store.ErrDecisionSupersessionConflict) {
		t.Fatalf("competing confirmation error=%v, want typed supersession conflict", err)
	}
	dismissed, err := st.DismissDecision(ctx, competingDecision.ID)
	if err != nil || dismissed.Status != core.DecisionDismissed || dismissed.DismissedBy == "" || dismissed.DismissedAt.IsZero() {
		t.Fatalf("dismissed decision=%+v err=%v", dismissed, err)
	}
	if _, err = st.DismissDecision(ctx, competingDecision.ID); err != nil {
		t.Fatalf("idempotent dismissal error=%v", err)
	}
	if _, err = st.ConfirmDecision(ctx, competingDecision.ID); !errors.Is(err, store.ErrDecisionSupersessionConflict) {
		t.Fatalf("dismissed confirmation error=%v, want typed lifecycle conflict", err)
	}
	if predecessor, getErr := st.GetDecision(ctx, secondDecision.ID); getErr != nil || predecessor.Status != core.DecisionConfirmed {
		t.Fatalf("dismissal altered confirmed successor=%+v err=%v", predecessor, getErr)
	}
	planningDecision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Record planning provenance.", Context: "Production planning emits session origins.", AlternativesRejected: "Task-only coverage misses the live path.", Origin: core.DecisionOriginPlanning, OriginSessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"system_design:versions":                 false,
		"system_design_version:supersedes":       false,
		"system_design_version:governs-path":     false,
		"system_design_version:proposed-task":    false,
		"system_design_version:proposed-session": false,
		"planning_session:produced-design":       false,
		"decision:proposed_by-task":              false,
		"decision:proposed_by-session":           false,
		"decision:supersedes":                    false,
	}
	for _, link := range links {
		switch {
		case link.SrcType == core.LineageSystemDesign && link.SrcID == document.ID && link.DstType == core.LineageSystemDesignVersion && link.DstID == core.SystemDesignVersionLineageID(document.ID, third.Version) && link.Kind == "versions":
			want["system_design:versions"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(document.ID, third.Version) && link.DstType == core.LineageSystemDesignVersion && link.DstID == core.SystemDesignVersionLineageID(document.ID, second.Version) && link.Kind == "supersedes":
			want["system_design_version:supersedes"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(document.ID, third.Version) && link.DstType == core.LineageRepositoryPath && link.DstID == core.RepoPathComponentLineageID("conveyor", "internal/dispatch/**") && link.Kind == "governs":
			want["system_design_version:governs-path"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(document.ID, second.Version) && link.DstType == core.LineageTask && link.DstID == originTaskID && link.Kind == "proposed_by":
			want["system_design_version:proposed-task"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageSystemDesignVersion && link.SrcID == core.SystemDesignVersionLineageID(document.ID, third.Version) && link.DstType == core.LineagePlanningSession && link.DstID == session.ID && link.Kind == "proposed_by":
			want["system_design_version:proposed-session"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineagePlanningSession && link.SrcID == session.ID && link.DstType == core.LineageSystemDesign && link.DstID == document.ID && link.Kind == "produced_design":
			want["planning_session:produced-design"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageDecision && link.SrcID == secondDecision.ID && link.DstType == core.LineageTask && link.Kind == "proposed_by":
			want["decision:proposed_by-task"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageDecision && link.SrcID == planningDecision.ID && link.DstType == core.LineagePlanningSession && link.DstID == session.ID && link.Kind == "proposed_by":
			want["decision:proposed_by-session"] = link.CreatedByEventID > 0
		case link.SrcType == core.LineageDecision && link.SrcID == secondDecision.ID && link.DstType == core.LineageDecision && link.DstID == firstDecision.ID && link.Kind == "supersedes":
			want["decision:supersedes"] = link.CreatedByEventID > 0
		}
	}
	for edge, found := range want {
		if !found {
			t.Fatalf("missing event-provenanced %s edge: %+v", edge, links)
		}
	}
}

func assertReferenceDocumentLineage(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	documentID := "ref-" + core.NewTaskID()
	document, first, err := st.CreateReferenceDocument(ctx,
		core.ReferenceDocument{ID: documentID, Name: "Lineage conformance " + documentID},
		core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# Overview\n\nInitial context."})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SupersedeReferenceDocument(ctx, document.ID,
		core.ReferenceDocumentVersion{Filename: "overview.md", ContentType: "text/markdown", Content: "# Overview\n\nCurrent context."})
	if err != nil || second.SupersedesVersion != first.Version {
		t.Fatalf("reference supersede=%+v err=%v", second, err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Reference lineage"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.RecordReferenceDocumentConsulted(ctx, document.ID, second.Version, session.ID); err != nil {
		t.Fatal(err)
	}
	requirementID := "req-" + core.NewTaskID()
	requirement, version, err := st.CreateRequirement(ctx,
		core.Requirement{ID: requirementID, Title: "Derived reference lineage"},
		core.RequirementVersion{
			Content: "Derived reference lineage.", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
			Statements:  []core.RequirementStatement{{ID: "REQ-1", Statement: "Reference lineage stays durable."}},
			DerivedFrom: &core.RequirementDerivation{DocumentID: document.ID, Version: second.Version, SectionAnchor: "#overview", TargetID: "REQ-1"},
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]core.LineageLink{
		"consulted": {
			SrcType: core.LineagePlanningSession, SrcID: session.ID,
			DstType: core.LineageReferenceDocumentVersion, DstID: core.ReferenceDocumentVersionLineageID(document.ID, second.Version), Kind: "consulted",
		},
		"derived_from": {
			SrcType: core.LineageRequirementVersion, SrcID: core.RequirementVersionLineageID(requirement.ID, version.Version),
			DstType: core.LineageReferenceDocumentVersion, DstID: core.ReferenceDocumentVersionLineageID(document.ID, second.Version), Kind: "derived_from",
		},
	}
	for _, link := range links {
		candidate, ok := want[link.Kind]
		if ok && link.SrcType == candidate.SrcType && link.SrcID == candidate.SrcID && link.DstType == candidate.DstType && link.DstID == candidate.DstID && link.CreatedByEventID > 0 {
			delete(want, link.Kind)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing corrected reference lineage edges: want=%+v links=%+v", want, links)
	}
}

func assertLineageArtifactOrderingAndBounds(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	now := time.Now().UTC()
	rootID := core.NewTaskID()
	root := core.Task{ID: rootID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + rootID, State: core.TaskRunning, CreatedAt: now}
	if err := st.CreateTask(ctx, root); err != nil {
		t.Fatal(err)
	}
	dependencies := make([]core.Task, 36)
	for i := range dependencies {
		id := core.NewTaskID()
		dependencies[i] = core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, CreatedAt: now.Add(time.Duration(i) * time.Second)}
		if err := st.CreateTask(ctx, dependencies[i]); err != nil {
			t.Fatal(err)
		}
		if err := st.AppendEvent(ctx, core.Event{TaskID: rootID, At: now.Add(time.Duration(i) * time.Second), Kind: "task.dependency_added", Payload: core.JSONPayload(map[string]any{
			"task_id": rootID, "depends_on_task_id": id,
		})}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateArtifact(ctx, core.Artifact{Name: fmt.Sprintf("artifact-%02d.txt", i), ContentType: "text/plain", TaskID: id}, []byte(fmt.Sprintf("unique-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	sharedContent := []byte("content-deduplicated-artifact")
	shared, err := st.CreateArtifact(ctx, core.Artifact{Name: "shared-near.txt", ContentType: "text/plain", TaskID: dependencies[0].ID}, sharedContent)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, repeatErr := st.CreateArtifact(ctx, core.Artifact{Name: "shared-farther.txt", ContentType: "text/plain", TaskID: dependencies[1].ID}, sharedContent); repeatErr != nil || repeated.ID != shared.ID {
		t.Fatalf("deduplicated artifact=%+v err=%v, want id %s", repeated, repeatErr, shared.ID)
	}
	roleContent := []byte("same-owner-two-role-ordering")
	roleArtifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "same-owner-context.txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext, TaskID: rootID, CreatedAt: now}, roleContent)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, repeatErr := st.CreateArtifact(ctx, core.Artifact{Name: "same-owner-output.txt", ContentType: "text/plain", Role: core.ArtifactRoleGeneratedOutput, TaskID: rootID, CreatedAt: now}, roleContent); repeatErr != nil || repeated.ID != roleArtifact.ID {
		t.Fatalf("same-owner role artifact=%+v err=%v, want id %s", repeated, repeatErr, roleArtifact.ID)
	}
	budget := core.LineageTraversalBudget{MaxDepth: 1, MaxNodes: config.DefaultLineageContextNodes, MaxLinks: config.DefaultLineageContextNodes * config.DefaultLineageContextLinksPerNode, Workspace: workspace}
	links, err := st.ListLineageNeighborhood(ctx, []core.LineageNode{{Type: core.LineageTask, ID: rootID}}, budget)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := core.TraverseLineage(links, []core.LineageNode{{Type: core.LineageTask, ID: rootID}}, budget)
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Truncated || len(graph.Nodes) != config.DefaultLineageContextNodes {
		t.Fatalf("over-budget graph=%+v", graph)
	}
	invalid := core.LineageNode{Type: core.LineageNodeType("invalid"), ID: "must-not-match"}
	withInvalid := append(append([]core.LineageNode(nil), graph.Nodes...), invalid)
	artifacts, err := st.ListArtifactsForLineage(ctx, graph.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	invalidArtifacts, err := st.ListArtifactsForLineage(ctx, withInvalid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(artifacts, invalidArtifacts) {
		t.Fatalf("invalid node changed artifact ordering:\nvalid=%+v\ninvalid=%+v", artifacts, invalidArtifacts)
	}
	ranks := make(map[string]int, len(graph.Nodes))
	for i, node := range graph.Nodes {
		ranks[node.ID] = i
	}
	lastRank, sharedNear, sharedFar, outputRole, contextRole := -1, -1, -1, -1, -1
	for i, artifact := range artifacts {
		rank, ok := ranks[artifact.TaskID]
		if !ok || rank < lastRank {
			t.Fatalf("artifact order diverged at %d (%+v), rank=%d after %d", i, artifact, rank, lastRank)
		}
		lastRank = rank
		if artifact.ID == shared.ID && artifact.TaskID == dependencies[0].ID {
			sharedNear = i
		}
		if artifact.ID == shared.ID && artifact.TaskID == dependencies[1].ID {
			sharedFar = i
		}
		if artifact.ID == roleArtifact.ID && artifact.Role == core.ArtifactRoleGeneratedOutput {
			outputRole = i
		}
		if artifact.ID == roleArtifact.ID && artifact.Role == core.ArtifactRoleTaskContext {
			contextRole = i
		}
	}
	if sharedNear < 0 || sharedFar < 0 || sharedNear >= sharedFar {
		t.Fatalf("deduplicated artifact relation order near=%d farther=%d artifacts=%+v", sharedNear, sharedFar, artifacts)
	}
	if outputRole < 0 || contextRole < 0 || outputRole >= contextRole {
		t.Fatalf("same-owner role order output=%d context=%d artifacts=%+v", outputRole, contextRole, artifacts)
	}
}

func assertCanonicalVocabulary(t *testing.T) {
	t.Helper()
	want := []string{"consulted", "creates", "depends_on", "derived_from", "dispatches", "governs", "materializes", "merged_range", "produced_blueprint", "produced_bundle", "produced_design", "produced_requirement", "produced_verdict", "proposed_by", "proposes", "serves", "submitted_as", "submitted_range", "supersedes", "supports", "versions"}
	got := store.CanonicalLineageKinds()
	if len(got) != len(want) {
		t.Fatalf("canonical lineage vocabulary=%v, want %v", got, want)
	}
	for _, kind := range want {
		if _, ok := got[kind]; !ok {
			t.Fatalf("canonical lineage vocabulary omitted %q: %v", kind, got)
		}
	}
}

func assertAbandonedDraftSupersession(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	blueprintID := core.NewTaskID()
	blueprint := core.Task{ID: blueprintID, Workspace: workspaceForFixture(ctx), Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-" + blueprintID, Title: "Abandoned blueprint draft", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, blueprint); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"confirmed v1", "abandoned v2", "proposed v3"} {
		if _, err := st.CreateSpecVersion(ctx, core.SpecVersion{
			TaskID: blueprintID, Content: content,
			Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	_ = findLineageLink(t, st, ctx, "supersedes", core.BlueprintVersionLineageID(blueprintID, 3), core.BlueprintVersionLineageID(blueprintID, 2))
	_ = findLineageLink(t, st, ctx, "supersedes", core.BlueprintVersionLineageID(blueprintID, 2), core.BlueprintVersionLineageID(blueprintID, 1))

	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Lineage conformance"})
	if err != nil {
		t.Fatal(err)
	}
	requirementID := "req-" + core.NewTaskID()
	version := func(content string, statement int) core.RequirementVersion {
		return core.RequirementVersion{
			RequirementID: requirementID, Content: content, Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Intent revision " + content}},
		}
	}
	_, first, err := st.CreateRequirement(ctx, core.Requirement{ID: requirementID, Title: "Intent"}, version("one", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirementID, first.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeRequirementVersion(ctx, version("abandoned", 2)); err != nil {
		t.Fatal(err)
	}
	third, err := st.ProposeRequirementVersion(ctx, version("confirmed", 3))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirementID, third.Version); err != nil {
		t.Fatal(err)
	}
	wantSrc, wantDst := core.RequirementVersionLineageID(requirementID, 3), core.RequirementVersionLineageID(requirementID, 1)
	_ = findLineageLink(t, st, ctx, "supersedes", wantSrc, wantDst)
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirementID, 2); err == nil {
		t.Fatal("late confirmation of superseded version 2 succeeded after version 3")
	} else {
		var conflict *store.RequirementVersionConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("late confirmation error=%v, want RequirementVersionConflict", err)
		}
	}
	assertNoLineageLink(t, st, ctx, "supersedes", core.RequirementVersionLineageID(requirementID, 2), core.RequirementVersionLineageID(requirementID, 3))
}

func workspaceForFixture(ctx context.Context) string {
	workspace, _ := store.WorkspaceFromContext(ctx)
	return workspace
}

func assertEligibleReviewSupport(t *testing.T, st store.Store, ctx context.Context, workspace, reviewer string) {
	t.Helper()
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID,
		Title: reviewer, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: taskID + "-review-1-seat-1", TaskID: taskID, Stage: core.StageReview, State: core.JobRunning}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1}); err != nil {
		t.Fatal(err)
	}
	eligible, err := st.CreateArtifact(ctx, core.Artifact{Name: reviewer + ".png", ContentType: "image/png", Role: core.ArtifactRoleVerificationEvidence, TaskID: taskID}, []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	ineligible, err := st.CreateArtifact(ctx, core.Artifact{Name: reviewer + ".txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext, TaskID: taskID}, []byte("context"))
	if err != nil {
		t.Fatal(err)
	}
	decision := core.ReviewDecision{TaskID: taskID, JobID: job.ID, ReviewWorkOrderID: job.ID,
		ReviewRound: 1, ReviewSeat: 1, Verdict: "changes_requested", ReasonCode: "tests", Summary: reviewer,
		Reviewer: reviewer, EvidenceIDs: []string{eligible.ID}, MaxBounces: 3}
	if err = For(st).AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatal(err)
	}
	first := findLineageLink(t, st, ctx, "supports", eligible.ID, core.VerdictLineageID(job.ID))
	if err = For(st).AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatalf("idempotent review retry: %v", err)
	}
	repeated := findLineageLink(t, st, ctx, "supports", eligible.ID, core.VerdictLineageID(job.ID))
	if repeated.CreatedByEventID != first.CreatedByEventID {
		t.Fatalf("repeated review replaced first provenance: before=%+v after=%+v", first, repeated)
	}
	assertNoLineageLink(t, st, ctx, "supports", ineligible.ID, core.VerdictLineageID(job.ID))
}

func findLineageLink(t *testing.T, st store.Store, ctx context.Context, kind, srcID, dstID string) core.LineageLink {
	t.Helper()
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Kind == kind && link.SrcID == srcID && link.DstID == dstID {
			return link
		}
	}
	t.Fatalf("missing lineage %s %s -> %s in %+v", kind, srcID, dstID, links)
	return core.LineageLink{}
}

func assertNoLineageLink(t *testing.T, st store.Store, ctx context.Context, kind, srcID, dstID string) {
	t.Helper()
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Kind == kind && link.SrcID == srcID && link.DstID == dstID {
			t.Fatalf("unexpected lineage %s %s -> %s: %+v", kind, srcID, dstID, link)
		}
	}
}

func lineageSnapshot(t *testing.T, st store.Store, ctx context.Context) map[string]int64 {
	t.Helper()
	links, err := st.ListLineageLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Kind < links[j].Kind })
	out := map[string]int64{}
	canonical := store.CanonicalLineageKinds()
	for _, link := range links {
		if _, ok := canonical[link.Kind]; !ok {
			continue // retained non-projector-owned legacy link
		}
		if !link.SrcType.Valid() || !link.DstType.Valid() || link.CreatedByEventID == 0 {
			t.Fatalf("invalid lineage link: %+v", link)
		}
		out[link.Kind+"\x00"+link.SrcID+"\x00"+link.DstID] = link.CreatedByEventID
	}
	return out
}

func hasLineageKind(snapshot map[string]int64, kind string) bool {
	for key := range snapshot {
		if len(key) > len(kind) && key[:len(kind)+1] == kind+"\x00" {
			return true
		}
	}
	return false
}
