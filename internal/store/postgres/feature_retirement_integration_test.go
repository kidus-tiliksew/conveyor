package postgres

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Task 261007-9d50e0 retires the features entity. Migration 142 detaches any
// residual feature-owned attachment link into a workspace-unattached link,
// drops the feature columns and table, and recreates the three-owner check
// and unattached uniqueness rule (component-persistence, component-artifacts).
// Recorded history stays unchanged (component-lineage).

const featureDropVersion = 142

// rowsAsJSON returns each row of query as canonical JSON text, sorted, so two
// snapshots compare by value.
func rowsAsJSON(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT row_to_json(snapshot)::text FROM (`+query+`) snapshot ORDER BY 1`, args...)
	if err != nil {
		t.Fatalf("snapshot %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertFeatureSchemaRetired checks that no feature table, column, index, or
// constraint survives and that the recreated attachment rules name exactly
// the task, requirement, and planning-session owners.
func assertFeatureSchemaRetired(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var tables, columns, indexes int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='features'),
		(SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND column_name='feature_id' AND table_name IN ('tasks','artifact_links')),
		(SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND (indexname LIKE '%feature%' OR indexdef LIKE '%feature_id%'))`).Scan(&tables, &columns, &indexes); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || columns != 0 || indexes != 0 {
		t.Fatalf("feature schema survives: tables=%d columns=%d indexes=%d", tables, columns, indexes)
	}
	var check, unique string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
		JOIN pg_class r ON r.oid=c.conrelid JOIN pg_namespace n ON n.oid=r.relnamespace
		WHERE n.nspname=current_schema() AND r.relname='artifact_links' AND c.conname='artifact_links_check'`).Scan(&check); err != nil {
		t.Fatalf("artifact_links_check: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname='artifact_links_workspace_unique'`).Scan(&unique); err != nil {
		t.Fatalf("artifact_links_workspace_unique: %v", err)
	}
	for _, definition := range []string{check, unique} {
		if strings.Contains(definition, "feature") || !strings.Contains(definition, "task_id") ||
			!strings.Contains(definition, "requirement_id") || !strings.Contains(definition, "planning_session_id") {
			t.Fatalf("attachment rule does not name exactly the three owners: %s", definition)
		}
	}
	if !strings.Contains(unique, "UNIQUE") || !strings.Contains(unique, "role") {
		t.Fatalf("unattached uniqueness lost its role key: %s", unique)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conveyor_schema_migrations WHERE version=$1 AND name='142_drop_features.sql'`, featureDropVersion).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("drop migration ledger rows=%d err=%v", ledger, err)
	}
}

func TestFeatureRetirementFreshSchemaIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	workspace := "feature-retired-fresh-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	assertFeatureSchemaRetired(t, ctx, st.pool)

	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Title: "Fresh", BaseBranch: "main", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	task.Branch = "conveyor/task-" + task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Fresh"})
	if err != nil {
		t.Fatal(err)
	}
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Fresh"}, core.RequirementVersion{
		Content: "# Fresh", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep owners exclusive."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "fresh.txt", ContentType: "text/plain"}, []byte("fresh unattached"))
	if err != nil {
		t.Fatal(err)
	}
	// The recreated check refuses two owners even below the Go validator.
	for _, owners := range [][3]any{{task.ID, requirement.ID, nil}, {task.ID, nil, session.ID}, {nil, requirement.ID, session.ID}} {
		_, err = st.pool.Exec(ctx, `INSERT INTO artifact_links (workspace_id,artifact_id,task_id,requirement_id,planning_session_id,role) VALUES ($1,$2,$3,$4,$5,'task_context')`,
			workspace, artifact.ID, owners[0], owners[1], owners[2])
		if err == nil || !strings.Contains(err.Error(), "artifact_links_check") {
			t.Fatalf("two-owner link owners=%v err=%v", owners, err)
		}
	}
	// One unattached link per workspace, artifact, and role.
	if _, err = st.pool.Exec(ctx, `INSERT INTO artifact_links (workspace_id,artifact_id,role) VALUES ($1,$2,'task_context')`, workspace, artifact.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO artifact_links (workspace_id,artifact_id,role) VALUES ($1,$2,'task_context')`, workspace, artifact.ID); err == nil || !strings.Contains(err.Error(), "artifact_links_workspace_unique") {
		t.Fatalf("duplicate unattached link err=%v", err)
	}
	if _, err = st.pool.Exec(ctx, `INSERT INTO artifact_links (workspace_id,artifact_id,role) VALUES ($1,$2,'generated_output')`, workspace, artifact.ID); err != nil {
		t.Fatalf("a distinct role is a distinct unattached link: %v", err)
	}
}

