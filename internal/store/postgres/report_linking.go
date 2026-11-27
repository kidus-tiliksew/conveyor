package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var _ store.ReportLinkingStore = (*Store)(nil)

// lockFeedbackReport reads one report under its row lock, the report write
// lock of the linking work. A report request updates the same row, so the
// two wait for each other, and neither takes the other's environment lock
// while it holds the row (component-feedback-reports invariants 6 and 15).
func lockFeedbackReport(ctx context.Context, tx pgx.Tx, workspaceID, reportID string) (store.FeedbackReport, error) {
	reports, err := queryFeedbackReports(ctx, tx, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, reportID)
	if err != nil {
		return store.FeedbackReport{}, err
	}
	if len(reports) != 1 {
		return store.FeedbackReport{}, fmt.Errorf("%w: feedback report %s", store.ErrNotFound, reportID)
	}
	return reports[0], nil
}

// ClaimFeedbackReportLinking implements store.ReportLinkingStore.
func (s *Store) ClaimFeedbackReportLinking(ctx context.Context, reportID string, now time.Time) (store.FeedbackReport, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ClaimFeedbackReportLinking")
	if _, err := store.RequireActor(ctx); err != nil {
		return store.FeedbackReport{}, false, err
	}
	workspaceID := workspace(ctx)
	var report store.FeedbackReport
	claimed := false
	err := s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		var err error
		claimed = false
		report, err = lockFeedbackReport(ctx, tx, workspaceID, reportID)
		if err != nil {
			return err
		}
		if !store.FeedbackReportLinkingClaimable(report, now) {
			return nil
		}
		at := store.EnvironmentTime(now)
		tag, err := tx.Exec(ctx, `UPDATE feedback_reports SET linking_last_attempt_at=$3 WHERE workspace_id=$1 AND id=$2`, workspaceID, reportID, at)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("claim linking of feedback report %s: %d rows changed", reportID, tag.RowsAffected())
		}
		report.LinkingLastAttemptAt, claimed = &at, true
		return nil
	})
	if err != nil {
		return store.FeedbackReport{}, false, err
	}
	return report, claimed, nil
}

// ReadFeedbackReportLineRead implements store.ReportLinkingStore.
func (s *Store) ReadFeedbackReportLineRead(ctx context.Context, reportID string) (store.FeedbackReportLineRead, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadFeedbackReportLineRead")
	read := store.FeedbackReportLineRead{}
	var answer string
	err := s.boundary.QueryRow(ctx, `SELECT report_id, repository, resolved_commit, file, line, answer::text, answered_at FROM feedback_report_line_reads WHERE workspace_id=$1 AND report_id=$2`,
		workspace(ctx), reportID).Scan(&read.ReportID, &read.Repository, &read.ResolvedCommit, &read.File, &read.Line, &answer, &read.AnsweredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.FeedbackReportLineRead{}, false, nil
	}
	if err != nil {
		return store.FeedbackReportLineRead{}, false, err
	}
	if read.Answer, err = store.ParseFeedbackReportLineAnswer([]byte(answer)); err != nil {
		return store.FeedbackReportLineRead{}, false, err
	}
	read.AnsweredAt = store.EnvironmentTime(read.AnsweredAt)
	return read, true, nil
}

// RecordFeedbackReportLineRead implements store.ReportLinkingStore. A stored
// answer is kept.
func (s *Store) RecordFeedbackReportLineRead(ctx context.Context, read store.FeedbackReportLineRead) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackReportLineRead")
	if _, err := store.RequireActor(ctx); err != nil {
		return err
	}
	read, err := store.NormalizeFeedbackReportLineRead(read)
	if err != nil {
		return err
	}
	workspaceID := workspace(ctx)
	return s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM feedback_reports WHERE workspace_id=$1 AND id=$2)`, workspaceID, read.ReportID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: feedback report %s", store.ErrNotFound, read.ReportID)
		}
		_, err := tx.Exec(ctx, `INSERT INTO feedback_report_line_reads (workspace_id, report_id, repository, resolved_commit, file, line, answer, answered_at)
