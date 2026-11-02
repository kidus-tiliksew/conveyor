package singlestore

// SingleStore review-summary versions under the cross-partition visibility
// window (feature-review-round-summary SU-2, SU-7; component-persistence,
// SingleStore claimed records; task 261101-187e02). Removing the latest
// summary event stands for a predecessor whose lock row was released before
// its event became visible.

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func summaryIntegrationRequest(taskID, requestID string) store.ReviewSummaryRequest {
	return store.ReviewSummaryRequest{TaskID: taskID, ReviewRound: 1, RequestID: requestID, Summary: "Round one summary " + requestID,
		Author: store.ReviewSummaryAuthor{ActorID: "user:summary-author", ActorRole: core.ActorUser}}
}

// settleSummaryRound appends the review.round_completed event that settles
// round 1 with one approving seat; it is the only event a summary reads to
// find a settled round.
func settleSummaryRound(t *testing.T, st *Store, ctx context.Context, taskID string) {
	t.Helper()
	if err := st.withTx(ctx, func(tx *sql.Tx) error {
		return taskEvent(ctx, tx, core.Event{TaskID: taskID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{
			"review_round": 1, "verdict": "approve", "reviews": []map[string]any{{"review_seat": 1, "verdict": "approve"}},
		})})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewSummaryWaitsForUnseenPredecessorIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, ws, task := seedClaimTask(t, st, "summary-window")
	settleSummaryRound(t, st, ctx, task.ID)
	first, err := st.RecordReviewSummaryCommand(ctx, summaryIntegrationRequest(task.ID, "first"))
	if err != nil || first.Version != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	key := reviewSummaryClaimKey(ws, task.ID, 1)
	if got := claimHolder(t, st, key); got != (claimedRecord{Table: claimedEventsTable, ID: first.EventID, Note: "1"}).holder() {
		t.Fatalf("round claim=%q after version 1", got)
	}

	// A new request must not count only the visible history: it waits for
	// the hidden version 1 and receives version 2.
	hidden := hideEvent(t, st, ws, first.EventID)
	waitCtx, obs := observeClaimedRecordWait(ctx)
	type outcome struct {
		result store.ReviewSummaryResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, e := st.RecordReviewSummaryCommand(waitCtx, summaryIntegrationRequest(task.ID, "second"))
		done <- outcome{r, e}
	}()
	awaitClaimWait(t, obs)
	select {
	case o := <-done:
		t.Fatalf("recorded before version 1 was visible: %+v", o)
	default:
	}
	hidden.restore(t, st)
	second := <-done
	if second.err != nil || second.result.Version != 2 || second.result.Replayed {
		t.Fatalf("second=%+v err=%v, want new version 2", second.result, second.err)
	}

	// An identical replay inside the window returns the original result
	// instead of appending a duplicate.
	hidden = hideEvent(t, st, ws, second.result.EventID)
	waitCtx, obs = observeClaimedRecordWait(ctx)
	go func() {
		r, e := st.RecordReviewSummaryCommand(waitCtx, summaryIntegrationRequest(task.ID, "second"))
		done <- outcome{r, e}
	}()
	awaitClaimWait(t, obs)
	hidden.restore(t, st)
	replay := <-done
	if replay.err != nil || !replay.result.Replayed || replay.result.Version != 2 || replay.result.EventID != second.result.EventID || !replay.result.RecordedAt.Equal(second.result.RecordedAt) {
		t.Fatalf("replay=%+v err=%v, want the recorded %+v", replay.result, replay.err, second.result)
	}

	// The next version derives from the version the claim row records, not
	// only from a count over the history.
	nameClaim(t, st, ctx, key, claimedRecord{Table: claimedEventsTable, ID: second.result.EventID, Note: "5"}.holder())
	third, err := st.RecordReviewSummaryCommand(ctx, summaryIntegrationRequest(task.ID, "third"))
	if err != nil || third.Version != 6 {
		t.Fatalf("third=%+v err=%v, want version 6 after a recorded 5", third, err)
	}
	var summaries int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE workspace_id=? AND task_id=? AND kind=?`, ws, task.ID, store.ReviewSummaryRecordedEvent).Scan(&summaries); err != nil || summaries != 3 {
		t.Fatalf("summary events=%d err=%v, want 3", summaries, err)
	}
}

func TestReviewSummaryClaimTimeoutWritesNothingIntegration(t *testing.T) {
	st := integrationStore(t)
	shortClaimWait(t)
	ctx, ws, task := seedClaimTask(t, st, "summary-timeout")
	settleSummaryRound(t, st, ctx, task.ID)
	nameClaim(t, st, ctx, reviewSummaryClaimKey(ws, task.ID, 1), claimedRecord{Table: claimedEventsTable, ID: 1 << 60, Note: "7"}.holder())
	before := countTaskEvents(t, st, ws, task.ID)
	if _, err := st.RecordReviewSummaryCommand(ctx, summaryIntegrationRequest(task.ID, "never-visible")); !errors.Is(err, store.ErrRetryable) {
		t.Fatalf("err=%v, want ErrRetryable", err)
	}
	if after := countTaskEvents(t, st, ws, task.ID); after != before {
		t.Fatalf("task events %d -> %d after a refused summary", before, after)
	}
}
