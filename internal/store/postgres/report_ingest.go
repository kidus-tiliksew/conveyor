package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var _ store.ReportStore = (*Store)(nil)

const feedbackReportColumns = `id, environment_id, problem_key, kind, summary, summary_redactions::text, service, occurrence_count, first_seen, last_seen, first_entry_seen, file, line,
first_repository, first_commit, first_version, latest_repository, latest_commit, latest_version, state, in_inbox, source, source_label, actor_id, created_at,
closed_at, closed_by, close_reason, reopened_at, reopen_source, reopen_source_label, reopen_actor_id, linking_ended_at, fix_task_id, linking_last_attempt_at`

func queryFeedbackReports(ctx context.Context, q environmentQuerier, sql string, args ...any) ([]store.FeedbackReport, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reports := []store.FeedbackReport{}
	for rows.Next() {
		var report store.FeedbackReport
		var redactions string
		var line *int32
		var closedAt, reopenedAt, linkingEndedAt, linkingAttemptAt pgtype.Timestamptz
		var closedBy, closeReason, reopenSource, reopenLabel, reopenActor, fixTask *string
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
		if line != nil {
			report.Line = int(*line)
		}
		if closedAt.Valid && closedBy != nil && closeReason != nil {
			at := store.EnvironmentTime(closedAt.Time)
			report.ClosedAt, report.ClosedBy, report.CloseReason = &at, *closedBy, *closeReason
		}
		if reopenedAt.Valid && reopenSource != nil && reopenLabel != nil && reopenActor != nil {
			at := store.EnvironmentTime(reopenedAt.Time)
			report.ReopenedAt, report.ReopenOrigin = &at, &store.FeedbackReportOrigin{Source: *reopenSource, Label: *reopenLabel, Actor: *reopenActor}
		}
		if linkingEndedAt.Valid {
			at := store.EnvironmentTime(linkingEndedAt.Time)
			report.LinkingEndedAt = &at
		}
		if linkingAttemptAt.Valid {
			at := store.EnvironmentTime(linkingAttemptAt.Time)
			report.LinkingLastAttemptAt = &at
		}
		if fixTask != nil {
			report.FixTaskID = *fixTask
		}
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

// CheckFeedbackReportRepositories implements store.ReportStore.
func (s *Store) CheckFeedbackReportRepositories(ctx context.Context, batch store.FeedbackReportBatch) error {
	ctx = dbmetrics.WithReadLabel(ctx, "CheckFeedbackReportRepositories")
	repositories, err := feedbackRepositories(ctx, s.boundary, workspace(ctx))
	if err != nil {
		return translateBackendConflict(err)
	}
	return store.CheckFeedbackReportRepositories(batch, repositories)
}

// RecordFeedbackReports implements store.ReportStore. It takes the
// environment row lock and the key row lock that a signal takes, so the
// report and signal writes of one environment, the first insert included,
// run one at a time, and the open count it reads holds until it commits
// (component-feedback-reports invariants 6 and 8).
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
	workspaceID := workspace(ctx)
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		label, err := lockEnvironmentKey(ctx, tx, workspaceID, prepared.EnvironmentID, prepared.KeyID)
		if err != nil {
			return err
		}
		repositories, err := feedbackRepositories(ctx, tx, workspaceID)
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
		matched, err := queryFeedbackReports(ctx, tx, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=$1 AND environment_id=$2 AND key_digest=ANY($3::text[])`,
			workspaceID, prepared.EnvironmentID, digests)
		if err != nil {
			return err
		}
		stored := make(map[string]store.FeedbackReport, len(matched))
		for _, report := range matched {
			stored[report.Key] = report
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM feedback_reports WHERE workspace_id=$1 AND environment_id=$2 AND state='open'`, workspaceID, prepared.EnvironmentID).Scan(&open); err != nil {
			return err
		}
		plan, err := store.PlanFeedbackReports(ctx, prepared, label, stored, open, feedbackReportIDTaken(ctx, tx, workspaceID))
		if err != nil {
			return err
		}
		for _, change := range plan.Changes {
			if err := writeFeedbackReport(ctx, tx, workspaceID, change); err != nil {
				return err
			}
		}
		for _, event := range plan.Events(prepared.ArrivedAt) {
			if err := insertWorkspaceEvent(ctx, q, event); err != nil {
				return err
			}
		}
		return touchEnvironmentKey(ctx, tx, workspaceID, prepared.KeyID)
	})
}

