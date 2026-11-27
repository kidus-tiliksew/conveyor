package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var _ store.ReportStore = (*Store)(nil)

const feedbackReportColumns = `id,environment_id,problem_key,kind,summary,summary_redactions,service,occurrence_count,first_seen,last_seen,first_entry_seen,file,line,
first_repository,first_commit,first_version,latest_repository,latest_commit,latest_version,state,in_inbox,source,source_label,actor_id,created_at,
closed_at,closed_by,close_reason,reopened_at,reopen_source,reopen_source_label,reopen_actor_id,linking_ended_at,fix_task_id,linking_last_attempt_at`

func collectFeedbackReports(rows environmentRows) ([]store.FeedbackReport, error) {
	defer rows.Close()
	reports := []store.FeedbackReport{}
	for rows.Next() {
		var report store.FeedbackReport
		var redactions string
		var line sql.NullInt64
		var closedAt, reopenedAt, linkingEndedAt, linkingAttemptAt sql.NullTime
		var closedBy, closeReason, reopenSource, reopenLabel, reopenActor, fixTask sql.NullString
		if err := rows.Scan(&report.ID, &report.EnvironmentID, &report.Key, &report.Kind, &report.Summary, &redactions, &report.Service, &report.Count,
			&report.FirstSeen, &report.LastSeen, &report.FirstEntrySeen, &report.File, &line,
			&report.First.Repository, &report.First.Commit, &report.First.Version, &report.Latest.Repository, &report.Latest.Commit, &report.Latest.Version,
			&report.State, &report.InInbox, &report.Origin.Source, &report.Origin.Label, &report.Origin.Actor, &report.CreatedAt,
			&closedAt, &closedBy, &closeReason, &reopenedAt, &reopenSource, &reopenLabel, &reopenActor, &linkingEndedAt, &fixTask, &linkingAttemptAt); err != nil {
			return nil, err
		}
		if err := store.ApplyFeedbackReportRedactions(&report, []byte(redactions)); err != nil {
			return nil, err
		}
		report.FirstSeen, report.LastSeen = store.EnvironmentTime(report.FirstSeen), store.EnvironmentTime(report.LastSeen)
		report.FirstEntrySeen, report.CreatedAt = store.EnvironmentTime(report.FirstEntrySeen), store.EnvironmentTime(report.CreatedAt)
		if line.Valid {
			report.Line = int(line.Int64)
		}
		if closedAt.Valid && closedBy.Valid && closeReason.Valid {
			at := store.EnvironmentTime(closedAt.Time)
			report.ClosedAt, report.ClosedBy, report.CloseReason = &at, closedBy.String, closeReason.String
		}
		if reopenedAt.Valid && reopenSource.Valid && reopenLabel.Valid && reopenActor.Valid {
			at := store.EnvironmentTime(reopenedAt.Time)
			report.ReopenedAt, report.ReopenOrigin = &at, &store.FeedbackReportOrigin{Source: reopenSource.String, Label: reopenLabel.String, Actor: reopenActor.String}
		}
		if linkingEndedAt.Valid {
			at := store.EnvironmentTime(linkingEndedAt.Time)
			report.LinkingEndedAt = &at
		}
		if linkingAttemptAt.Valid {
			at := store.EnvironmentTime(linkingAttemptAt.Time)
			report.LinkingLastAttemptAt = &at
		}
		report.FixTaskID = fixTask.String
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

// CheckFeedbackReportRepositories implements store.ReportStore.
func (s *Store) CheckFeedbackReportRepositories(ctx context.Context, batch store.FeedbackReportBatch) error {
	ctx = dbmetrics.WithReadLabel(ctx, "CheckFeedbackReportRepositories")
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	var configYAML string
	err = backendRow(ctx, s.db, `SELECT config_yaml FROM workspaces WHERE id=?`, ws).Scan(&configYAML)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: workspace %s", store.ErrNotFound, ws)
	}
	if err != nil {
		return translateBackendConflict(err)
	}
	repositories, err := store.EnvironmentRepositories(configYAML)
	if err != nil {
		return err
	}
	return store.CheckFeedbackReportRepositories(batch, repositories)
}

// RecordFeedbackReports implements store.ReportStore. It holds the workspace
// environment claim that a signal, an archive, and a revocation take, waits
// until the last holder's writes are visible, and stamps the environment row
// as its own witness. The report table shares the shard key of that row, so
// a later holder sees every row this request wrote, and the open count it
// reads holds until it commits (component-feedback-reports invariants 6 and
// 8; component-persistence invariant 8).
func (s *Store) RecordFeedbackReports(ctx context.Context, batch store.FeedbackReportBatch) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackReports")
	// A request without an actor is refused before any write
	// (component-persistence, Actor context).
	if _, err := store.RequireActor(ctx); err != nil {
		return err
	}
	prepared, err := store.PrepareFeedbackReports(ctx, s, batch)
	if err != nil {
		return err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	// SingleStore has no CHECK constraint, so the store holds the reference.
	if prepared.EnvironmentID == "" || prepared.KeyID == "" {
		return store.ErrInvalidEnvironmentKey
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		claim, err := claimEnvironments(ctx, tx, ws)
		if err != nil {
			return err
		}
		label, err := environmentKeyLabel(ctx, tx, ws, prepared.EnvironmentID, prepared.KeyID)
		if err != nil {
			return err
		}
		repositories, err := environmentRepositoriesTx(ctx, tx, ws)
		if err != nil {
			return err
		}
		if err := store.CheckFeedbackReportRepositories(prepared.FeedbackReportBatch, repositories); err != nil {
			return err
		}
		keys := store.FeedbackReportKeys(prepared)
		digests := make([]string, len(keys))
		for i, key := range keys {
			digests[i] = store.FeedbackReportKeyDigest(key)
		}
		list, args := feedbackList(digests)
		rows, err := tx.QueryContext(ctx, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=? AND environment_id=? AND key_digest IN (`+list+`)`,
			append([]any{ws, prepared.EnvironmentID}, args...)...)
		if err != nil {
			return err
		}
		matched, err := collectFeedbackReports(rows)
		if err != nil {
			return err
		}
		stored := make(map[string]store.FeedbackReport, len(matched))
		for _, report := range matched {
			stored[report.Key] = report
		}
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_reports WHERE workspace_id=? AND environment_id=? AND state='open'`, ws, prepared.EnvironmentID).Scan(&open); err != nil {
			return err
		}
		// Report IDs are unique in the workspace. The workspace environment
		// claim serializes the report writes of every environment of the
		// workspace, so the answer holds until this request commits
		// (component-feedback-reports invariant 6).
		taken := func(id string) (bool, error) {
			var count int
			err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_reports WHERE workspace_id=? AND id=?`, ws, id).Scan(&count)
			return count > 0, err
		}
		plan, err := store.PlanFeedbackReports(ctx, prepared, label, stored, open, taken)
		if err != nil {
			return err
		}
		for _, change := range plan.Changes {
			if err := writeFeedbackReport(ctx, tx, ws, change); err != nil {
				return err
			}
		}
		for _, event := range plan.Events(prepared.ArrivedAt) {
			if err := insertWorkspaceEvent(ctx, tx, event); err != nil {
				return err
			}
		}
		if err := touchEnvironmentKey(ctx, tx, ws, prepared.KeyID); err != nil {
			return err
		}
		return claim.touchEnvironment(ctx, tx, prepared.EnvironmentID)
	})
}

// writeFeedbackReport inserts a new report or writes the members that a
// later entry changes: the count, the times, latest, and for a reopening
// the state, the Inbox flag, the closing, and the reopening.
func writeFeedbackReport(ctx context.Context, tx *sql.Tx, ws string, change store.FeedbackReportChange) error {
	r := change.Report
	if change.New {
		var line any
		if r.Line > 0 {
			line = r.Line
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO feedback_reports (workspace_id,id,environment_id,key_digest,problem_key,kind,summary,summary_redactions,service,occurrence_count,
first_seen,last_seen,first_entry_seen,file,line,first_repository,first_commit,first_version,latest_repository,latest_commit,latest_version,
state,in_inbox,source,source_label,actor_id,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ws, r.ID, r.EnvironmentID, store.FeedbackReportKeyDigest(r.Key), r.Key, r.Kind, r.Summary, store.FeedbackReportRedactionsJSON(r.SummaryRedactions), r.Service, r.Count,
			r.FirstSeen, r.LastSeen, r.FirstEntrySeen, r.File, line, r.First.Repository, r.First.Commit, r.First.Version, r.Latest.Repository, r.Latest.Commit, r.Latest.Version,
			r.State, r.InInbox, r.Origin.Source, r.Origin.Label, r.Origin.Actor, r.CreatedAt)
		return err
	}
	if change.Reopened {
		_, err := tx.ExecContext(ctx, `UPDATE feedback_reports SET occurrence_count=?,first_seen=?,last_seen=?,latest_repository=?,latest_commit=?,latest_version=?,
state='open',in_inbox=1,closed_at=NULL,closed_by=NULL,close_reason=NULL,reopened_at=?,reopen_source=?,reopen_source_label=?,reopen_actor_id=? WHERE workspace_id=? AND id=?`,
			r.Count, r.FirstSeen, r.LastSeen, r.Latest.Repository, r.Latest.Commit, r.Latest.Version,
			*r.ReopenedAt, r.ReopenOrigin.Source, r.ReopenOrigin.Label, r.ReopenOrigin.Actor, ws, r.ID)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE feedback_reports SET occurrence_count=?,first_seen=?,last_seen=?,latest_repository=?,latest_commit=?,latest_version=? WHERE workspace_id=? AND id=?`,
		r.Count, r.FirstSeen, r.LastSeen, r.Latest.Repository, r.Latest.Commit, r.Latest.Version, ws, r.ID)
	return err
}

// ListEnvironmentFeedbackReports implements store.ReportStore.
func (s *Store) ListEnvironmentFeedbackReports(ctx context.Context, environmentID string) ([]store.FeedbackReport, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEnvironmentFeedbackReports")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=? AND environment_id=? ORDER BY id`, ws, environmentID)
	if err != nil {
		return nil, err
	}
	return collectFeedbackReports(rows)
}
