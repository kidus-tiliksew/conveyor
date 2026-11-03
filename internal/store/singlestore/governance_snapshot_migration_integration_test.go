package singlestore

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestGovernanceSnapshotMigrationMovesInlineRowsIntegration replays migration
// 0031 over pre-digest work_orders that still carry inline governance_snapshot
// bodies. Identical claim-time JSON shares one stored row, GetWorkOrder returns
// the stored digest, a restart converges, and a later claim adds no row.
func TestGovernanceSnapshotMigrationMovesInlineRowsIntegration(t *testing.T) {
	if os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL") == "" {
		t.Skip("CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}

	st := integrationStore(t)
	base := store.WithActor(t.Context(), store.SystemActor())
	ws := "gov-snap-" + core.NewTaskID()
	ctx := store.WithWorkspace(base, ws)
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(context.Background(), `DELETE FROM conveyor_singlestore_migrations WHERE version=31`)
		if err := st.migrate(context.Background()); err != nil {
			t.Logf("restore 0031: %v", err)
		}
	})
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: core.NewTaskID(), Workspace: ws, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	ids := [3]string{task.ID + "-same-1", task.ID + "-same-2", task.ID + "-other"}
	for _, id := range ids {
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
	}

	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(base, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	columnExists := func(table, column string) bool {
		t.Helper()
		exists, err := st.columnExists(base, table, column)
		if err != nil {
			t.Fatal(err)
		}
		return exists
	}

	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=31`)
	exec(`DROP TABLE IF EXISTS governance_snapshots`)
	if columnExists("work_orders", "governance_snapshot_digest") {
		exec(`ALTER TABLE work_orders DROP COLUMN governance_snapshot_digest`)
	}
	if !columnExists("work_orders", "governance_snapshot") {
		exec(`ALTER TABLE work_orders ADD COLUMN governance_snapshot JSON`)
	}

	same := `{"designs":[{"id":"component-persistence","title":"Persistence","category":"component","version":1,"content":"inline-body"}]}`
	other := `{"designs":[{"id":"component-persistence","title":"Persistence","category":"component","version":1,"content":"other-body"}]}`
	bodies := [3]string{same, same, other}
	deadline := now.Add(time.Hour)
	for i, id := range ids {
		if _, err := st.db.ExecContext(base, `INSERT INTO work_orders
			(id,workspace_id,task_id,job_id,stage,state,queue_entered_at,queue_deadline,created_at,updated_at,governance_snapshot)
			VALUES (?,?,?,?, 'review','queued',?,?,?,?,?)`,
			id, ws, task.ID, id, now, deadline, now, now, bodies[i]); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertMigrated := func(label string) {
		t.Helper()
		if columnExists("work_orders", "governance_snapshot") {
			t.Fatalf("%s: legacy governance_snapshot column remains", label)
		}
		if !columnExists("work_orders", "governance_snapshot_digest") {
			t.Fatalf("%s: digest column missing", label)
		}
		var snapshots int
		if err := st.db.QueryRowContext(base, `SELECT count(*) FROM governance_snapshots WHERE workspace_id=?`, ws).Scan(&snapshots); err != nil {
			t.Fatal(err)
		}
		if snapshots != 2 {
			t.Fatalf("%s: snapshot rows=%d want 2", label, snapshots)
		}
		var shared string
		for i, id := range ids {
			var digest string
			var content []byte
			if err := st.db.QueryRowContext(base, `SELECT wo.governance_snapshot_digest, gs.content
				FROM work_orders wo JOIN governance_snapshots gs ON gs.workspace_id=wo.workspace_id AND gs.digest=wo.governance_snapshot_digest
				WHERE wo.workspace_id=? AND wo.id=?`, ws, id).Scan(&digest, &content); err != nil {
				t.Fatalf("%s: %s: %v", label, id, err)
			}
			var snapshot core.GovernanceSnapshot
			if err := json.Unmarshal(content, &snapshot); err != nil {
				t.Fatal(err)
			}
			want, _ := store.EncodeGovernanceSnapshot(&snapshot)
			if digest != want {
				t.Fatalf("%s: %s digest=%q, want EncodeGovernanceSnapshot %q", label, id, digest, want)
			}
			got, err := st.GetWorkOrder(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.GovernanceSnapshotDigest != digest {
				t.Fatalf("%s: GetWorkOrder %s digest=%q, want stored %q", label, id, got.GovernanceSnapshotDigest, digest)
			}
			if got.GovernanceSnapshot == nil || got.GovernanceSnapshot.Designs[0].Content != snapshot.Designs[0].Content {
				t.Fatalf("%s: GetWorkOrder %s snapshot=%+v", label, id, got.GovernanceSnapshot)
			}
			if i == 0 {
				shared = digest
			}
			if i == 1 && digest != shared {
				t.Fatalf("%s: identical governance stored as %q and %q", label, shared, digest)
			}
			if i == 2 && digest == shared {
				t.Fatalf("%s: distinct governance shared digest %q", label, digest)
			}
		}
		var sharedBodies int
		if err := st.db.QueryRowContext(base, `SELECT count(*) FROM governance_snapshots WHERE workspace_id=? AND digest=?`, ws, shared).Scan(&sharedBodies); err != nil {
			t.Fatal(err)
		}
		if sharedBodies != 1 {
			t.Fatalf("%s: identical governance bodies=%d want 1", label, sharedBodies)
		}
	}
	assertMigrated("upgrade")
	if err := st.migrateGovernanceSnapshots(base); err != nil {
		t.Fatal(err)
	}
	assertMigrated("hook replay")
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=31`)
	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertMigrated("restart")

	order, err := st.GetWorkOrder(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ClaimWorkOrder(ctx, st, ids[0], core.WorkOrderClaim{
		ClaimantID: core.TaskRunClaimantID("migration-worker"), SessionID: "migration-session", ClientToken: "migration-token",
		Lease: time.Hour, ExecutionTimeout: time.Hour, Governance: order.GovernanceSnapshot,
	}); err != nil {
		t.Fatal(err)
	}
	assertMigrated("claim")
}
