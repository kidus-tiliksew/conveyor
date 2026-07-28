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
		rawSQL, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		checksum := migrationChecksum(rawSQL)
		sql, err := renderMigration(rawSQL)
		if err != nil {
			return fmt.Errorf("render migration %s: %w", name, err)
		}
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
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
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

func auditPersistedLifecycles(ctx context.Context, tx pgx.Tx) ([]controlstore.LifecycleAuditViolation, error) {
	rows, err := tx.Query(ctx, `SELECT id,task_id,job_id,kind,payload_json,at FROM events ORDER BY at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []core.Event
	for rows.Next() {
		var event core.Event
		var jobID *string
		if err = rows.Scan(&event.ID, &event.TaskID, &jobID, &event.Kind, &event.Payload, &event.At); err != nil {
			return nil, err
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
