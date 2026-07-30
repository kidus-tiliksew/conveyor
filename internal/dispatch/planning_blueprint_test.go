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
