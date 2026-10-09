package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Task 261007-9d50e0 retires the features entity. The version-18 hook detaches
// residual feature-owned attachment links into workspace-unattached links and
// drops both feature columns, and file 0018 drops the table
// (component-persistence, component-artifacts). Recorded history stays
// unchanged (component-lineage).

const featureDropVersion = 18

// restorePreFeatureDrop models a database recorded below 0018 on the shared
// test database: the version-one feature table and columns exist again and
// the ledger lacks version 18. Tests end by migrating, which drops them.
func restorePreFeatureDrop(t *testing.T, st *Store) {
	t.Helper()
	ctx := t.Context()
	for _, table := range []string{"artifact_links", "tasks"} {
		exists, err := st.columnExists(ctx, table, "feature_id")
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			if _, err = st.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN feature_id LONGTEXT"); err != nil {
				t.Fatal(err)
			}
		}
	}
	raw, err := migrationFiles.ReadFile("migrations/0001_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	restored := false
	for _, statement := range strings.Split(string(raw), ";") {
		if strings.Contains(statement, "CREATE ROWSTORE TABLE IF NOT EXISTS `features`") {
			if _, err = st.db.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
			restored = true
		}
	}
	if !restored {
		t.Fatal("version-one schema no longer defines the features table")
	}
	if _, err = st.db.ExecContext(ctx, "DELETE FROM conveyor_singlestore_migrations WHERE version=?", featureDropVersion); err != nil {
		t.Fatal(err)
	}
}

func assertSingleStoreFeatureSchemaRetired(t *testing.T, st *Store) {
	t.Helper()
	ctx := t.Context()
	var tables, columns, ledger int
	if err := st.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='features'),
		(SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND column_name='feature_id' AND table_name IN ('tasks','artifact_links')),
		(SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=18 AND name='0018_drop_features.sql')`).Scan(&tables, &columns, &ledger); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || columns != 0 || ledger != 1 {
		t.Fatalf("feature schema tables=%d columns=%d ledger rows=%d", tables, columns, ledger)
	}
}

// featureFixture seeds one workspace with residual feature rows written
// through the retired store methods: shared, duplicate, already-detached,
// mixed, and untouched attachment links, a task assignment, and recorded
// feature history.
type featureFixture struct {
	ctx                                context.Context
	workspace                          string
	task                               core.Task
	shared, existing, mixed, untouched core.Artifact
	suffix                             string
}

func seedFeatureFixture(t *testing.T, st *Store) featureFixture {
	t.Helper()
	f := featureFixture{workspace: "feature-retired-" + core.NewTaskID(), suffix: core.NewTaskID()}
	f.ctx = store.WithWorkspace(t.Context(), f.workspace)
	if _, err := st.BootstrapWorkspaceConfig(f.ctx, &config.Config{Workspace: f.workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	f.task = core.Task{ID: core.NewTaskID(), Workspace: f.workspace, Repo: "repo", Title: "Retired", BaseBranch: "main", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	f.task.Branch = "conveyor/task-" + f.task.ID
	if err := st.CreateTask(f.ctx, f.task); err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(f.ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Retired"})
	if err != nil {
		t.Fatal(err)
	}
	requirement, _, err := st.CreateRequirement(f.ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Retired"}, core.RequirementVersion{
		Content: "# Retired", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep attachments."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	featureOne, featureTwo := "feat-one-"+f.suffix, "feat-two-"+f.suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(f.ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for _, feature := range []string{featureOne, featureTwo} {
		exec(`INSERT INTO features(id,workspace_id,name) VALUES(?,?,?)`, feature, f.workspace, feature)
	}
	exec(`UPDATE tasks SET feature_id=? WHERE workspace_id=? AND id=?`, featureOne, f.workspace, f.task.ID)
	upload := func(name string) core.Artifact {
		t.Helper()
		artifact, err := st.CreateArtifact(f.ctx, core.Artifact{Name: name, ContentType: "text/plain"}, []byte("bytes of "+name+" "+f.suffix))
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	link := func(artifactID, column, owner, role string) {
		t.Helper()
		if column == "" {
			exec(`INSERT INTO artifact_links(workspace_id,artifact_id,role) VALUES(?,?,?)`, f.workspace, artifactID, role)
			return
		}
		exec(`INSERT INTO artifact_links(workspace_id,artifact_id,`+column+`,role) VALUES(?,?,?,?)`, f.workspace, artifactID, owner, role)
	}
	f.shared = upload("shared.txt")
	link(f.shared.ID, "feature_id", featureOne, "task_context")
	link(f.shared.ID, "feature_id", featureTwo, "task_context")
	link(f.shared.ID, "feature_id", featureOne, "generated_output")
	f.existing = upload("existing.txt")
	link(f.existing.ID, "", "", "task_context")
	link(f.existing.ID, "feature_id", featureTwo, "task_context")
	f.mixed = upload("mixed.txt")
	link(f.mixed.ID, "task_id", f.task.ID, "task_context")
	link(f.mixed.ID, "feature_id", featureOne, "task_context")
	f.untouched = upload("untouched.txt")
	link(f.untouched.ID, "task_id", f.task.ID, "task_context")
	link(f.untouched.ID, "requirement_id", requirement.ID, "task_context")
	link(f.untouched.ID, "planning_session_id", session.ID, "task_context")
	if err = st.AppendEvent(f.ctx, core.Event{TaskID: f.task.ID, Kind: "task.feature_assigned", Payload: core.JSONPayload(map[string]string{"feature_id": featureOne})}); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO links(workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event) VALUES(?,'requirement',?,'task',?,'historical_feature_assignment','feature.migrated')`, f.workspace, "req-"+featureOne, f.task.ID)
	return f
}

