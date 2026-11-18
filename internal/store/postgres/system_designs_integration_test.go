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

// TestListGovernanceDesignsOneStatementIntegration proves the governance
// read is one statement that returns content, admits no design of another
// repository, and succeeds when another repository's design holds a governs
// value that does not decode (req-task-authority-boundary REQ-5).
func TestListGovernanceDesignsOneStatementIntegration(t *testing.T) {
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "postgres-governance-one-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}, {Name: "other", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	for _, fixture := range []struct{ id, fence string }{
		{"design-a", "- repo: conveyor\n  paths:\n    - internal/**"},
		{"design-b", "- repo: conveyor\n  paths:\n    - cmd/**\n- repo: other\n  paths:\n    - web/**"},
		{"design-other", "- repo: other\n  paths:\n    - internal/**"},
		{"design-other-bad", "- repo: other\n  paths:\n    - docs/**"},
		{"design-other-bad-repo", "- repo: other\n  paths:\n    - scripts/**"},
	} {
		content := "# " + fixture.id + "\n\n```conveyor:governs\n" + fixture.fence + "\n```"
		document, version, createErr := st.CreateSystemDesign(ctx, core.SystemDesign{ID: fixture.id, Title: fixture.id, Category: "Architecture"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, _, createErr = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version); createErr != nil {
			t.Fatal(createErr)
		}
		contents[fixture.id] = content
	}
	// No command writes a governs value that does not decode, so the two
	// other-repository rows are rewritten directly: one holds a scalar scope
	// element and one names its repository with a number.
	for id, raw := range map[string]string{
		"design-other-bad":      `[{"repository":"other","paths":["docs/**"]},5]`,
		"design-other-bad-repo": `[{"repository":7,"paths":["scripts/**"]},{"repository":"other","paths":"scripts/**"}]`,
	} {
		tag, execErr := st.pool.Exec(ctx, `UPDATE system_design_versions v SET governs=$3::jsonb FROM system_designs d WHERE d.workspace_id=v.workspace_id AND d.id=v.document_id AND v.version=d.current_version AND v.workspace_id=$1 AND v.document_id=$2`, workspace, id, raw)
		if execErr != nil || tag.RowsAffected() != 1 {
			t.Fatalf("rewrite %s governs: rows=%d err=%v", id, tag.RowsAffected(), execErr)
		}
	}
	st.ReadStats(1 << 16) // start from an empty interval
	designs, err := st.ListGovernanceDesigns(ctx, "conveyor")
	if err != nil {
		t.Fatalf("governance read failed on another repository's undecodable governs: %v", err)
	}
	if got := readAccountingTotals(st.ReadStats(1 << 16))["ListGovernanceDesigns"].Queries; got != 1 {
		t.Fatalf("ListGovernanceDesigns statements = %d, want 1", got)
	}
	got := make([]string, 0, len(designs))
	for _, design := range designs {
		got = append(got, design.ID)
		if design.Content != contents[design.ID] {
			t.Fatalf("%s content = %q, want %q", design.ID, design.Content, contents[design.ID])
		}
		if !core.GovernsRepositoryPaths(design.Governs, "conveyor") {
			t.Fatalf("%s returned without a conveyor scope: %+v", design.ID, design.Governs)
		}
	}
	if want := []string{"design-a", "design-b"}; !slices.Equal(got, want) {
		t.Fatalf("conveyor governance = %v, want %v", got, want)
	}
	// The same malformed rows still fail the read of the repository they
	// govern, so a malformed row never silently widens or narrows governance.
	if _, err = st.ListGovernanceDesigns(ctx, "other"); err == nil {
		t.Fatal("other governance read decoded a malformed governs value")
	}
}
