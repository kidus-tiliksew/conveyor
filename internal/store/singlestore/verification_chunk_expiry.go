package singlestore

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// migrateVerificationChunkExpiryIndex runs before migration 0017. SingleStore
// DDL commits implicitly, so the hook creates the ordered expiry index only
// when information_schema lacks it and a retried start succeeds
// (component-verification-evidence; component-persistence).
func (s *Store) migrateVerificationChunkExpiryIndex(ctx context.Context) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='verification_upload_chunks' AND index_name='verification_upload_chunks_expiry'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `CREATE INDEX verification_upload_chunks_expiry ON verification_upload_chunks(workspace_id,expires_at,id)`)
	return err
}

// ExpireVerificationChunks deletes at most limit expired staging rows of the
// workspace for the internal reconciler (component-verification-evidence).
// The candidate read loads scalar metadata only. Each task group takes the
// task command lock, the workspace verification lock, and the task row, in the
// order every verification command uses, then rechecks, deletes, and appends
// one audit event in one transaction.
func (s *Store) ExpireVerificationChunks(ctx context.Context, limit int) (int, error) {
	ws, cutoff, err := store.AuthorizeVerificationChunkExpiry(ctx)
	if err != nil {
		return 0, err
	}
	var candidates []store.VerificationChunkCandidate
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,task_id,logical_key,expires_at FROM verification_upload_chunks WHERE workspace_id=? AND state='staging' AND expires_at IS NOT NULL AND expires_at<=? ORDER BY expires_at,id LIMIT ?`, ws, cutoff, store.VerificationChunkExpiryLimitOrDefault(limit))
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
		err := s.withTx(ctx, func(tx *sql.Tx) error {
			if err := s.lockTaskOperation(ctx, tx, ws, taskID); err != nil {
				return err
			}
			if err := lockKey(ctx, tx, "verification:"+ws); err != nil {
				return err
			}
			var found string
			if err := tx.QueryRowContext(ctx, "SELECT id FROM tasks WHERE workspace_id=? AND id=? FOR UPDATE", ws, taskID).Scan(&found); err != nil {
				return store.ErrVerificationAccess
			}
			marks := strings.TrimSuffix(strings.Repeat("?,", len(group)), ",")
			args := []any{ws, taskID}
			for _, c := range group {
				args = append(args, c.ID)
			}
			rows, err := tx.QueryContext(ctx, `SELECT id,task_id,logical_key,state,expires_at FROM verification_upload_chunks WHERE workspace_id=? AND task_id=? AND id IN (`+marks+`) FOR UPDATE`, args...)
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
			args = []any{ws, taskID}
			for _, c := range expired {
				args = append(args, c.ID)
			}
			result, err := tx.ExecContext(ctx, `DELETE FROM verification_upload_chunks WHERE workspace_id=? AND task_id=? AND id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(expired)), ",")+`)`, args...)
			if err != nil {
				return err
			}
			if n, e := result.RowsAffected(); e != nil || int(n) != len(expired) {
				return store.ErrVerificationState
			}
			if err = store.VerificationFault(ctx, "evidence"); err != nil {
				return err
			}
			if err = taskEvent(ctx, tx, store.VerificationChunkExpiryEvent(taskID, cutoff, expired)); err != nil {
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
