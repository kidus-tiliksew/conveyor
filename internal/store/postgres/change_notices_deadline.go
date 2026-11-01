package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Stored deadlines on PostgreSQL (component-persistence, Deadlines;
// req-quiet-idle-runtime AC-2.4).

var _ store.DeadlineReader = (*Store)(nil)

// nextDeadlineSQL reads, in one statement, the earliest work-order,
// verification-delivery, and staging upload chunk expiry deadline after $2
// (the chunk branch uses verification_upload_chunks_expiry), every failed publication's last
// automatic re-enqueue, and the workspace document that names the recovery
// window those re-enqueues wait for. Each branch mirrors
// store.WorkOrderDeadline and ListElapsedWorkOrderTaskIDs.
const nextDeadlineSQL = `
SELECT 'deadline', min(d), NULL::text FROM (
	SELECT lease_expires_at AS d FROM work_orders
		WHERE workspace_id=$1 AND state='claimed' AND lease_expires_at > $2
	UNION ALL SELECT queue_deadline FROM work_orders
		WHERE workspace_id=$1 AND state='queued' AND execution_started_at IS NULL AND queue_blocked_at IS NULL AND queue_deadline > $2
	UNION ALL SELECT execution_deadline FROM work_orders
		WHERE workspace_id=$1 AND state IN ('queued','claimed') AND execution_deadline > $2
	UNION ALL SELECT next_automatic_recovery_at FROM work_orders
		WHERE workspace_id=$1 AND state IN ('queued','claimed','stale','timed_out') AND next_automatic_recovery_at > $2
	UNION ALL SELECT next_attempt_at FROM verification_publication_deliveries
		WHERE workspace_id=$1 AND state='failed' AND next_attempt_at > $2
	UNION ALL SELECT min(expires_at) FROM verification_upload_chunks
		WHERE workspace_id=$1 AND state='staging' AND expires_at > $2
) deadlines
UNION ALL SELECT 'reenqueue', automatic_reenqueue_at, NULL::text FROM github_lifecycles
	WHERE workspace_id=$1 AND state='failed' AND automatic_reenqueue_at IS NOT NULL
UNION ALL SELECT 'reenqueue', automatic_reenqueue_at, NULL::text FROM review_publications
	WHERE workspace_id=$1 AND state='failed' AND automatic_reenqueue_at IS NOT NULL
UNION ALL SELECT 'config', NULL::timestamptz, config_yaml FROM workspaces WHERE id=$1`

// NextDeadline returns the earliest stored deadline after after for the
// context's workspace.
func (s *Store) NextDeadline(ctx context.Context, after time.Time) (time.Time, bool, error) {
	rows, err := s.boundary.Query(ctx, nextDeadlineSQL, workspace(ctx), after.UTC())
	if err != nil {
		return time.Time{}, false, err
	}
	defer rows.Close()
	var earliest time.Time
	var reenqueues []time.Time
	configYAML := ""
	for rows.Next() {
		var kind string
		var at pgtype.Timestamptz
		var raw pgtype.Text
		if err = rows.Scan(&kind, &at, &raw); err != nil {
			return time.Time{}, false, err
		}
		switch kind {
		case "deadline":
			if at.Valid {
				earliest = at.Time.UTC()
			}
		case "reenqueue":
			if at.Valid {
				reenqueues = append(reenqueues, at.Time.UTC())
			}
		case "config":
			configYAML = raw.String
		}
	}
	if err = rows.Err(); err != nil {
		return time.Time{}, false, err
	}
	if len(reenqueues) > 0 {
		policy := config.DefaultRecoveryPolicy()
		if configYAML != "" {
			if policy, _, err = config.ReadClockPolicy([]byte(configYAML)); err != nil {
				return time.Time{}, false, err
			}
		}
		for _, stamp := range reenqueues {
			due, ok := store.PublicationReenqueueDeadline(true, stamp, policy.AutomaticRecoveries, policy.Window)
			if ok && due.After(after) && (earliest.IsZero() || due.Before(earliest)) {
				earliest = due
			}
		}
	}
	return earliest, !earliest.IsZero(), nil
}

// raiseEventDeadline raises the deadline notice of an event inserted
// through exec on that transaction's batch, so it is published only after
// commit (component-persistence, Deadlines).
func raiseEventDeadline(exec any, workspaceID, kind string, payload []byte, at time.Time) {
	if n, ok := store.EventDeadlineNotice(workspaceID, kind, payload, at); ok {
		raiseTx(exec, n)
	}
}
