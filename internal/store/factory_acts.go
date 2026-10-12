package store

import (
	"encoding/json"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

const factoryActLimit = 100

var factoryActKinds = map[string]bool{
	"pipeline.bounced":              true,
	"work_order.expired":            true,
	"work_order.released":           true,
	"work_order.requeued":           true,
	"merge.conflict_fix_dispatched": true,
	"review.refresh_round_created":  true,
	"review.seat_recovered":         true,
	"merge.confirmed":               true,
}

// SelectFactoryActs keeps the factory's own acts. Callers pass candidates
// already limited to the workspace, the kind set, and at >= since.
func SelectFactoryActs(events []core.Event, since time.Time, limit int) []nextaction.Act {
	if limit <= 0 || limit > factoryActLimit {
		limit = factoryActLimit
	}
	ordered := append([]core.Event(nil), events...)
	sortEventsOldest(ordered)
	retryCount := map[string]int{}
	kept := map[int64]bool{}
	for _, event := range ordered {
		if !factoryActKinds[event.Kind] || event.At.Before(since) {
			continue
		}
		if !factoryActor(event) {
			continue
		}
		switch event.Kind {
		case "pipeline.bounced":
			if operatorRequestChanges(event) {
				continue
			}
		case "work_order.expired", "work_order.released":
			continue
		case "work_order.requeued":
			count := automaticRetryCount(event)
			previous := retryCount[event.TaskID]
			if count > previous {
				kept[event.ID] = true
				markPairedRelease(ordered, event, kept)
			}
			retryCount[event.TaskID] = count
			continue
		case "merge.confirmed":
			if event.ActorRole != core.ActorRunner {
				continue
			}
		}
		kept[event.ID] = true
	}
	acts := make([]nextaction.Act, 0, len(kept))
	for i := len(ordered) - 1; i >= 0; i-- {
		event := ordered[i]
		if !kept[event.ID] {
			continue
		}
		acts = append(acts, nextaction.Act{ID: event.ID, TaskID: event.TaskID, Kind: event.Kind, At: event.At, Automatic: true})
		if len(acts) == limit {
			break
		}
	}
	return acts
}

func factoryActor(event core.Event) bool {
	return event.ActorRole == core.ActorSystem || event.ActorRole == core.ActorRunner
}

func operatorRequestChanges(event core.Event) bool {
	var payload struct {
		Source     string `json:"source"`
		ReasonCode string `json:"reason_code"`
	}
	_ = json.Unmarshal(event.Payload, &payload)
	return payload.Source == "request-changes" || payload.ReasonCode == "user_changes_requested"
}

func automaticRetryCount(event core.Event) int {
	var payload struct {
		Count int `json:"automatic_retry_count"`
	}
	_ = json.Unmarshal(event.Payload, &payload)
	return payload.Count
}

func markPairedRelease(events []core.Event, requeue core.Event, kept map[int64]bool) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.TaskID != requeue.TaskID || !event.At.Before(requeue.At) && event.ID >= requeue.ID {
			continue
		}
		if event.Kind == "work_order.expired" || event.Kind == "work_order.released" {
			if factoryActor(event) {
				kept[event.ID] = true
			}
			return
		}
	}
}

func sortEventsOldest(events []core.Event) {
	for i := 1; i < len(events); i++ {
		item := events[i]
		j := i
		for j > 0 && (events[j-1].At.After(item.At) || events[j-1].At.Equal(item.At) && events[j-1].ID > item.ID) {
			events[j] = events[j-1]
			j--
		}
		events[j] = item
	}
}