VALUES ($1, $2, $3, $4, $5, $6, $7::text::jsonb, $8) ON CONFLICT (workspace_id, report_id) DO NOTHING`,
			workspaceID, read.ReportID, read.Repository, read.ResolvedCommit, read.File, read.Line, store.FeedbackReportLineAnswerJSON(read.Answer), read.AnsweredAt)
		return err
	})
}

// FinishFeedbackReportLinking implements store.ReportLinkingStore.
func (s *Store) FinishFeedbackReportLinking(ctx context.Context, linking store.FeedbackReportLinking) (bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "FinishFeedbackReportLinking")
	if _, err := store.RequireActor(ctx); err != nil {
		return false, err
	}
	if linking.EndedAt.IsZero() {
		return false, fmt.Errorf("the end of linking needs its time")
	}
	workspaceID := workspace(ctx)
	finished := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		finished = false
		report, err := lockFeedbackReport(ctx, tx, workspaceID, linking.ReportID)
		if err != nil {
			return err
		}
		if report.LinkingEndedAt != nil {
			return nil
		}
		if !store.FeedbackLinkingAttemptHolds(report, linking.AttemptAt) {
			return store.ErrFeedbackLinkingSuperseded
		}
		stored, err := queryFeedbackReportLinks(ctx, tx, workspaceID, report.ID)
		if err != nil {
			return err
		}
		write := store.PlanFeedbackReportLinking(report, stored, linking)
		for _, link := range write.New {
			if _, err := tx.Exec(ctx, `INSERT INTO feedback_report_links (workspace_id, id, report_id, task_id, state, evidence, created_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6::text::jsonb, $7, $8)`,
				workspaceID, link.ID, link.ReportID, link.TaskID, link.State, store.FeedbackReportEvidenceJSON(link.Evidence), link.CreatedAt, link.CreatedBy); err != nil {
				return err
			}
		}
		for _, link := range write.Extended {
			tag, err := tx.Exec(ctx, `UPDATE feedback_report_links SET evidence=$3::text::jsonb WHERE workspace_id=$1 AND id=$2`, workspaceID, link.ID, store.FeedbackReportEvidenceJSON(link.Evidence))
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("extend link %s: %d rows changed", link.ID, tag.RowsAffected())
			}
		}
		for _, note := range write.Notes {
			if _, err := tx.Exec(ctx, `INSERT INTO feedback_report_notes (workspace_id, id, report_id, kind, detail, created_at) VALUES ($1, $2, $3, $4, $5::text::jsonb, $6)`,
				workspaceID, note.ID, report.ID, note.Note.Kind, store.FeedbackReportNoteJSON(note.Note), write.Event.At); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE feedback_reports SET linking_ended_at=$3 WHERE workspace_id=$1 AND id=$2`, workspaceID, report.ID, write.Event.At); err != nil {
			return err
		}
		if err := insertWorkspaceEvent(ctx, q, write.Event); err != nil {
			return err
		}
		finished = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return finished, nil
}

