package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) ReadVerificationReview(ctx context.Context, taskID, orderID string) (store.VerificationReviewState, error) {
	var state store.VerificationReviewState
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		records, err := q.ListVerificationRecords(ctx, workspace(ctx), taskID)
		if err != nil {
			return err
		}
		order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2", workspace(ctx), orderID))
		if err != nil {
			return err
		}
		if order.TaskID != taskID {
			return store.ErrVerificationAccess
		}
		state = store.VerificationReviewState{Rows: verificationRows(records), Order: order}
		return nil
	})
	return state, err
}
