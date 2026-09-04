package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) UpsertWorkOrderActivitySnapshot(ctx context.Context, workOrderID string, claim core.WorkOrderClaimIdentity, content string) error {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO work_order_activity_snapshots (workspace_id, work_order_id, attempt_id, content, captured_at)
		SELECT w.workspace_id, w.id, w.attempt_id, $1, $2
		FROM work_orders w
		WHERE w.workspace_id=$3 AND w.id=$4 AND w.state='claimed'
		  AND w.worker_id=$5 AND w.claimant_id=$6 AND w.session_id=$7
		  AND w.attempt_id<>'' AND w.lease_expires_at>$2
		  AND (w.execution_deadline IS NULL OR w.execution_deadline>$2)
		FOR UPDATE OF w
		ON CONFLICT (workspace_id, work_order_id) DO UPDATE
		SET attempt_id=EXCLUDED.attempt_id, content=EXCLUDED.content, captured_at=EXCLUDED.captured_at`,
		content, now, workspace(ctx), workOrderID, claim.WorkerID, claim.ClaimantID, claim.SessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrWorkOrderClaimLost
	}
	return nil
}

func (s *Store) FinalizeWorkOrderAttemptObservability(ctx context.Context, workOrderID, workerID string, checkpoint core.WorkOrderAttemptCheckpoint) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var authorized bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1
		FROM work_orders w
		WHERE w.workspace_id=$1 AND w.id=$2
		  AND EXISTS (
			SELECT 1 FROM events claimed
			WHERE claimed.workspace_id=w.workspace_id AND claimed.task_id=w.task_id
			  AND claimed.kind='work_order.claimed' AND claimed.payload_json->>'id'=w.id
			  AND COALESCE(claimed.payload_json->>'worker_id','')=$3
			  AND claimed.payload_json->>'session_id'=$4
			  AND claimed.payload_json->>'attempt_id'=$5
		  )
		  AND EXISTS (
			SELECT 1 FROM events checkpointed
			WHERE checkpointed.workspace_id=w.workspace_id AND checkpointed.task_id=w.task_id
			  AND checkpointed.kind='work_order.attempt_checkpointed'
			  AND checkpointed.payload_json->>'work_order_id'=w.id
			  AND checkpointed.payload_json->>'attempt_id'=$5
			  AND checkpointed.payload_json->>'commit_sha'=$6
		  )
	)`, workspace(ctx), workOrderID, workerID, checkpoint.SessionID, checkpoint.AttemptID, checkpoint.CommitSHA).Scan(&authorized)
	if err != nil {
		return err
	}
	if !authorized {
		return store.ErrWorkOrderClaimLost
	}
	if _, err = tx.Exec(ctx, `DELETE FROM work_order_activity_snapshots
		WHERE workspace_id=$1 AND work_order_id=$2 AND attempt_id=$3`, workspace(ctx), workOrderID, checkpoint.AttemptID); err != nil {
		return err
	}
	if checkpoint.Transcript != nil {
		if _, err = tx.Exec(ctx, `INSERT INTO work_order_transcript_captures
			(workspace_id, work_order_id, attempt_id, content, termination_reason, truncated, captured_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (workspace_id, work_order_id, attempt_id) DO NOTHING`,
			workspace(ctx), workOrderID, checkpoint.AttemptID, checkpoint.Transcript.Content,
			checkpoint.TerminationReason, checkpoint.Transcript.Truncated, time.Now().UTC()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) GetWorkOrderActivitySnapshot(ctx context.Context, workOrderID string) (core.WorkOrderActivitySnapshot, bool, error) {
	var snapshot core.WorkOrderActivitySnapshot
	err := s.pool.QueryRow(ctx, `SELECT attempt_id, content, captured_at
		FROM work_order_activity_snapshots
		WHERE workspace_id=$1 AND work_order_id=$2`, workspace(ctx), workOrderID).
		Scan(&snapshot.AttemptID, &snapshot.Content, &snapshot.CapturedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.WorkOrderActivitySnapshot{}, false, nil
		}
		return core.WorkOrderActivitySnapshot{}, false, fmt.Errorf("get work-order activity snapshot: %w", err)
	}
	return snapshot, true, nil
}

func (s *Store) ListWorkOrderTranscriptCaptures(ctx context.Context, workOrderID string) ([]core.WorkOrderTranscriptCapture, error) {
	rows, err := s.pool.Query(ctx, `SELECT attempt_id, content, termination_reason, truncated, captured_at
		FROM work_order_transcript_captures
		WHERE workspace_id=$1 AND work_order_id=$2
		ORDER BY captured_at, attempt_id`, workspace(ctx), workOrderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	captures := make([]core.WorkOrderTranscriptCapture, 0)
	for rows.Next() {
		var capture core.WorkOrderTranscriptCapture
		if err = rows.Scan(&capture.AttemptID, &capture.Content, &capture.TerminationReason, &capture.Truncated, &capture.CapturedAt); err != nil {
			return nil, err
		}
		captures = append(captures, capture)
	}
	return captures, rows.Err()
}
