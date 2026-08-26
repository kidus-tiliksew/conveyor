package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func TestDecisionSupersessionSweepLifecycleIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "decision-sweep-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	createRequirement := func(id, content string) (core.Requirement, core.RequirementVersion) {
		t.Helper()
		requirement, version, createErr := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, core.RequirementVersion{
			Content: content, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep decision citations current."}}, Origin: core.RequirementOriginOperator,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, _, createErr = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); createErr != nil {
			t.Fatal(createErr)
		}
		return requirement, version
	}
	stale, _ := createRequirement("req-stale", "DEC-1 governs this requirement.")
	createRequirement("req-boundary", "DEC-18 governs this requirement.")
	first, err := st.ProposeDecision(ctx, core.Decision{Statement: "Use the first mechanism.", Context: "The corpus needs a stable choice.", AlternativesRejected: "No decision loses traceability.", Origin: core.DecisionOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmDecision(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := st.ProposeDecision(ctx, core.Decision{Statement: "Use the replacement mechanism.", Context: "The prior choice is retired.", AlternativesRejected: "Keeping both choices live is ambiguous.", Origin: core.DecisionOriginOperator, Supersedes: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if second, err = st.ConfirmDecision(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if len(second.Sweep.Entries) != 1 || second.Sweep.Entries[0].DocumentID != stale.ID || second.Sweep.Clean {
		t.Fatalf("initial sweep=%+v", second.Sweep)
	}

	proposed, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: stale.ID, Content: "The replacement governs this requirement.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep decision citations current."}}, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, stale.ID, proposed.Version); err != nil {
		t.Fatal(err)
	}
	second, err = st.GetDecision(ctx, second.ID)
	if err != nil || len(second.Sweep.Entries) != 1 || second.Sweep.Entries[0].Status != core.DecisionSweepAutoCleared || !second.Sweep.Clean {
		t.Fatalf("cleared sweep=%+v err=%v", second.Sweep, err)
	}
	var clearEvents int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1 AND kind='decision.supersession_sweep_auto_cleared' AND payload_json->>'document_id'=$2`, workspace, stale.ID).Scan(&clearEvents); err != nil || clearEvents != 1 {
		t.Fatalf("auto-clear events=%d err=%v", clearEvents, err)
	}

	reopened, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: stale.ID, Content: "DEC-1 is cited again.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep decision citations current."}}, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, stale.ID, reopened.Version); err != nil {
		t.Fatal(err)
	}
	entry, err := st.DismissDecisionSupersessionSweep(store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman}), second.ID, core.DecisionSweepTierRequirement, stale.ID)
	if err != nil || entry.Status != core.DecisionSweepDismissed || entry.ResolvedBy != "operator" {
		t.Fatalf("dismissed entry=%+v err=%v", entry, err)
	}
	var dismissEvents int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1 AND kind='decision.supersession_sweep_dismissed' AND payload_json->>'document_id'=$2`, workspace, stale.ID).Scan(&dismissEvents); err != nil || dismissEvents != 1 {
		t.Fatalf("dismiss events=%d err=%v", dismissEvents, err)
	}

	sibling := store.WithWorkspace(t.Context(), "decision-sweep-sibling-"+core.NewTaskID())
	if _, err = st.GetDecision(sibling, second.ID); err == nil {
		t.Fatal("decision sweep crossed workspace boundary")
	}
}

func TestDecisionSupersessionSweepBackfillIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "decision_sweep_backfill_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(t.Context(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateControlPlaneToVersion(t.Context(), pool, 112); err != nil {
		t.Fatal(err)
	}
	workspace := "backfill-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	st := &Store{pool: pool, queries: db.New(pool)}
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = pool.Exec(ctx, `INSERT INTO reference_documents(workspace_id,id,name,current_version,created_at,updated_at) VALUES($1,'ref-wake','Wake',1,$2,$2)`, workspace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO reference_document_versions(workspace_id,document_id,version,filename,content_type,content,created_by,created_at) VALUES($1,'ref-wake',1,'wake.md','text/markdown','DEC-27 remains cited.','operator',$2)`, workspace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO decisions(workspace_id,id,statement,context,alternatives_rejected,status,origin,confirmed_by,confirmed_at,created_at) VALUES($1,'DEC-27','Old','Context','Alternative','confirmed','operator','operator',$2,$2)`, workspace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO decisions(workspace_id,id,statement,context,alternatives_rejected,status,origin,supersedes,confirmed_by,confirmed_at,created_at) VALUES($1,'DEC-31','New','Context','Alternative','confirmed','operator','DEC-27','operator',$2,$2)`, workspace, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE decisions SET status='superseded',superseded_by='DEC-31' WHERE workspace_id=$1 AND id='DEC-27'`, workspace); err != nil {
		t.Fatal(err)
	}
	if err = migrateControlPlane(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if err = migrateControlPlane(t.Context(), pool); err != nil {
		t.Fatalf("repeated startup migration: %v", err)
	}
	var entries, events int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM decision_supersession_sweeps WHERE workspace_id=$1 AND decision_id='DEC-31' AND document_id='ref-wake' AND status='open'`, workspace).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE workspace_id=$1 AND kind='decision.supersession_sweep_opened' AND payload_json->>'decision_id'='DEC-31'`, workspace).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if entries != 1 || events != 1 {
		t.Fatalf("backfill entries=%d events=%d", entries, events)
	}
}
