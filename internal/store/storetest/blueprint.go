package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type BlueprintFixture struct {
	Store     store.Store
	Context   context.Context
	Workspace string
}

type BlueprintFactory func(*testing.T, []config.Repo) BlueprintFixture

type blueprintItem struct {
	ID        string   `json:"id"`
	Repo      string   `json:"repo"`
	Summary   string   `json:"summary"`
	DependsOn []string `json:"depends_on"`
}

// RunBlueprintConformance exercises the externally visible materialization
// contract against any Store implementation.
func RunBlueprintConformance(t *testing.T, factory BlueprintFactory) {
	t.Helper()
	t.Run("versions terminal history dependencies and parent close", func(t *testing.T) {
		repos := []config.Repo{
			{Name: "primary", URL: "https://example.test/primary", Base: "main"},
			{Name: "auxiliary", URL: "https://example.test/auxiliary", Base: "release"},
		}
		fixture := factory(t, repos)
		st, ctx := fixture.Store, fixture.Context
		featureID := "feature-" + core.NewTaskID()
		if err := st.CreateFeature(ctx, core.Feature{
			ID: featureID, Workspace: fixture.Workspace, Name: "Blueprint feature",
		}); err != nil {
			t.Fatal(err)
		}
		parent := blueprintParent(fixture.Workspace, featureID)
		if err := st.CreateTask(ctx, parent); err != nil {
			t.Fatal(err)
		}

		longSummary := strings.Repeat("界", 100)
		v1Items := []blueprintItem{
			{ID: "SUB-1", Repo: "primary", Summary: "Persistence"},
			{ID: "SUB-2", Repo: "auxiliary", Summary: "Runtime", DependsOn: []string{"SUB-1"}},
			{ID: "SUB-3", Repo: "primary", Summary: longSummary, DependsOn: []string{"SUB-2"}},
		}
		v1 := createBlueprintSpec(t, ctx, st, parent.ID, v1Items)
		children := approveBlueprint(t, ctx, st, parent.ID, v1.Version)
		if len(children) != 3 {
			t.Fatalf("v1 children=%d, want 3", len(children))
		}
		bySub := tasksByOrigin(children)
		assertMaterializedChild(t, bySub["SUB-1"], parent, v1.Version, "primary", "main")
		assertMaterializedChild(t, bySub["SUB-2"], parent, v1.Version, "auxiliary", "release")
		assertMaterializedChild(t, bySub["SUB-3"], parent, v1.Version, "primary", "main")
		assertReturnedDependency(t, bySub["SUB-2"], bySub["SUB-1"].ID, true)
		assertReturnedDependency(t, bySub["SUB-3"], bySub["SUB-2"].ID, true)
		if len(bySub["SUB-3"].Title) > 200 || !utf8.ValidString(bySub["SUB-3"].Title) {
			t.Fatalf("multibyte title bytes=%d valid=%v", len(bySub["SUB-3"].Title), utf8.ValidString(bySub["SUB-3"].Title))
		}
		assertBlocking(t, ctx, st, bySub["SUB-2"].ID, bySub["SUB-1"].ID)
		assertMaterializedEvent(t, ctx, st, parent.ID, 1, v1.Version, 3, 3)

		transitionBlueprintTaskToMerged(t, ctx, st, bySub["SUB-1"].ID)
		repeated := approveBlueprint(t, ctx, st, parent.ID, v1.Version)
		assertSameOrigins(t, repeated, bySub, v1.Version)
		repeatedBySub := tasksByOrigin(repeated)
		assertReturnedDependency(t, repeatedBySub["SUB-2"], bySub["SUB-1"].ID, false)
		assertTaskCount(t, ctx, st, 4)
		assertBlocking(t, ctx, st, bySub["SUB-2"].ID)
		assertMaterializedEvent(t, ctx, st, parent.ID, 1, v1.Version, 3, 3)

		v2Items := append([]blueprintItem(nil), v1Items...)
		v2Items[1].Summary = "Revised runtime summary"
		v2 := createBlueprintSpec(t, ctx, st, parent.ID, v2Items)
		revised := approveBlueprint(t, ctx, st, parent.ID, v2.Version)
		assertSameOrigins(t, revised, bySub, v1.Version)
		assertTaskCount(t, ctx, st, 4)
		assertMaterializedEvent(t, ctx, st, parent.ID, 1, v1.Version, 3, 3)

		v3Items := append(v2Items, blueprintItem{
			ID: "SUB-4", Repo: "auxiliary", Summary: "New worker", DependsOn: []string{"SUB-1"},
		})
		v3 := createBlueprintSpec(t, ctx, st, parent.ID, v3Items)
		expanded := approveBlueprint(t, ctx, st, parent.ID, v3.Version)
		if len(expanded) != 4 {
			t.Fatalf("expanded children=%d, want 4", len(expanded))
		}
		expandedBySub := tasksByOrigin(expanded)
		assertSameOrigins(t, expanded[:3], bySub, v1.Version)
		assertMaterializedChild(t, expandedBySub["SUB-4"], parent, v3.Version, "auxiliary", "release")
		assertReturnedDependency(t, expandedBySub["SUB-4"], bySub["SUB-1"].ID, false)
		assertBlocking(t, ctx, st, expandedBySub["SUB-4"].ID)
		assertTaskCount(t, ctx, st, 5)
		assertMaterializedEvent(t, ctx, st, parent.ID, 2, v3.Version, 1, 4)

		// The governing blueprint is the newest *approved* version, which the
		// children cannot report: reuse means every child here still records
		// v1 except SUB-4. A later draft has materialized nothing, so it never
		// displaces the version delivery answers to.
		assertApprovedSpecVersion(t, ctx, st, parent.ID, v3.Version)
		draft := createBlueprintSpec(t, ctx, st, parent.ID, append(v3Items, blueprintItem{
			ID: "SUB-5", Repo: "primary", Summary: "Proposed but unapproved",
		}))
		if draft.Version <= v3.Version || draft.Approved {
			t.Fatalf("draft=%+v, want an unapproved version above %d", draft, v3.Version)
		}
		assertApprovedSpecVersion(t, ctx, st, parent.ID, v3.Version)
		assertTaskCount(t, ctx, st, 5)

		for _, subID := range []string{"SUB-2", "SUB-3", "SUB-4"} {
			if _, err := taskops.New(st).Perform(ctx, expandedBySub[subID].ID, taskops.Command{Kind: core.TaskCancel}); err != nil {
				t.Fatalf("cancel %s: %v", subID, err)
			}
		}
		closed, err := st.GetTask(ctx, parent.ID)
		if err != nil || closed.State != core.TaskClosed {
			t.Fatalf("parent=%+v err=%v, want closed", closed, err)
		}
	})

	t.Run("configured repository validation and empty decomposition", func(t *testing.T) {
		fixture := factory(t, []config.Repo{{Name: "primary", URL: "https://example.test/primary", Base: "main"}})
		st, ctx := fixture.Store, fixture.Context
		parent := blueprintParent(fixture.Workspace, "")
		if err := st.CreateTask(ctx, parent); err != nil {
			t.Fatal(err)
		}
		empty := createBlueprintSpec(t, ctx, st, parent.ID, nil)
		if err := st.ApproveSpecVersion(ctx, parent.ID, empty.Version); err != nil {
			t.Fatal(err)
		}
		assertTaskCount(t, ctx, st, 1)
		if count, err := st.CountEvents(ctx, parent.ID, "blueprint.materialized"); err != nil || count != 0 {
			t.Fatalf("empty materialized events=%d err=%v", count, err)
		}

		invalid := createBlueprintSpec(t, ctx, st, parent.ID, []blueprintItem{{
			ID: "SUB-1", Repo: "missing", Summary: "Must fail",
		}})
		if _, err := st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, invalid.Version); err == nil ||
			!strings.Contains(err.Error(), `repository "missing" is not configured`) {
			t.Fatalf("unconfigured repository error=%v", err)
		}
		latest, ok, err := st.GetLatestSpecVersion(ctx, parent.ID)
		if err != nil || !ok || latest.Approved {
			t.Fatalf("latest=%+v ok=%v err=%v, want unapproved", latest, ok, err)
		}
		assertTaskCount(t, ctx, st, 1)
	})

	t.Run("ordinary approval materializes", func(t *testing.T) {
		fixture := factory(t, []config.Repo{{Name: "primary", URL: "https://example.test/primary", Base: "main"}})
		st, ctx := fixture.Store, fixture.Context
		parent := blueprintParent(fixture.Workspace, "")
		if err := st.CreateTask(ctx, parent); err != nil {
			t.Fatal(err)
		}
		spec := createBlueprintSpec(t, ctx, st, parent.ID, []blueprintItem{{
			ID: "SUB-1", Repo: "primary", Summary: "Created through ApproveSpecVersion",
		}})
		if _, err := st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, spec.Version); err != nil {
			t.Fatal(err)
		}
		assertTaskCount(t, ctx, st, 2)
		assertMaterializedEvent(t, ctx, st, parent.ID, 1, spec.Version, 1, 1)
	})
}

