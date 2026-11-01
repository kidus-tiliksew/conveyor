package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Stored deadlines on SingleStore (component-persistence, Deadlines;
// req-quiet-idle-runtime AC-2.4).

var _ store.DeadlineReader = (*Store)(nil)

// nextDeadlineSQL reads, in one statement, the earliest work-order and
// verification-delivery deadline after the bound time, every failed
// publication's last automatic re-enqueue, and the workspace document that
// names the recovery window. Each branch mirrors store.WorkOrderDeadline.
const nextDeadlineSQL = `
SELECT 'deadline' AS kind, MIN(d) AS at, CAST(NULL AS CHAR) AS config_yaml FROM (
	SELECT lease_expires_at AS d FROM work_orders
		WHERE workspace_id=? AND state='claimed' AND lease_expires_at > ?
	UNION ALL SELECT queue_deadline AS d FROM work_orders
		WHERE workspace_id=? AND state='queued' AND execution_started_at IS NULL AND queue_blocked_at IS NULL AND queue_deadline > ?
	UNION ALL SELECT execution_deadline AS d FROM work_orders
		WHERE workspace_id=? AND state IN ('queued','claimed') AND execution_deadline > ?
	UNION ALL SELECT next_automatic_recovery_at AS d FROM work_orders
		WHERE workspace_id=? AND state IN ('queued','claimed','stale','timed_out') AND next_automatic_recovery_at > ?
	UNION ALL SELECT next_attempt_at AS d FROM verification_publication_deliveries
		WHERE workspace_id=? AND state='failed' AND next_attempt_at > ?
) deadlines
UNION ALL SELECT 'reenqueue' AS kind, automatic_reenqueue_at AS at, CAST(NULL AS CHAR) AS config_yaml FROM github_lifecycles
	WHERE workspace_id=? AND state='failed' AND automatic_reenqueue_at IS NOT NULL
UNION ALL SELECT 'reenqueue' AS kind, automatic_reenqueue_at AS at, CAST(NULL AS CHAR) AS config_yaml FROM review_publications
	WHERE workspace_id=? AND state='failed' AND automatic_reenqueue_at IS NOT NULL
UNION ALL SELECT 'config' AS kind, NULL AS at, config_yaml FROM workspaces WHERE id=?`

// NextDeadline returns the earliest stored deadline after after for the
// context's workspace.
func (s *Store) NextDeadline(ctx context.Context, after time.Time) (time.Time, bool, error) {
	ws := documentWorkspace(ctx)
	bound := after.UTC()
	rows, err := documentRows(ctx, s.db, nextDeadlineSQL, ws, bound, ws, bound, ws, bound, ws, bound, ws, bound, ws, ws, ws)
	if err != nil {
		return time.Time{}, false, err
	}
	defer rows.Close()
	var earliest time.Time
	var reenqueues []time.Time
	configYAML := ""
	for rows.Next() {
		var kind string
		var at sql.NullTime
		var raw sql.NullString
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

// raiseEventDeadline raises the deadline notice of an events row inserted
// through tx on that transaction's batch, so it is published only after
// commit (component-persistence, Deadlines).
func raiseEventDeadline(tx *sql.Tx, values map[string]any) {
	workspaceID, _ := values["workspace_id"].(string)
	kind, _ := values["kind"].(string)
	var payload json.RawMessage
	switch raw := values["payload_json"].(type) {
	case []byte:
		payload = raw
	case json.RawMessage:
		payload = raw
	case string:
		payload = json.RawMessage(raw)
	}
	at, _ := values["at"].(time.Time)
	if n, ok := store.EventDeadlineNotice(workspaceID, kind, payload, at); ok {
		raiseTx(tx, n)
	}
}
