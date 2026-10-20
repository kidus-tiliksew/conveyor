package workorder

import (
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Review round summaries in rework context (req-review-gates-evidence
// AC-10.6; DEC-79; feature-review-round-summary SU-5; component-work-orders).
// A summary is informative: it sits beside bounce_history and its merged seat
// feedback, never replaces them, and stays outside the context-freshness
// snapshot, so recording one marks no order stale.

// reviewSummariesForBounces returns, for each pipeline.bounced event that
// carries a positive review_round, in the ledger order contextForOrder copies
// into bounce_history, the raw payload of that round's latest
// review.summary_recorded event. "Latest" is the maximum (at, id) tuple, the
// persistence order, independent of slice order. A bounce whose round has no
// summary contributes nothing, and a summary whose round never bounced is not
// included. Without summary events the result is nil, so the serialized
// context is unchanged.
func reviewSummariesForBounces(events []core.Event) []json.RawMessage {
	latest := map[int]core.Event{}
	for _, event := range events {
		if event.Kind != store.ReviewSummaryRecordedEvent {
			continue
		}
		round, ok := eventReviewRound(event.Payload)
		if !ok {
			continue
		}
		if current, seen := latest[round]; seen && !store.TaskEventAfter(event, store.TaskEventPosition{At: current.At, ID: current.ID}) {
			continue
		}
		latest[round] = event
	}
	if len(latest) == 0 {
		return nil
	}
	var summaries []json.RawMessage
	for _, event := range events {
		if event.Kind != "pipeline.bounced" {
			continue
		}
		round, ok := eventReviewRound(event.Payload)
		if !ok {
			continue
		}
		if summary, found := latest[round]; found {
			summaries = append(summaries, summary.Payload)
		}
	}
	return summaries
}

// eventReviewRound reads a payload's integer review_round above zero.
func eventReviewRound(payload json.RawMessage) (int, bool) {
	var fields struct {
		ReviewRound *int `json:"review_round"`
	}
	if json.Unmarshal(payload, &fields) != nil || fields.ReviewRound == nil || *fields.ReviewRound < 1 {
		return 0, false
	}
	return *fields.ReviewRound, true
}