func blueprintParent(workspace, featureID string) core.Task {
	id := core.NewTaskID()
	return core.Task{
		ID: id, Workspace: workspace, Source: "test:blueprint", Title: "Blueprint",
		Body: "Parent body", Class: "feature", Level: "L2", Hold: true,
		SpecApproval: true, MergeApproval: true, PolicyVersion: 7,
		SetupName: "default", Repo: "primary", BaseBranch: "main",
		Branch: "conveyor/task-" + id, State: core.TaskQueued,
		NextStage: core.StageImplement, FeatureID: featureID, CreatedAt: time.Now().UTC(),
	}
}

func createBlueprintSpec(t *testing.T, ctx context.Context, st store.Store, taskID string, items []blueprintItem) core.SpecVersion {
	t.Helper()
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: taskID, Content: "blueprint", Acceptance: core.JSONPayload([]any{}),
		Decomposition: core.JSONPayload(items),
	})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func approveBlueprint(t *testing.T, ctx context.Context, st store.Store, taskID string, version int) []core.Task {
	t.Helper()
	children, err := st.ApproveSpecVersionAndMaterialize(ctx, taskID, version)
	if err != nil {
		t.Fatal(err)
	}
	return children
}

func tasksByOrigin(tasks []core.Task) map[string]core.Task {
	result := make(map[string]core.Task, len(tasks))
	for _, task := range tasks {
		result[task.OriginSubID] = task
	}
	return result
}

