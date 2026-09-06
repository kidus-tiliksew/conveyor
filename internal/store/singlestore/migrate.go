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
