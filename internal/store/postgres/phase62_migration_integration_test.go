package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// Migration 046 retires the curated feature tree into the flat requirement
// corpus. The contract is losslessness: a node
// that carried anything becomes a pending requirement document and every
// reference to it survives as durable lineage, while a node that carried
// nothing drops. This exercises the upgrade on real legacy rows because that is
// the only way the data migration is ever executed (AC-5).

type phase62Fixture struct {
	pool      *pgxpool.Pool
	store     *Store
	ctx       context.Context
	workspace string
	seeded    time.Time
}

func newPhase62MigrationFixture(t *testing.T) *phase62Fixture {
	t.Helper()
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "phase62_migration_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
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
	// Pin the schema to the last pre-046 version so the legacy rows below are
	// written in exactly the shape the upgrade will find in production.
	if err = migrateControlPlaneToVersion(t.Context(), pool, 45); err != nil {
		t.Fatalf("migrate isolated schema to version 45: %v", err)
	}
	workspace := "phase62-migration-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	st := &Store{pool: pool, queries: db.New(pool)}
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	return &phase62Fixture{
		pool: pool, store: st, ctx: ctx, workspace: workspace,
		seeded: time.Now().UTC().Add(-time.Hour),
	}
}

func (f *phase62Fixture) feature(t *testing.T, id, name, description, parentID string, offset time.Duration) {
	t.Helper()
	var parent any
	if parentID != "" {
		parent = parentID
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO features (id, workspace_id, parent_id, name, description, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		id, f.workspace, parent, name, description, f.seeded.Add(offset)); err != nil {
		t.Fatalf("seed feature %s: %v", id, err)
	}
}

func (f *phase62Fixture) task(t *testing.T, featureID, parentTaskID string) string {
	t.Helper()
	id := core.NewTaskID()
	if _, err := f.store.queries.InsertTask(f.ctx, taskInsertParams(core.Task{
		ID: id, Workspace: f.workspace, Repo: "repo", Branch: "conveyor/task-" + id,
		State: core.TaskQueued, FeatureID: featureID, ParentTaskID: parentTaskID,
		CreatedAt: f.seeded,
	})); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return id
}

// artifact writes a feature-scoped attachment in the pre-046 shape: at version
// 45 artifact_links has no requirement_id column at all.
func (f *phase62Fixture) artifact(t *testing.T, featureID, role string) string {
	t.Helper()
	content := []byte("attachment for " + featureID)
	id := core.NewTaskID()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO artifacts (id, workspace_id, name, content_type, size_bytes, content, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, f.workspace, "notes.txt", "text/plain", len(content), content, f.seeded); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO artifact_links (workspace_id, artifact_id, feature_id, role)
		 VALUES ($1,$2,$3,$4)`, f.workspace, id, featureID, role); err != nil {
		t.Fatalf("seed artifact link: %v", err)
	}
	return id
}

func (f *phase62Fixture) upgrade(t *testing.T) {
	t.Helper()
	f.upgradeTo(t, 46)
}

func (f *phase62Fixture) upgradeTo(t *testing.T, version int) {
	t.Helper()
	if err := migrateControlPlaneToVersion(t.Context(), f.pool, version); err != nil {
		t.Fatalf("migrate to version %d: %v", version, err)
	}
}

func TestMigration058CanonicalizesOnlyDerivablePullRequestIdentitiesIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	taskID := f.task(t, "", "")
	f.upgradeTo(t, 57)
	if _, err := f.pool.Exec(f.ctx, `UPDATE repos SET github_slug='acme/repo' WHERE workspace_id=$1 AND name='repo'`, f.workspace); err != nil {
		t.Fatal(err)
	}
	var eventRepository, eventTask int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO events
		(workspace_id,task_id,kind,actor_id,actor_role,payload_json,at)
		VALUES ($1,$2,'pull_request.opened','legacy','system',$3,now()) RETURNING id`,
		f.workspace, taskID, core.JSONPayload(map[string]any{"repository": "event/repo", "number": 41})).Scan(&eventRepository); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO events
		(workspace_id,task_id,kind,actor_id,actor_role,payload_json,at)
		VALUES ($1,$2,'pull_request.opened','legacy','system',$3,now()) RETURNING id`,
		f.workspace, taskID, core.JSONPayload(map[string]any{"number": 42})).Scan(&eventTask); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO repos
		(workspace_id,name,url,github_slug,default_base) VALUES ($1,'second','https://example.test/second','acme/second','main')`, f.workspace); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		src, dst, legacy string
		event            any
	}{
		{taskID, "41", "", eventRepository}, {taskID, "42", "", eventTask},
		{"missing-task", "43", "migration-057", nil}, {taskID, "not-a-number", "", eventTask},
	} {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO links
			(workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,legacy_created_by_event,created_at)
			VALUES ($1,'task',$2,'pull_request',$3,'submitted_as',$4,NULLIF($5,''),now())`, f.workspace, row.src, row.dst, row.event, row.legacy); err != nil {
			t.Fatal(err)
		}
	}
	f.upgradeTo(t, 58)
	var canonical, excluded, retained int
	if err := f.pool.QueryRow(f.ctx, `SELECT
		count(*) FILTER (WHERE dst_id IN ('event/repo#41','acme/repo#42')),
		count(*) FILTER (WHERE dst_id IN ('43','not-a-number')),
		(SELECT count(*) FROM lineage_repair_exclusions WHERE workspace_id=$1 AND kind='submitted_as')
		FROM links WHERE workspace_id=$1 AND kind='submitted_as'`, f.workspace).Scan(&canonical, &retained, &excluded); err != nil {
		t.Fatal(err)
	}
	if canonical != 2 || retained != 2 || excluded != 2 {
		t.Fatalf("canonical=%d retained=%d excluded=%d", canonical, retained, excluded)
	}
}

func TestMigration060RepairsNameFallbackAndUsesOnlyDerivableIdentityIntegration(t *testing.T) {
	t.Run("empty slug reverts name fallback and records exclusion audit", func(t *testing.T) {
		f := newPhase62MigrationFixture(t)
		taskID := f.task(t, "", "")
		f.upgradeTo(t, 57)
		var eventID int64
		if err := f.pool.QueryRow(f.ctx, `INSERT INTO events
			(workspace_id,task_id,kind,actor_id,actor_role,payload_json,at)
			VALUES ($1,$2,'pull_request.opened','legacy','system',$3,now()) RETURNING id`,
			f.workspace, taskID, core.JSONPayload(map[string]any{"number": 42})).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO links
			(workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
			VALUES ($1,'task',$2,'pull_request','42','submitted_as',$3,now())`, f.workspace, taskID, eventID); err != nil {
			t.Fatal(err)
		}
		f.upgradeTo(t, 59)
		var nameFallback int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM links WHERE workspace_id=$1 AND dst_id='repo#42'`, f.workspace).Scan(&nameFallback); err != nil || nameFallback != 1 {
			t.Fatalf("058 name fallback count=%d err=%v", nameFallback, err)
		}
		started := time.Now().UTC().Add(-time.Second)
		f.upgradeTo(t, 60)
		var bare, split, auditCount int
		var reason string
		var auditAt time.Time
		var payload []byte
		if err := f.pool.QueryRow(f.ctx, `SELECT
			count(*) FILTER (WHERE dst_id='42'),count(*) FILTER (WHERE dst_id='repo#42')
			FROM links WHERE workspace_id=$1 AND kind='submitted_as'`, f.workspace).Scan(&bare, &split); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(f.ctx, `SELECT reason FROM lineage_repair_exclusions
			WHERE workspace_id=$1 AND src_id=$2 AND dst_id='42' AND kind='submitted_as'`, f.workspace, taskID).Scan(&reason); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) OVER(),at,payload_json FROM events
			WHERE workspace_id=$1 AND kind='lineage.pull_request_identity_repaired' ORDER BY id DESC LIMIT 1`, f.workspace).Scan(&auditCount, &auditAt, &payload); err != nil {
			t.Fatal(err)
		}
		if bare != 1 || split != 0 || reason != "empty github_slug and missing event repository" {
			t.Fatalf("bare=%d split=%d reason=%q", bare, split, reason)
		}
		if auditCount != 1 || auditAt.Before(started) || !strings.Contains(string(payload), `"reverted_name_fallback_count": 1`) || !strings.Contains(string(payload), `"excluded_count": 1`) {
			t.Fatalf("audit count=%d at=%s payload=%s", auditCount, auditAt, payload)
		}
		f.upgradeTo(t, 60)
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM events WHERE workspace_id=$1
			AND kind='lineage.pull_request_identity_repaired'`, f.workspace).Scan(&auditCount); err != nil || auditCount != 1 {
			t.Fatalf("idempotent audit count=%d err=%v", auditCount, err)
		}
	})

	t.Run("github slug and projector converge on one identity", func(t *testing.T) {
		f := newPhase62MigrationFixture(t)
		taskID := f.task(t, "", "")
		f.upgradeTo(t, 59)
		if _, err := f.pool.Exec(f.ctx, `UPDATE repos SET github_slug='acme/repo' WHERE workspace_id=$1 AND name='repo'`, f.workspace); err != nil {
			t.Fatal(err)
		}
		var eventID int64
		if err := f.pool.QueryRow(f.ctx, `INSERT INTO events
			(workspace_id,task_id,kind,actor_id,actor_role,payload_json,at)
			VALUES ($1,$2,'pull_request.opened','legacy','system',$3,now()) RETURNING id`,
			f.workspace, taskID, core.JSONPayload(map[string]any{"number": 43})).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO links
			(workspace_id,src_type,src_id,dst_type,dst_id,kind,created_by_event_id,created_at)
			VALUES ($1,'task',$2,'pull_request','43','submitted_as',$3,now())`, f.workspace, taskID, eventID); err != nil {
			t.Fatal(err)
		}
		f.upgradeTo(t, 60)
		if err := f.store.AppendEvent(f.ctx, core.Event{TaskID: taskID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "acme/repo", "number": 43})}); err != nil {
			t.Fatal(err)
		}
		var canonical, bare int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE dst_id='acme/repo#43'),count(*) FILTER (WHERE dst_id='43')
			FROM links WHERE workspace_id=$1 AND kind='submitted_as'`, f.workspace).Scan(&canonical, &bare); err != nil || canonical != 1 || bare != 0 {
			t.Fatalf("canonical=%d bare=%d err=%v", canonical, bare, err)
		}
	})
}

func TestLineageRepairRemoves054VocabularyWithExactExclusionsIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	taskID := f.task(t, "", "")
	f.upgradeTo(t, 54)
	for _, row := range []struct{ srcType, srcID, dstType, dstID, kind string }{
		{"task", taskID, "work_order", taskID + "-implement-1", "executes_as"},
		{"pull_request", "7", "commit", "head-sha", "head"},
		{"task", taskID, "commit", "head-sha", "implemented_by"},
		{"task", taskID, "evidence", "unknown", "produces"},
	} {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO links
			(workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,'migration-054',now())`, f.workspace, row.srcType, row.srcID, row.dstType, row.dstID, row.kind); err != nil {
			t.Fatal(err)
		}
	}
	f.upgradeTo(t, 58)
	var oldKinds, commitNodes, exclusions, dispatches int
	if err := f.pool.QueryRow(f.ctx, `SELECT
		count(*) FILTER (WHERE kind IN ('executes_as','delivered_by','produces','head','implemented_by')),
		count(*) FILTER (WHERE src_type='commit' OR dst_type='commit'),
		(SELECT count(*) FROM lineage_repair_exclusions WHERE workspace_id=$1),
		count(*) FILTER (WHERE kind='dispatches' AND src_id=$2)
		FROM links WHERE workspace_id=$1`, f.workspace, taskID).Scan(&oldKinds, &commitNodes, &exclusions, &dispatches); err != nil {
		t.Fatal(err)
	}
	if oldKinds != 0 || commitNodes != 0 || exclusions != 3 || dispatches != 1 {
		t.Fatalf("old_kinds=%d commit_nodes=%d exclusions=%d dispatches=%d", oldKinds, commitNodes, exclusions, dispatches)
	}
}