func assertMaterializedChild(t *testing.T, child, parent core.Task, version int, repo, base string) {
	t.Helper()
	if child.ID == "" || child.Workspace != parent.Workspace || child.Source == "" ||
		child.Title == "" || child.Body == "" || child.Class != parent.Class ||
		child.Level != parent.Level || child.Hold != parent.Hold ||
		child.SpecApproval != parent.SpecApproval || child.MergeApproval != parent.MergeApproval ||
		child.PolicyVersion != parent.PolicyVersion || child.SetupName != parent.SetupName ||
		!reflect.DeepEqual(child.SetupContract, parent.SetupContract) ||
		child.Repo != repo || child.BaseBranch != base || child.Branch == "" ||
		child.State != core.TaskQueued || child.NextStage != core.StageImplement ||
		child.ParentTaskID != parent.ID || child.OriginSpecVersion != version ||
		child.OriginSubID == "" || child.FeatureID != "" || child.CreatedAt.IsZero() {
		t.Fatalf("partially populated or invalid child: %+v", child)
	}
}

func assertSameOrigins(t *testing.T, got []core.Task, want map[string]core.Task, originVersion int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("children=%d, want %d", len(got), len(want))
	}
	for _, child := range got {
		original, ok := want[child.OriginSubID]
		if !ok || child.ID != original.ID || child.OriginSpecVersion != originVersion {
			t.Fatalf("child %+v did not retain original %+v", child, original)
		}
		if child.Workspace == "" || child.Source == "" || child.Body == "" ||
			child.Repo == "" || child.BaseBranch == "" || child.Branch == "" || child.CreatedAt.IsZero() {
			t.Fatalf("repeat approval returned partially populated child: %+v", child)
		}
	}
}

