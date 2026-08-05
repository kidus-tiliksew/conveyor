package postgres

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestSystemDesignOwnershipForeignKeysIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "postgres-design-fk-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
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