func TestFeatureRetirementHistoricalUpgradeIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	// A version-45 installation: content-bearing and empty features, a
	// blueprint parent and child, a shared attachment, and monitor and drift
	// references, all in the pre-046 shape.
	f.feature(t, "feat-text", "Payments", "Payments reconcile nightly.", "", 0)
	f.feature(t, "feat-parent", "Parent", "", "", time.Second)
	f.feature(t, "feat-shared-1", "Shared One", "", "", 2*time.Second)
	f.feature(t, "feat-shared-2", "Shared Two", "", "", 3*time.Second)
	f.feature(t, "feat-monitor", "Monitor", "", "", 4*time.Second)
	f.feature(t, "feat-empty", "Empty", "", "", 5*time.Second)
	parent := f.task(t, "feat-parent", "")
	child := f.task(t, "", parent)
	attachment := f.artifact(t, "feat-shared-1", "task_context")
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO artifact_links (workspace_id, artifact_id, feature_id, role) VALUES ($1,$2,'feat-shared-2','task_context')`, f.workspace, attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO monitor_observations
		(workspace_id, identity, repository, kind, occurrence_id, source_url, feature_id, observed_at)
		VALUES ($1,'identity-retired','repo','direct_push','occ-retired','https://example.test/retired','feat-monitor',$2)`, f.workspace, f.seeded); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO repository_drift
		(workspace_id, id, repository, kind, source_url, feature_id, task_id, detected_at)
		VALUES ($1,'drift-retired','repo','direct_push','https://example.test/retired','feat-monitor',$2,$3)`, f.workspace, parent, f.seeded); err != nil {
		t.Fatal(err)
	}

	f.upgradeTo(t, featureDropVersion-1)
	snapshot := func() map[string][]string {
		return map[string][]string{
			"requirements":         rowsAsJSON(t, f.ctx, f.pool, `SELECT * FROM requirements WHERE workspace_id=$1`, f.workspace),
			"requirement_versions": rowsAsJSON(t, f.ctx, f.pool, `SELECT * FROM requirement_versions WHERE workspace_id=$1`, f.workspace),
			"artifacts":            rowsAsJSON(t, f.ctx, f.pool, `SELECT id,name,content_type,size_bytes,md5(content) AS digest,created_at FROM artifacts WHERE workspace_id=$1`, f.workspace),
			"links":                rowsAsJSON(t, f.ctx, f.pool, `SELECT * FROM links WHERE workspace_id=$1`, f.workspace),
			"events":               rowsAsJSON(t, f.ctx, f.pool, `SELECT id,task_id,kind,payload_json,at FROM events WHERE workspace_id=$1`, f.workspace),
			"ledger":               rowsAsJSON(t, f.ctx, f.pool, `SELECT version,name,checksum,applied_at FROM conveyor_schema_migrations WHERE version<$1`, featureDropVersion),
			"monitor":              rowsAsJSON(t, f.ctx, f.pool, `SELECT identity,requirement_id FROM monitor_observations WHERE workspace_id=$1`, f.workspace),
			"drift":                rowsAsJSON(t, f.ctx, f.pool, `SELECT id,requirement_id,task_id FROM repository_drift WHERE workspace_id=$1`, f.workspace),
		}
	}
	before := snapshot()
	var residual int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM features WHERE workspace_id=$1`, f.workspace).Scan(&residual); err != nil || residual == 0 {
		t.Fatalf("historical bound kept no content-bearing feature rows: %d err=%v", residual, err)
	}
	if len(before["requirements"]) < 4 || len(before["links"]) == 0 || len(before["events"]) == 0 {
		t.Fatalf("historical bound produced no migrated history: %v", before)
	}

	f.upgradeTo(t, 0)
	assertFeatureSchemaRetired(t, f.ctx, f.pool)
	after := snapshot()
	if !reflect.DeepEqual(before, after) {
		for key := range before {
			if !reflect.DeepEqual(before[key], after[key]) {
				t.Errorf("%s changed across the drop:\nbefore=%v\nafter=%v", key, before[key], after[key])
			}
		}
		t.FailNow()
	}
	for _, requirementID := range []string{"req-feat-text", "req-feat-parent", "req-feat-shared-1", "req-feat-shared-2", "req-feat-monitor"} {
		if _, err := f.store.GetRequirement(f.ctx, requirementID); err != nil {
			t.Fatalf("migrated requirement %s unreadable after the drop: %v", requirementID, err)
		}
	}
	for _, taskID := range []string{parent, child} {
		if _, err := f.store.GetTask(f.ctx, taskID); err != nil {
			t.Fatalf("task %s unreadable after the drop: %v", taskID, err)
		}
	}
	read, content, err := f.store.GetArtifact(f.ctx, attachment)
	if err != nil || !strings.Contains(string(content), "feat-shared-1") || read.RequirementID == "" {
		t.Fatalf("shared attachment after the drop=%+v content=%q err=%v", read, content, err)
	}
}

