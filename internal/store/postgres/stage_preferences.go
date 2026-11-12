package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// The stage preference statements live only in this file
// (component-stage-preferences invariant 5).
const (
	stagePreferenceSelect = `SELECT version, plan_json, implement_json, verify_json, review_json, updated_at FROM user_stage_preferences WHERE user_id=$1`
	// stagePreferenceReserve creates the person's row at version 0 when it is
	// absent, so concurrent first writers queue on one row lock. A rolled
	// back write removes the reserved row again.
	stagePreferenceReserve = `INSERT INTO user_stage_preferences (user_id, version, updated_at) VALUES ($1, 0, $2) ON CONFLICT (user_id) DO NOTHING`
	stagePreferenceUpdate  = `UPDATE user_stage_preferences SET version=$2, plan_json=$3, implement_json=$4, verify_json=$5, review_json=$6, updated_at=$7 WHERE user_id=$1 AND version=$8`
)

// GetStagePreferences implements store.StagePreferenceStore.
func (s *Store) GetStagePreferences(ctx context.Context, userID string) (store.StagePreferences, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetStagePreferences")
	return scanStagePreferences(s.boundary.QueryRow(ctx, stagePreferenceSelect, userID))
}

// UpdateStagePreferences implements store.StagePreferenceStore. The row lock
// taken by SELECT ... FOR UPDATE serializes writers for one person, and the
// version predicate on the UPDATE proves no write slipped in between
// (component-stage-preferences invariant 4).
func (s *Store) UpdateStagePreferences(ctx context.Context, userID string, apply store.StagePreferenceUpdate) (store.StagePreferences, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "UpdateStagePreferences")
	tx, err := s.begin(ctx)
	if err != nil {
		return store.StagePreferences{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	now := store.StagePreferenceTime(time.Now())
	if _, err := tx.Exec(ctx, stagePreferenceReserve, userID, now); err != nil {
		return store.StagePreferences{}, translateBackendConflict(err)
	}
	current, err := scanStagePreferences(tx.QueryRow(ctx, stagePreferenceSelect+` FOR UPDATE`, userID))
	if err != nil {
		return store.StagePreferences{}, err
	}
	next, err := apply(current)
	if err != nil {
		return store.StagePreferences{}, err
	}
	columns, err := store.EncodeStagePreferenceColumns(next)
	if err != nil {
		return store.StagePreferences{}, err
	}
	version := current.Version + 1
	tag, err := tx.Exec(ctx, stagePreferenceUpdate, userID, version, nullableJSON(columns.Plan), nullableJSON(columns.Implement), nullableJSON(columns.Verify), nullableJSON(columns.Review), now, current.Version)
	if err != nil {
		return store.StagePreferences{}, translateBackendConflict(err)
	}
	if tag.RowsAffected() != 1 {
		return store.StagePreferences{}, errors.New("stage preferences changed under the row lock")
	}
	if err := tx.Commit(ctx); err != nil {
		return store.StagePreferences{}, translateBackendConflict(err)
	}
	stages, err := store.DecodeStagePreferenceColumns(columns)
	if err != nil {
		return store.StagePreferences{}, err
	}
	return store.StagePreferences{Version: version, UpdatedAt: &now, Stages: stages}, nil
}

// scanStagePreferences reads one row. No row, and the reserved version-0 row,
// read as the never-saved set (req-stage-preferences AC-2.4).
func scanStagePreferences(row pgx.Row) (store.StagePreferences, error) {
	var (
		version   int64
		columns   store.StagePreferenceColumns
		updatedAt time.Time
	)
	err := row.Scan(&version, &columns.Plan, &columns.Implement, &columns.Verify, &columns.Review, &updatedAt)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		return store.StagePreferences{}, nil
	}
	if err != nil {
		return store.StagePreferences{}, translateBackendConflict(err)
	}
	if version == 0 {
		return store.StagePreferences{}, nil
	}
	stages, err := store.DecodeStagePreferenceColumns(columns)
	if err != nil {
		return store.StagePreferences{}, err
	}
	at := store.StagePreferenceTime(updatedAt)
	return store.StagePreferences{Version: version, UpdatedAt: &at, Stages: stages}, nil
}

// nullableJSON sends an unset stage as SQL NULL rather than an empty value.
func nullableJSON(raw []byte) any {
	if raw == nil {
		return nil
	}
	return json.RawMessage(raw)
}
