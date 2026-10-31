package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const eventColumns = "id,task_id,job_id,kind,actor_id,actor_role,payload_json,at,workspace_id"

// checkEventsPrerequisite checks only the application schema. Copy validation,
// writer freeze and allocator preparation belong to the operator procedure;
// startup must not copy, rename, drop or synchronize any event table.
func (s *Store) checkEventsPrerequisite(ctx context.Context) error {
	const procedure = "complete the operator procedure in docs/sql/events-columnstore before starting Conveyor"
	storage, err := s.eventTableStorage(ctx, "events_columnstore")
	if err != nil {
		return fmt.Errorf("check events_columnstore prerequisite: %w", err)
	}
	if storage != "COLUMNSTORE" {
		return fmt.Errorf("events_columnstore must be an operator-prepared COLUMNSTORE (found %q); %s", storage, procedure)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+`,event_shard_key FROM events_columnstore LIMIT 0`)
	if err != nil {
		return fmt.Errorf("events_columnstore schema is not ready; %s: %w", procedure, err)
	}
	return rows.Close()
}

func (s *Store) eventTableStorage(ctx context.Context, table string) (string, error) {
	var storage string
	err := s.db.QueryRowContext(ctx, `SELECT storage_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&storage)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return storage, err
}
