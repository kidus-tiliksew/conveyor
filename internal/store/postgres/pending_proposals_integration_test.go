package postgres

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPendingProposalsProjectionIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "pending-proposals-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	taskID := core.NewTaskID()
	if err = st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Pending proposal provenance"})
	if err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-pending", Title: "Pending design", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Pending\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: taskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	requirement, requirementVersion, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-pending", Title: "Pending requirement"}, core.RequirementVersion{
		Content: "Pending", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Surface pending authority."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	newerRequirementVersion, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
		RequirementID: requirement.ID, Content: "Pending newer", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Surface pending authority promptly."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Pending decision", Context: "Projection", AlternativesRejected: "Hidden proposal", Origin: core.DecisionOriginImplementation, OriginTaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.ListPendingProposals(ctx)
	if err != nil || len(items) != 4 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.Tier] = true
		if item.ID == "" || item.Title == "" || item.ProposedAt.IsZero() {
			t.Fatalf("incomplete item=%+v", item)
		}
	}
	if items[0].OriginID == "" && items[1].OriginID == "" && items[2].OriginID == "" {
		t.Fatalf("origin provenance missing: %+v", items)
	}
	if !seen["system_design"] || !seen["requirement"] || !seen["decision"] {
		t.Fatalf("tiers=%v", seen)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, requirementVersion.Version); err != nil {
		t.Fatal(err)
	}
	items, err = st.ListPendingProposals(ctx)
	newerVisible := false
	for _, item := range items {
		newerVisible = newerVisible || (item.Tier == "requirement" && item.Version == newerRequirementVersion.Version)
	}
	if err != nil || len(items) != 2 || !newerVisible {
		t.Fatalf("newer pending requirement lost after older confirmation: items=%+v err=%v", items, err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, newerRequirementVersion.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DismissDecision(store.WithActor(ctx, store.Actor{ID: "operator", Role: "operator"}), decision.ID); err != nil {
		t.Fatal(err)
	}
	items, err = st.ListPendingProposals(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("resolved items=%+v err=%v", items, err)
	}
}