func queryFeedbackReportLinks(ctx context.Context, q environmentQuerier, workspaceID, reportID string) ([]store.FeedbackReportLink, error) {
	rows, err := q.Query(ctx, `SELECT id, report_id, task_id, crate_id, state, evidence::text, created_at, created_by, confirmed_by, confirmed_at, removed_by, removed_at
FROM feedback_report_links WHERE workspace_id=$1 AND report_id=$2 ORDER BY id`, workspaceID, reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []store.FeedbackReportLink{}
	for rows.Next() {
		var link store.FeedbackReportLink
		var taskID, crateID, confirmedBy, removedBy *string
		var confirmedAt, removedAt pgtype.Timestamptz
		var evidence string
		if err := rows.Scan(&link.ID, &link.ReportID, &taskID, &crateID, &link.State, &evidence, &link.CreatedAt, &link.CreatedBy, &confirmedBy, &confirmedAt, &removedBy, &removedAt); err != nil {
			return nil, err
		}
		if link.Evidence, err = store.ParseFeedbackReportEvidence([]byte(evidence)); err != nil {
			return nil, err
		}
		link.CreatedAt = store.EnvironmentTime(link.CreatedAt)
		if taskID != nil {
			link.TaskID = *taskID
		}
		if crateID != nil {
			link.CrateID = *crateID
		}
		if confirmedAt.Valid && confirmedBy != nil {
			at := store.EnvironmentTime(confirmedAt.Time)
			link.ConfirmedAt, link.ConfirmedBy = &at, *confirmedBy
		}
		if removedAt.Valid && removedBy != nil {
			at := store.EnvironmentTime(removedAt.Time)
			link.RemovedAt, link.RemovedBy = &at, *removedBy
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// ListFeedbackReportLinks implements store.ReportLinkingStore.
func (s *Store) ListFeedbackReportLinks(ctx context.Context, reportID string) ([]store.FeedbackReportLink, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackReportLinks")
	return queryFeedbackReportLinks(ctx, s.boundary, workspace(ctx), reportID)
}

// ListFeedbackReportNotes implements store.ReportLinkingStore.
func (s *Store) ListFeedbackReportNotes(ctx context.Context, reportID string) ([]store.FeedbackReportNote, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackReportNotes")
	rows, err := s.boundary.Query(ctx, `SELECT detail::text FROM feedback_report_notes WHERE workspace_id=$1 AND report_id=$2 ORDER BY id`, workspace(ctx), reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := []store.FeedbackReportNote{}
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return nil, err
		}
		note, err := store.ParseFeedbackReportNote([]byte(detail))
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

// ListPendingFeedbackReports implements store.ReportLinkingStore.
func (s *Store) ListPendingFeedbackReports(ctx context.Context, environmentID string) ([]store.FeedbackReport, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListPendingFeedbackReports")
	return queryFeedbackReports(ctx, s.boundary, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=$1 AND environment_id=$2 AND linking_ended_at IS NULL ORDER BY id`, workspace(ctx), environmentID)
}

// SaveFeedbackLineProgress implements store.ReportLinkingStore under the
// report row lock.
func (s *Store) SaveFeedbackLineProgress(ctx context.Context, reportID string, attemptAt time.Time, step store.FeedbackLineStep) (store.FeedbackLineProgress, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "SaveFeedbackLineProgress")
	if _, err := store.RequireActor(ctx); err != nil {
		return store.FeedbackLineProgress{}, err
	}
	workspaceID := workspace(ctx)
	var saved store.FeedbackLineProgress
	err := s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		report, err := lockFeedbackReport(ctx, tx, workspaceID, reportID)
		if err != nil {
			return err
		}
		progress, changed, err := store.ApplyFeedbackLineStep(report.LineProgress, step)
		if err != nil {
			return err
		}
		if report.LinkingEndedAt == nil && !store.FeedbackLinkingAttemptHolds(report, attemptAt) {
			return store.ErrFeedbackLinkingSuperseded
		}
		if !changed || report.LinkingEndedAt != nil {
			saved = store.FeedbackLineProgress{}
			if report.LineProgress != nil {
				saved = *report.LineProgress
			}
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE feedback_reports SET line_progress=$3::text::jsonb WHERE workspace_id=$1 AND id=$2`, workspaceID, reportID, store.FeedbackLineProgressJSON(progress)); err != nil {
			return err
		}
		saved = progress
		return nil
	})
	if err != nil {
		return store.FeedbackLineProgress{}, err
	}
	return saved, nil
}

// SaveFeedbackTimingResult implements store.ReportLinkingStore under the
// report row lock.
func (s *Store) SaveFeedbackTimingResult(ctx context.Context, reportID string, attemptAt time.Time, result store.FeedbackTimingResult) (*store.FeedbackTimingResult, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "SaveFeedbackTimingResult")
	if _, err := store.RequireActor(ctx); err != nil {
		return nil, err
	}
	result, err := store.NormalizeFeedbackTimingResult(result)
	if err != nil {
		return nil, err
	}
	workspaceID := workspace(ctx)
	var saved *store.FeedbackTimingResult
	err = s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		report, err := lockFeedbackReport(ctx, tx, workspaceID, reportID)
		if err != nil {
			return err
		}
		if report.LinkingEndedAt == nil && !store.FeedbackLinkingAttemptHolds(report, attemptAt) {
			return store.ErrFeedbackLinkingSuperseded
		}
		if report.TimingResult != nil || report.LinkingEndedAt != nil {
			saved = report.TimingResult
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE feedback_reports SET timing_result=$3::text::jsonb WHERE workspace_id=$1 AND id=$2`, workspaceID, reportID, store.FeedbackTimingResultJSON(result)); err != nil {
			return err
		}
		saved = &result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}
