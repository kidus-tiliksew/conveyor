package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ExpireVerificationChunks deletes at most limit expired staging rows of the
// workspace for the internal reconciler (component-verification-evidence).
// The candidate read loads scalar metadata only and uses migration 132's
// verification_upload_chunks_expiry index. Each task group then takes the
// locks every verification command takes, in the same order, rechecks the
// selected rows, deletes them, and appends one audit event in one transaction.
func (s *Store) ExpireVerificationChunks(ctx context.Context, limit int) (int, error) {
	ws, cutoff, err := store.AuthorizeVerificationChunkExpiry(ctx)
	if err != nil {
		return 0, err
	}
	var candidates []store.VerificationChunkCandidate
	err = s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		rows, err := tx.Query(ctx, `SELECT id,task_id,logical_key,expires_at FROM verification_upload_chunks WHERE workspace_id=$1 AND state='staging' AND expires_at IS NOT NULL AND expires_at<=$2 ORDER BY expires_at,id LIMIT $3`, ws, cutoff, store.VerificationChunkExpiryLimitOrDefault(limit))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c store.VerificationChunkCandidate
			if err = rows.Scan(&c.ID, &c.TaskID, &c.UploadID, &c.ExpiresAt); err != nil {
				return err
			}
			candidates = append(candidates, c)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	return store.ExecuteVerificationChunkExpiry(ctx, s, store.GroupVerificationChunkCandidates(candidates, limit), func(_ taskops.TaskLease, group []store.VerificationChunkCandidate) (int, error) {
		taskID := group[0].TaskID
		deleted := 0
		err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			if err := lockWorkOrderTaskTx(ctx, tx, ws, taskID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:verification:"+ws); err != nil {
				return err
			}
			var found string
			if err := tx.QueryRow(ctx, "SELECT id FROM tasks WHERE workspace_id=$1 AND id=$2 FOR UPDATE", ws, taskID).Scan(&found); err != nil {
				return store.ErrVerificationAccess
			}
			ids := make([]string, 0, len(group))
			for _, c := range group {
				ids = append(ids, c.ID)
			}
			rows, err := tx.Query(ctx, `SELECT id,task_id,logical_key,state,expires_at FROM verification_upload_chunks WHERE workspace_id=$1 AND task_id=$2 AND id=ANY($3) FOR UPDATE`, ws, taskID, ids)
			if err != nil {
				return err
			}
			current := map[string]store.VerificationRow{}
			for rows.Next() {
				row := store.VerificationRow{Table: "verification_upload_chunks"}
				var expires *time.Time
				if err = rows.Scan(&row.ID, &row.TaskID, &row.LogicalKey, &row.State, &expires); err != nil {
					rows.Close()
					return err
				}
				row.ExpiresAt = expires
				current[row.ID] = row
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				return err
			}
			expired := store.VerificationChunkExpiryRecheck(taskID, cutoff, group, current)
			if len(expired) == 0 {
				return nil
			}
			ids = ids[:0]
			for _, c := range expired {
				ids = append(ids, c.ID)
			}
			tag, err := tx.Exec(ctx, `DELETE FROM verification_upload_chunks WHERE workspace_id=$1 AND task_id=$2 AND id=ANY($3)`, ws, taskID, ids)
			if err != nil {
				return err
			}
			if int(tag.RowsAffected()) != len(expired) {
				return store.ErrVerificationState
			}
			if err = store.VerificationFault(ctx, "evidence"); err != nil {
				return err
			}
			if err = insertEvent(ctx, q, store.VerificationChunkExpiryEvent(taskID, cutoff, expired)); err != nil {
				return err
			}
			if err = store.VerificationFault(ctx, "event"); err != nil {
				return err
			}
			deleted = len(expired)
			return nil
		})
		if err != nil {
			return 0, err
		}
		return deleted, nil
	})
}
