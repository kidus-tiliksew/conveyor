package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// Risk ratings (req-risk-based-review AC-5.5, AC-5.6, REQ-6;
// component-risk-review; component-persistence). Each write runs in taskTx,
// under the task's operation lock, and inserts one row and its activity event
// in the same transaction. SingleStore has no CHECK constraint, so the shared
// store helpers validate the source and tier before any write.

func (s *Store) RecordRiskRating(ctx context.Context, rating core.RiskRating) (core.RiskRating, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordRiskRating")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.RiskRating{}, false, err
	}
	var result core.RiskRating
	created := false
	err := s.taskTx(ctx, rating.TaskID, func(tx *sql.Tx) error {
		task, existing, err := riskTaskTx(ctx, tx, rating.TaskID)
		if err != nil {
			return err
		}
		result, created, err = store.PrepareRiskRating(task, existing, rating, time.Now())
		if err != nil || !created {
			return err
		}
		return insertRiskRatingTx(ctx, tx, result)
	})
	if err != nil {
		return core.RiskRating{}, false, err
	}
	return result, created, nil
}

func (s *Store) RaiseTaskRiskTier(ctx context.Context, raise core.RiskRating) (core.RiskRating, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RaiseTaskRiskTier")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.RiskRating{}, err
	}
	var result core.RiskRating
	err := s.taskTx(ctx, raise.TaskID, func(tx *sql.Tx) error {
		task, existing, err := riskTaskTx(ctx, tx, raise.TaskID)
		if err != nil {
			return err
		}
		result, err = store.PrepareRiskRaise(task, existing, raise, time.Now())
		if err != nil {
			return err
		}
		return insertRiskRatingTx(ctx, tx, result)
	})
	if err != nil {
		return core.RiskRating{}, err
	}
	return result, nil
}

func (s *Store) ListTaskRiskRatings(ctx context.Context, taskID string) ([]core.RiskRating, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskRiskRatings")
	if _, err := getTaskRow(ctx, s.db, taskID); err != nil {
		return nil, err
	}
	return listRiskRatings(ctx, s.db, taskID)
}

