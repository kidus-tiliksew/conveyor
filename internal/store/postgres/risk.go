package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// Risk ratings (req-risk-based-review AC-5.5, AC-5.6, REQ-6;
// component-risk-review; component-persistence). Each write takes the task's
// operation lock, reads the recorded ratings, and inserts one row and its
// activity event in the same transaction.

func (s *Store) RecordRiskRating(ctx context.Context, rating core.RiskRating) (core.RiskRating, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordRiskRating")
	if _, err := store.RequireActor(ctx); err != nil {
		return core.RiskRating{}, false, err
	}
	var result core.RiskRating
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		task, existing, err := riskTaskTx(ctx, tx, q, rating.TaskID)
		if err != nil {
			return err
		}
		result, created, err = store.PrepareRiskRating(task, existing, rating, time.Now())
		if err != nil || !created {
			return err
		}
		return insertRiskRatingTx(ctx, tx, q, result)
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
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		task, existing, err := riskTaskTx(ctx, tx, q, raise.TaskID)
		if err != nil {
			return err
		}
		result, err = store.PrepareRiskRaise(task, existing, raise, time.Now())
		if err != nil {
			return err
		}
		return insertRiskRatingTx(ctx, tx, q, result)
	})
	if err != nil {
		return core.RiskRating{}, err
	}
	return result, nil
}

func (s *Store) ListTaskRiskRatings(ctx context.Context, taskID string) ([]core.RiskRating, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListTaskRiskRatings")
	if _, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)}); err != nil {
		return nil, notFound(err, "task %s", taskID)
	}
	return listRiskRatings(ctx, s.boundary, taskID)
}

type riskRowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func listRiskRatings(ctx context.Context, querier riskRowQuerier, taskID string) ([]core.RiskRating, error) {
	rows, err := querier.Query(ctx, `SELECT payload_json FROM task_risk_ratings WHERE workspace_id=$1 AND task_id=$2 ORDER BY seq`, workspace(ctx), taskID)
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

// riskTaskTx takes the task's operation lock and reads the task and its
// recorded ratings.
func riskTaskTx(ctx context.Context, tx pgx.Tx, q *db.Queries, taskID string) (core.Task, []core.RiskRating, error) {
	if err := lockWorkOrderTaskTx(ctx, tx, workspace(ctx), taskID); err != nil {
		return core.Task{}, nil, err
	}
	row, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return core.Task{}, nil, notFound(err, "task %s", taskID)
	}
	existing, err := listRiskRatings(ctx, tx, taskID)
	if err != nil {
		return core.Task{}, nil, err
	}
	return taskFromDB(row), existing, nil
}

func insertRiskRatingTx(ctx context.Context, tx pgx.Tx, q *db.Queries, rating core.RiskRating) error {
	if _, err := tx.Exec(ctx, `INSERT INTO task_risk_ratings(workspace_id,task_id,rating_key,seq,source,tier,payload_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		workspace(ctx), rating.TaskID, rating.Key, rating.Seq, string(rating.Source), string(rating.Tier), core.JSONPayload(rating), rating.CreatedAt); err != nil {
		return err
	}
	return insertEvent(ctx, q, store.RiskRatedEvent(rating))
}

// recordReviewerRiskTx records a verdict's reviewer rating inside the
// PostgreSQL verdict transaction, which already holds the task lock.
func recordReviewerRiskTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task core.Task, decision core.ReviewDecision) error {
	if decision.RiskTier == "" {
		return nil
	}
	existing, err := listRiskRatings(ctx, tx, task.ID)
	if err != nil {
		return err
	}
	rating, created, err := store.ReviewerRiskRating(task, existing, decision, time.Now())
	if err != nil || !created {
		return err
	}
	return insertRiskRatingTx(ctx, tx, q, rating)
}

// recordPlanRiskTx records an approved plan version's rating inside the
// PostgreSQL approval transaction, which already holds the task lock
// (req-risk-based-review AC-4.3; feature-risk-based-review RR-4).
func recordPlanRiskTx(ctx context.Context, tx pgx.Tx, q *db.Queries, taskID string, version int, content string) error {
	row, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return notFound(err, "task %s", taskID)
	}
	task := taskFromDB(row)
	if !task.SetupContract.Risk.Enabled() {
		return nil
	}
	existing, err := listRiskRatings(ctx, tx, taskID)
	if err != nil {
		return err
	}
	rating, created, err := store.PlanApprovalRiskRating(task, existing, version, content, time.Now())
	if err != nil || !created {
		return err
	}
	return insertRiskRatingTx(ctx, tx, q, rating)
}

// recordIntakePlanRiskTx records an approved intake plan's rating inside the
// creating transaction (req-risk-based-review AC-4.3).
func recordIntakePlanRiskTx(ctx context.Context, tx pgx.Tx, q *db.Queries, task core.Task) error {
	rating, err := store.IntakePlanRiskRating(task)
	if err != nil || rating == nil {
		return err
	}
	return insertRiskRatingTx(ctx, tx, q, *rating)
}
