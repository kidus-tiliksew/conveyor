package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// Risk policy status publication rows (req-risk-based-review AC-10.1,
// AC-10.2; component-git-delivery invariant 37). Every write runs in taskTx,
// under the task operation lock, and claims the head's policy-status claim
// row, which records the revision of the row's latest write. A commit that
// spans partitions can release the lock before the row it wrote is visible,
// so a writer waits until the claimed revision is visible before it decides,
// and two concurrent writers never both miss each other's write.

const reviewPolicyStatusMaxAttempts = 5

func reviewPolicyStatusClaimKey(ctx context.Context, taskID, head string) string {
	return "review-policy-status:" + documentWorkspace(ctx) + ":" + taskID + ":" + head
}

func (s *Store) QueueReviewPolicyStatus(ctx context.Context, taskID, head, desired string) (core.ReviewPolicyStatus, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "QueueReviewPolicyStatus")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	head = strings.TrimSpace(head)
	var result core.ReviewPolicyStatus
	wrote := false
	err := s.taskTx(ctx, taskID, func(tx *sql.Tx) error {
		task, err := getTaskRow(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if head == "" {
			_, _, err = store.PlanReviewPolicyStatusQueue(task, nil, head, desired, time.Now())
			return err
		}
		existing, found, err := lockReviewPolicyStatusTx(ctx, tx, taskID, head)
		if err != nil {
			return err
		}
		var current *core.ReviewPolicyStatus
		if found {
			current = &existing
		}
		next, write, err := store.PlanReviewPolicyStatusQueue(task, current, head, desired, time.Now())
		if err != nil || !write {
			result = next
			return err
		}
		if err := writeReviewPolicyStatusTx(ctx, tx, found, existing.Revision, next); err != nil {
			return err
		}
		if err := taskEvent(ctx, tx, store.ReviewPolicyStatusEvent(next)); err != nil {
			return err
		}
		args := queue.ReviewPolicyStatusArgs{WorkspaceID: documentWorkspace(ctx), TaskID: taskID, HeadSHA: head}
		if _, err := logqueue.Enqueue(logTx(ctx, tx), s.log, documentWorkspace(ctx), args.Kind(), args.UniqueKey(), args, reviewPolicyStatusMaxAttempts, time.Now().UTC()); err != nil {
			return fmt.Errorf("enqueue policy status of task %s head %s: %w", taskID, head, err)
		}
		result, wrote = next, true
		return nil
	})
	if err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	return result, wrote, nil
}

func (s *Store) GetReviewPolicyStatus(ctx context.Context, taskID, head string) (core.ReviewPolicyStatus, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetReviewPolicyStatus")
	if _, err := getTaskRow(ctx, s.db, taskID); err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	return getReviewPolicyStatus(ctx, s.db, taskID, head)
}

