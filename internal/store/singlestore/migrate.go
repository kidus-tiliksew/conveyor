package singlestore

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version             int
	name, checksum, sql string
}

func migrations() ([]migration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var result []migration
	seen := map[int]bool{}
	for _, entry := range entries {
		name := entry.Name()
		parts := strings.SplitN(name, "_", 2)
		version, err := strconv.Atoi(parts[0])
		if err != nil || version < 1 || len(parts) != 2 || seen[version] {
			return nil, fmt.Errorf("invalid or duplicate migration %s", name)
		}
		seen[version] = true
		data, err := migrationFiles.ReadFile(path.Join("migrations", name))
		if err != nil {
			return nil, err
		}
		result = append(result, migration{version, name, fmt.Sprintf("%x", sha256.Sum256(data)), string(data)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].version < result[j].version })
	return result, nil
}
func checkLedger(record migration, embedded map[int]migration, latest int) error {
	if record.version > latest {
		return fmt.Errorf("SingleStore migration %d is newer than binary; install a release at least as new as the database", record.version)
	}
	expected, ok := embedded[record.version]
	if !ok {
		return fmt.Errorf("unknown SingleStore migration %d", record.version)
	}
	if record.name != expected.name {
		return fmt.Errorf("SingleStore migration %d name mismatch", record.version)
	}
	if record.checksum != expected.checksum {
		return fmt.Errorf("SingleStore migration %d checksum mismatch", record.version)
	}
	return nil
}