func TestPendingLineageMigrationSurvivesMalformedNumericPayloadsIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	taskID := f.task(t, "", "")
	f.upgradeTo(t, 53)
	for _, event := range []struct {
		kind    string
		taskID  any
		payload map[string]any
	}{
		{kind: "task.created", taskID: taskID, payload: map[string]any{"parent_task_id": "blueprint", "origin_spec_version": "not-a-number"}},
		{kind: "requirement.version_confirmed", payload: map[string]any{"workspace_id": f.workspace, "requirement_id": "req-malformed", "version": "not-a-number"}},
		{kind: "spec.version_created", taskID: taskID, payload: map[string]any{"version": "not-a-number"}},
		{kind: "pull_request.opened", taskID: taskID, payload: map[string]any{"repository": "example/repo", "number": "not-a-number"}},
	} {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO events
			(workspace_id,task_id,kind,actor_id,actor_role,payload_json,at)
			VALUES ($1,$2,$3,'migration-test','system',$4,now())`,
			f.workspace, event.taskID, event.kind, core.JSONPayload(event.payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateControlPlaneToVersion(t.Context(), f.pool, 0); err != nil {
		t.Fatalf("full upgrade from pre-054 malformed history: %v", err)
	}
	var version int
	if err := f.pool.QueryRow(f.ctx, `SELECT max(version) FROM conveyor_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 57 {
		t.Fatalf("migration version=%d, want at least 57", version)
	}
}

