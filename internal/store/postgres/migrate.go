package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/queue/riverqueue"
	controlstore "github.com/kidus-tiliksew/conveyor/internal/store"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate applies Conveyor's versioned schema followed by River's bundled
// queue migrations. A database-wide session lock serializes the complete
// sequence because River v0.30.2 discovers pending versions before opening the
// per-version transaction that records them (design-database).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	pooled, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	// Detach the lock connection so a single-connection pool can still execute
	// the migrations through its ordinary executor while this session waits.
	lockConn := pooled.Hijack()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock(hashtext('conveyor:startup-migrations'))"); err != nil {
		_ = lockConn.Close(ctx)
		return fmt.Errorf("lock startup migrations: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = lockConn.Exec(unlockCtx, "SELECT pg_advisory_unlock(hashtext('conveyor:startup-migrations'))")
		_ = lockConn.Close(unlockCtx)
	}()

	if err := migrateControlPlane(ctx, pool); err != nil {
		return err
	}
	if err := riverqueue.Migrate(ctx, pool); err != nil {
		return err
	}
	// The event log's tables are owned by its driver and created the same way
	// River's are: idempotently, under the startup lock, outside the numbered
	// control-plane sequence (log-core migration plan, phase 1).
	if err := pglog.EnsureSchema(ctx, pool); err != nil {
		return err
	}
	// v1.10 adds workspace identity to River payloads. Backfill jobs inserted by
	// v1.8 so an in-place upgrade does not strand queued dispatch or review
	// publication work with an empty context.
	if _, err := pool.Exec(ctx, `
UPDATE river_job r SET args = jsonb_set(r.args, '{workspace_id}', to_jsonb(t.workspace_id), true)
FROM tasks t
WHERE r.kind = 'dispatch_task'
  AND r.args->>'task_id' = t.id
  AND COALESCE(r.args->>'workspace_id','') = '';
UPDATE river_job r SET args = jsonb_set(r.args, '{workspace_id}', to_jsonb(p.workspace_id), true)
FROM review_publications p
WHERE r.kind = 'review_publication'
  AND r.args->>'review_work_order_id' = p.review_work_order_id
  AND COALESCE(r.args->>'workspace_id','') = ''`); err != nil {
		return fmt.Errorf("backfill River workspace context: %w", err)
	}
	return nil
}

func migrateControlPlane(ctx context.Context, pool *pgxpool.Pool) error {
	return migrateControlPlaneToVersion(ctx, pool, 0)
}

