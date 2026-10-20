package httpapi

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Review seat metadata of the bounded task-detail read
// (feature-review-seat-minimum RS-7, RS-3; req-review-gates-evidence AC-2.7,
// AC-2.8; component-http-api, Bounded detail view).

// taskReviewSeatMetadata is one review round's seat count beside the task's
// minimum. A recorded round reports its stored record; a legacy round reports
// its non-superseded seat orders with source minimum.
type taskReviewSeatMetadata struct {
	ReviewRound      int    `json:"review_round"`
	SeatCount        int    `json:"seat_count"`
	SeatCountSource  string `json:"seat_count_source"`
	MinimumSeatCount int    `json:"minimum_seat_count"`
	PlannedSeatCount *int   `json:"planned_seat_count,omitempty"`
	CarriedFromRound *int   `json:"carried_from_round,omitempty"`
	Superseded       bool   `json:"superseded,omitempty"`
}

// taskReviewRoundSeats keys seat metadata by positive review round and, for
// round-zero panels that predate rounds, by review work order ID.
type taskReviewRoundSeats struct {
	ByRound map[string]taskReviewSeatMetadata `json:"by_round"`
	ByOrder map[string]taskReviewSeatMetadata `json:"by_order"`
}

// taskReviewSeatPolicy states the task's minimum and the latest approved
// plan's count even before the first review round opens.
type taskReviewSeatPolicy struct {
	MinimumSeatCount int  `json:"minimum_seat_count"`
	PlannedSeatCount *int `json:"planned_seat_count,omitempty"`
}

// enrichReviewSeatSummaries adds the review seat projection to summaries that
// deriveTaskDetailSummaries already folded from the complete ledger. It
// reuses the handler's complete events and work orders, reads the round
// records and the approved plan once, and reads the workspace configuration
// only when the task freezes no seat list. A read or configuration failure is
// returned; the helper never reports a guessed minimum.
func (s *Server) enrichReviewSeatSummaries(ctx context.Context, task core.Task, orders []core.WorkOrder, events []core.Event, summaries *taskDetailSummaries) error {
	scoped := store.WithWorkspace(ctx, task.Workspace)
	records, err := s.Store.ListReviewRoundSeats(scoped, task.ID)
	if err != nil {
		return fmt.Errorf("list review round seats: %w", err)
	}
	var planned *int
	plan, ok, err := s.Store.GetApprovedSpecVersion(scoped, task.ID)
	if err != nil {
		return fmt.Errorf("read approved plan: %w", err)
	}
	if ok && plan.ReviewSeats != nil {
		count := *plan.ReviewSeats
		planned = &count
	}
	// Like Service.AddReviewSeats, a missing provider counts no workspace seats.
	workspaceSeats := 0
	if task.SetupContract.Review.Seats == nil && s.ConfigProvider != nil {
		cfg, cfgErr := s.ConfigProvider(scoped)
		if cfgErr != nil {
			return fmt.Errorf("resolve review seat minimum: %w", cfgErr)
		}
		if cfg != nil {
			workspaceSeats = len(cfg.Review.Seats)
		}
	}
	minimum := store.ReviewSeatMinimum(task.SetupContract, workspaceSeats)
	applyReviewSeatSummaries(summaries, minimum, planned, orders, events, records)
	return nil
}

// applyReviewSeatSummaries is the pure projection behind
// enrichReviewSeatSummaries.
func applyReviewSeatSummaries(summaries *taskDetailSummaries, minimum int, planned *int, orders []core.WorkOrder, events []core.Event, records []store.ReviewRoundSeats) {
	rounds, latest := projectReviewRoundSeats(minimum, planned, orders, events, records)
	summaries.ReviewRoundSeats = &rounds
	summaries.ReviewSeatPolicy = &taskReviewSeatPolicy{MinimumSeatCount: minimum, PlannedSeatCount: copyIntPointer(planned)}
	if latest == 0 {
		return
	}
	key := strconv.Itoa(latest)
	metadata := rounds.ByRound[key]
	// The settlement fields stay only when the latest review.round_completed
	// is the latest round's own completion; an open round carries seat fields
	// alone, so no verdict or approval is fabricated for it.
	settled, ok := summaries.ReviewRounds.ByRound[key]
	if ok && summaries.ReviewRound != nil && summaries.ReviewRound.EventID == settled.EventID {
		summaries.ReviewRound.taskReviewSeatMetadata = &metadata
		return
	}
	summaries.ReviewRound = &taskReviewRoundSummary{taskReviewSeatMetadata: &metadata}
}

// projectReviewRoundSeats returns the per-round and round-zero seat metadata
// and the latest positive review round, or 0 when the task has none.
func projectReviewRoundSeats(minimum int, planned *int, orders []core.WorkOrder, events []core.Event, records []store.ReviewRoundSeats) (taskReviewRoundSeats, int) {
	result := taskReviewRoundSeats{ByRound: map[string]taskReviewSeatMetadata{}, ByOrder: map[string]taskReviewSeatMetadata{}}
	superseded := store.SupersededReviewWorkOrders(events)
	recorded := make(map[int]store.ReviewRoundSeats, len(records))
	// A positive round exists when a review order or a record names it, the
	// same round set store.ReviewRoundSeatInputsFor selects predecessors from.
	exists := map[int]bool{}
	for _, record := range records {
		if record.ReviewRound > 0 {
			recorded[record.ReviewRound] = record
			exists[record.ReviewRound] = true
		}
	}
	live := map[int]int{}
	retired := map[int]bool{}
	for _, order := range orders {
		if order.Stage != core.StageReview {
			continue
		}
		if order.ReviewRound <= 0 {
			entry := taskReviewSeatMetadata{SeatCount: 1, SeatCountSource: store.ReviewSeatSourceMinimum,
				MinimumSeatCount: minimum, PlannedSeatCount: copyIntPointer(planned)}
			if superseded[order.ID] {
				entry.SeatCount, entry.Superseded = 0, true
			}
			result.ByOrder[order.ID] = entry
			continue
		}
		exists[order.ReviewRound] = true
		if superseded[order.ID] {
			retired[order.ReviewRound] = true
		} else {
			live[order.ReviewRound]++
		}
	}
	latest := 0
	for round := range exists {
		entry := taskReviewSeatMetadata{ReviewRound: round, PlannedSeatCount: copyIntPointer(planned), Superseded: retired[round]}
		if record, ok := recorded[round]; ok {
			entry.SeatCount, entry.SeatCountSource, entry.MinimumSeatCount = record.SeatCount, record.SeatCountSource, record.MinimumSeatCount
		} else {
			entry.SeatCount, entry.SeatCountSource, entry.MinimumSeatCount = live[round], store.ReviewSeatSourceMinimum, minimum
		}
		if entry.SeatCountSource == store.ReviewSeatSourceCarried {
			if previous := previousReviewRound(exists, round); previous > 0 {
				entry.CarriedFromRound = &previous
			}
		}
		result.ByRound[strconv.Itoa(round)] = entry
		if round > latest {
			latest = round
		}
	}
	return result, latest
}

// previousReviewRound is the greatest existing round below round, never an
// assumed round-1, or 0 when none exists.
func previousReviewRound(exists map[int]bool, round int) int {
	previous := 0
	for candidate := range exists {
		if candidate < round && candidate > previous {
			previous = candidate
		}
	}
	return previous
}

func copyIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