func TestPhase62FeatureMigrationSeedsPendingRequirementsIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)

	// One node per reason a node is content-bearing, plus one that carries
	// nothing at all and must drop.
	f.feature(t, "feat-text", "Payments Reconciliation", "Payments must reconcile nightly.", "", 0)
	f.feature(t, "feat-task", "Task Bearing", "", "", time.Second)
	f.feature(t, "feat-artifact", "Artifact Bearing", "", "", 2*time.Second)
	f.feature(t, "feat-empty", "Empty Taxonomy", "", "", 3*time.Second)
	assignedTask := f.task(t, "feat-task", "")
	attachment := f.artifact(t, "feat-artifact", "task_context")

	f.upgrade(t)

	// Every content-bearing node became a document; the empty one did not.
	for _, seeded := range []string{"req-feat-text", "req-feat-task", "req-feat-artifact"} {
		var exists bool
		if err := f.pool.QueryRow(f.ctx,
			`SELECT EXISTS (SELECT 1 FROM requirements WHERE workspace_id=$1 AND id=$2)`,
			f.workspace, seeded).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("content-bearing feature did not seed requirement %s", seeded)
		}
	}
	var emptySeeded, emptySurvives bool
	if err := f.pool.QueryRow(f.ctx,
		`SELECT EXISTS (SELECT 1 FROM requirements WHERE workspace_id=$1 AND id='req-feat-empty'),
		        EXISTS (SELECT 1 FROM features WHERE workspace_id=$1 AND id='feat-empty')`,
		f.workspace).Scan(&emptySeeded, &emptySurvives); err != nil {
		t.Fatal(err)
	}
	if emptySeeded {
		t.Error("empty taxonomy node seeded a requirement; it should have dropped")
	}
	if emptySurvives {
		t.Error("empty taxonomy node survived the migration")
	}
	// A node that still carries content is never deleted.
	var contentSurvives int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM features WHERE workspace_id=$1 AND id <> 'feat-empty'`,
		f.workspace).Scan(&contentSurvives); err != nil {
		t.Fatal(err)
	}
	if contentSurvives != 3 {
		t.Errorf("surviving content-bearing features = %d, want 3", contentSurvives)
	}

	// A seed is visibly pending, never silently authoritative: current_version
	// stays NULL until an operator confirms.
	var (
		currentVersion *int32
		slug, title    string
		highWaterMark  int
	)
	if err := f.pool.QueryRow(f.ctx,
		`SELECT current_version, slug, title, statement_high_water_mark
		 FROM requirements WHERE workspace_id=$1 AND id='req-feat-text'`,
		f.workspace).Scan(&currentVersion, &slug, &title, &highWaterMark); err != nil {
		t.Fatal(err)
	}
	if currentVersion != nil {
		t.Errorf("seeded requirement current_version = %d, want NULL (pending)", *currentVersion)
	}
	if slug != "payments-reconciliation" {
		t.Errorf("seeded slug = %q, want payments-reconciliation", slug)
	}
	if title != "Payments Reconciliation" {
		t.Errorf("seeded title = %q", title)
	}
	// A seed issues no REQ-n, so the operator's first confirmed block starts at
	// REQ-1 rather than inheriting an invented identifier.
	if highWaterMark != 0 {
		t.Errorf("seeded high-water mark = %d, want 0", highWaterMark)
	}

	// The node's accumulated text survives verbatim as version 1, pending.
	var (
		content    string
		statements string
		origin     string
		confirmed  bool
		version    int
	)
	if err := f.pool.QueryRow(f.ctx,
		`SELECT version, content, statements_json::text, origin, confirmed
		 FROM requirement_versions WHERE workspace_id=$1 AND requirement_id='req-feat-text'`,
		f.workspace).Scan(&version, &content, &statements, &origin, &confirmed); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Errorf("seeded version = %d, want 1", version)
	}
	if want := "Payments Reconciliation\n\nPayments must reconcile nightly."; content != want {
		t.Errorf("seeded content = %q, want %q", content, want)
	}
	if statements != "[]" {
		t.Errorf("seeded statements = %s, want [] (Conveyor invents no intent)", statements)
	}
	if origin != "feature_migration" {
		t.Errorf("seeded origin = %q", origin)
	}
	if confirmed {
		t.Error("seeded version is confirmed; a migration seed must stay pending")
	}
	// A node with no description still seeds an honest document from its name.
	var bareContent string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT content FROM requirement_versions
		 WHERE workspace_id=$1 AND requirement_id='req-feat-task'`,
		f.workspace).Scan(&bareContent); err != nil {
		t.Fatal(err)
	}
	if bareContent != "Task Bearing" {
		t.Errorf("description-free seed content = %q, want %q", bareContent, "Task Bearing")
	}

	// tasks.feature_id converted to a durable history link.
	var linkKind, provenance string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT kind, legacy_created_by_event FROM links
		 WHERE workspace_id=$1 AND src_type='requirement' AND src_id='req-feat-task'
		   AND dst_type='task' AND dst_id=$2`,
		f.workspace, assignedTask).Scan(&linkKind, &provenance); err != nil {
		t.Fatalf("task feature assignment was not converted to a link: %v", err)
	}
	if linkKind != "historical_feature_assignment" {
		t.Errorf("link kind = %q", linkKind)
	}
	if provenance != "feature.migrated" {
		t.Errorf("link provenance = %q, want feature.migrated", provenance)
	}

	// The attachment re-homes onto the seeded requirement and claims exactly
	// one owner.
	var requirementID, featureID string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT COALESCE(requirement_id,''), COALESCE(feature_id,'') FROM artifact_links
		 WHERE workspace_id=$1 AND artifact_id=$2`,
		f.workspace, attachment).Scan(&requirementID, &featureID); err != nil {
		t.Fatal(err)
	}
	if requirementID != "req-feat-artifact" {
		t.Errorf("artifact re-homed to %q, want req-feat-artifact", requirementID)
	}
	if featureID != "" {
		t.Errorf("artifact still claims feature %q after re-homing", featureID)
	}
}