func assertReturnedDependency(t *testing.T, task core.Task, dependencyID string, blocking bool) {
	t.Helper()
	if len(task.Dependencies) != 1 || task.Dependencies[0].ID != dependencyID {
		t.Fatalf("returned dependencies for %s=%+v, want %s", task.ID, task.Dependencies, dependencyID)
	}
	wantBlocking := 0
	if blocking {
		wantBlocking = 1
	}
	if len(task.BlockingTaskIDs) != wantBlocking ||
		(blocking && task.BlockingTaskIDs[0] != dependencyID) {
		t.Fatalf("returned blocking IDs for %s=%v, blocking=%v dependency=%s",
			task.ID, task.BlockingTaskIDs, blocking, dependencyID)
	}
}

func assertApprovedSpecVersion(t *testing.T, ctx context.Context, st store.Store, taskID string, want int) {
	t.Helper()
	spec, ok, err := st.GetApprovedSpecVersion(ctx, taskID)
	if err != nil || !ok {
		t.Fatalf("approved spec for %s: ok=%v err=%v", taskID, ok, err)
	}
	if spec.Version != want || !spec.Approved || spec.Content == "" || len(spec.Decomposition) == 0 {
		t.Fatalf("approved spec=%+v, want fully populated version %d", spec, want)
	}
}

func assertTaskCount(t *testing.T, ctx context.Context, st store.Store, want int) {
	t.Helper()
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != want {
		t.Fatalf("tasks=%d, want %d", len(tasks), want)
	}
}

func assertBlocking(t *testing.T, ctx context.Context, st store.Store, taskID string, want ...string) {
	t.Helper()
	got, err := st.ListBlockingTaskIDs(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("blocking %s=%v, want %v", taskID, got, want)
	}
}

func assertMaterializedEvent(t *testing.T, ctx context.Context, st store.Store, taskID string, wantCount, version, created, total int) {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var materialized []core.Event
	for _, event := range events {
		if event.Kind == "blueprint.materialized" {
			materialized = append(materialized, event)
		}
	}
	if len(materialized) != wantCount {
		t.Fatalf("materialized events=%d, want %d", len(materialized), wantCount)
	}
	var payload struct {
		Version         int `json:"version"`
		ChildrenCreated int `json:"children_created"`
		ChildrenTotal   int `json:"children_total"`
	}
	if err := json.Unmarshal(materialized[len(materialized)-1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Version != version || payload.ChildrenCreated != created || payload.ChildrenTotal != total {
		t.Fatalf("materialized payload=%+v, want version=%d created=%d total=%d", payload, version, created, total)
	}
}

func transitionBlueprintTaskToMerged(t *testing.T, ctx context.Context, st store.Store, id string) {
	t.Helper()
	for _, command := range []taskops.Command{
		{Kind: core.TaskDispatchStart},
		{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true},
		{Kind: core.TaskDispatchStart},
		{Kind: core.TaskGateMerge},
		{Kind: core.TaskInterventionApproveReview},
		{Kind: core.TaskMergeConfirm},
	} {
		if _, err := taskops.New(st).Perform(ctx, id, command); err != nil {
			t.Fatalf("%s on %s: %v", command.Kind, id, err)
		}
	}
}
