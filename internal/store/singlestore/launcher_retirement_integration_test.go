package singlestore

import (
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestLauncherRetirementMigrationIntegration proves migration 0028 and its
// column hook remove the worker, pairing, activity-snapshot, and
// transcript-capture tables and the continuation columns, revoke run-bound
// agent credentials, converge on retry, and leave worker-actor events readable
// (task 261101-699ace; DEC-97, DEC-94, DEC-98; component-persistence,
// Launcher-record retirement).
func TestLauncherRetirementMigrationIntegration(t *testing.T) {
	s := integrationStore(t)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	retiredTables := "('workers','worker_pairings','work_order_activity_snapshots','work_order_transcript_captures')"
	continuationColumns := "('continuation_session_id','continuation_attempt_id','continuation_harness','continuation_launch_environment')"
	requireAbsent := func(stage string) {
		t.Helper()
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN "+retiredTables).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: retired tables=%d err=%v", stage, count, err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='work_orders' AND column_name IN "+continuationColumns).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: continuation columns=%d err=%v", stage, count, err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='work_orders' AND column_name IN ('worker_id','rate_limit','rate_limit_observed_at')").Scan(&count); err != nil || count != 3 {
			t.Fatalf("%s: kept work_orders columns=%d err=%v", stage, count, err)
		}
	}
	requireAbsent("fresh migration")

	// Restore the retired tables and columns from the published schema to model
	// a pre-upgrade database, then apply the hook and file twice.
	raw, err := migrationFiles.ReadFile("migrations/0001_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(string(raw), ";") {
		for _, table := range []string{"workers", "worker_pairings", "work_order_activity_snapshots", "work_order_transcript_captures"} {
			if strings.Contains(statement, "CREATE ROWSTORE TABLE IF NOT EXISTS `"+table+"`") {
				if _, err := s.db.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, column := range []string{"continuation_session_id", "continuation_attempt_id", "continuation_harness", "continuation_launch_environment"} {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE work_orders ADD COLUMN "+column+" LONGTEXT NOT NULL DEFAULT ''"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Retirement", Email: "retirement@example.test", DisplayName: "Retirement"}, "retirement-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := s.VerifyPersonalAccessToken(ctx, "retirement-token")
	if err != nil {
		t.Fatal(err)
	}
	runChild, err := s.IssueAgentCredential(ctx, owner.ID, `conveyor-run:{"workspace_id":"demo","work_order_id":"order","session_id":"session"}`)
	if err != nil {
		t.Fatal(err)
	}
	plainAgent, err := s.IssueAgentCredential(ctx, owner.ID, "plain agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.VerifyCredential(ctx, runChild.Value); err != nil {
		t.Fatalf("run-bound credential before the migration: %v", err)
	}
	// A worker-actor event recorded before DEC-97; no current writer can.
	if _, err = s.db.ExecContext(ctx, "INSERT INTO deployment_events(kind,actor_id,actor_role,payload_json,at) VALUES('worker.revoked','worker:retired-worker','runner','{\"worker_id\":\"retired-worker\"}',CURRENT_TIMESTAMP(6))"); err != nil {
		t.Fatal(err)
	}
	file, err := migrationFiles.ReadFile("migrations/0028_retire_launcher_records.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.migrateLauncherRetirement(ctx); err != nil {
			t.Fatal(err)
		}
		for _, statement := range strings.Split(string(file), ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := s.db.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	requireAbsent("reapplied migration")
	if _, err = s.VerifyCredential(ctx, runChild.Value); !errors.Is(err, core.ErrInvalidCredential) {
		t.Fatalf("run-bound credential after migration err=%v, want invalid", err)
	}
	if credential, verifyErr := s.VerifyCredential(ctx, plainAgent.Value); verifyErr != nil || credential.Kind != core.CredentialAgent {
		t.Fatalf("unbound agent credential=%+v err=%v, want unchanged", credential, verifyErr)
	}
	var actorID, actorRole string
	if err = s.db.QueryRowContext(ctx, "SELECT actor_id,actor_role FROM deployment_events WHERE kind='worker.revoked' AND actor_id='worker:retired-worker'").Scan(&actorID, &actorRole); err != nil {
		t.Fatal(err)
	}
	if actorRole != string(core.ActorWorker) {
		t.Fatalf("worker-actor event read back as %s/%s", actorID, actorRole)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