// A parent assigned after its blueprint children were materialized leaves each
// child's feature_id NULL, so the inherited assignment exists only implicitly.
// It is lineage all the same and AC-5 requires converting it.
func TestPhase62FeatureMigrationConvertsInheritedBlueprintChildrenIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.feature(t, "feat-parent", "Inherited Parent", "", "", 0)
	parentTask := f.task(t, "feat-parent", "")
	inheritedChild := f.task(t, "", parentTask)

	f.upgrade(t)

	var exists bool
	if err := f.pool.QueryRow(f.ctx,
		`SELECT EXISTS (SELECT 1 FROM requirements WHERE workspace_id=$1 AND id='req-feat-parent')`,
		f.workspace).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("a node owning only an inherited blueprint child was treated as empty")
	}
	for _, taskID := range []string{parentTask, inheritedChild} {
		var linked bool
		if err := f.pool.QueryRow(f.ctx,
			`SELECT EXISTS (SELECT 1 FROM links WHERE workspace_id=$1
			   AND src_type='requirement' AND src_id='req-feat-parent'
			   AND dst_type='task' AND dst_id=$2
			   AND kind='historical_feature_assignment')`,
			f.workspace, taskID).Scan(&linked); err != nil {
			t.Fatal(err)
		}
		if !linked {
			t.Errorf("task %s lost its feature assignment lineage", taskID)
		}
	}
}

