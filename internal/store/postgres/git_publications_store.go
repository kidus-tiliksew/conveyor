package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) hydrateGitHubLifecycle(ctx context.Context, task *core.Task) error {
	closeProjection, exists, closeErr := s.GetPullRequestClose(ctx, task.ID)
	if closeErr != nil {
		return closeErr
	}
	if exists {
		task.PullRequestClose = &closeProjection
		task.PullRequestCloseState = closeProjection.State
	}
	if err := s.hydrateTaskAssignee(ctx, task); err != nil {
		return err
	}
	lifecycle, ok, err := s.GetGitHubLifecycle(ctx, task.ID)
	if err != nil {
		return err
	}
	if ok {
		task.GitHub = &lifecycle
	}
	return nil
}

func (s *Store) hydrateGitHubLifecyclesBatch(ctx context.Context, tasks []core.Task) error {
	if err := s.hydratePullRequestCloses(ctx, tasks); err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	if err := s.hydrateTaskAssignees(ctx, tasks); err != nil {
		return err
	}
	taskIDs := make([]string, len(tasks))
	for i := range tasks {
		taskIDs[i] = tasks[i].ID
	}
	rows, err := s.boundary.Query(ctx, "SELECT "+githubLifecycleColumns+" FROM github_lifecycles WHERE workspace_id=$1 AND task_id=ANY($2::text[]) ORDER BY task_id", workspace(ctx), taskIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	byTask := make(map[string]core.GitHubLifecycle, len(tasks))
	for rows.Next() {
		lifecycle, scanErr := scanGitHubLifecycle(rows)
		if scanErr != nil {
			return scanErr
		}
		byTask[lifecycle.TaskID] = lifecycle
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range tasks {
		if lifecycle, ok := byTask[tasks[i].ID]; ok {
			copy := lifecycle
			tasks[i].GitHub = &copy
		}
	}
	return nil
}

const githubLifecycleColumns = `task_id, repository, spec_version, source,
source_issue_number, issue_number, issue_url, outcome, state, create_state,
create_attempts, reconcile_misses, attempts, forge_error_category, last_error,
forge_author_class, forge_author_user_id,
created_at, updated_at`

func (s *Store) QueueGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	if err := store.NormalizeForgeAuthorProjectionForWrite(&lifecycle.ForgeAuthorClass, &lifecycle.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if lifecycle.State == "" {
			lifecycle.State = core.GitHubPublicationQueued
		}
		if lifecycle.CreateState == "" {
			lifecycle.CreateState = core.GitHubCreateNotStarted
		}
		if lifecycle.CreatedAt.IsZero() {
			lifecycle.CreatedAt = time.Now().UTC()
		}
		command, err := tx.Exec(ctx, `INSERT INTO github_lifecycles (
			workspace_id, task_id, repository, spec_version, source,
			source_issue_number, state, create_state, forge_author_class, forge_author_user_id, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)
		ON CONFLICT (workspace_id, task_id) DO NOTHING`, workspace(ctx),
			lifecycle.TaskID, lifecycle.Repository, lifecycle.SpecVersion,
			lifecycle.Source, lifecycle.SourceIssueNumber, lifecycle.State, lifecycle.CreateState,
			lifecycle.ForgeAuthorClass, lifecycle.ForgeAuthorUserID, lifecycle.CreatedAt)
		if err != nil {
			return err
		}
		if command.RowsAffected() == 1 {
			if err = insertEvent(ctx, q, core.Event{TaskID: lifecycle.TaskID, Kind: "github_issue.publication_queued", Payload: core.JSONPayload(lifecycle)}); err != nil {
				return err
			}
		}
		return s.queue.enqueueGitHubIssuePublicationTx(ctx, tx, workspace(ctx), lifecycle.TaskID)
	})
}