func (s *Store) ListReviewPolicyStatuses(ctx context.Context, taskID string) ([]core.ReviewPolicyStatus, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListReviewPolicyStatuses")
	if _, err := getTaskRow(ctx, s.db, taskID); err != nil {
		return nil, err
	}
	return queryReviewPolicyStatuses(ctx, s.db, `SELECT payload_json FROM review_policy_statuses WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), taskID)
}

func (s *Store) ListOpenReviewPolicyStatuses(ctx context.Context) ([]core.ReviewPolicyStatus, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListOpenReviewPolicyStatuses")
	return queryReviewPolicyStatuses(ctx, s.db, `SELECT payload_json FROM review_policy_statuses WHERE workspace_id=? AND state <> 'published'`, documentWorkspace(ctx))
}

func (s *Store) UpdateReviewPolicyStatus(ctx context.Context, status core.ReviewPolicyStatus) (core.ReviewPolicyStatus, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "UpdateReviewPolicyStatus")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.ReviewPolicyStatus{}, err
	}
	var result core.ReviewPolicyStatus
	err := s.taskTx(ctx, status.TaskID, func(tx *sql.Tx) error {
		current, found, err := lockReviewPolicyStatusTx(ctx, tx, status.TaskID, status.HeadSHA)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: policy status of task %s head %s", store.ErrNotFound, status.TaskID, status.HeadSHA)
		}
		next, err := store.PrepareReviewPolicyStatusUpdate(current, status, time.Now())
		if err != nil {
			return err
		}
		if err := writeReviewPolicyStatusTx(ctx, tx, true, current.Revision, next); err != nil {
			return err
		}
		result = next
		return taskEvent(ctx, tx, store.ReviewPolicyStatusEvent(next))
	})
	if err != nil {
		return core.ReviewPolicyStatus{}, err
	}
	return result, nil
}

// lockReviewPolicyStatusTx claims the head's claim row and reads the head's
// row once its latest claimed revision is visible.
func lockReviewPolicyStatusTx(ctx context.Context, tx *sql.Tx, taskID, head string) (core.ReviewPolicyStatus, bool, error) {
	holder, err := claimKey(ctx, tx, reviewPolicyStatusClaimKey(ctx, taskID, head))
	if err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	latest := 0
	if holder != "" {
		if latest, err = strconv.Atoi(holder); err != nil || latest < 1 {
			return core.ReviewPolicyStatus{}, false, fmt.Errorf("policy status claim of task %s head %s holds %q, not a revision", taskID, head, holder)
		}
	}
	var row core.ReviewPolicyStatus
	found := false
	visible, err := awaitClaimedVisibility(ctx, func() (bool, error) {
		var readErr error
		row, found, readErr = getReviewPolicyStatus(ctx, tx, taskID, head)
		if readErr != nil {
			return false, readErr
		}
		return latest == 0 || (found && row.Revision >= latest), nil
	})
	if err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	if !visible {
		return core.ReviewPolicyStatus{}, false, fmt.Errorf("%w: policy status revision %d of task %s head %s is not visible", store.ErrRetryable, latest, taskID, head)
	}
	return row, found, nil
}

// writeReviewPolicyStatusTx inserts or replaces the row at revision and
// records the new revision on the claim row.
func writeReviewPolicyStatusTx(ctx context.Context, tx *sql.Tx, exists bool, revision int, next core.ReviewPolicyStatus) error {
	if exists {
		res, err := documentExec(ctx, tx, `UPDATE review_policy_statuses SET state=?, desired=?, revision=?, payload_json=?, updated_at=? WHERE workspace_id=? AND task_id=? AND head_sha=? AND revision=?`,
			string(next.State), next.Desired, next.Revision, core.JSONPayload(next), next.UpdatedAt, documentWorkspace(ctx), next.TaskID, next.HeadSHA, revision)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return store.ErrReviewPolicyStatusStale
		}
	} else if _, err := documentExec(ctx, tx, `INSERT INTO review_policy_statuses(workspace_id,task_id,head_sha,state,desired,revision,payload_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		documentWorkspace(ctx), next.TaskID, next.HeadSHA, string(next.State), next.Desired, next.Revision, core.JSONPayload(next), next.CreatedAt, next.UpdatedAt); err != nil {
		return err
	}
	return setClaim(ctx, tx, reviewPolicyStatusClaimKey(ctx, next.TaskID, next.HeadSHA), strconv.Itoa(next.Revision))
}

type reviewPolicyStatusReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getReviewPolicyStatus(ctx context.Context, db reviewPolicyStatusReader, taskID, head string) (core.ReviewPolicyStatus, bool, error) {
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT payload_json FROM review_policy_statuses WHERE workspace_id=? AND task_id=? AND head_sha=?`, documentWorkspace(ctx), taskID, head).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ReviewPolicyStatus{}, false, nil
	}
	if err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	var row core.ReviewPolicyStatus
	if err := json.Unmarshal(raw, &row); err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	return row, true, nil
}

func queryReviewPolicyStatuses(ctx context.Context, db reviewPolicyStatusReader, query string, args ...any) ([]core.ReviewPolicyStatus, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.ReviewPolicyStatus{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var row core.ReviewPolicyStatus
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortReviewPolicyStatuses(out)
	return out, nil
}
