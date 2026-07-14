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
	// v1.9 adds workspace identity to River payloads. Backfill jobs inserted by
	// v1.8 so an in-place upgrade does not strand queued dispatch or review
	// publication work with an empty context (spec §21.9).
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
		sql, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(sql)
		checksum := hex.EncodeToString(digest[:])
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