func queryRows(t *testing.T, st *Store, ctx context.Context, query string, args ...any) []string {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(values))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func (f featureFixture) snapshot(t *testing.T, st *Store) map[string][]string {
	t.Helper()
	return map[string][]string{
		"artifacts": queryRows(t, st, f.ctx, `SELECT id,name,content_type,size_bytes,content,created_at FROM artifacts WHERE workspace_id=?`, f.workspace),
		"owned":     queryRows(t, st, f.ctx, `SELECT artifact_id,task_id,requirement_id,planning_session_id,role FROM artifact_links WHERE workspace_id=? AND (task_id IS NOT NULL OR requirement_id IS NOT NULL OR planning_session_id IS NOT NULL)`, f.workspace),
		"events":    queryRows(t, st, f.ctx, `SELECT id,task_id,kind,payload_json FROM events WHERE workspace_id=?`, f.workspace),
		"links":     queryRows(t, st, f.ctx, `SELECT src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_by_event_id FROM links WHERE workspace_id=?`, f.workspace),
	}
}

// assertConverged checks the lossless detach: one unattached link per
// distinct workspace, artifact, and role, no duplicate, and every owned link,
// artifact row, recorded event, and historical link unchanged.
func (f featureFixture) assertConverged(t *testing.T, st *Store, before map[string][]string) {
	t.Helper()
	assertSingleStoreFeatureSchemaRetired(t, st)
	after := f.snapshot(t, st)
	for key := range before {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Fatalf("%s changed across the drop:\nbefore=%v\nafter=%v", key, before[key], after[key])
		}
	}
	detached := map[string]string{}
	for _, row := range queryRows(t, st, f.ctx, `SELECT artifact_id,role,COUNT(*) FROM artifact_links WHERE workspace_id=? AND task_id IS NULL AND requirement_id IS NULL AND planning_session_id IS NULL GROUP BY artifact_id,role`, f.workspace) {
		detached[row] = row
	}
	want := []string{
		fmt.Sprint([]sql.NullString{{String: f.shared.ID, Valid: true}, {String: "task_context", Valid: true}, {String: "1", Valid: true}}),
		fmt.Sprint([]sql.NullString{{String: f.shared.ID, Valid: true}, {String: "generated_output", Valid: true}, {String: "1", Valid: true}}),
		fmt.Sprint([]sql.NullString{{String: f.existing.ID, Valid: true}, {String: "task_context", Valid: true}, {String: "1", Valid: true}}),
		fmt.Sprint([]sql.NullString{{String: f.mixed.ID, Valid: true}, {String: "task_context", Valid: true}, {String: "1", Valid: true}}),
	}
	if len(detached) != len(want) {
		t.Fatalf("detached links=%v want %v", detached, want)
	}
	for _, row := range want {
		if _, ok := detached[row]; !ok {
			t.Fatalf("detached links=%v missing %s", detached, row)
		}
	}
	selected, err := st.ListArtifactsForLineage(f.ctx, []core.LineageNode{{Type: core.LineageTask, ID: f.task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, artifact := range selected {
		if artifact.TaskID != f.task.ID {
			t.Fatalf("task lineage selected a detached link: %+v", artifact)
		}
		ids[artifact.ID] = true
	}
	if !ids[f.mixed.ID] || !ids[f.untouched.ID] || ids[f.shared.ID] || ids[f.existing.ID] {
		t.Fatalf("task lineage selection=%v", ids)
	}
	for _, artifact := range []core.Artifact{f.shared, f.existing, f.mixed, f.untouched} {
		read, content, err := st.GetArtifact(f.ctx, artifact.ID)
		if err != nil || string(content) != "bytes of "+artifact.Name+" "+f.suffix || (read.TaskID == "" && read.EligibleVerificationEvidence()) {
			t.Fatalf("artifact %s read=%+v content=%q err=%v", artifact.Name, read, content, err)
		}
	}
	if read, err := st.GetTask(f.ctx, f.task.ID); err != nil || read.ID != f.task.ID {
		t.Fatalf("task after the drop=%+v err=%v", read, err)
	}
}

func TestFeatureRetirementFreshSchemaIntegration(t *testing.T) {
	st := integrationStore(t)
	// The shared database was created and migrated from empty by this binary.
	assertSingleStoreFeatureSchemaRetired(t, st)
	if err := st.migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertSingleStoreFeatureSchemaRetired(t, st)
}

func TestFeatureRetirementHistoricalUpgradeIntegration(t *testing.T) {
	st := integrationStore(t)
	restorePreFeatureDrop(t, st)
	f := seedFeatureFixture(t, st)
	before := f.snapshot(t, st)
	if err := st.migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.assertConverged(t, st, before)
}

func TestFeatureRetirementPartialDDLRetryIntegration(t *testing.T) {
	steps := len((&Store{}).featureRetirementSteps())
	// Stop after each committed step of the hook, and after the file's table
	// drop, with the ledger still below 0018; the next start converges.
	for stopAfter := 1; stopAfter <= steps+1; stopAfter++ {
		t.Run(fmt.Sprintf("after step %d", stopAfter), func(t *testing.T) {
			st := integrationStore(t)
			restorePreFeatureDrop(t, st)
			f := seedFeatureFixture(t, st)
			before := f.snapshot(t, st)
			for index, step := range st.featureRetirementSteps() {
				if index >= stopAfter {
					break
				}
				if err := step(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if stopAfter > steps {
				if _, err := st.db.ExecContext(t.Context(), `DROP TABLE IF EXISTS features`); err != nil {
					t.Fatal(err)
				}
			}
			var recorded int
			if err := st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=?`, featureDropVersion).Scan(&recorded); err != nil || recorded != 0 {
				t.Fatalf("ledger recorded 0018 before the start finished: rows=%d err=%v", recorded, err)
			}
			if err := st.migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.assertConverged(t, st, before)
			// A second start is a no-op.
			if err := st.migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.assertConverged(t, st, before)
		})
	}
}