// feedbackReportIDTaken answers whether a stored report of the workspace
// holds an ID. Report IDs are unique in the workspace, and the environment row
// lock serializes one environment only, so the first check takes a
// transaction lock on the workspace's report IDs. It holds until the commit,
// so a report that another environment's request stores waits for it, and
// the answer stays true for this request (component-feedback-reports
// invariant 6). Every report writer takes the environment row lock first and
// this lock second, so no two writers wait on each other in a cycle.
func feedbackReportIDTaken(ctx context.Context, tx pgx.Tx, workspaceID string) store.FeedbackReportIDTaken {
	locked := false
	return func(id string) (bool, error) {
		if !locked {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "conveyor:feedback-report-ids:"+workspaceID); err != nil {
				return false, err
			}
			locked = true
		}
		var held bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM feedback_reports WHERE workspace_id=$1 AND id=$2)`, workspaceID, id).Scan(&held)
		return held, err
	}
}

// writeFeedbackReport inserts a new report or writes the members that a
// later entry changes: the count, the times, latest, and for a reopening
// the state, the Inbox flag, the closing, and the reopening.
func writeFeedbackReport(ctx context.Context, tx pgx.Tx, workspaceID string, change store.FeedbackReportChange) error {
	r := change.Report
	if change.New {
		var line *int
		if r.Line > 0 {
			line = &r.Line
		}
		_, err := tx.Exec(ctx, `INSERT INTO feedback_reports (workspace_id, id, environment_id, key_digest, problem_key, kind, summary, summary_redactions, service, occurrence_count,
first_seen, last_seen, first_entry_seen, file, line, first_repository, first_commit, first_version, latest_repository, latest_commit, latest_version,
state, in_inbox, source, source_label, actor_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::text::jsonb, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)`,
			workspaceID, r.ID, r.EnvironmentID, store.FeedbackReportKeyDigest(r.Key), r.Key, r.Kind, r.Summary, store.FeedbackReportRedactionsJSON(r.SummaryRedactions), r.Service, r.Count,
			r.FirstSeen, r.LastSeen, r.FirstEntrySeen, r.File, line, r.First.Repository, r.First.Commit, r.First.Version, r.Latest.Repository, r.Latest.Commit, r.Latest.Version,
			r.State, r.InInbox, r.Origin.Source, r.Origin.Label, r.Origin.Actor, r.CreatedAt)
		return err
	}
	if change.Reopened {
		_, err := tx.Exec(ctx, `UPDATE feedback_reports SET occurrence_count=$3, first_seen=$4, last_seen=$5, latest_repository=$6, latest_commit=$7, latest_version=$8,
state='open', in_inbox=true, closed_at=NULL, closed_by=NULL, close_reason=NULL, reopened_at=$9, reopen_source=$10, reopen_source_label=$11, reopen_actor_id=$12
WHERE workspace_id=$1 AND id=$2`,
			workspaceID, r.ID, r.Count, r.FirstSeen, r.LastSeen, r.Latest.Repository, r.Latest.Commit, r.Latest.Version,
			*r.ReopenedAt, r.ReopenOrigin.Source, r.ReopenOrigin.Label, r.ReopenOrigin.Actor)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE feedback_reports SET occurrence_count=$3, first_seen=$4, last_seen=$5, latest_repository=$6, latest_commit=$7, latest_version=$8 WHERE workspace_id=$1 AND id=$2`,
		workspaceID, r.ID, r.Count, r.FirstSeen, r.LastSeen, r.Latest.Repository, r.Latest.Commit, r.Latest.Version)
	return err
}

// ListEnvironmentFeedbackReports implements store.ReportStore.
func (s *Store) ListEnvironmentFeedbackReports(ctx context.Context, environmentID string) ([]store.FeedbackReport, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEnvironmentFeedbackReports")
	return queryFeedbackReports(ctx, s.boundary, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=$1 AND environment_id=$2 ORDER BY id`, workspace(ctx), environmentID)
}
