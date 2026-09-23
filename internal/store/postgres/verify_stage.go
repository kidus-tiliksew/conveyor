package postgres

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func verifyReviewReadyTx(ctx context.Context, tx pgx.Tx, task core.Task) (bool, error) {
	if !task.SetupContract.VerifyStage {
		return true, nil
	}
	var ready bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='verify' AND state='completed' AND head_sha=$3 AND head_sha<>'' AND verification_context_id<>'')`, workspace(ctx), task.ID, core.VerifyStageHead(task)).Scan(&ready)
	return ready, err
}
func requireVerifyReviewTx(ctx context.Context, tx pgx.Tx, task core.Task) error {
	ready, err := verifyReviewReadyTx(ctx, tx, task)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("review requires completed verification for the submitted head")
	}
	return nil
}