func (s *Store) GetGitHubLifecycle(ctx context.Context, taskID string) (core.GitHubLifecycle, bool, error) {
	lifecycle, err := scanGitHubLifecycle(s.boundary.QueryRow(ctx, "SELECT "+githubLifecycleColumns+" FROM github_lifecycles WHERE workspace_id=$1 AND task_id=$2", workspace(ctx), taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.GitHubLifecycle{}, false, nil
	}
	return lifecycle, err == nil, err
}

func (s *Store) UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	if err := store.NormalizeForgeAuthorProjectionForWrite(&lifecycle.ForgeAuthorClass, &lifecycle.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var current core.GitHubPublicationState
		if err := tx.QueryRow(ctx, `SELECT state FROM github_lifecycles WHERE workspace_id=$1 AND task_id=$2 FOR UPDATE`, workspace(ctx), lifecycle.TaskID).Scan(&current); err != nil {
			return notFound(err, "GitHub lifecycle for task %s", lifecycle.TaskID)
		}
		if err := store.ValidateGitHubPublicationTransition(current, lifecycle.State); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE github_lifecycles SET
			issue_number=$1, issue_url=$2, outcome=$3, state=$4, attempts=$5,
			forge_error_category=$6, last_error=$7, create_state=$8, create_attempts=$9, reconcile_misses=$10,
			forge_author_class=$11, forge_author_user_id=$12, updated_at=now()
			WHERE workspace_id=$13 AND task_id=$14`, lifecycle.IssueNumber,
			lifecycle.IssueURL, lifecycle.Outcome, lifecycle.State, lifecycle.Attempts,
			lifecycle.ForgeErrorCategory, lifecycle.LastError, lifecycle.CreateState, lifecycle.CreateAttempts,
			lifecycle.ReconcileMisses, lifecycle.ForgeAuthorClass, lifecycle.ForgeAuthorUserID, workspace(ctx), lifecycle.TaskID)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return fmt.Errorf("GitHub lifecycle for task %s not found", lifecycle.TaskID)
		}
		var kind string
		switch {
		case lifecycle.State == core.GitHubPublicationPublished:
			kind = "github_issue.publication_published"
		case lifecycle.State == core.GitHubPublicationFailed:
			kind = "github_issue.publication_failed"
		case lifecycle.State == core.GitHubPublicationRetrying && strings.TrimSpace(lifecycle.LastError) != "":
			kind = "github_issue.publication_retry"
		}
		if kind == "" {
			return nil
		}
		return insertEvent(ctx, q, core.Event{TaskID: lifecycle.TaskID, Kind: kind, Payload: core.JSONPayload(lifecycle)})
	})
}

func (s *Store) ReconcileGitHubLifecycles(ctx context.Context) (int, error) {
	rows, err := s.boundary.Query(ctx, `SELECT `+githubLifecycleColumns+` FROM github_lifecycles
		WHERE workspace_id=$1 AND state <> 'published' ORDER BY created_at`, workspace(ctx))
	if err != nil {
		return 0, err
	}
	var pending []core.GitHubLifecycle
	for rows.Next() {
		lifecycle, scanErr := scanGitHubLifecycle(rows)
		if scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		pending = append(pending, lifecycle)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, lifecycle := range pending {
		if err = s.QueueGitHubLifecycle(ctx, lifecycle); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}

func scanGitHubLifecycle(row interface{ Scan(...any) error }) (core.GitHubLifecycle, error) {
	var lifecycle core.GitHubLifecycle
	var state, createState string
	err := row.Scan(&lifecycle.TaskID, &lifecycle.Repository, &lifecycle.SpecVersion,
		&lifecycle.Source, &lifecycle.SourceIssueNumber, &lifecycle.IssueNumber,
		&lifecycle.IssueURL, &lifecycle.Outcome, &state, &createState,
		&lifecycle.CreateAttempts, &lifecycle.ReconcileMisses, &lifecycle.Attempts,
		&lifecycle.ForgeErrorCategory, &lifecycle.LastError,
		&lifecycle.ForgeAuthorClass, &lifecycle.ForgeAuthorUserID,
		&lifecycle.CreatedAt, &lifecycle.UpdatedAt)
	lifecycle.State = core.GitHubPublicationState(state)
	lifecycle.CreateState = core.GitHubCreateState(createState)
	return lifecycle, err
}

const reviewPublicationColumns = `review_work_order_id, task_id, job_id, verdict,
reason_code, summary, feedback, reviewed_commit_sha, reviewer_model,
reviewer_session, same_model_as_implementer, review_round, review_seat,
required_model, required_harness, required_effort, model_enforcement, state, attempts, check_run_id,
comment_id, forge_error_category, last_error, forge_author_class, forge_author_user_id, created_at, updated_at`

func (s *Store) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		return s.queueReviewPublicationTx(ctx, tx, q, publication)
	})
}

func (s *Store) queueReviewPublicationTx(ctx context.Context, tx pgx.Tx, q *db.Queries, publication core.ReviewPublication) error {
	if err := store.NormalizeForgeAuthorProjectionForWrite(&publication.ForgeAuthorClass, &publication.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	if publication.State == "" {
		publication.State = core.ReviewPublicationQueued
	}
	if publication.CreatedAt.IsZero() {
		publication.CreatedAt = time.Now().UTC()
	}
	publication.UpdatedAt = publication.CreatedAt
	command, err := tx.Exec(ctx, `INSERT INTO review_publications (
			review_work_order_id, workspace_id, task_id, job_id, verdict, reason_code,
			summary, feedback, reviewed_commit_sha, reviewer_model, reviewer_session,
			same_model_as_implementer, review_round, review_seat, required_model,
			required_harness, required_effort, model_enforcement, state, forge_author_class, forge_author_user_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (review_work_order_id) DO NOTHING`, publication.ReviewWorkOrderID,
		workspace(ctx), publication.TaskID, publication.JobID, publication.Verdict,
		publication.ReasonCode, publication.Summary, publication.Feedback,
		publication.ReviewedCommitSHA, publication.ReviewerModel,
		publication.ReviewerSession, publication.SameModelAsImplementer,
		publication.ReviewRound, publication.ReviewSeat, publication.RequiredModel,
		publication.RequiredHarness, publication.RequiredEffort, publication.ModelEnforcement,
		publication.State, publication.ForgeAuthorClass, publication.ForgeAuthorUserID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return nil
	}
	if err := insertEvent(ctx, q, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: "review.publication_queued", Payload: core.JSONPayload(publication)}); err != nil {
		return err
	}
	return s.enqueueReviewPublicationJobTx(ctx, tx, publication.ReviewWorkOrderID)
}

func (s *Store) enqueueReviewPublicationJobTx(ctx context.Context, tx pgx.Tx, reviewWorkOrderID string) error {
	return s.queue.enqueueReviewPublicationTx(ctx, tx, workspace(ctx), reviewWorkOrderID)
}

func (s *Store) GetReviewPublication(ctx context.Context, id string) (core.ReviewPublication, error) {
	publication, err := scanReviewPublication(s.boundary.QueryRow(ctx, "SELECT "+reviewPublicationColumns+" FROM review_publications WHERE workspace_id=$1 AND review_work_order_id=$2", workspace(ctx), id))
	if err != nil {
		return core.ReviewPublication{}, notFound(err, "review publication %s", id)
	}
	return publication, nil
}

func (s *Store) UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	if err := store.NormalizeForgeAuthorProjectionForWrite(&publication.ForgeAuthorClass, &publication.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var current core.ReviewPublication
		var state string
		if err := tx.QueryRow(ctx, `SELECT state, comment_id FROM review_publications WHERE workspace_id=$1 AND review_work_order_id=$2 FOR UPDATE`, workspace(ctx), publication.ReviewWorkOrderID).Scan(&state, &current.CommentID); err != nil {
			return notFound(err, "review publication %s", publication.ReviewWorkOrderID)
		}
		current.State = core.ReviewPublicationState(state)
		if err := store.ValidateReviewPublicationUpdate(current, publication); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE review_publications SET state=$1, attempts=$2,
			check_run_id=$3, comment_id=$4, reviewed_commit_sha=$5, forge_error_category=$6, last_error=$7,
			forge_author_class=$8, forge_author_user_id=$9, updated_at=now() WHERE workspace_id=$10 AND review_work_order_id=$11`,
			publication.State, publication.Attempts, publication.CheckRunID,
			publication.CommentID, publication.ReviewedCommitSHA, publication.ForgeErrorCategory, publication.LastError,
			publication.ForgeAuthorClass, publication.ForgeAuthorUserID, workspace(ctx), publication.ReviewWorkOrderID)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return fmt.Errorf("review publication %s not found", publication.ReviewWorkOrderID)
		}
		kind := "review.publication_retry"
		if publication.State == core.ReviewPublicationPublished {
			kind = "review.publication_published"
		} else if publication.State == core.ReviewPublicationFailed {
			kind = "review.publication_failed"
		}
		return insertEvent(ctx, q, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: kind, Payload: core.JSONPayload(publication)})
	})
}

