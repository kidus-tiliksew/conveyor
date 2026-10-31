package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const eventColumns = "id,task_id,job_id,kind,actor_id,actor_role,payload_json,at,workspace_id"
const migratedEventShardKey = "COALESCE(NULLIF(task_id,''),CONCAT('event/',CAST(id AS CHAR)))"

// migrateEventsColumnstore runs under the startup migration lock with all old
// writers stopped. Each INSERT SELECT commits one complete ID range. MAX(id) on
// the destination is therefore a durable copy cursor even after a lost commit
// acknowledgement. Never use INSERT IGNORE: it could hide a lost audit row.
func (s *Store) migrateEventsColumnstore(ctx context.Context) error {
	storage, err := s.eventTableStorage(ctx, "events")
	if err != nil {
		return err
	}
	switch storage {
	case "INMEMORY_ROWSTORE":
		backup, err := s.eventTableStorage(ctx, "events_rowstore_0027")
		if err != nil {
			return err
		}
		if backup != "" {
			return fmt.Errorf("events rowstore and migration backup both exist")
		}
		for batches := 0; ; batches++ {
			copied, err := s.copyEventsColumnstoreBatch(ctx)
			if err != nil {
				return err
			}
			if !copied {
				break
			}
			// Columnstore inserts first occupy an in-memory row buffer.
			// Flush periodically rather than hold a second full event log in RAM.
			if batches%32 == 31 {
				if _, err := s.db.ExecContext(ctx, `OPTIMIZE TABLE events_columnstore_0027 FLUSH`); err != nil {
					return err
				}
			}
		}
		var source, destination int64
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM events),(SELECT COUNT(*) FROM events_columnstore_0027)`).Scan(&source, &destination); err != nil {
			return err
		}
		if source != destination {
			return fmt.Errorf("events copy count mismatch: source=%d destination=%d", source, destination)
		}
		if _, err := s.db.ExecContext(ctx, `OPTIMIZE TABLE events_columnstore_0027 FLUSH`); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE events RENAME TO events_rowstore_0027`); err != nil {
			return err
		}
		fallthrough
	case "": // A previous start renamed the source but not the destination.
		backup, err := s.eventTableStorage(ctx, "events_rowstore_0027")
		if err != nil {
			return err
		}
		if backup != "INMEMORY_ROWSTORE" {
			return fmt.Errorf("events migration has no rowstore backup")
		}
		var source, destination int64
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM events_rowstore_0027),(SELECT COUNT(*) FROM events_columnstore_0027)`).Scan(&source, &destination); err != nil {
			return err
		}
		if source != destination {
			return fmt.Errorf("events promotion count mismatch: source=%d destination=%d", source, destination)
		}
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE events_columnstore_0027 RENAME TO events`); err != nil {
			return err
		}
	case "COLUMNSTORE": // The rename committed before ledger advancement.
	default:
		return fmt.Errorf("unexpected events storage type %q", storage)
	}
	// Explicit copied IDs do not advance SingleStore's per-aggregator counters.
	// Sync before writes resume so new cursor IDs cannot collide with history.
	var database string
	if err := s.db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&database); err != nil {
		return err
	}
	table := "`" + strings.ReplaceAll(database, "`", "``") + "`.`events`"
	if _, err := s.db.ExecContext(ctx, `AGGREGATOR SYNC AUTO_INCREMENT ON `+table+` ALL`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS events_rowstore_0027`); err != nil {
		return err
	}
	// The SQL file recreates an empty staging table on a retry after promotion.
	_, err = s.db.ExecContext(ctx, `DROP TABLE IF EXISTS events_columnstore_0027`)
	return err
}

func (s *Store) eventTableStorage(ctx context.Context, table string) (string, error) {
	var storage string
	err := s.db.QueryRowContext(ctx, `SELECT storage_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&storage)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return storage, err
}

func (s *Store) copyEventsColumnstoreBatch(ctx context.Context) (bool, error) {
	var cursor sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM events_columnstore_0027`).Scan(&cursor); err != nil {
		return false, err
	}
	filter := ""
	var args []any
	if cursor.Valid {
		filter = " WHERE id>?"
		args = append(args, cursor.Int64)
	}
	// Read only fixed-width metadata into Go. Payloads stay in the database,
	// with at most 256 rows / roughly 8 MiB per statement (one oversized row
	// is still copied). Equal IDs in different workspaces stay in one range.
	rows, err := s.db.QueryContext(ctx, `SELECT id,LENGTH(payload_json) FROM events`+filter+` ORDER BY id LIMIT 256`, args...)
	if err != nil {
		return false, err
	}
	var high int64
	var bytes int64
	found := false
	for rows.Next() {
		var size int64
		if err := rows.Scan(&high, &size); err != nil {
			rows.Close()
			return false, err
		}
		found = true
		bytes += size
		if bytes >= 8<<20 {
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !found {
		return false, err
	}
	if filter == "" {
		filter = " WHERE id<=?"
	} else {
		filter += " AND id<=?"
	}
	args = append(args, high)
	_, err = s.db.ExecContext(ctx, `INSERT INTO events_columnstore_0027 (`+eventColumns+`,event_shard_key) SELECT `+eventColumns+`,`+migratedEventShardKey+` FROM events`+filter, args...)
	return err == nil, err
}
