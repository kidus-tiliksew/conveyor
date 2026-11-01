package postgres

import (
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestLauncherRetirementMigrationIntegration proves migration 153 removes the
// worker, pairing, model-failure, activity-snapshot, and transcript-capture
// tables and the continuation columns, revokes run-bound agent credentials,
// is idempotent, and leaves worker-actor events readable (task 261101-699ace;
// DEC-97, DEC-94, DEC-98; component-persistence, Launcher-record retirement).
func TestLauncherRetirementMigrationIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	var present int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('workers','worker_pairings','harness_model_failures','work_order_activity_snapshots','work_order_transcript_captures')`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 0 {
		t.Fatalf("%d retired tables remain", present)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'work_orders' AND column_name LIKE 'continuation\_%'`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 0 {
		t.Fatalf("%d continuation columns remain", present)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'work_orders' AND column_name IN ('worker_id','rate_limit','rate_limit_observed_at')`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 3 {
		t.Fatalf("kept work_orders columns = %d, want worker_id, rate_limit, and rate_limit_observed_at", present)
	}

	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Retirement", Email: "retirement@example.test", DisplayName: "Retirement"}, "retirement-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := st.VerifyPersonalAccessToken(ctx, "retirement-token")
	if err != nil {
		t.Fatal(err)
	}
	runChild, err := st.IssueAgentCredential(ctx, owner.ID, `conveyor-run:{"workspace_id":"demo","work_order_id":"order","session_id":"session"}`)
	if err != nil {
		t.Fatal(err)
	}
	plainAgent, err := st.IssueAgentCredential(ctx, owner.ID, "plain agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.VerifyCredential(ctx, runChild.Value); err != nil {
		t.Fatalf("run-bound credential before reapplying the migration: %v", err)
	}
	workspace := "retirement-" + core.NewTaskID()
	if _, err = st.BootstrapWorkspaceConfig(store.WithWorkspace(ctx, workspace), isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	// A worker-actor event recorded before DEC-97, written as the worker
	// plane wrote it; no current writer can (store.ErrRetiredWorkerActor).
	var eventID int64
	if err = st.pool.QueryRow(ctx, `INSERT INTO events (task_id, kind, actor_id, actor_role, payload_json, at, workspace_id)
		VALUES (NULL, 'worker.enrolled', 'worker:retired-worker', 'runner', '{"worker_id":"retired-worker"}', now(), $1) RETURNING id`, workspace).Scan(&eventID); err != nil {
		t.Fatal(err)
	}

	migration, err := migrationFiles.ReadFile("migrations/153_retire_launcher_records.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = st.pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("reapply migration 153: %v", err)
		}
	}
	if _, err = st.VerifyCredential(ctx, runChild.Value); !errors.Is(err, core.ErrInvalidCredential) && !errors.Is(err, ErrInvalidPersonalAccessToken) {
		t.Fatalf("run-bound credential after migration err=%v, want refused", err)
	}
	if credential, verifyErr := st.VerifyCredential(ctx, plainAgent.Value); verifyErr != nil || credential.Kind != core.CredentialAgent {
		t.Fatalf("unbound agent credential=%+v err=%v, want unchanged", credential, verifyErr)
	}
	var actorID, actorRole, payload string
	if err = st.pool.QueryRow(ctx, `SELECT actor_id, actor_role, payload_json::text FROM events WHERE id = $1`, eventID).Scan(&actorID, &actorRole, &payload); err != nil {
		t.Fatal(err)
	}
	if actorID != "worker:retired-worker" || actorRole != string(core.ActorWorker) || payload != `{"worker_id": "retired-worker"}` {
		t.Fatalf("worker-actor event read back as %s/%s %s", actorID, actorRole, payload)
	}
}
