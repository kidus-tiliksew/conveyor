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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	controlstore "github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate applies Conveyor's versioned schema followed by River's bundled
// queue migrations. Both use Postgres advisory locking, so multiple daemon
// instances may start safely against the same database (spec §17.0, §18.1).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if err := migrateControlPlane(ctx, pool); err != nil {
		return err
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("create River migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("migrate River schema: %w", err)
	}
	// v1.10 adds workspace identity to River payloads. Backfill jobs inserted by
	// v1.8 so an in-place upgrade does not strand queued dispatch or review
	// publication work with an empty context (spec §21.10).
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
		if _, err := tx.Exec(ctx,
			"INSERT INTO conveyor_schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
			version, filepath.Base(name), checksum,
		); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit control-plane migrations: %w", err)
	}
	return nil
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
	// carry no task and are not lifecycle edges (spec §21.37 change 6).
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