// Feature names were unique only per parent, but requirement slugs are unique
// per workspace, so equal names in different branches must be disambiguated
// deterministically rather than colliding and failing the upgrade.
func TestPhase62FeatureMigrationDisambiguatesSlugCollisionsIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.feature(t, "feat-root", "Shared Name", "First branch.", "", 0)
	f.feature(t, "feat-nested", "Shared Name", "Second branch.", "feat-root", time.Second)

	f.upgrade(t)

	slugs := map[string]string{}
	rows, err := f.pool.Query(f.ctx,
		`SELECT id, slug FROM requirements WHERE workspace_id=$1 ORDER BY id`, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			t.Fatal(err)
		}
		slugs[id] = slug
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Creation order decides which document keeps the bare slug.
	if slugs["req-feat-root"] != "shared-name" {
		t.Errorf("earlier node slug = %q, want shared-name", slugs["req-feat-root"])
	}
	if slugs["req-feat-nested"] != "shared-name-2" {
		t.Errorf("later node slug = %q, want shared-name-2", slugs["req-feat-nested"])
	}
}

// Migration 040 denormalized feature_id onto monitor_observations and
// repository_drift as plain text with no foreign key. Dropping a node they name
// would strand the reference silently, so such a node is content-bearing.
func TestPhase62FeatureMigrationPreservesMonitorReferencesIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.feature(t, "feat-monitor", "Monitor Referenced", "", "", 0)
	f.feature(t, "feat-drift", "Drift Referenced", "", "", time.Second)
	driftTask := f.task(t, "", "")

	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO monitor_observations
		 (workspace_id, identity, repository, kind, occurrence_id, source_url, feature_id, observed_at)
		 VALUES ($1,$2,'repo','direct_push','occ-1','https://example.test/1','feat-monitor',$3)`,
		f.workspace, "identity-1", f.seeded); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO repository_drift
		 (workspace_id, id, repository, kind, source_url, feature_id, task_id, detected_at)
		 VALUES ($1,'drift-1','repo','direct_push','https://example.test/2','feat-drift',$2,$3)`,
		f.workspace, driftTask, f.seeded); err != nil {
		t.Fatal(err)
	}

	f.upgrade(t)

	for _, referenced := range []string{"feat-monitor", "feat-drift"} {
		var survives bool
		if err := f.pool.QueryRow(f.ctx,
			`SELECT EXISTS (SELECT 1 FROM features WHERE workspace_id=$1 AND id=$2)`,
			f.workspace, referenced).Scan(&survives); err != nil {
			t.Fatal(err)
		}
		if !survives {
			t.Errorf("feature %s was dropped while a monitor or drift row still names it", referenced)
		}
		var seeded bool
		if err := f.pool.QueryRow(f.ctx,
			`SELECT EXISTS (SELECT 1 FROM requirements WHERE workspace_id=$1 AND id=$2)`,
			f.workspace, "req-"+referenced).Scan(&seeded); err != nil {
			t.Fatal(err)
		}
		if !seeded {
			t.Errorf("feature %s did not seed a requirement", referenced)
		}
	}
}

