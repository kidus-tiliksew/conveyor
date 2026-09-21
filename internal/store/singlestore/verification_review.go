package singlestore

import (
	"context"
	"database/sql"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) ReadVerificationReview(ctx context.Context, taskID, orderID string) (store.VerificationReviewState, error) {
	var state store.VerificationReviewState
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := verificationRowsTx(ctx, tx, documentWorkspace(ctx), taskID)
		if err != nil {
			return err
		}
		order, err := getOrderRow(ctx, tx, orderID)
		if err != nil {
			return err
		}
		if order.TaskID != taskID {
			return store.ErrVerificationAccess
		}
		state = store.VerificationReviewState{Rows: rows, Order: order}
		return nil
	})
	return state, err
}
