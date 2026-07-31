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
)

// Migration 046 retires the curated feature tree into the flat requirement
// corpus (spec §21.46 changes 2 and 7). The contract is losslessness: a node
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
	if err := migrateControlPlaneToVersion(t.Context(), f.pool, 46); err != nil {
		t.Fatalf("migrate to version 46: %v", err)
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
	// stays NULL until an operator confirms (spec §4.2 item 1).
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

	// tasks.feature_id converted to a durable history link (spec §16).
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
	// one owner (spec §21.46 change 5).
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

	artifact, content, err := f.store.GetArtifactForContext(f.ctx, attachment, taskID)
	if err != nil {
		t.Fatalf("migrated attachment left the task's context: %v", err)
	}
	if artifact.RequirementID != "req-feat-shared" {
		t.Errorf("resolved artifact requirement = %q, want req-feat-shared", artifact.RequirementID)
	}
	if !strings.Contains(string(content), "feat-shared") {
		t.Errorf("resolved unexpected content %q", content)
	}
	// The task no longer needs to know the retired feature id to reach it.
	if _, _, err = f.store.GetArtifactForContext(f.ctx, attachment, taskID); err != nil {
		t.Errorf("attachment unreachable without a feature id: %v", err)
	}
	// An unrelated task must not gain access through the requirement.
	other := f.task(t, "", "")
	if _, _, err = f.store.GetArtifactForContext(f.ctx, attachment, other); err == nil {
		t.Error("an unrelated task resolved a requirement-scoped attachment")
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
