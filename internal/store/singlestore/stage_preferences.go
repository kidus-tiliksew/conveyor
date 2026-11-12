package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// The stage preference statements live only in this file
// (component-stage-preferences invariant 5).
const (
	stagePreferenceSelect = `SELECT version, plan_json, implement_json, verify_json, review_json, updated_at FROM user_stage_preferences WHERE user_id=?`
	// stagePreferenceReserve creates the person's row at version 0 when it is
	// absent, so concurrent first writers queue on one row lock. The row and
	// its lock share one partition, so the next holder reads the previous
	// holder's committed write. A rolled back write removes the row again.
	stagePreferenceReserve = `INSERT INTO user_stage_preferences (user_id, version, updated_at) VALUES (?, 0, ?) ON DUPLICATE KEY UPDATE user_id=user_id`
	stagePreferenceUpdate  = `UPDATE user_stage_preferences SET version=?, plan_json=?, implement_json=?, verify_json=?, review_json=?, updated_at=? WHERE user_id=? AND version=?`
)

// GetStagePreferences implements store.StagePreferenceStore.
func (s *Store) GetStagePreferences(ctx context.Context, userID string) (store.StagePreferences, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetStagePreferences")
	return scanStagePreferences(backendRow(ctx, s.db, stagePreferenceSelect, userID))
}

// UpdateStagePreferences implements store.StagePreferenceStore. The row lock
// taken by SELECT ... FOR UPDATE serializes writers for one person, and the
// version predicate on the UPDATE proves no write slipped in between
// (component-stage-preferences invariant 4).
func (s *Store) UpdateStagePreferences(ctx context.Context, userID string, apply store.StagePreferenceUpdate) (store.StagePreferences, error) {
	var saved store.StagePreferences
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := store.StagePreferenceTime(time.Now())
		if _, err := tx.ExecContext(ctx, stagePreferenceReserve, userID, now); err != nil {
			return err
		}
		current, err := scanStagePreferences(tx.QueryRowContext(ctx, stagePreferenceSelect+` FOR UPDATE`, userID))
		if err != nil {
			return err
		}
		next, err := apply(current)
		if err != nil {
			return err
		}
		columns, err := store.EncodeStagePreferenceColumns(next)
		if err != nil {
			return err
		}
		version := current.Version + 1
		result, err := tx.ExecContext(ctx, stagePreferenceUpdate, version, nullableJSON(columns.Plan), nullableJSON(columns.Implement), nullableJSON(columns.Verify), nullableJSON(columns.Review), now, userID, current.Version)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return errors.New("stage preferences changed under the row lock")
		}
		stages, err := store.DecodeStagePreferenceColumns(columns)
		if err != nil {
			return err
		}
		saved = store.StagePreferences{Version: version, UpdatedAt: &now, Stages: stages}
		return nil
	})
	if err != nil {
		return store.StagePreferences{}, err
	}
	return saved, nil
}

// scanStagePreferences reads one row. No row, and the reserved version-0 row,
// read as the never-saved set (req-stage-preferences AC-2.4).
func scanStagePreferences(row interface{ Scan(...any) error }) (store.StagePreferences, error) {
	var (
		version   int64
		columns   store.StagePreferenceColumns
		updatedAt time.Time
	)
	err := row.Scan(&version, &columns.Plan, &columns.Implement, &columns.Verify, &columns.Review, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