// migrateControlPlaneToVersion runs the production migration path through an
// optional upper bound. Production passes zero for every embedded migration;
// integration coverage uses a historical bound to exercise real upgrades.
func migrateControlPlaneToVersion(ctx context.Context, pool *pgxpool.Pool, maxVersion int) error {
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	embeddedVersion, err := latestMigrationVersion(files)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck -- commit below owns the outcome
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('conveyor:control-plane-migrations'))"); err != nil {
		return fmt.Errorf("lock control-plane migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
CREATE TABLE IF NOT EXISTS conveyor_schema_migrations (
    version integer PRIMARY KEY,
    name text NOT NULL,
    checksum text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	if _, err := tx.Exec(ctx, "ALTER TABLE conveyor_schema_migrations ADD COLUMN IF NOT EXISTS checksum text NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("upgrade migration ledger: %w", err)
	}
	var storeVersion int
	if err := tx.QueryRow(ctx, "SELECT COALESCE(max(version), 0) FROM conveyor_schema_migrations").Scan(&storeVersion); err != nil {
		return fmt.Errorf("inspect store schema version: %w", err)
	}
	if storeVersion > embeddedVersion {
		return fmt.Errorf("store schema version %d is newer than this Conveyor binary (latest embedded migration %d); install a Conveyor release at least as new as the one that upgraded this database before restarting", storeVersion, embeddedVersion)
	}
	for _, name := range files {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if maxVersion > 0 && version > maxVersion {
			break
		}
		rawSQL, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		checksum := migrationChecksum(rawSQL)
		var appliedName, appliedChecksum string
		err = tx.QueryRow(ctx,
			"SELECT name, checksum FROM conveyor_schema_migrations WHERE version = $1",
			version,
		).Scan(&appliedName, &appliedChecksum)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check migration %d: %w", version, err)
		}
		if err == nil {
			if appliedName != filepath.Base(name) {
				if compatibleHistoricalMigration(version, appliedName, appliedChecksum) {
					continue
				}
				return fmt.Errorf("migration version %d was recorded as %q, now %q", version, appliedName, filepath.Base(name))
			}
			if appliedChecksum == "" {
				if _, err := tx.Exec(ctx, "UPDATE conveyor_schema_migrations SET checksum = $2 WHERE version = $1", version, checksum); err != nil {
					return fmt.Errorf("backfill migration %d checksum: %w", version, err)
				}
			} else if appliedChecksum != checksum {
				return fmt.Errorf("migration %s checksum changed after application", name)
			}
			continue
		}
		if version == 35 {
			violations, auditErr := auditPersistedLifecycles(ctx, tx)
			if auditErr != nil {
				return fmt.Errorf("audit lifecycle history before migration %s: %w", name, auditErr)
			}
			if len(violations) != 0 {
				limit := len(violations)
				if limit > 20 {
					limit = 20
				}
				return fmt.Errorf("migration %s blocked by %d non-canonical lifecycle edge(s): %+v", name, len(violations), violations[:limit])
			}
		}
		sql, err := renderMigration(rawSQL)
		if err != nil {
			return fmt.Errorf("render migration %s: %w", name, err)
		}
		sql, err = repairPendingMigration(version, sql)
		if err != nil {
			return fmt.Errorf("prepare pending migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if version == 57 {
			if err := recordLineageRepairAudit(ctx, tx); err != nil {
				return fmt.Errorf("record lineage repair audit: %w", err)
			}
		}
		if version == 60 {
			if err := recordPullRequestIdentityRepairAudit(ctx, tx); err != nil {
				return fmt.Errorf("record pull request identity repair audit: %w", err)
			}
		}
		if version == 94 {
			if err := recordRequirementVersionRetirementAudit(ctx, tx); err != nil {
				return fmt.Errorf("record requirement version retirement audit: %w", err)
			}
		}
		if version == 101 {
			if err := recordWorkOrderZombieRetirementAudit(ctx, tx); err != nil {
				return fmt.Errorf("record work-order zombie retirement audit: %w", err)
			}
		}
		if version == 115 {
			if err := recordDecisionSupersessionSweepBackfillAudit(ctx, tx); err != nil {
				return fmt.Errorf("record decision supersession sweep backfill audit: %w", err)
			}
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO conveyor_schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
			version, filepath.Base(name), checksum,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	// Every legacy event insert mirrors into the event log, so the log's
	// tables must exist wherever the control-plane schema does, including
	// fixtures migrated to an older version.
	if err := pglog.EnsureSchema(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit control-plane migrations: %w", err)
	}
	return nil
}

const (
	historicalArchivalMigrationName     = "118_requirement_system_design_archival.sql"
	historicalArchivalMigrationChecksum = "065f9572d4b23afed5548d861478e1e9a5c84dcaf917a11afd7624cc6a1666df"
)

// compatibleHistoricalMigration recognizes the one known ledger identity
// created by the version-118 branch collision. The row remains immutable;
// migration 119 idempotently establishes both colliding schemas.
func compatibleHistoricalMigration(version int, name, checksum string) bool {
	return version == 118 &&
		name == historicalArchivalMigrationName &&
		(checksum == "" || checksum == historicalArchivalMigrationChecksum)
}

func recordDecisionSupersessionSweepBackfillAudit(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
INSERT INTO events (workspace_id,kind,actor_id,actor_role,payload_json,at)
SELECT workspace_id,'decision.supersession_sweep_opened','migration-115','system',
       jsonb_build_object(
         'decision_id',decision_id,
         'superseded_decision_id',superseded_decision_id,
         'document_tier',document_tier,
         'document_id',document_id,
         'status',status,
         'detected_by',detected_by,
         'detected_at',detected_at
       ),detected_at
FROM decision_supersession_sweeps
WHERE detected_by='migration-115'`)
	return err
}

func latestMigrationVersion(files []string) (int, error) {
	if len(files) == 0 {
		return 0, errors.New("Conveyor binary contains no embedded store migrations")
	}
	latest := 0
	for _, name := range files {
		version, err := migrationVersion(name)
		if err != nil {
			return 0, err
		}
		if version > latest {
			latest = version
		}
	}
	return latest, nil
}

// recordLineageRepairAudit deliberately lives in application code: schema
// migrations may repair projections but never manufacture ledger events.
func recordLineageRepairAudit(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
INSERT INTO events (workspace_id,kind,actor_id,actor_role,payload_json,at)
SELECT w.id,'lineage.vocabulary_repaired','system','system',jsonb_build_object(
  'reason','migration 057 canonicalized the lineage projection',
  'excluded_count',count(x.*),
  'excluded',COALESCE(jsonb_agg(jsonb_build_object('src_type',x.src_type,'src_id',x.src_id,'dst_type',x.dst_type,'dst_id',x.dst_id,'kind',x.kind,'reason',x.reason)) FILTER (WHERE x.workspace_id IS NOT NULL),'[]'::jsonb)
),now()
FROM workspaces w LEFT JOIN lineage_repair_exclusions x ON x.workspace_id=w.id
WHERE NOT EXISTS (SELECT 1 FROM events e WHERE e.workspace_id=w.id AND e.kind='lineage.vocabulary_repaired')
GROUP BY w.id;
INSERT INTO events (workspace_id,kind,actor_id,actor_role,payload_json,at)
SELECT w.id,'lineage.historical_fabrication_recorded','system','system',jsonb_build_object(
  'migration',54,'event_kind','task.dependency_added','known_ids','74871-74888',
  'reason','migration 054 fabricated backdated dependency events; records remain immutable'
),now()
FROM workspaces w
WHERE EXISTS (SELECT 1 FROM events historical
  WHERE historical.workspace_id=w.id AND historical.kind='task.dependency_added'
    AND historical.id BETWEEN 74871 AND 74888)
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.workspace_id=w.id AND e.kind='lineage.historical_fabrication_recorded')`)
	return err
}

// recordPullRequestIdentityRepairAudit records only workspaces whose migration
// 060 projection changed or whose exclusion reason was newly established.
// The SQL migration owns projection repair; application code owns ledger events.
func recordPullRequestIdentityRepairAudit(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
WITH reason_counts AS (
  SELECT workspace_id,action,reason,count(*) AS count
  FROM migration_060_actions
  GROUP BY workspace_id,action,reason
), summaries AS (
  SELECT workspace_id,
    sum(count) FILTER (WHERE action='canonicalized') AS canonicalized_count,
    sum(count) FILTER (WHERE action='reverted_name_fallback') AS reverted_name_fallback_count,
    sum(count) FILTER (WHERE action='excluded') AS excluded_count,
    jsonb_agg(jsonb_build_object('action',action,'reason',reason,'count',count)
      ORDER BY action,reason) AS reasons
  FROM reason_counts GROUP BY workspace_id
)
INSERT INTO events (workspace_id,kind,actor_id,actor_role,payload_json,at)
SELECT summary.workspace_id,'lineage.pull_request_identity_repaired','system','system',
  jsonb_build_object(
    'migration',60,
    'canonicalized_count',COALESCE(summary.canonicalized_count,0),
    'reverted_name_fallback_count',COALESCE(summary.reverted_name_fallback_count,0),
    'excluded_count',COALESCE(summary.excluded_count,0),
    'reasons',summary.reasons
  ),now()
FROM summaries summary
WHERE NOT EXISTS (
  SELECT 1 FROM events event
  WHERE event.workspace_id=summary.workspace_id
    AND event.kind='lineage.pull_request_identity_repaired'
)`)
	return err
}

// recordRequirementVersionRetirementAudit appends one lifecycle event for
// each migration-094 repair. The schema migration owns the projection update;
// application code owns append-only ledger writes (design-database).
func recordRequirementVersionRetirementAudit(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
INSERT INTO events (workspace_id,kind,actor_id,actor_role,payload_json,at)
SELECT repair.workspace_id,'requirement.version_retired','migration-094','system',jsonb_build_object(
  'workspace_id',repair.workspace_id,
  'requirement_id',repair.requirement_id,
  'version',repair.version,
  'retired_by','migration-094',
  'confirmed_version',repair.confirmed_version,
  'reason','superseded before migration 094'
),now()
FROM migration_094_retired_requirement_versions repair`)
	return err
}

// recordWorkOrderZombieRetirementAudit appends the lifecycle evidence for the
// projection repaired by migration 101. The temporary table is transaction-
// local, so projection and ledger either commit together or not at all.
func recordWorkOrderZombieRetirementAudit(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
INSERT INTO events (workspace_id,task_id,job_id,kind,actor_id,actor_role,payload_json,at)
SELECT repair.workspace_id,repair.task_id,repair.job_id,'work_order.retired','migration-101','system',jsonb_build_object(
  'work_order_id',repair.work_order_id,
  'authoritative_work_order_id',repair.authoritative_work_order_id,
  'stage',repair.stage,
  'prior_state',repair.prior_state,
  'new_state','cancelled',
  'reason',repair.reason,
  'migration',101
),now()
FROM work_order_zombie_backfill repair`)
	return err
}

// repairPendingMigration overlays safety repairs only while an affected
// historical migration is still pending. The embedded bytes and the checksum
// recorded in conveyor_schema_migrations remain unchanged, so databases that
// already applied the version retain an immutable ledger. Forward migration
// 050 repairs their projections; this overlay prevents pre-046 databases from
// failing before they can reach it.
func repairPendingMigration(version int, sql []byte) ([]byte, error) {
	if version == 55 {
		text := string(sql)
		const old = "(event.payload_json ->> 'version')::integer"
		const replacement = "(CASE WHEN event.payload_json ->> 'version' ~ '^[0-9]{1,9}$' THEN (event.payload_json ->> 'version')::integer ELSE 0 END)"
		if found := strings.Count(text, old); found != 1 {
			return nil, fmt.Errorf("pending-055 repair found %d occurrences of %q, want 1", found, old)
		}
		return []byte("-- Pending-055 safety overlay: guard historical numeric payloads.\n" + strings.ReplaceAll(text, old, replacement)), nil
	}
	if version == 54 {
		text := string(sql)
		for _, replacement := range []struct {
			old   string
			new   string
			count int
		}{
			{
				old:   "(event.payload_json ->> 'origin_spec_version')::integer",
				new:   "(CASE WHEN event.payload_json ->> 'origin_spec_version' ~ '^[0-9]{1,9}$' THEN (event.payload_json ->> 'origin_spec_version')::integer ELSE 0 END)",
				count: 1,
			},
			{
				old:   "(event.payload_json ->> 'version')::integer",
				new:   "(CASE WHEN event.payload_json ->> 'version' ~ '^[0-9]{1,9}$' THEN (event.payload_json ->> 'version')::integer ELSE 0 END)",
				count: 4,
			},
		} {
			if found := strings.Count(text, replacement.old); found != replacement.count {
				return nil, fmt.Errorf("pending-054 repair found %d occurrences of %q, want %d", found, replacement.old, replacement.count)
			}
			text = strings.ReplaceAll(text, replacement.old, replacement.new)
		}
		return []byte("-- Pending-054 safety overlay: guard historical numeric payloads.\n" + text), nil
	}
	if version != 46 {
		return sql, nil
	}
	text := string(sql)
	var err error
	text, err = replaceMigrationSection(text,
		"CREATE TEMPORARY TABLE migration_046_seeded_requirements AS",
		") collision ON collision.id = source.id;",
		pending046SlugAllocationSQL)
	if err != nil {
		return nil, err
	}
	text, err = insertMigrationSQL(text,
		"-- Feature-scoped artifact attachments re-home onto the seeded requirement.\nUPDATE artifact_links link",
		pending046ArtifactIndexSQL)
	if err != nil {
		return nil, err
	}
	text, err = insertMigrationSQL(text,
		"-- Empty nodes drop only now that every surviving reference is recorded",
		pending046DroppedFeatureAuditSQL)
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

func replaceMigrationSection(text, start, end, replacement string) (string, error) {
	startAt := strings.Index(text, start)
	if startAt < 0 {
		return "", fmt.Errorf("repair anchor %q not found", start)
	}
	endAt := strings.Index(text[startAt:], end)
	if endAt < 0 {
		return "", fmt.Errorf("repair anchor %q not found", end)
	}
	endAt += startAt + len(end)
	return text[:startAt] + replacement + text[endAt:], nil
}

func insertMigrationSQL(text, anchor, insertion string) (string, error) {
	at := strings.Index(text, anchor)
	if at < 0 {
		return "", fmt.Errorf("repair anchor %q not found", anchor)
	}
	return text[:at] + insertion + "\n\n" + text[at:], nil
}

const pending046SlugAllocationSQL = `-- Pending-046 safety overlay: allocate against the full workspace slug set.
CREATE TEMPORARY TABLE migration_046_seeded_requirements AS
WITH RECURSIVE candidates AS (
    SELECT
        content.id AS feature_id,
        content.workspace_id,
        'req-' || content.id AS requirement_id,
        content.name AS title,
        content.description,
        coalesce(
            nullif(
                btrim(
                    left(
                        btrim(regexp_replace(lower(content.name), '[^a-z0-9]+', '-', 'g'), '-'),
                        80
                    ),
                    '-'
                ),
                ''
            ),
            'requirement'
        ) AS base_slug,
        row_number() OVER (
            PARTITION BY content.workspace_id
            ORDER BY feature.created_at, content.id
        ) AS allocation_order
    FROM migration_046_content_features content
    JOIN features feature
      ON feature.id = content.id
     AND feature.workspace_id = content.workspace_id
), allocated AS (
    SELECT
        candidate.feature_id, candidate.workspace_id, candidate.requirement_id,
        candidate.title, candidate.description, candidate.base_slug,
        candidate.allocation_order, candidate.base_slug AS slug,
        ARRAY[candidate.base_slug]::text[] AS used_slugs
    FROM candidates candidate
    WHERE candidate.allocation_order = 1

    UNION ALL

    SELECT
        candidate.feature_id, candidate.workspace_id, candidate.requirement_id,
        candidate.title, candidate.description, candidate.base_slug,
        candidate.allocation_order, choice.slug,
        prior.used_slugs || choice.slug
    FROM allocated prior
    JOIN candidates candidate
      ON candidate.workspace_id = prior.workspace_id
     AND candidate.allocation_order = prior.allocation_order + 1
    CROSS JOIN LATERAL (
        SELECT proposed.slug
        FROM generate_series(1, candidate.allocation_order::integer + 1) ordinal
        CROSS JOIN LATERAL (
            SELECT CASE
                WHEN ordinal = 1 THEN candidate.base_slug
                ELSE rtrim(
                    left(candidate.base_slug, greatest(1, 80 - length('-' || ordinal::text))),
                    '-'
                ) || '-' || ordinal::text
            END AS slug
        ) proposed
        WHERE NOT proposed.slug = ANY(prior.used_slugs)
        ORDER BY ordinal
        LIMIT 1
    ) choice
)
SELECT feature_id, workspace_id, requirement_id, title, slug, description
FROM allocated;`

const pending046ArtifactIndexSQL = `-- Pending-046 safety overlay: the requirement owner must participate in the
-- unattached-link predicate before feature ownership is cleared.
DROP INDEX artifact_links_workspace_unique;
CREATE UNIQUE INDEX artifact_links_workspace_unique
    ON artifact_links (workspace_id, artifact_id, role)
    WHERE task_id IS NULL AND feature_id IS NULL AND requirement_id IS NULL;`

const pending046DroppedFeatureAuditSQL = `-- Pending-046 safety overlay: retain identifying data until migration 050
-- can append the workspace audit events allowed by the final event constraint.
CREATE TABLE conveyor_migration_046_dropped_features (
    workspace_id text NOT NULL,
    feature_id text NOT NULL,
    parent_id text,
    name text NOT NULL,
    description text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, feature_id)
);
INSERT INTO conveyor_migration_046_dropped_features
    (workspace_id, feature_id, parent_id, name, description, created_at)
SELECT feature.workspace_id, feature.id, feature.parent_id, feature.name,
       feature.description, feature.created_at
FROM features feature
WHERE NOT EXISTS (
    SELECT 1 FROM migration_046_content_features content
    WHERE content.id = feature.id
      AND content.workspace_id = feature.workspace_id
);`

func auditPersistedLifecycles(ctx context.Context, tx pgx.Tx) ([]controlstore.LifecycleAuditViolation, error) {
	// Workspace-scoped events (worker heartbeats, config updates, pairing)
	// carry no task and are not lifecycle edges.
	rows, err := tx.Query(ctx, `SELECT id,task_id,job_id,kind,payload_json,at FROM events WHERE task_id IS NOT NULL ORDER BY at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []core.Event
	for rows.Next() {
		var event core.Event
		var taskID, jobID *string
		if err = rows.Scan(&event.ID, &taskID, &jobID, &event.Kind, &event.Payload, &event.At); err != nil {
			return nil, err
		}
		if taskID != nil {
			event.TaskID = *taskID
		}
		if jobID != nil {
			event.JobID = *jobID
		}
		events = append(events, event)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return controlstore.AuditLifecycleHistory(events), nil
}

func renderMigration(sql []byte) ([]byte, error) {
	text := string(sql)
	text = strings.ReplaceAll(text, "{{task_states}}", quotedTaskStates())
	text = strings.ReplaceAll(text, "{{work_order_states}}", quotedWorkOrderStates())
	if strings.Contains(text, "{{") || strings.Contains(text, "}}") {
		return nil, fmt.Errorf("unknown migration template marker")
	}
	return []byte(text), nil
}

func quotedTaskStates() string {
	values := make([]string, 0, len(core.TaskStates()))
	for _, state := range core.TaskStates() {
		values = append(values, "'"+string(state)+"'")
	}
	return strings.Join(values, ", ")
}

func quotedWorkOrderStates() string {
	values := make([]string, 0, len(core.WorkOrderStates()))
	for _, state := range core.WorkOrderStates() {
		values = append(values, "'"+string(state)+"'")
	}
	return strings.Join(values, ", ")
}

func migrationChecksum(sql []byte) string {
	digest := sha256.Sum256(sql)
	return hex.EncodeToString(digest[:])
}

func migrationVersion(name string) (int, error) {
	base := filepath.Base(name)
	prefix, _, ok := strings.Cut(base, "_")
	if !ok {
		return 0, fmt.Errorf("migration %q has no numeric prefix", base)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("migration %q has invalid version", base)
	}
	return version, nil
}
