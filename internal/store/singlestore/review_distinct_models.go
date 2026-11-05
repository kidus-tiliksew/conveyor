package singlestore

// SingleStore distinct review seat names under the cross-partition visibility
// window (req-review-gates-evidence AC-2.10; DEC-104(4); component-review;
// component-persistence, SingleStore visibility).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// reviewSeatModelClaimKey names the claim row that records a round's latest
// seat claim under a frozen true review.distinct_models.
func reviewSeatModelClaimKey(workspace, taskID string, round int) string {
	return "review-seat-model:" + workspace + ":" + taskID + ":" + strconv.Itoa(round)
}

// reviewSeatClaimNote is the note on a round's claim record. It identifies the
// order row image the comparison must see instead of carrying a model name, so
// nothing accumulates: a released, expired, or cancelled seat stops counting
// through the same row comparison as before.
type reviewSeatClaimNote struct {
	OrderID   string
	AttemptID string
	// ReplacedAt is the updated_at of the row image the claim overwrote.
	ReplacedAt time.Time
}

func (n reviewSeatClaimNote) String() string {
	return strconv.FormatInt(n.ReplacedAt.UnixMicro(), 10) + ":" + n.AttemptID + ":" + n.OrderID
}

func parseReviewSeatClaimNote(note string) (reviewSeatClaimNote, error) {
	parts := strings.SplitN(note, ":", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return reviewSeatClaimNote{}, fmt.Errorf("review seat claim note %q names no order", note)
	}
	micros, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return reviewSeatClaimNote{}, fmt.Errorf("review seat claim note %q names no order", note)
	}
	return reviewSeatClaimNote{OrderID: parts[2], AttemptID: parts[1], ReplacedAt: time.UnixMicro(micros)}, nil
}

// showsOrFollowsClaim reports whether an order row image is the named claim's
// image or a later one. A row keeps the attempt while the claim stands. A later
// write that clears the attempt also leaves a stamp other than the one the
// claim replaced, because the claim itself overwrote that stamp. Only the image
// the claim replaced fails both, so the rule needs no write path to report that
// a claim ended and reads no clock order across daemons.
func (n reviewSeatClaimNote) showsOrFollowsClaim(attemptID string, updatedAt time.Time) bool {
	return attemptID == n.AttemptID || updatedAt.UnixMicro() != n.ReplacedAt.UnixMicro()
}

// reviewSeatSiblingsTx returns the task's orders for the distinct-model
// comparison, read only after the round's latest claim is visible on its own
// order row. The holder names the claim's work_order.claimed event, and
// awaitClaimedRecord waits for it, but the event and the row live on different
// partitions, so a visible event doesn't prove a visible row. This call waits
// for the row too. Each holder waited for its predecessor's row before it wrote
// its own claim, so every earlier claim of the round is visible as well.
//
// A wait that runs out returns store.ErrRetryable, which rolls the claim
// transaction back with nothing written.
func (s *Store) reviewSeatSiblingsTx(ctx context.Context, tx *sql.Tx, o core.WorkOrder) ([]core.WorkOrder, error) {
	ws := documentWorkspace(ctx)
	record, claimed, err := awaitClaimedRecord(ctx, tx, reviewSeatModelClaimKey(ws, o.TaskID, o.ReviewRound), ws, o.TaskID)
	if err != nil {
		return nil, err
	}
	if claimed {
		note, err := parseReviewSeatClaimNote(record.Note)
		if err != nil {
			return nil, err
		}
		if err = awaitReviewSeatClaimRow(ctx, tx, ws, note); err != nil {
			return nil, err
		}
	}
	return s.listOrders(ctx, tx, []string{o.TaskID})
}

// awaitReviewSeatClaimRow waits until the named order row is no longer the
// image the claim replaced. A missing row was deleted after the claim read it,
// so it holds no name.
func awaitReviewSeatClaimRow(ctx context.Context, tx *sql.Tx, ws string, note reviewSeatClaimNote) error {
	visible, err := awaitClaimedVisibility(ctx, func() (bool, error) {
		var attemptID string
		var updatedAt time.Time
		err := tx.QueryRowContext(ctx, `SELECT attempt_id,updated_at FROM work_orders WHERE workspace_id=? AND id=?`, ws, note.OrderID).Scan(&attemptID, &updatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return note.showsOrFollowsClaim(attemptID, updatedAt), nil
	})
	if err != nil {
		return err
	}
	if !visible {
		return fmt.Errorf("%w: claim of review seat %s is not visible", store.ErrRetryable, note.OrderID)
	}
	return nil
}

// lockReviewSeatRowTx locks the row a seat claim is about to overwrite and
// returns its stamp. The order read at the start of the transaction can predate
// a committed write that isn't visible yet; the locked read waits for that
// write, so the note names the image the claim really replaces.
func lockReviewSeatRowTx(ctx context.Context, tx *sql.Tx, ws, orderID string) (time.Time, error) {
	var updatedAt time.Time
	err := tx.QueryRowContext(ctx, `SELECT updated_at FROM work_orders WHERE workspace_id=? AND id=? FOR UPDATE`, ws, orderID).Scan(&updatedAt)
	return updatedAt, err
}

// recordReviewSeatClaimTx names the work_order.claimed event this transaction
// just appended on the round's claim row, so the name commits with the release.
func recordReviewSeatClaimTx(ctx context.Context, tx *sql.Tx, o core.WorkOrder, replacedAt time.Time) error {
	ws := documentWorkspace(ctx)
	appended, err := lastInsertedRecord(ctx, tx, claimedEventsTable, ws, true)
	if err != nil {
		return err
	}
	appended.Note = reviewSeatClaimNote{OrderID: o.ID, AttemptID: o.AttemptID, ReplacedAt: replacedAt}.String()
	return recordClaimedRecord(ctx, tx, reviewSeatModelClaimKey(ws, o.TaskID, o.ReviewRound), appended)
}
