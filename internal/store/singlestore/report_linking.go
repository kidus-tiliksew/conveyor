package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var _ store.ReportLinkingStore = (*Store)(nil)

// feedbackReportLinkingAttemptMigrationVersion is the migration whose Go step
// adds feedback_reports.linking_last_attempt_at (component-feedback-reports
// invariant 15).
const feedbackReportLinkingAttemptMigrationVersion = 54

var feedbackReportLinkingAttemptColumns = []nullableColumn{
	{"feedback_reports", "linking_last_attempt_at", "DATETIME(6) NULL"},
	{"feedback_reports", "line_progress", "JSON NULL"},
	{"feedback_reports", "timing_result", "JSON NULL"},
	// Approved plan version 4: the GitHub repository a version's commit was
	// read from, written when the version resolves.
	{"feedback_resolutions", "resolved_github_repository", "LONGTEXT NULL"},
}

// lockFeedbackReport takes the workspace environment claim, the lock that
// serializes every report write of the workspace, and reads one report under
// it (component-feedback-reports invariants 6 and 15; component-persistence
// invariant 8). A transaction that writes stamps the report's environment row
// as its witness before it commits.
func lockFeedbackReport(ctx context.Context, tx *sql.Tx, ws, reportID string) (environmentClaim, store.FeedbackReport, error) {
	claim, err := claimEnvironments(ctx, tx, ws)
	if err != nil {
		return claim, store.FeedbackReport{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=? AND id=?`, ws, reportID)
	if err != nil {
		return claim, store.FeedbackReport{}, err
	}
	reports, err := collectFeedbackReports(rows)
	if err != nil {
		return claim, store.FeedbackReport{}, err
	}
	if len(reports) != 1 {
		return claim, store.FeedbackReport{}, fmt.Errorf("%w: feedback report %s", store.ErrNotFound, reportID)
	}
	return claim, reports[0], nil
}

// ClaimFeedbackReportLinking implements store.ReportLinkingStore.
func (s *Store) ClaimFeedbackReportLinking(ctx context.Context, reportID string, now time.Time) (store.FeedbackReport, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ClaimFeedbackReportLinking")
	if _, err := store.RequireActor(ctx); err != nil {
		return store.FeedbackReport{}, false, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.FeedbackReport{}, false, err
	}
	var report store.FeedbackReport
	claimed := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		claimed = false
		claim, locked, err := lockFeedbackReport(ctx, tx, ws, reportID)
		if err != nil {
			return err
		}
		report = locked
		if !store.FeedbackReportLinkingClaimable(report, now) {
			return nil
		}
		at := store.EnvironmentTime(now)
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_reports SET linking_last_attempt_at=? WHERE workspace_id=? AND id=?`, at, ws, reportID); err != nil {
			return err
		}
		if err := claim.touchEnvironment(ctx, tx, report.EnvironmentID); err != nil {
			return err
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
	ws, err := workspace(ctx)
	if err != nil {
		return store.FeedbackReportLineRead{}, false, err
	}
	read := store.FeedbackReportLineRead{}
	var answer string
	err = backendRow(ctx, s.db, `SELECT report_id,repository,resolved_commit,file,line,answer,answered_at FROM feedback_report_line_reads WHERE workspace_id=? AND report_id=?`, ws, reportID).
		Scan(&read.ReportID, &read.Repository, &read.ResolvedCommit, &read.File, &read.Line, &answer, &read.AnsweredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.FeedbackReportLineRead{}, false, nil
	}
	if err != nil {
		return store.FeedbackReportLineRead{}, false, err
	}
	if read.Answer, err = store.ParseFeedbackReportLineAnswer([]byte(answer)); err != nil {
		return store.FeedbackReportLineRead{}, false, err
	}
	read.AnsweredAt = store.EnvironmentTime(read.AnsweredAt)
	// A stored read that a write would refuse proves nothing and reads as
	// not stored.
	read, ok := store.CheckStoredFeedbackReportLineRead(read)
	return read, ok, nil
}

// RecordFeedbackReportLineRead implements store.ReportLinkingStore. It holds
// the workspace environment claim, so a stored answer is seen and kept.
func (s *Store) RecordFeedbackReportLineRead(ctx context.Context, read store.FeedbackReportLineRead) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackReportLineRead")
	if _, err := store.RequireActor(ctx); err != nil {
		return err
	}
	read, err := store.NormalizeFeedbackReportLineRead(read)
	if err != nil {
		return err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		claim, report, err := lockFeedbackReport(ctx, tx, ws, read.ReportID)
		if err != nil {
			return err
		}
		if report.LinkingEndedAt != nil {
			return store.ErrFeedbackLinkingSuperseded
		}
		var stored int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_report_line_reads WHERE workspace_id=? AND report_id=?`, ws, read.ReportID).Scan(&stored); err != nil {
			return err
		}
		if stored > 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO feedback_report_line_reads (workspace_id,report_id,repository,resolved_commit,file,line,answer,answered_at) VALUES (?,?,?,?,?,?,?,?)`,
			ws, read.ReportID, read.Repository, read.ResolvedCommit, read.File, read.Line, store.FeedbackReportLineAnswerJSON(read.Answer), read.AnsweredAt); err != nil {
			return err
		}
		return claim.touchEnvironment(ctx, tx, report.EnvironmentID)
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
	ws, err := workspace(ctx)
	if err != nil {
		return false, err
	}
	finished := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		finished = false
		claim, report, err := lockFeedbackReport(ctx, tx, ws, linking.ReportID)
		if err != nil {
			return err
		}
		if report.LinkingEndedAt != nil {
			return nil
		}
		if !store.FeedbackLinkingAttemptHolds(report, linking.AttemptAt) {
			return store.ErrFeedbackLinkingSuperseded
		}
		rows, err := tx.QueryContext(ctx, feedbackReportLinkSQL, ws, report.ID)
		if err != nil {
			return err
		}
		stored, err := collectFeedbackReportLinks(rows)
		if err != nil {
			return err
		}
		write := store.PlanFeedbackReportLinking(report, stored, linking)
		for _, link := range write.New {
			if _, err := tx.ExecContext(ctx, `INSERT INTO feedback_report_links (workspace_id,id,report_id,task_id,state,evidence,created_at,created_by) VALUES (?,?,?,?,?,?,?,?)`,
				ws, link.ID, link.ReportID, link.TaskID, link.State, store.FeedbackReportEvidenceJSON(link.Evidence), link.CreatedAt, link.CreatedBy); err != nil {
				return err
			}
		}
		for _, link := range write.Extended {
			if _, err := tx.ExecContext(ctx, `UPDATE feedback_report_links SET evidence=? WHERE workspace_id=? AND id=?`, store.FeedbackReportEvidenceJSON(link.Evidence), ws, link.ID); err != nil {
				return err
			}
		}
		for _, note := range write.Notes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO feedback_report_notes (workspace_id,id,report_id,kind,detail,created_at) VALUES (?,?,?,?,?,?)`,
				ws, note.ID, report.ID, note.Note.Kind, store.FeedbackReportNoteJSON(note.Note), write.Event.At); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_reports SET linking_ended_at=? WHERE workspace_id=? AND id=?`, write.Event.At, ws, report.ID); err != nil {
			return err
		}
		if err := insertWorkspaceEvent(ctx, tx, write.Event); err != nil {
			return err
		}
		if err := claim.touchEnvironment(ctx, tx, report.EnvironmentID); err != nil {
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

const feedbackReportLinkSQL = `SELECT id,report_id,task_id,crate_id,state,evidence,created_at,created_by,confirmed_by,confirmed_at,removed_by,removed_at
FROM feedback_report_links WHERE workspace_id=? AND report_id=? ORDER BY id`

func collectFeedbackReportLinks(rows environmentRows) ([]store.FeedbackReportLink, error) {
	defer rows.Close()
	links := []store.FeedbackReportLink{}
	for rows.Next() {
		var link store.FeedbackReportLink
		var taskID, crateID, confirmedBy, removedBy sql.NullString
		var confirmedAt, removedAt sql.NullTime
		var evidence string
		if err := rows.Scan(&link.ID, &link.ReportID, &taskID, &crateID, &link.State, &evidence, &link.CreatedAt, &link.CreatedBy, &confirmedBy, &confirmedAt, &removedBy, &removedAt); err != nil {
			return nil, err
		}
		var err error
		if link.Evidence, err = store.ParseFeedbackReportEvidence([]byte(evidence)); err != nil {
			return nil, err
		}
		link.CreatedAt = store.EnvironmentTime(link.CreatedAt)
		link.TaskID, link.CrateID = taskID.String, crateID.String
		if confirmedAt.Valid && confirmedBy.Valid {
			at := store.EnvironmentTime(confirmedAt.Time)
			link.ConfirmedAt, link.ConfirmedBy = &at, confirmedBy.String
		}
		if removedAt.Valid && removedBy.Valid {
			at := store.EnvironmentTime(removedAt.Time)
			link.RemovedAt, link.RemovedBy = &at, removedBy.String
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// ListFeedbackReportLinks implements store.ReportLinkingStore.
func (s *Store) ListFeedbackReportLinks(ctx context.Context, reportID string) ([]store.FeedbackReportLink, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackReportLinks")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, feedbackReportLinkSQL, ws, reportID)
	if err != nil {
		return nil, err
	}
	return collectFeedbackReportLinks(rows)
}

// ListFeedbackReportNotes implements store.ReportLinkingStore.
func (s *Store) ListFeedbackReportNotes(ctx context.Context, reportID string) ([]store.FeedbackReportNote, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackReportNotes")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT detail FROM feedback_report_notes WHERE workspace_id=? AND report_id=? ORDER BY id`, ws, reportID)
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
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT `+feedbackReportColumns+` FROM feedback_reports WHERE workspace_id=? AND environment_id=? AND linking_ended_at IS NULL ORDER BY id`, ws, environmentID)
	if err != nil {
		return nil, err
	}
	return collectFeedbackReports(rows)
}

// SaveFeedbackLineProgress implements store.ReportLinkingStore under the
// workspace environment claim.
func (s *Store) SaveFeedbackLineProgress(ctx context.Context, reportID string, attemptAt time.Time, step store.FeedbackLineStep) (store.FeedbackLineProgress, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "SaveFeedbackLineProgress")
	if _, err := store.RequireActor(ctx); err != nil {
		return store.FeedbackLineProgress{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.FeedbackLineProgress{}, err
	}
	var saved store.FeedbackLineProgress
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		claim, report, err := lockFeedbackReport(ctx, tx, ws, reportID)
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
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_reports SET line_progress=? WHERE workspace_id=? AND id=?`, store.FeedbackLineProgressJSON(progress), ws, reportID); err != nil {
			return err
		}
		if err := claim.touchEnvironment(ctx, tx, report.EnvironmentID); err != nil {
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
// workspace environment claim.
func (s *Store) SaveFeedbackTimingResult(ctx context.Context, reportID string, attemptAt time.Time, result store.FeedbackTimingResult) (*store.FeedbackTimingResult, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "SaveFeedbackTimingResult")
	if _, err := store.RequireActor(ctx); err != nil {
		return nil, err
	}
	result, err := store.NormalizeFeedbackTimingResult(result)
	if err != nil {
		return nil, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	var saved *store.FeedbackTimingResult
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		claim, report, err := lockFeedbackReport(ctx, tx, ws, reportID)
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
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_reports SET timing_result=? WHERE workspace_id=? AND id=?`, store.FeedbackTimingResultJSON(result), ws, reportID); err != nil {
			return err
		}
		if err := claim.touchEnvironment(ctx, tx, report.EnvironmentID); err != nil {
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
