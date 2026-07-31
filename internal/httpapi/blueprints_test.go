package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type decompositionFixture struct {
	ID        string   `json:"id"`
	Repo      string   `json:"repo"`
	Summary   string   `json:"summary"`
	DependsOn []string `json:"depends_on"`
}

// materializeBlueprint creates an anchor and fans it out into children through
// the ordinary approval path, so the fixture exercises the same relations the
// projection reads.
func materializeBlueprint(t *testing.T, st store.Store, id string, items []decompositionFixture) core.Task {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	anchor := core.Task{
		ID: id, Workspace: "demo", Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-" + id, Title: "Ship bounded retries",
		Source: "planning", State: core.TaskAwaiting, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, anchor); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: anchor.ID, Content: "# Blueprint\n\nDeliver bounded retries.",
		Decomposition: core.JSONPayload(items),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApproveSpecVersionAndMaterialize(ctx, anchor.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	materialized, err := st.GetTask(ctx, anchor.ID)
	if err != nil {
		t.Fatal(err)
	}
	return materialized
}

func childBySub(t *testing.T, anchor core.Task, sub string) core.TaskRelation {
	t.Helper()
	for _, child := range anchor.Children {
		if child.OriginSubID == sub {
			return child
		}
	}
	t.Fatalf("anchor has no child %s: %+v", sub, anchor.Children)
	return core.TaskRelation{}
}

func listBlueprintViews(t *testing.T, handler http.Handler) []blueprintView {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/blueprints?workspace_id=demo", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("blueprints status=%d body=%s", response.Code, response.Body.String())
	}
	var views []blueprintView
	if err := json.Unmarshal(response.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	return views
}

func listActivityTaskIDs(t *testing.T, handler http.Handler, path string) []string {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
	}
	var items []activityItem
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.Task.ID)
	}
	return ids
}

// AC-1: the board feed and its counts drop blueprint anchors, and the children
// it materialized stay ordinary tasks on it.
func TestActivityFeedExcludesBlueprintAnchorsButKeepsChildren(t *testing.T) {
	st := store.NewMemoryWithConfig(&config.Config{
		Workspace: "demo", Repos: []config.Repo{{Name: "conveyor", Base: "main"}},
	})
	ctx := store.WithWorkspace(t.Context(), "demo")
	anchor := materializeBlueprint(t, st, "anchor-feed", []decompositionFixture{
		{ID: "SUB-1", Repo: "conveyor", Summary: "First"},
		{ID: "SUB-2", Repo: "conveyor", Summary: "Second", DependsOn: []string{"SUB-1"}},
	})
	ordinary := core.Task{
		ID: "ordinary-task", Workspace: "demo", Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-ordinary-task", Title: "Unrelated work",
		State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, ordinary); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()

	feed := listActivityTaskIDs(t, handler, "/v1/activity?workspace_id=demo")
	for _, id := range feed {
		if id == anchor.ID {
			t.Fatalf("board feed still carries the blueprint anchor: %v", feed)
		}
	}
	if len(feed) != 3 {
		t.Fatalf("board feed=%v, want the two children plus the ordinary task", feed)
	}
}

// AC-5: only the ambient board presentation moves — the review inbox
// projection still carries an anchor so its spec-gate card is unchanged.
func TestReviewInboxKeepsBlueprintAnchors(t *testing.T) {
	st := store.NewMemoryWithConfig(&config.Config{
		Workspace: "demo", Repos: []config.Repo{{Name: "conveyor", Base: "main"}},
	})
	ctx := store.WithWorkspace(t.Context(), "demo")
	anchor := materializeBlueprint(t, st, "anchor-inbox", []decompositionFixture{
		{ID: "SUB-1", Repo: "conveyor", Summary: "First"},
	})
	// An anchor awaiting its spec gate has not materialized children yet, so
	// it is not classified as an anchor at all and stays on the board too.
	gated := core.Task{
		ID: "gated-blueprint", Workspace: "demo", Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-gated-blueprint", Title: "Blueprint awaiting approval",
		State: core.TaskAwaiting, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, gated); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()

	inbox := listActivityTaskIDs(t, handler, "/v1/reviews?workspace_id=demo")
	found := false
	for _, id := range inbox {
		if id == anchor.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("review inbox dropped the blueprint anchor: %v", inbox)
	}
	board := listActivityTaskIDs(t, handler, "/v1/activity?workspace_id=demo")
	gatedOnBoard := false
	for _, id := range board {
		if id == gated.ID {
			gatedOnBoard = true
		}
	}
	if !gatedOnBoard {
		t.Fatalf("a blueprint awaiting its spec gate left the board: %v", board)
	}
}