// A migrated attachment must stay exactly as reachable as it was before the
// feature tree retired: the conversion recorded the task's assignment, so
// resolving through that edge keeps the artifact in the task's context.
func TestPhase62MigratedAttachmentStaysInTaskContextIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.feature(t, "feat-shared", "Shared Context", "Context for the whole feature.", "", 0)
	taskID := f.task(t, "feat-shared", "")
	attachment := f.artifact(t, "feat-shared", "task_context")

	f.upgrade(t)
	// Exercise the live scoped context APIs on the current schema after the
	// feature-era migration has produced its legacy lineage rows.
	f.upgradeTo(t, 0)

	assertReachable := func(t *testing.T, candidateTask string, want bool) {
		root := core.LineageNode{Type: core.LineageTask, ID: candidateTask}
		budget := core.LineageTraversalBudget{MaxDepth: config.DefaultLineageContextDepth, MaxNodes: config.DefaultLineageContextNodes, Workspace: f.workspace}
		links, err := f.store.ListLineageNeighborhood(f.ctx, []core.LineageNode{root}, budget)
		if err != nil {
			t.Fatal(err)
		}
		graph, err := core.TraverseLineage(links, []core.LineageNode{root}, budget)
		if err != nil {
			t.Fatal(err)
		}
		artifacts, err := f.store.ListArtifactsForLineage(f.ctx, graph.Nodes)
		if err != nil {
			t.Fatal(err)
		}
		selection, err := core.SelectContextArtifacts(links, []core.LineageNode{root}, artifacts, core.ContextArtifactSelectionOptions{Workspace: f.workspace, LocalTaskID: candidateTask})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, artifact := range selection.Artifacts {
			found = found || artifact.ID == attachment
		}
		if found != want {
			t.Fatalf("task %s reachable=%v want %v selection=%+v", candidateTask, found, want, selection)
		}
	}
	assertReachable(t, taskID, true)
	order := core.WorkOrder{ID: taskID + "-implement-1", TaskID: taskID, JobID: taskID + "-implement-1", Stage: core.StageImplement}
	if err := f.store.CreateJob(f.ctx, core.Job{ID: order.JobID, TaskID: taskID, Stage: order.Stage, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(f.store).CreateWorkOrder(f.ctx, order); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(f.store).ClaimWorkOrder(f.ctx, order.ID, core.WorkOrderClaim{SessionID: "migrated-reader", ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	service := &workorder.Service{Store: f.store}
	if _, err := service.ReadArtifact(f.ctx, order.ID, "migrated-reader", attachment); err != nil {
		t.Fatalf("MCP read before rebuild: %v", err)
	}
	artifact, content, err := f.store.GetArtifact(f.ctx, attachment)
	if err != nil || artifact.RequirementID != "req-feat-shared" || !strings.Contains(string(content), "feat-shared") {
		t.Fatalf("migrated artifact=%+v content=%q err=%v", artifact, content, err)
	}
	other := f.task(t, "", "")
	assertReachable(t, other, false)
	if _, err = f.store.RebuildLineage(f.ctx, core.LineageRebuildRequest{Reason: "migration reachability regression", RequestID: core.NewTaskID()}); err != nil {
		t.Fatal(err)
	}
	assertReachable(t, taskID, true)
	if _, err := service.ReadArtifact(f.ctx, order.ID, "migrated-reader", attachment); err != nil {
		t.Fatalf("MCP read after rebuild: %v", err)
	}
}

func TestRetiredFeatureConsumersBecomeRequirementReferencesIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.feature(t, "feat-runtime", "Runtime Intent", "", "", 0)
	taskID := f.task(t, "feat-runtime", "")
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO monitor_observations
		 (workspace_id, identity, repository, kind, occurrence_id, source_url, feature_id, observed_at)
		 VALUES ($1,'identity-runtime','repo','direct_push','occ-runtime',
		         'https://example.test/runtime','feat-runtime',$2)`,
		f.workspace, f.seeded); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO repository_drift
		 (workspace_id, id, repository, kind, source_url, feature_id, task_id, detected_at)
		 VALUES ($1,'drift-runtime','repo','direct_push',
		         'https://example.test/runtime','feat-runtime',$2,$3)`,
		f.workspace, taskID, f.seeded); err != nil {
		t.Fatal(err)
	}

	if err := migrateControlPlaneToVersion(t.Context(), f.pool, 47); err != nil {
		t.Fatalf("migrate to version 47: %v", err)
	}
	var observationRequirement, driftRequirement, taskFeature string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT observation.requirement_id, drift.requirement_id,
		        COALESCE(task.feature_id,'')
		   FROM monitor_observations observation
		   JOIN repository_drift drift
		     ON drift.workspace_id=observation.workspace_id
		    AND drift.id='drift-runtime'
		   JOIN tasks task
		     ON task.workspace_id=observation.workspace_id
		    AND task.id=$2
		  WHERE observation.workspace_id=$1
		    AND observation.identity='identity-runtime'`,
		f.workspace, taskID).Scan(&observationRequirement, &driftRequirement, &taskFeature); err != nil {
		t.Fatal(err)
	}
	if observationRequirement != "req-feat-runtime" ||
		driftRequirement != "req-feat-runtime" || taskFeature != "" {
		t.Fatalf("observation requirement=%q drift requirement=%q task feature=%q",
			observationRequirement, driftRequirement, taskFeature)
	}
	var historicalLinks, retiredColumns int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT
		    (SELECT count(*) FROM links
		      WHERE workspace_id=$1
		        AND kind='historical_feature_assignment'
		        AND src_id='req-feat-runtime'
		        AND dst_id=$2),
		    (SELECT count(*) FROM information_schema.columns
		      WHERE table_schema=current_schema()
		        AND table_name IN ('monitor_observations','repository_drift')
		        AND column_name='feature_id')`,
		f.workspace, taskID).Scan(&historicalLinks, &retiredColumns); err != nil {
		t.Fatal(err)
	}
	if historicalLinks != 1 || retiredColumns != 0 {
		t.Fatalf("historical links=%d retired monitor columns=%d", historicalLinks, retiredColumns)
	}
}