func (s *Store) ReconcileReviewPublications(ctx context.Context) (int, error) {
	rows, err := s.boundary.Query(ctx, `SELECT e.task_id, COALESCE(e.job_id,''), e.payload_json
		FROM events e
		JOIN tasks t ON t.id=e.task_id
		LEFT JOIN review_publications p ON p.workspace_id=t.workspace_id
			AND p.review_work_order_id=e.payload_json->>'review_work_order_id'
		WHERE t.workspace_id=$1 AND e.kind='review.completed'
			AND COALESCE(e.payload_json->>'review_work_order_id','') <> ''
			AND e.payload_json @> '{"publication_eligible": true}'::jsonb
			AND p.review_work_order_id IS NULL
		ORDER BY e.id`, workspace(ctx))
	if err != nil {
		return 0, err
	}
	var missing []core.ReviewPublication
	for rows.Next() {
		var taskID, jobID string
		var payload []byte
		if err = rows.Scan(&taskID, &jobID, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		var publication core.ReviewPublication
		if json.Unmarshal(payload, &publication) == nil && publication.ReviewWorkOrderID != "" {
			publication.TaskID, publication.JobID = taskID, jobID
			publication.State = core.ReviewPublicationQueued
			if publication.ForgeAuthorClass == "" || publication.ForgeAuthorClass == core.ForgeAuthorClass("host") {
				publication.ForgeAuthorClass = core.ForgeAuthorWorkspace
				publication.ForgeAuthorUserID = ""
			}
			missing = append(missing, publication)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	invalidRows, err := s.boundary.Query(ctx, "SELECT "+reviewPublicationColumns+` FROM review_publications
		WHERE workspace_id=$1 AND state='published' AND comment_id=0
		ORDER BY created_at, review_work_order_id`, workspace(ctx))
	if err != nil {
		return 0, err
	}
	var invalid []core.ReviewPublication
	for invalidRows.Next() {
		publication, scanErr := scanReviewPublication(invalidRows)
		if scanErr != nil {
			invalidRows.Close()
			return 0, scanErr
		}
		invalid = append(invalid, publication)
	}
	if err = invalidRows.Err(); err != nil {
		invalidRows.Close()
		return 0, err
	}
	invalidRows.Close()

	created := 0
	for _, publication := range missing {
		if err = s.QueueReviewPublication(ctx, publication); err != nil {
			return created, err
		}
		created++
	}
	for _, publication := range invalid {
		repaired, repairErr := s.repairPublishedReviewPublication(ctx, publication)
		if repairErr != nil {
			return created, repairErr
		}
		if repaired {
			created++
		}
	}
	return created, nil
}

func (s *Store) repairPublishedReviewPublication(ctx context.Context, publication core.ReviewPublication) (bool, error) {
	repaired := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		result, err := tx.Exec(ctx, `UPDATE review_publications
			SET state='retrying', forge_error_category='', last_error=$1, updated_at=now()
			WHERE workspace_id=$2 AND review_work_order_id=$3
				AND state='published' AND comment_id=0`,
			"reconciling published review projection without required comment",
			workspace(ctx), publication.ReviewWorkOrderID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return nil
		}
		repaired = true
		publication.State = core.ReviewPublicationRetrying
		publication.ForgeErrorCategory = ""
		publication.LastError = "reconciling published review projection without required comment"
		if err = insertEvent(ctx, q, core.Event{
			TaskID: publication.TaskID, JobID: publication.JobID,
			Kind: "review.publication_retry", Payload: core.JSONPayload(publication),
		}); err != nil {
			return err
		}
		return s.enqueueReviewPublicationJobTx(ctx, tx, publication.ReviewWorkOrderID)
	})
	return repaired, err
}

func reviewPublicationFromDecision(decision core.ReviewDecision) core.ReviewPublication {
	return core.ReviewPublication{
		ReviewWorkOrderID: decision.ReviewWorkOrderID, TaskID: decision.TaskID, JobID: decision.JobID,
		Verdict: decision.Verdict, ReasonCode: decision.ReasonCode, Summary: decision.Summary,
		Feedback: decision.Feedback, ReviewedCommitSHA: decision.ReviewedCommitSHA,
		ReviewerModel: decision.ReviewerModel, ReviewerSession: decision.ReviewerSession,
		SameModelAsImplementer: decision.SameModelAsImplementer,
		ReviewRound:            decision.ReviewRound, ReviewSeat: decision.ReviewSeat,
		RequiredModel: decision.RequiredModel, RequiredHarness: decision.RequiredHarness, RequiredEffort: decision.RequiredEffort,
		ModelEnforcement: decision.ModelEnforcement, ForgeAuthorClass: core.ForgeAuthorWorkspace,
	}
}

func scanReviewPublication(row interface{ Scan(...any) error }) (core.ReviewPublication, error) {
	var publication core.ReviewPublication
	var state string
	err := row.Scan(&publication.ReviewWorkOrderID, &publication.TaskID, &publication.JobID,
		&publication.Verdict, &publication.ReasonCode, &publication.Summary,
		&publication.Feedback, &publication.ReviewedCommitSHA, &publication.ReviewerModel,
		&publication.ReviewerSession, &publication.SameModelAsImplementer,
		&publication.ReviewRound, &publication.ReviewSeat, &publication.RequiredModel,
		&publication.RequiredHarness, &publication.RequiredEffort, &publication.ModelEnforcement, &state,
		&publication.Attempts, &publication.CheckRunID, &publication.CommentID,
		&publication.ForgeErrorCategory, &publication.LastError, &publication.ForgeAuthorClass, &publication.ForgeAuthorUserID,
		&publication.CreatedAt, &publication.UpdatedAt)
	publication.State = core.ReviewPublicationState(state)
	return publication, err
}