// migrate holds its startup row lock on a dedicated connection: SingleStore
// DDL on the separate executor commits implicitly and cannot share that tx.
// Files must be restart-safe. The ledger advances only after every statement
// succeeds. A partial DDL failure is retried on the next start.
func (s *Store) migrate(ctx context.Context) error {
	files, err := migrations()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no SingleStore migrations embedded")
	}
	if _, err = s.db.ExecContext(ctx, lockSchema); err != nil {
		return err
	}
	release, err := s.sessionLock(ctx, "conveyor:startup-migrations")
	if err != nil {
		return err
	}
	defer release()
	if _, err = s.db.ExecContext(ctx, `CREATE ROWSTORE TABLE IF NOT EXISTS conveyor_singlestore_migrations (version INT NOT NULL,name VARCHAR(255) NOT NULL,checksum CHAR(64) NOT NULL,applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),PRIMARY KEY(version),SHARD KEY(version))`); err != nil {
		return err
	}
	embedded := map[int]migration{}
	for _, file := range files {
		embedded[file.version] = file
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version,name,checksum FROM conveyor_singlestore_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	applied := map[int]bool{}
	for rows.Next() {
		var record migration
		if err = rows.Scan(&record.version, &record.name, &record.checksum); err != nil {
			rows.Close()
			return err
		}
		if err = checkLedger(record, embedded, files[len(files)-1].version); err != nil {
			rows.Close()
			return err
		}
		applied[record.version] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if err = s2log.EnsureSchema(ctx, s.db); err != nil {
		return err
	}
	for _, file := range files {
		if applied[file.version] {
			continue
		}
		if file.version == 2 {
			if err := s.migrateRepositoryInstallColumn(ctx); err != nil {
				return fmt.Errorf("SingleStore migration %s: %w", file.name, err)
			}
		}
		if file.version == 6 {
			if err := s.migrateTaskStartOver(ctx); err != nil {
				return fmt.Errorf("SingleStore migration %s: %w", file.name, err)
			}
		}
		if file.version == 8 {
			if err := s.migrateOpenTaskBranchUnique(ctx); err != nil {
				return fmt.Errorf("SingleStore migration %s: %w", file.name, err)
			}
		}
		if file.version == 16 {
			if err := s.migrateDocumentDismissalArchival(ctx); err != nil {
				return fmt.Errorf("SingleStore migration %s: %w", file.name, err)
			}
		}
		if file.version == 4 {
			if err := s.migrateDocumentDismissalNotes(ctx); err != nil {
				return fmt.Errorf("SingleStore migration %s: %w", file.name, err)
			}
		}
		if file.version == 17 {
			if err := s.migrateVerificationChunkExpiryIndex(ctx); err != nil {
				return fmt.Errorf("SingleStore migration %s: %w", file.name, err)
			}
		}
		if file.version == 18 {
			if err := s.migrateFeatureRetirement(ctx); err != nil {
				return fmt.Errorf("SingleStore migration %s: %w", file.name, err)
			}
		}
		for _, statement := range strings.Split(file.sql, ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err = s.db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("SingleStore migration %s: %w", file.name, err)
			}
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO conveyor_singlestore_migrations(version,name,checksum) VALUES (?,?,?)`, file.version, file.name, file.checksum); err != nil {
			return err
		}
	}
	return nil
}

// The startup migration lock serializes this check with other migrators.
// A retry after implicit DDL commit must preserve any explicit on values.
func (s *Store) migrateRepositoryInstallColumn(ctx context.Context) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='repos' AND column_name='install_conveyor'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `ALTER TABLE repos ADD COLUMN install_conveyor BOOLEAN NOT NULL DEFAULT false`)
	return err
}

// DDL commits implicitly. Check each column under the startup lock so a retry
// after adding only one column preserves both historical rows and later notes.
func (s *Store) migrateDocumentDismissalNotes(ctx context.Context) error {
	for _, table := range []string{"requirement_versions", "system_design_versions"} {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name='dismissal_note'`, table).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN dismissal_note TEXT"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) migrateTaskStartOver(ctx context.Context) error {
	for _, column := range []struct{ name, definition string }{
		{"supersedes", "VARCHAR(255) NULL"}, {"superseded_by", "VARCHAR(255) NULL"}, {"intake_operator_direction", "LONGTEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='tasks' AND column_name=?`, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := s.db.ExecContext(ctx, "ALTER TABLE tasks ADD COLUMN "+column.name+" "+column.definition); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) migrateOpenTaskBranchUnique(ctx context.Context) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='tasks' AND index_name='tasks_branch_key'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `ALTER TABLE tasks DROP INDEX tasks_branch_key`)
	return err
}

// migrateFeatureRetirement retires the features entity before file 0018 drops
// its table (task 261007-9d50e0; component-persistence, component-artifacts).
// DDL and autocommitted DML cannot roll back together, so every step is
// guarded by information_schema and restart-safe under the startup lock: a
// retry after any committed step converges without a duplicate link. A
// residual feature-owned attachment link becomes one workspace-unattached link
// per workspace, artifact, and role, matching PostgreSQL migration 142, and
// artifact rows, bytes, and every other link stay untouched.
func (s *Store) migrateFeatureRetirement(ctx context.Context) error {
	for _, step := range s.featureRetirementSteps() {
		if err := step(ctx); err != nil {
			return err
		}
	}
	return nil
}

// featureRetirementSteps lists the committed steps in order. Each one checks
// information_schema first, so a start that stopped after any of them
// resumes with the next.
func (s *Store) featureRetirementSteps() []func(context.Context) error {
	whileColumn := func(table string, step func(context.Context) error) func(context.Context) error {
		return func(ctx context.Context) error {
			exists, err := s.columnExists(ctx, table, "feature_id")
			if err != nil || !exists {
				return err
			}
			return step(ctx)
		}
	}
	return []func(context.Context) error{
		whileColumn("artifact_links", s.insertDetachedFeatureLinks),
		whileColumn("artifact_links", func(ctx context.Context) error {
			_, err := s.db.ExecContext(ctx, `DELETE FROM artifact_links WHERE feature_id IS NOT NULL`)
			return err
		}),
		whileColumn("artifact_links", func(ctx context.Context) error {
			_, err := s.db.ExecContext(ctx, `ALTER TABLE artifact_links DROP COLUMN feature_id`)
			return err
		}),
		whileColumn("tasks", func(ctx context.Context) error {
			_, err := s.db.ExecContext(ctx, `ALTER TABLE tasks DROP COLUMN feature_id`)
			return err
		}),
	}
}

func (s *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, table, column).Scan(&count); err != nil {
		return false, err
	}
	return count != 0, nil
}

// insertDetachedFeatureLinks inserts one unattached link per distinct
// workspace, artifact, and role that a feature owns and that lacks one. A
// retry finds each unattached link already present and inserts nothing twice.
func (s *Store) insertDetachedFeatureLinks(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT workspace_id,artifact_id,role FROM artifact_links WHERE feature_id IS NOT NULL`)
	if err != nil {
		return err
	}
	type residualLink struct{ workspace, artifact, role string }
	var residual []residualLink
	for rows.Next() {
		var link residualLink
		if err = rows.Scan(&link.workspace, &link.artifact, &link.role); err != nil {
			rows.Close()
			return err
		}
		residual = append(residual, link)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, link := range residual {
		var unattached int
		if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_links WHERE workspace_id=? AND artifact_id=? AND role=? AND task_id IS NULL AND feature_id IS NULL AND requirement_id IS NULL AND planning_session_id IS NULL`, link.workspace, link.artifact, link.role).Scan(&unattached); err != nil {
			return err
		}
		if unattached != 0 {
			continue
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO artifact_links(workspace_id,artifact_id,role) VALUES(?,?,?)`, link.workspace, link.artifact, link.role); err != nil {
			return err
		}
	}
	return nil
}
