package dispatch

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"gopkg.in/yaml.v3"
)

func TestCreatePlanningBlueprintUsesExistingSpecGate(t *testing.T) {
	document := implementationModelDocument(config.ModelPolicyExplicit, "gpt-implement", nil)
	document.Repos = []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}
	document.Execution = config.ExecutionPolicy{SpecApproval: true, MergeApproval: true}
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseWorkspaceDocument(raw, &config.Config{Workspace: "demo", PackDir: "."}, "planning blueprint test")
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	requirement, _, err := st.CreateRequirement(ctx,
		core.Requirement{ID: "req-retries", Title: "Retry policy"},
		core.RequirementVersion{
			Content: "Retries are bounded.",
			Statements: []core.RequirementStatement{{
				ID: "REQ-1", Statement: "Retries stop at the configured bound.",
			}},
			Origin: core.RequirementOriginChat, OriginSessionID: "session-seed",
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreatePlanningSession(ctx, core.PlanningSession{
		ID: "session-260730-c0ffee", RequirementContextID: requirement.ID,
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, cfg, nil)
	value := pipeline.StructuredSpec{
		Markdown: "## Intent\n\nBound retries.\n\n## Non-goals\n\nNo queue rewrite.",
		Acceptance: []pipeline.AcceptanceCriterion{{
			ID: "AC-1", Criterion: "Retries stop at the configured bound.", Verify: "test",
		}},
	}
	task, version, err := dispatcher.CreatePlanningBlueprint(
		ctx, "session-260730-c0ffee", "260730-c0ffee", "Bound retries",
		"conveyor", value, "gpt-planner",
	)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != core.TaskAwaiting || task.NextStage != "" || task.RecoveryStage != core.StageImplement ||
		task.Source != "planning:session-260730-c0ffee" || !task.SpecApproval ||
		version.Version != 1 || version.Approved || version.Model != "gpt-planner" ||
		version.Agent != "planning-agent" {
		t.Fatalf("task=%+v version=%+v", task, version)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 0 {
		t.Fatalf("planning blueprint work orders=%+v err=%v", orders, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	suggestions := 0
	for _, event := range events {
		if event.Kind == "task.requirement_suggested" {
			suggestions++
		}
	}
	if suggestions != 1 {
		t.Fatalf("requirement suggestions=%d events=%+v", suggestions, events)
	}
	repeatedTask, repeatedVersion, err := dispatcher.CreatePlanningBlueprint(
		ctx, "session-260730-c0ffee", "260730-c0ffee", "Bound retries",
		"conveyor", value, "gpt-planner",
	)
	if err != nil || repeatedTask.ID != task.ID || repeatedVersion.Version != version.Version {
		t.Fatalf("repeat task=%+v version=%+v err=%v", repeatedTask, repeatedVersion, err)
	}
	// Model the crash window after a gates-off workspace has already approved
	// the first spec. A revised retry must create a new unapproved §4.1 version
	// without replacing the approved version that may already be in delivery.
	if err = st.ApproveSpecVersion(ctx, task.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	revised := value
	revised.Markdown = "## Intent\n\nBound retries with jitter.\n\n## Non-goals\n\nNo queue rewrite."
	revisedTask, revisedVersion, err := dispatcher.CreatePlanningBlueprint(
		ctx, "session-260730-c0ffee", "260730-c0ffee", "Bound retries",
		"conveyor", revised, "gpt-planner",
	)
	if err != nil || revisedTask.ID != task.ID || revisedVersion.Version != 2 || revisedVersion.Approved ||
		revisedVersion.Content == version.Content {
		t.Fatalf("revised orphan task=%+v version=%+v err=%v", revisedTask, revisedVersion, err)
	}
	approved, exists, err := st.GetApprovedSpecVersion(ctx, task.ID)
	if err != nil || !exists || approved.Version != version.Version {
		t.Fatalf("approved delivery version=%+v exists=%t err=%v", approved, exists, err)
	}
	if _, _, err = dispatcher.CreatePlanningBlueprint(
		ctx, "session-other", "260730-c0ffee", "Bound retries", "conveyor", revised, "gpt-planner",
	); err == nil {
		t.Fatal("cross-session deterministic task collision was adopted")
	}
	events, err = st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	suggestions = 0
	for _, event := range events {
		if event.Kind == "task.requirement_suggested" {
			suggestions++
		}
	}
	if suggestions != 1 {
		t.Fatalf("idempotent repeat requirement suggestions=%d", suggestions)
	}
}