func TestFeatureRetirementUpgradeFromPreviousHeadIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, featureDropVersion-1)
	workspace := "feature-retired-head-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "repo", Title: "Head", BaseBranch: "main", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	task.Branch = "conveyor/task-" + task.ID
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Head"})
	if err != nil {
		t.Fatal(err)
	}
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Head"}, core.RequirementVersion{
		Content: "# Head", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep attachments."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Residual feature rows written after 046/047 through the retired store
	// methods, and a task assignment.
	suffix := core.NewTaskID()
	featureOne, featureTwo := "feat-one-"+suffix, "feat-two-"+suffix
	for _, feature := range []string{featureOne, featureTwo} {
		if _, err = st.pool.Exec(ctx, `INSERT INTO features (id,workspace_id,name) VALUES ($1,$2,$1)`, feature, workspace); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.pool.Exec(ctx, `UPDATE tasks SET feature_id=$1 WHERE workspace_id=$2 AND id=$3`, featureOne, workspace, task.ID); err != nil {
		t.Fatal(err)
	}
	upload := func(name string) core.Artifact {
		t.Helper()
		artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: name, ContentType: "text/plain"}, []byte("bytes of "+name+" "+suffix))
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	link := func(artifactID, column, owner, role string) {
		t.Helper()
		if column == "" {
			if _, err := st.pool.Exec(ctx, `INSERT INTO artifact_links (workspace_id,artifact_id,role) VALUES ($1,$2,$3)`, workspace, artifactID, role); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := st.pool.Exec(ctx, `INSERT INTO artifact_links (workspace_id,artifact_id,`+column+`,role) VALUES ($1,$2,$3,$4)`, workspace, artifactID, owner, role); err != nil {
			t.Fatal(err)
		}
	}
	shared := upload("shared.txt") // two features, one role: one detached link
	link(shared.ID, "feature_id", featureOne, "task_context")
	link(shared.ID, "feature_id", featureTwo, "task_context")
	link(shared.ID, "feature_id", featureOne, "generated_output") // distinct role survives
	existing := upload("existing.txt")                            // already unattached: no duplicate
	link(existing.ID, "", "", "task_context")
	link(existing.ID, "feature_id", featureTwo, "task_context")
	mixed := upload("mixed.txt") // a task link stays owned
	link(mixed.ID, "task_id", task.ID, "task_context")
	link(mixed.ID, "feature_id", featureOne, "task_context")
	untouched := upload("untouched.txt") // non-feature links only
	link(untouched.ID, "task_id", task.ID, "task_context")
	link(untouched.ID, "requirement_id", requirement.ID, "task_context")
	link(untouched.ID, "planning_session_id", session.ID, "task_context")

	artifactsBefore := rowsAsJSON(t, ctx, st.pool, `SELECT id,name,content_type,size_bytes,md5(content) AS digest,created_at FROM artifacts WHERE workspace_id=$1`, workspace)
	ownedBefore := rowsAsJSON(t, ctx, st.pool, `SELECT artifact_id,task_id,requirement_id,planning_session_id,role FROM artifact_links WHERE workspace_id=$1 AND feature_id IS NULL AND (task_id IS NOT NULL OR requirement_id IS NOT NULL OR planning_session_id IS NOT NULL)`, workspace)
	eventsBefore := rowsAsJSON(t, ctx, st.pool, `SELECT id,kind,payload_json FROM events WHERE workspace_id=$1`, workspace)

	if err = Migrate(ctx, st.pool); err != nil {
		t.Fatal(err)
	}
	assertFeatureSchemaRetired(t, ctx, st.pool)
	if got := rowsAsJSON(t, ctx, st.pool, `SELECT id,name,content_type,size_bytes,md5(content) AS digest,created_at FROM artifacts WHERE workspace_id=$1`, workspace); !reflect.DeepEqual(artifactsBefore, got) {
		t.Fatalf("artifact rows changed:\nbefore=%v\nafter=%v", artifactsBefore, got)
	}
	if got := rowsAsJSON(t, ctx, st.pool, `SELECT artifact_id,task_id,requirement_id,planning_session_id,role FROM artifact_links WHERE workspace_id=$1 AND (task_id IS NOT NULL OR requirement_id IS NOT NULL OR planning_session_id IS NOT NULL)`, workspace); !reflect.DeepEqual(ownedBefore, got) {
		t.Fatalf("owned links changed:\nbefore=%v\nafter=%v", ownedBefore, got)
	}
	if got := rowsAsJSON(t, ctx, st.pool, `SELECT id,kind,payload_json FROM events WHERE workspace_id=$1`, workspace); !reflect.DeepEqual(eventsBefore, got) {
		t.Fatalf("the drop wrote or changed events:\nbefore=%v\nafter=%v", eventsBefore, got)
	}
	detached := map[string]int{}
	rows, err := st.pool.Query(ctx, `SELECT artifact_id,role,count(*) FROM artifact_links WHERE workspace_id=$1 AND task_id IS NULL AND requirement_id IS NULL AND planning_session_id IS NULL GROUP BY artifact_id,role`, workspace)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var artifactID, role string
		var count int
		if err = rows.Scan(&artifactID, &role, &count); err != nil {
			t.Fatal(err)
		}
		detached[artifactID+"/"+role] = count
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		shared.ID + "/task_context": 1, shared.ID + "/generated_output": 1,
		existing.ID + "/task_context": 1, mixed.ID + "/task_context": 1,
	}
	if !reflect.DeepEqual(detached, want) {
		t.Fatalf("detached links=%v want %v", detached, want)
	}
	// Live reads: every artifact keeps its bytes, a detached link is never task
	// evidence, and task lineage selects only the task's own links.
	listed, err := st.ListArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range listed {
		if artifact.TaskID == "" && artifact.EligibleVerificationEvidence() {
			t.Fatalf("detached link became task evidence: %+v", artifact)
		}
	}
	selected, err := st.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	selectedIDs := map[string]bool{}
	for _, artifact := range selected {
		if artifact.TaskID != task.ID {
			t.Fatalf("task lineage selected a detached link: %+v", artifact)
		}
		selectedIDs[artifact.ID] = true
	}
	if !selectedIDs[mixed.ID] || !selectedIDs[untouched.ID] || selectedIDs[shared.ID] || selectedIDs[existing.ID] {
		t.Fatalf("task lineage selection=%v", selectedIDs)
	}
	for _, artifact := range []core.Artifact{shared, existing, mixed, untouched} {
		if _, content, err := st.GetArtifact(ctx, artifact.ID); err != nil || string(content) != "bytes of "+artifact.Name+" "+suffix {
			t.Fatalf("artifact %s content=%q err=%v", artifact.Name, content, err)
		}
	}
	if read, err := st.GetTask(ctx, task.ID); err != nil || read.ID != task.ID {
		t.Fatalf("task after the drop=%+v err=%v", read, err)
	}
}

func TestFeatureRetirementRestartIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 0)
	// Restarts on a dropped schema skip every recorded version, so neither the
	// historical 046 file nor its pending overlay reads the missing table.
	for range 2 {
		if err := Migrate(t.Context(), st.pool); err != nil {
			t.Fatalf("restart on a dropped schema: %v", err)
		}
	}
	assertFeatureSchemaRetired(t, t.Context(), st.pool)
}