// The upgrade must be a no-op for an installation that never used the feature
// tree — the overwhelmingly common case.
func TestPhase62FeatureMigrationIsInertWithoutFeaturesIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.upgrade(t)

	var requirements, links int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT (SELECT count(*) FROM requirements WHERE workspace_id=$1),
		        (SELECT count(*) FROM links WHERE workspace_id=$1
		           AND kind='historical_feature_assignment')`,
		f.workspace).Scan(&requirements, &links); err != nil {
		t.Fatal(err)
	}
	if requirements != 0 || links != 0 {
		t.Errorf("inert upgrade produced requirements=%d links=%d, want 0 and 0", requirements, links)
	}
	var version int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT max(version) FROM conveyor_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 46 {
		t.Errorf("schema version = %d, want 46", version)
	}
}

func TestPhase62RepairMigrationHandlesSharedArtifactsAndGlobalSlugCollisionsIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.feature(t, "feat-auth-1", "Auth", "First auth requirement.", "", 0)
	f.feature(t, "feat-auth-2", "Auth", "Second auth requirement.", "feat-auth-1", time.Second)
	f.feature(t, "feat-auth-2-base", "Auth 2", "Independent suffix-shaped base.", "", 2*time.Second)
	f.feature(t, "feat-empty-audit", "Empty Audit Node", "", "", 3*time.Second)

	artifactID := core.NewTaskID()
	content := []byte("shared auth context")
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO artifacts (id, workspace_id, name, content_type, size_bytes, content, created_at)
		 VALUES ($1,$2,'shared.txt','text/plain',$3,$4,$5)`,
		artifactID, f.workspace, len(content), content, f.seeded); err != nil {
		t.Fatal(err)
	}
	for _, featureID := range []string{"feat-auth-1", "feat-auth-2"} {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO artifact_links (workspace_id, artifact_id, feature_id, role)
			 VALUES ($1,$2,$3,'task_context')`, f.workspace, artifactID, featureID); err != nil {
			t.Fatal(err)
		}
	}

	f.upgradeTo(t, 50)

	wantSlugs := map[string]string{
		"req-feat-auth-1":      "auth",
		"req-feat-auth-2":      "auth-2",
		"req-feat-auth-2-base": "auth-2-2",
	}
	rows, err := f.pool.Query(f.ctx,
		`SELECT id,slug FROM requirements WHERE workspace_id=$1`, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			t.Fatal(err)
		}
		if want, exists := wantSlugs[id]; exists {
			if slug != want {
				t.Errorf("requirement %s slug=%q want %q", id, slug, want)
			}
			delete(wantSlugs, id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(wantSlugs) != 0 {
		t.Fatalf("missing migrated slugs: %v", wantSlugs)
	}
	var linkCount, distinctRequirements int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*), count(DISTINCT requirement_id)
		 FROM artifact_links WHERE workspace_id=$1 AND artifact_id=$2`,
		f.workspace, artifactID).Scan(&linkCount, &distinctRequirements); err != nil {
		t.Fatal(err)
	}
	if linkCount != 2 || distinctRequirements != 2 {
		t.Fatalf("shared artifact links=%d distinct requirements=%d", linkCount, distinctRequirements)
	}
	for requirementID := range map[string]bool{
		"req-feat-auth-1": true, "req-feat-auth-2": true, "req-feat-auth-2-base": true,
	} {
		events, err := f.store.ListRequirementEvents(f.ctx, requirementID)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[0].Kind != "requirement.created" ||
			events[1].Kind != "requirement.version_proposed" {
			t.Errorf("seed lineage for %s=%+v", requirementID, events)
		}
	}
	var droppedName string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT payload_json->>'name' FROM events
		 WHERE workspace_id=$1 AND kind='migration.feature_node_dropped'
		   AND payload_json->>'feature_id'='feat-empty-audit'`, f.workspace).Scan(&droppedName); err != nil {
		t.Fatal(err)
	}
	if droppedName != "Empty Audit Node" {
		t.Fatalf("dropped-node audit name=%q", droppedName)
	}
	var checksum string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT checksum FROM conveyor_schema_migrations WHERE version=46`).Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if checksum != "3824782d447b5128661e770239b3517dde302f3f18a0f889a1e82e1e448d80e7" {
		t.Fatalf("migration 046 ledger checksum=%s", checksum)
	}
}

func TestPhase62RepairMigrationUpgradesApplied046And047AndNullsDanglingReferencesIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.feature(t, "feat-valid", "Valid", "Valid requirement.", "", 0)
	driftTask := f.task(t, "", "")
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO monitor_observations
		 (workspace_id, identity, repository, kind, occurrence_id, source_url, feature_id, observed_at)
		 VALUES ($1,'identity-dangling','repo','direct_push','occ-dangling',
		         'https://example.test/dangling','missing-feature',$2)`,
		f.workspace, f.seeded); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO repository_drift
		 (workspace_id, id, repository, kind, source_url, feature_id, task_id, detected_at)
		 VALUES ($1,'drift-dangling','repo','direct_push',
		         'https://example.test/dangling','missing-feature',$2,$3)`,
		f.workspace, driftTask, f.seeded); err != nil {
		t.Fatal(err)
	}
	f.upgradeTo(t, 47)

	before := map[int]string{}
	for _, version := range []int{46, 47} {
		var checksum string
		if err := f.pool.QueryRow(f.ctx,
			`SELECT checksum FROM conveyor_schema_migrations WHERE version=$1`, version).Scan(&checksum); err != nil {
			t.Fatal(err)
		}
		before[version] = checksum
	}
	f.upgradeTo(t, 50)

	var observationRequirement, driftRequirement *string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT observation.requirement_id, drift.requirement_id
		 FROM monitor_observations observation
		 JOIN repository_drift drift ON drift.workspace_id=observation.workspace_id
		 WHERE observation.workspace_id=$1 AND observation.identity='identity-dangling'
		   AND drift.id='drift-dangling'`, f.workspace).Scan(&observationRequirement, &driftRequirement); err != nil {
		t.Fatal(err)
	}
	if observationRequirement != nil || driftRequirement != nil {
		t.Fatalf("dangling references observation=%v drift=%v", observationRequirement, driftRequirement)
	}
	var repairEvents, foreignKeys int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT
		   (SELECT count(*) FROM events WHERE workspace_id=$1
		      AND kind='migration.requirement_reference_repaired'),
		   (SELECT count(*) FROM information_schema.table_constraints
		      WHERE table_schema=current_schema()
		        AND constraint_name IN ('monitor_observations_requirement_fk','repository_drift_requirement_fk'))`,
		f.workspace).Scan(&repairEvents, &foreignKeys); err != nil {
		t.Fatal(err)
	}
	if repairEvents != 2 || foreignKeys != 2 {
		t.Fatalf("repair events=%d foreign keys=%d", repairEvents, foreignKeys)
	}
	for version, checksum := range before {
		var after string
		if err := f.pool.QueryRow(f.ctx,
			`SELECT checksum FROM conveyor_schema_migrations WHERE version=$1`, version).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != checksum {
			t.Errorf("migration %d checksum changed from %s to %s", version, checksum, after)
		}
	}
	events, err := f.store.ListRequirementEvents(f.ctx, "req-feat-valid")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("backfilled applied-046 lineage=%+v", events)
	}
}

