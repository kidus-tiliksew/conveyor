package singlestore

// DEC-39; component-persistence. Forge projections and their queue intents
// commit together and preserve the PostgreSQL lifecycle contract.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const githubLifecycleColumns = `task_id, repository, spec_version, source,
source_issue_number, issue_number, issue_url, outcome, state, create_state,
create_attempts, reconcile_misses, attempts, forge_error_category, last_error,
forge_author_class, forge_author_user_id,
created_at, updated_at`

const reviewPublicationColumns = `review_work_order_id, task_id, job_id, verdict,
reason_code, summary, feedback, reviewed_commit_sha, reviewer_model,
reviewer_session, same_model_as_implementer, review_round, review_seat,
required_model, required_harness, required_effort, model_enforcement, state, attempts, check_run_id,
comment_id, forge_error_category, last_error, forge_author_class, forge_author_user_id, created_at, updated_at`

func (s *Store) GetGitHubLifecycle(ctx context.Context, taskID string) (core.GitHubLifecycle, bool, error) {
	lifecycle, err := scanGitHubLifecycle(documentRow(ctx, s.db, "SELECT "+githubLifecycleColumns+" FROM github_lifecycles WHERE workspace_id=? AND task_id=?", documentWorkspace(ctx), taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return core.GitHubLifecycle{}, false, nil
	}
	return lifecycle, err == nil, err
}

func (s *Store) UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	if err := store.NormalizeForgeAuthorProjectionForWrite(&lifecycle.ForgeAuthorClass, &lifecycle.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	if err := validateGitHubWrite(lifecycle, false); err != nil {
		return err
	}
	return s.taskTx(ctx, lifecycle.TaskID, func(tx *sql.Tx) error {
		if err := lockKey(ctx, tx, "github-issues:"+documentWorkspace(ctx)); err != nil {
			return err
		}
		var repository string
		var current core.GitHubPublicationState
		if err := documentRow(ctx, tx, `SELECT state,repository FROM github_lifecycles WHERE workspace_id=? AND task_id=? FOR UPDATE`, documentWorkspace(ctx), lifecycle.TaskID).Scan(&current, &repository); err != nil {
			return notFound(err, "GitHub lifecycle for task %s", lifecycle.TaskID)
		}
		if err := store.ValidateGitHubPublicationTransition(current, lifecycle.State); err != nil {
			return err
		}
		if lifecycle.IssueNumber > 0 {
			var exists bool
			if err := documentRow(ctx, tx, `SELECT EXISTS(SELECT 1 FROM github_lifecycles WHERE workspace_id=? AND repository=? AND issue_number=? AND task_id<>?)`, documentWorkspace(ctx), repository, lifecycle.IssueNumber, lifecycle.TaskID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("GitHub issue number already belongs to another task")
			}
		}
		_, err := documentExec(ctx, tx, `UPDATE github_lifecycles SET
			issue_number=?, issue_url=?, outcome=?, state=?, attempts=?,
			forge_error_category=?, last_error=?, create_state=?, create_attempts=?, reconcile_misses=?,
			forge_author_class=?, forge_author_user_id=?, updated_at=now()
			WHERE workspace_id=? AND task_id=?`, lifecycle.IssueNumber,
			lifecycle.IssueURL, lifecycle.Outcome, lifecycle.State, lifecycle.Attempts,
			lifecycle.ForgeErrorCategory, lifecycle.LastError, lifecycle.CreateState, lifecycle.CreateAttempts,
			lifecycle.ReconcileMisses, lifecycle.ForgeAuthorClass, lifecycle.ForgeAuthorUserID, documentWorkspace(ctx), lifecycle.TaskID)
		if err != nil {
			return err
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
		return taskEvent(ctx, tx, core.Event{TaskID: lifecycle.TaskID, Kind: kind, Payload: core.JSONPayload(lifecycle)})
	})
}

func (s *Store) ReconcileGitHubLifecycles(ctx context.Context) (int, error) {
	rows, err := documentRows(ctx, s.db, `SELECT `+githubLifecycleColumns+` FROM github_lifecycles
		WHERE workspace_id=? AND state <> 'published' ORDER BY created_at`, documentWorkspace(ctx))
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

func (s *Store) GetReviewPublication(ctx context.Context, id string) (core.ReviewPublication, error) {
	publication, err := scanReviewPublication(documentRow(ctx, s.db, "SELECT "+reviewPublicationColumns+" FROM review_publications WHERE workspace_id=? AND review_work_order_id=?", documentWorkspace(ctx), id))
	if err != nil {
		return core.ReviewPublication{}, notFound(err, "review publication %s", id)
	}
	return publication, nil
}

func (s *Store) UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	if err := store.NormalizeForgeAuthorProjectionForWrite(&publication.ForgeAuthorClass, &publication.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var current core.ReviewPublication
		var state string
		if err := documentRow(ctx, tx, `SELECT state, comment_id FROM review_publications WHERE workspace_id=? AND review_work_order_id=? FOR UPDATE`, documentWorkspace(ctx), publication.ReviewWorkOrderID).Scan(&state, &current.CommentID); err != nil {
			return notFound(err, "review publication %s", publication.ReviewWorkOrderID)
		}
		current.State = core.ReviewPublicationState(state)
		if err := store.ValidateReviewPublicationUpdate(current, publication); err != nil {
			return err
		}
		_, err := documentExec(ctx, tx, `UPDATE review_publications SET state=?, attempts=?,
			check_run_id=?, comment_id=?, reviewed_commit_sha=?, forge_error_category=?, last_error=?,
			forge_author_class=?, forge_author_user_id=?, updated_at=now() WHERE workspace_id=? AND review_work_order_id=?`,
			publication.State, publication.Attempts, publication.CheckRunID,
			publication.CommentID, publication.ReviewedCommitSHA, publication.ForgeErrorCategory, publication.LastError,
			publication.ForgeAuthorClass, publication.ForgeAuthorUserID, documentWorkspace(ctx), publication.ReviewWorkOrderID)
		if err != nil {
			return err
		}
		kind := "review.publication_retry"
		if publication.State == core.ReviewPublicationPublished {
			kind = "review.publication_published"
		} else if publication.State == core.ReviewPublicationFailed {
			kind = "review.publication_failed"
		}
		return taskEvent(ctx, tx, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: kind, Payload: core.JSONPayload(publication)})
	})
}

func (s *Store) ReconcileReviewPublications(ctx context.Context) (int, error) {
	rows, err := documentRows(ctx, s.db, `SELECT e.task_id, COALESCE(e.job_id,''), e.payload_json
		FROM events e
		JOIN tasks t ON t.workspace_id=e.workspace_id AND t.id=e.task_id
		LEFT JOIN review_publications p ON p.workspace_id=t.workspace_id
			AND p.review_work_order_id=JSON_EXTRACT_STRING(e.payload_json,'review_work_order_id')
		WHERE t.workspace_id=? AND e.kind='review.completed'
			AND COALESCE(JSON_EXTRACT_STRING(e.payload_json,'review_work_order_id'),'') <> ''
			AND JSON_EXTRACT_JSON(e.payload_json,'publication_eligible') = 'true'
			AND p.review_work_order_id IS NULL
		ORDER BY e.id`, documentWorkspace(ctx))
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
	invalidRows, err := documentRows(ctx, s.db, "SELECT "+reviewPublicationColumns+` FROM review_publications
		WHERE workspace_id=? AND state='published' AND comment_id=0
		ORDER BY created_at, review_work_order_id`, documentWorkspace(ctx))
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
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := documentExec(ctx, tx, `UPDATE review_publications
			SET state='retrying', forge_error_category='', last_error=?, updated_at=now()
			WHERE workspace_id=? AND review_work_order_id=?
				AND state='published' AND comment_id=0`,
			"reconciling published review projection without required comment",
			documentWorkspace(ctx), publication.ReviewWorkOrderID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return nil
		}
		repaired = true
		publication.State = core.ReviewPublicationRetrying
		publication.ForgeErrorCategory = ""
		publication.LastError = "reconciling published review projection without required comment"
		if err = taskEvent(ctx, tx, core.Event{
			TaskID: publication.TaskID, JobID: publication.JobID,
			Kind: "review.publication_retry", Payload: core.JSONPayload(publication),
		}); err != nil {
			return err
		}
		return s.enqueueReviewPublicationJobTx(ctx, tx, publication.ReviewWorkOrderID)
	})
	return repaired, err
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

func (s *Store) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	if publication.State != "" && publication.State != core.ReviewPublicationQueued && publication.State != core.ReviewPublicationRetrying && publication.State != core.ReviewPublicationPublished && publication.State != core.ReviewPublicationFailed {
		return fmt.Errorf("invalid review publication state")
	}
	if publication.Verdict != "approve" && publication.Verdict != "changes_requested" {
		return fmt.Errorf("invalid review verdict")
	}
	if publication.ReviewRound < 0 || publication.ReviewSeat < 0 {
		return fmt.Errorf("invalid review round or seat")
	}
	if publication.ModelEnforcement != "" && publication.ModelEnforcement != "worker-pinned" && publication.ModelEnforcement != "self-reported" {
		return fmt.Errorf("invalid review model enforcement")
	}
	if publication.RequiredEffort != "" && publication.RequiredEffort != "low" && publication.RequiredEffort != "medium" && publication.RequiredEffort != "high" {
		return fmt.Errorf("invalid review effort")
	}
	return s.taskTx(ctx, publication.TaskID, func(tx *sql.Tx) error { return s.queueReviewPublicationTx(ctx, tx, publication) })
}
func (s *Store) enqueueReviewPublicationJobTx(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := logqueue.Enqueue(s2log.WithTx(ctx, tx), s.log, documentWorkspace(ctx), queue.ReviewPublicationArgs{}.Kind(), id, queue.ReviewPublicationArgs{WorkspaceID: documentWorkspace(ctx), ReviewWorkOrderID: id}, 5, time.Now().UTC())
	return err
}
func (s *Store) QueueGitHubLifecycle(ctx context.Context, p core.GitHubLifecycle) error {
	if err := store.NormalizeForgeAuthorProjectionForWrite(&p.ForgeAuthorClass, &p.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	if p.State == "" {
		p.State = core.GitHubPublicationQueued
	}
	if p.CreateState == "" {
		p.CreateState = core.GitHubCreateNotStarted
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if err := validateGitHubWrite(p, true); err != nil {
		return err
	}
	return s.taskTx(ctx, p.TaskID, func(tx *sql.Tx) error {
		if err := documentParent(ctx, tx, "tasks", p.TaskID); err != nil {
			return err
		}
		var exists bool
		if err := documentRow(ctx, tx, `SELECT EXISTS(SELECT 1 FROM github_lifecycles WHERE workspace_id=? AND task_id=?)`, documentWorkspace(ctx), p.TaskID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			_, err := writeRow(ctx, tx, rowWrite{table: "github_lifecycles", operation: "INSERT", values: map[string]any{"workspace_id": documentWorkspace(ctx), "task_id": p.TaskID, "repository": p.Repository, "spec_version": p.SpecVersion, "source": p.Source, "source_issue_number": p.SourceIssueNumber, "state": p.State, "create_state": p.CreateState, "forge_author_class": p.ForgeAuthorClass, "forge_author_user_id": p.ForgeAuthorUserID, "created_at": p.CreatedAt, "updated_at": p.CreatedAt}})
			if err != nil {
				return err
			}
			if err = taskEvent(ctx, tx, core.Event{TaskID: p.TaskID, Kind: "github_issue.publication_queued", Payload: core.JSONPayload(p)}); err != nil {
				return err
			}
		}
		_, err := logqueue.Enqueue(s2log.WithTx(ctx, tx), s.log, documentWorkspace(ctx), queue.GitHubIssuePublicationArgs{}.Kind(), p.TaskID, queue.GitHubIssuePublicationArgs{WorkspaceID: documentWorkspace(ctx), TaskID: p.TaskID}, 5, time.Now().UTC())
		return err
	})
}

// PostgreSQL CHECKs and its positive-issue partial index have no SingleStore
// equivalent. The update locks the workspace issue registry before checking.
func validateGitHubWrite(p core.GitHubLifecycle, inserting bool) error {
	switch p.State {
	case core.GitHubPublicationQueued, core.GitHubPublicationRetrying, core.GitHubPublicationPublished, core.GitHubPublicationFailed:
	default:
		return fmt.Errorf("invalid GitHub publication state")
	}
	switch p.CreateState {
	case core.GitHubCreateNotStarted, core.GitHubCreateReconciling, core.GitHubCreateConfirmed:
	default:
		return fmt.Errorf("invalid GitHub create state")
	}
	if inserting {
		if p.SourceIssueNumber < 0 {
			return fmt.Errorf("negative source issue number")
		}
	} else {
		if p.IssueNumber < 0 {
			return fmt.Errorf("negative GitHub issue number")
		}
		if p.Outcome != "" && p.Outcome != "created" && p.Outcome != "reused" {
			return fmt.Errorf("invalid GitHub publication outcome")
		}
	}
	return nil
}
