package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestGovernanceSnapshotMigrationMovesInlineRowsIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "gov_snap_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
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
	t.Cleanup(pool.Close)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	if err = migrateControlPlaneToVersion(ctx, pool, 155); err != nil {
		t.Fatalf("migrate to 155: %v", err)
	}
	workspace := "gov-snap-" + core.NewTaskID()
	if _, err = pool.Exec(ctx, `INSERT INTO workspaces(id, name) VALUES ($1, $1)`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO repos(workspace_id, name, url, default_base) VALUES ($1, 'conveyor', 'https://example.test/conveyor', 'main')`, workspace); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	jobID := core.NewTaskID()
	orderID := jobID
	if _, err = pool.Exec(ctx, `INSERT INTO tasks
		(id,workspace_id,source,title,body,class,escalation_level,repo_name,base_branch,branch,state,created_at,mode,spec_approval,merge_approval)
		VALUES ($1,$2,'test','snapshot','','','L2','conveyor','main',$3,'queued',$4,'',true,true)`,
		taskID, workspace, "conveyor/task-"+taskID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO jobs (id, task_id, stage, harness, runner, confinement_tier, state)
		VALUES ($1,$2,'review','','','','pending')`, jobID, taskID); err != nil {
		t.Fatal(err)
	}
	inline := `{"designs":[{"id":"component-persistence","title":"Persistence","category":"component","version":1,"content":"inline-body"}]}`
	if _, err = pool.Exec(ctx, `INSERT INTO work_orders
		(id,workspace_id,task_id,job_id,stage,state,queue_entered_at,queue_deadline,created_at,updated_at,governance_snapshot)
		VALUES ($1,$2,$3,$4,'review','queued',$5,$6,$5,$5,$7::jsonb)`,
		orderID, workspace, taskID, jobID, now, now.Add(time.Hour), inline); err != nil {
		t.Fatal(err)
	}
	historical := `{"id":"` + orderID + `","governance_snapshot":` + inline + `}`
	if _, err = pool.Exec(ctx, `INSERT INTO events (workspace_id,task_id,kind,actor_id,actor_role,payload_json,at)
		VALUES ($1,$2,'work_order.claimed','migration','system',$3::jsonb,$4)`,
		workspace, taskID, historical, now); err != nil {
		t.Fatal(err)
	}

	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = pool.QueryRow(ctx, `SELECT max(version) FROM conveyor_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != embeddedMigrationHead(t) {
		t.Fatalf("schema version=%d, want embedded migration head %d", version, embeddedMigrationHead(t))
	}
	var legacy int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='work_orders' AND column_name='governance_snapshot'`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Fatal("legacy governance_snapshot column remains")
	}
	var digest string
	var content []byte
	if err = pool.QueryRow(ctx, `SELECT wo.governance_snapshot_digest, gs.content
		FROM work_orders wo JOIN governance_snapshots gs ON gs.workspace_id=wo.workspace_id AND gs.digest=wo.governance_snapshot_digest
		WHERE wo.id=$1`, orderID).Scan(&digest, &content); err != nil {
		t.Fatal(err)
	}
	if len(digest) != 64 || !strings.Contains(string(content), "inline-body") {
		t.Fatalf("migrated digest=%q content=%s", digest, content)
	}
	var snapshot core.GovernanceSnapshot
	if err = json.Unmarshal(content, &snapshot); err != nil {
		t.Fatal(err)
	}
	wantDigest, _ := store.EncodeGovernanceSnapshot(&snapshot)
	if digest != wantDigest {
		t.Fatalf("migrated digest=%q, want EncodeGovernanceSnapshot %q", digest, wantDigest)
	}
	wsCtx := store.WithWorkspace(ctx, workspace)
	st := newStore(pool)
	got, err := st.GetWorkOrder(wsCtx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GovernanceSnapshotDigest != digest {
		t.Fatalf("GetWorkOrder digest=%q, want stored %q", got.GovernanceSnapshotDigest, digest)
	}
	if got.GovernanceSnapshot == nil || got.GovernanceSnapshot.Designs[0].Content != "inline-body" {
		t.Fatalf("GetWorkOrder snapshot=%+v", got.GovernanceSnapshot)
	}
	if _, err = storetest.ClaimWorkOrder(wsCtx, st, orderID, core.WorkOrderClaim{
		ClaimantID: core.TaskRunClaimantID("migration-worker"), SessionID: "migration-session", ClientToken: "migration-token",
		Lease: time.Hour, ExecutionTimeout: time.Hour, Governance: &snapshot,
	}); err != nil {
		t.Fatal(err)
	}
	var bodies int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM governance_snapshots WHERE workspace_id=$1 AND digest=$2`, workspace, digest).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if bodies != 1 {
		t.Fatalf("claim duplicated snapshot bodies: %d", bodies)
	}
	var payload []byte
	if err = pool.QueryRow(ctx, `SELECT payload_json FROM events WHERE task_id=$1 AND kind='work_order.claimed' AND payload_json::text LIKE '%inline-body%'`, taskID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"governance_snapshot"`) || !strings.Contains(string(payload), "inline-body") {
		t.Fatalf("historical event rewritten: %s", payload)
	}

	raw, err := migrationFiles.ReadFile("migrations/156_governance_snapshots.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("replay 156: %v", err)
	}
	var after int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM governance_snapshots WHERE workspace_id=$1`, workspace).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 1 {
		t.Fatalf("replay duplicated snapshots: %d", after)
	}
}