func listRiskRatings(ctx context.Context, db s2log.Executor, taskID string) ([]core.RiskRating, error) {
	rows, err := documentRows(ctx, db, `SELECT payload_json FROM task_risk_ratings WHERE workspace_id=? AND task_id=? ORDER BY seq`, documentWorkspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ratings := []core.RiskRating{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var rating core.RiskRating
		if err := json.Unmarshal(raw, &rating); err != nil {
			return nil, err
		}
		ratings = append(ratings, rating)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortRiskRatings(ratings)
	return ratings, nil
}

func riskTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (core.Task, []core.RiskRating, error) {
	task, err := getTaskRow(ctx, tx, taskID)
	if err != nil {
		return core.Task{}, nil, err
	}
	existing, err := lockRiskRatingsTx(ctx, tx, taskID)
	if err != nil {
		return core.Task{}, nil, err
	}
	return task, existing, nil
}

// riskRatingClaimKey names the claim row that records the sequence of a
// task's latest rating.
func riskRatingClaimKey(ctx context.Context, taskID string) string {
	return "risk-rating:" + documentWorkspace(ctx) + ":" + taskID
}

// lockRiskRatingsTx claims the task's rating claim row and reads the task's
// ratings. A commit that spans partitions can release a lock row before the
// rows it wrote on other partitions are visible, so the claim row holds the
// sequence of the latest rating, committed with its release, and the read
// waits until that rating is visible. Two concurrent writers therefore never
// both miss each other's rating: the second sees the first's tier and
// sequence, and a raise at or below it is refused as not raised
// (req-risk-based-review AC-6.2; component-risk-review invariant 4;
// component-persistence).
func lockRiskRatingsTx(ctx context.Context, tx *sql.Tx, taskID string) ([]core.RiskRating, error) {
	holder, err := claimKey(ctx, tx, riskRatingClaimKey(ctx, taskID))
	if err != nil {
		return nil, err
	}
	latest := 0
	if holder != "" {
		if latest, err = strconv.Atoi(holder); err != nil || latest < 1 {
			return nil, fmt.Errorf("risk rating claim of task %s holds %q, not a rating sequence", taskID, holder)
		}
	}
	var ratings []core.RiskRating
	visible, err := awaitClaimedVisibility(ctx, func() (bool, error) {
		var readErr error
		ratings, readErr = listRiskRatings(ctx, tx, taskID)
		if readErr != nil {
			return false, readErr
		}
		seen := 0
		for _, rating := range ratings {
			seen = max(seen, rating.Seq)
		}
		return seen >= latest, nil
	})
	if err != nil {
		return nil, err
	}
	if !visible {
		return nil, fmt.Errorf("%w: risk rating %d of task %s is not visible", store.ErrRetryable, latest, taskID)
	}
	return ratings, nil
}

// insertRiskRatingTx inserts one rating and its activity event and records
// its sequence on the claim row lockRiskRatingsTx claimed.
func insertRiskRatingTx(ctx context.Context, tx *sql.Tx, rating core.RiskRating) error {
	if _, err := documentExec(ctx, tx, `INSERT INTO task_risk_ratings(workspace_id,task_id,rating_key,seq,source,tier,payload_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		documentWorkspace(ctx), rating.TaskID, rating.Key, rating.Seq, string(rating.Source), string(rating.Tier), core.JSONPayload(rating), rating.CreatedAt); err != nil {
		return err
	}
	if err := taskEvent(ctx, tx, store.RiskRatedEvent(rating)); err != nil {
		return err
	}
	return setClaim(ctx, tx, riskRatingClaimKey(ctx, rating.TaskID), strconv.Itoa(rating.Seq))
}

// recordReviewerRiskTx records a verdict's reviewer rating inside the
// SingleStore verdict transaction, which already holds the task lock.
func recordReviewerRiskTx(ctx context.Context, tx *sql.Tx, task core.Task, decision core.ReviewDecision) error {
	if decision.RiskTier == "" {
		return nil
	}
	existing, err := lockRiskRatingsTx(ctx, tx, task.ID)
	if err != nil {
		return err
	}
	rating, created, err := store.ReviewerRiskRating(task, existing, decision, time.Now())
	if err != nil || !created {
		return err
	}
	return insertRiskRatingTx(ctx, tx, rating)
}

// recordPlanRiskTx records an approved plan version's rating inside the
// SingleStore approval transaction, which already holds the task lock
// (req-risk-based-review AC-4.3; feature-risk-based-review RR-4).
func recordPlanRiskTx(ctx context.Context, tx *sql.Tx, taskID string, version int) error {
	task, err := getTaskRow(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if !task.SetupContract.Risk.Enabled() {
		return nil
	}
	var content string
	if err := tx.QueryRowContext(ctx, `SELECT content FROM task_specs WHERE workspace_id=? AND task_id=? AND version=?`, documentWorkspace(ctx), taskID, version).Scan(&content); err != nil {
		return fmt.Errorf("read approved plan version %d of task %s: %w", version, taskID, err)
	}
	existing, err := lockRiskRatingsTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	rating, created, err := store.PlanApprovalRiskRating(task, existing, version, content, time.Now())
	if err != nil || !created {
		return err
	}
	return insertRiskRatingTx(ctx, tx, rating)
}

// recordIntakePlanRiskTx records an approved intake plan's rating inside the
// creating transaction (req-risk-based-review AC-4.3).
func recordIntakePlanRiskTx(ctx context.Context, tx *sql.Tx, task core.Task) error {
	rating, err := store.IntakePlanRiskRating(task)
	if err != nil || rating == nil {
		return err
	}
	existing, err := lockRiskRatingsTx(ctx, tx, task.ID)
	if err != nil {
		return err
	}
	prepared, created, err := store.PrepareRiskRating(task, existing, *rating, rating.CreatedAt)
	if err != nil || !created {
		return err
	}
	return insertRiskRatingTx(ctx, tx, prepared)
}