func TestRequirementServesMigrationBackfillsSuggestionsAsProposalsIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.feature(t, "feat-serves", "Serves Backfill", "Legacy suggestion target.", "", 0)
	f.upgradeTo(t, 50)

	blueprintTaskID := f.task(t, "", "")
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE tasks SET source='planning:legacy-session' WHERE workspace_id=$1 AND id=$2`,
		f.workspace, blueprintTaskID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AppendEvent(f.ctx, core.Event{
		TaskID: blueprintTaskID, Kind: "task.requirement_suggested",
		ActorID: "legacy-planner", ActorRole: core.ActorAgent,
		Payload: []byte(`{"requirement_id":"req-feat-serves","requirement_title":"Serves Backfill"}`),
		At:      f.seeded.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	// Migration 051 first records the historical lifecycle; migration 103 folds
	// it into the single task-context proposal table used by current code.
	f.upgradeTo(t, 103)

	links, err := f.store.ListRequirementServes(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("backfilled serves links=%+v, want one proposal", links)
	}
	link := links[0]
	if link.BlueprintTaskID != blueprintTaskID || link.RequirementID != "req-feat-serves" ||
		link.State != core.RequirementServesProposed || link.Source != core.RequirementServesPlanning ||
		link.ProposedBy != "legacy-planner" || link.CreatedByEventID == 0 || link.DecisionEventID != 0 {
		t.Fatalf("backfilled serves link=%+v", link)
	}
}

// Migration 056 declares the session goal. Rows written
// before it predate the declaration, so they take `open` — which is exactly
// their historical behavior — and the CHECK refuses anything outside the three.
func TestPlanningSessionGoalMigrationDefaultsExistingRowsToOpenIntegration(t *testing.T) {
	f := newPhase62MigrationFixture(t)
	f.upgradeTo(t, 55)

	legacyID := "session-" + core.NewTaskID()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO planning_sessions (workspace_id,id,title,status,created_at,updated_at)
		 VALUES ($1,$2,$3,'active',now(),now())`,
		f.workspace, legacyID, "New requirement"); err != nil {
		t.Fatal(err)
	}

	f.upgradeTo(t, 56)

	var migratedGoal core.PlanningSessionGoal
	var migratedTitle string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT goal,title FROM planning_sessions WHERE workspace_id=$1 AND id=$2`,
		f.workspace, legacyID).Scan(&migratedGoal, &migratedTitle); err != nil {
		t.Fatal(err)
	}
	if migratedGoal != core.PlanningGoalOpen || migratedTitle != "New requirement" {
		t.Fatalf("migrated goal/title=%q/%q, want open/New requirement", migratedGoal, migratedTitle)
	}
	// The column constraint is the last line of defense behind the service and
	// API validation, so it must reject an unknown goal on its own.
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO planning_sessions (workspace_id,id,title,status,goal,created_at,updated_at)
		 VALUES ($1,$2,'Epic','active','epic',now(),now())`,
		f.workspace, "session-"+core.NewTaskID()); err == nil {
		t.Fatal("the goal CHECK accepted an unknown goal")
	}
	// Exercise the current store contract only after newer immutable session
	// fields have been installed by the remaining schema migrations.
	f.upgradeTo(t, 65)
	for _, goal := range []core.PlanningSessionGoal{
		core.PlanningGoalRequirement, core.PlanningGoalSystemDesign, core.PlanningGoalBlueprint, core.PlanningGoalOpen,
	} {
		created, createErr := f.store.CreatePlanningSession(f.ctx, core.PlanningSession{
			ID: "session-" + core.NewTaskID(), Title: goal.ProvisionalTitle(), Goal: goal,
		})
		if createErr != nil || created.Goal != goal {
			t.Fatalf("goal %q created=%+v err=%v", goal, created, createErr)
		}
	}
}
