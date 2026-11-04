package postgres

import (
	"slices"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestSystemDesignOwnershipForeignKeysIntegration(t *testing.T) {
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "postgres-design-fk-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	document, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-owned", Title: "Owned design", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Owned\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-owned", Goal: core.PlanningGoalSystemDesign, SystemDesignContextID: document.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{SessionID: session.ID, SystemDesignID: document.ID, Title: "Owned design"}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `DELETE FROM system_designs WHERE workspace_id=$1 AND id=$2`, workspace, document.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetPlanningSession(ctx, session.ID)
	if err != nil || stored.SystemDesignContextID != "" || stored.ProducedSystemDesignID != "" {
		t.Fatalf("planning session=%+v err=%v", stored, err)
	}
	var versions int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM system_design_versions WHERE workspace_id=$1 AND document_id=$2`, workspace, document.ID).Scan(&versions); err != nil || versions != 0 {
		t.Fatalf("remaining versions=%d err=%v", versions, err)
	}
}

func TestListGovernanceDesignsFiltersBeforeLoadingContentIntegration(t *testing.T) {
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "postgres-governance-scope-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}, {Name: "other", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	// The two mixed fixtures hold the repository and a non-empty list on
	// different scope elements, so a filter testing them independently fails
	// (req-task-authority-boundary REQ-5).
	for _, fixture := range []struct{ id, fence string }{
		{"design-conveyor", "- repo: conveyor\n  paths:\n    - internal/**"},
		{"design-other", "- repo: other\n  paths:\n    - internal/**"},
		{"design-no-path", "- repo: conveyor\n  paths: []"},
		{"design-empty-here", "- repo: conveyor\n  paths: []\n- repo: other\n  paths:\n    - internal/**"},
		{"design-empty-elsewhere", "- repo: conveyor\n  paths:\n    - cmd/**\n- repo: other\n  paths: []"},
	} {
		document, version, createErr := st.CreateSystemDesign(ctx, core.SystemDesign{ID: fixture.id, Title: fixture.id, Category: "Architecture"}, core.SystemDesignVersion{Content: "# " + fixture.id + "\n\n```conveyor:governs\n" + fixture.fence + "\n```", Origin: core.SystemDesignOriginOperator})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, _, createErr = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version); createErr != nil {
			t.Fatal(createErr)
		}
	}
	for repository, want := range map[string][]string{
		"conveyor": {"design-conveyor", "design-empty-elsewhere"},
		"other":    {"design-empty-here", "design-other"},
		"absent":   {},
	} {
		designs, listErr := st.ListGovernanceDesigns(ctx, repository)
		if listErr != nil {
			t.Fatal(listErr)
		}
		got := make([]string, 0, len(designs))
		for _, design := range designs {
			if design.Content == "" {
				t.Fatalf("%s governance omitted content: %+v", repository, design)
			}
			got = append(got, design.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s governance=%v want %v", repository, got, want)
		}
	}
}