// AC-2/AC-3: the projection carries the governing spec, dependency-ordered
// children, an honest merged-versus-closed rollup, and confirmed serves links.
func TestBlueprintsProjectionReportsDeliveryAndDependencyOrder(t *testing.T) {
	st := store.NewMemoryWithConfig(&config.Config{
		Workspace: "demo", Repos: []config.Repo{{Name: "conveyor", Base: "main"}},
	})
	ctx := store.WithWorkspace(t.Context(), "demo")
	// SUB-3 is declared before the item it depends on, so stored order alone
	// would render a child above its own dependency.
	anchor := materializeBlueprint(t, st, "anchor-delivery", []decompositionFixture{
		{ID: "SUB-1", Repo: "conveyor", Summary: "Foundation"},
		{ID: "SUB-3", Repo: "conveyor", Summary: "Depends on the surface", DependsOn: []string{"SUB-2"}},
		{ID: "SUB-2", Repo: "conveyor", Summary: "Surface", DependsOn: []string{"SUB-1"}},
		{ID: "SUB-4", Repo: "conveyor", Summary: "Independent"},
	})
	intent, err := st.CreatePlanningSession(ctx, core.PlanningSession{
		ID: "session-intent", Title: "Define retry intent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateRequirement(ctx, core.Requirement{
		ID: "req-retries", Slug: "retry-behavior", Title: "Retry behavior",
	}, core.RequirementVersion{
		Content: "Retries stay bounded.", Origin: core.RequirementOriginChat, OriginSessionID: intent.ID,
	}); err != nil {
		t.Fatal(err)
	}
	// A session opened from the requirement ("Plan work") and finalized into
	// this anchor is the confirmed serves link the surface renders.
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{
		ID: "session-anchor", Title: "Plan bounded retries", RequirementContextID: "req-retries",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{
		SessionID: session.ID, TaskID: anchor.ID,
	}); err != nil {
		t.Fatal(err)
	}
	// One child delivers and one closes without merging, so the rollup has to
	// keep the two apart rather than reporting "2 done".
	operations := taskops.New(st)
	first := childBySub(t, anchor, "SUB-1")
	if _, err = operations.Perform(ctx, first.ID, taskops.Command{
		Kind: core.TaskDispatchStart, ProjectStages: true, NextStage: core.StageImplement,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = operations.Perform(ctx, first.ID, taskops.Command{Kind: core.TaskGateMerge}); err != nil {
		t.Fatal(err)
	}
	if _, err = operations.Perform(ctx, first.ID, taskops.Command{Kind: core.TaskInterventionApproveReview}); err != nil {
		t.Fatal(err)
	}
	if _, err = operations.Perform(ctx, first.ID, taskops.Command{Kind: core.TaskMergeConfirm}); err != nil {
		t.Fatal(err)
	}
	second := childBySub(t, anchor, "SUB-4")
	if _, err = operations.Perform(ctx, second.ID, taskops.Command{Kind: core.TaskCancel}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	views := listBlueprintViews(t, server.Handler())

	if len(views) != 1 {
		t.Fatalf("blueprint views=%d, want 1", len(views))
	}
	view := views[0]
	if view.Task.ID != anchor.ID || view.Spec == nil || view.GoverningVersion != view.Spec.Version {
		t.Fatalf("view task=%s spec=%+v governing=%d", view.Task.ID, view.Spec, view.GoverningVersion)
	}
	order := make([]string, 0, len(view.Children))
	for _, child := range view.Children {
		order = append(order, child.OriginSubID)
	}
	want := []string{"SUB-1", "SUB-2", "SUB-3", "SUB-4"}
	for index, sub := range want {
		if index >= len(order) || order[index] != sub {
			t.Fatalf("child order=%v, want %v", order, want)
		}
	}
	if view.Children[1].Repo != "conveyor" || len(view.Children[1].DependsOn) != 1 ||
		view.Children[1].DependsOn[0] != "SUB-1" {
		t.Fatalf("child decomposition context=%+v", view.Children[1])
	}
	if view.Delivery.State != blueprintInDelivery || view.Delivery.Total != 4 ||
		view.Delivery.Merged != 1 || view.Delivery.Closed != 1 || view.Delivery.Open != 2 {
		t.Fatalf("delivery=%+v, want 1 merged and 1 closed kept apart across 4 children", view.Delivery)
	}
	if len(view.Serves) != 1 || view.Serves[0].ID != "req-retries" || view.Serves[0].Title != "Retry behavior" {
		t.Fatalf("serves=%+v, want the requirement the session was planned in", view.Serves)
	}
	materialized := false
	for _, event := range view.Events {
		if event.Kind == "blueprint.materialized" {
			materialized = true
		}
	}
	if !materialized {
		t.Fatalf("blueprint timeline is missing materialization: %+v", view.Events)
	}
}

// Completion and cancellation are both `closed` on the task; only the
// blueprint.closed audit event says delivery finished.
func TestBlueprintDeliveryDistinguishesCompletionFromCancellation(t *testing.T) {
	cancelled := core.Task{
		ID: "anchor-cancelled", State: core.TaskClosed,
		Children: []core.TaskRelation{
			{ID: "child-1", State: core.TaskMerged},
			{ID: "child-2", State: core.TaskClosed},
		},
	}
	delivery := blueprintDeliveryOf(cancelled, []core.Event{{Kind: "task.state_changed"}})
	if delivery.State != blueprintCancelled || delivery.Merged != 1 || delivery.Closed != 1 {
		t.Fatalf("cancelled delivery=%+v", delivery)
	}
	completed := blueprintDeliveryOf(cancelled, []core.Event{{Kind: "blueprint.closed"}})
	if completed.State != blueprintCompleted {
		t.Fatalf("completed delivery=%+v", completed)
	}
	open := blueprintDeliveryOf(core.Task{
		ID: "anchor-open", State: core.TaskQueued,
		Children: []core.TaskRelation{{ID: "child-1", State: core.TaskRunning}},
	}, nil)
	if open.State != blueprintInDelivery || open.Open != 1 {
		t.Fatalf("open delivery=%+v", open)
	}
}

func TestBlueprintsProjectionIsEmptyWithoutAnchors(t *testing.T) {
	st := store.NewMemoryWithConfig(&config.Config{
		Workspace: "demo", Repos: []config.Repo{{Name: "conveyor", Base: "main"}},
	})
	ctx := store.WithWorkspace(t.Context(), "demo")
	if err := st.CreateTask(ctx, core.Task{
		ID: "solo-task", Workspace: "demo", Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-solo-task", State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	if views := listBlueprintViews(t, server.Handler()); len(views) != 0 {
		t.Fatalf("blueprint views=%+v, want none", views)
	}
}
