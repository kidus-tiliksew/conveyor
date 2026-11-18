package store

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// reduceLeaseLifecycleEvents exercises a candidate reduction on canonical
// fixtures only. The backend folds use full histories because arbitrary
// stored payloads cannot be classified by this model.
func reduceLeaseLifecycleEvents(events []core.Event) []core.Event {
	key := func(event core.Event) string {
		var payload struct {
			ID                string `json:"id"`
			ReviewWorkOrderID string `json:"review_work_order_id"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		switch {
		case event.JobID != "":
			return event.JobID
		case payload.ID != "":
			return payload.ID
		}
		return payload.ReviewWorkOrderID
	}
	leaseOK := func(event core.Event) bool {
		if event.Kind == "work_order.claimed" {
			var claimed core.WorkOrder
			return json.Unmarshal(event.Payload, &claimed) == nil && !claimed.LeaseExpiresAt.IsZero()
		}
		var payload struct {
			LeaseExpiresAt time.Time `json:"lease_expires_at"`
		}
		return json.Unmarshal(event.Payload, &payload) == nil && !payload.LeaseExpiresAt.IsZero()
	}
	lifecycle := func(event core.Event) bool {
		if event.Kind == "work_order.lease_renewed" {
			return true
		}
		var payload struct {
			Stage string `json:"stage"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		return event.Kind == "work_order.claimed" && payload.Stage != string(core.StageImplement)
	}
	first, last, lease := map[string]int{}, map[string]int{}, map[string]int{}
	for index, event := range events {
		if !lifecycle(event) || key(event) == "" {
			continue
		}
		k := key(event)
		if event.Kind == "work_order.claimed" {
			if _, seen := first[k]; !seen {
				first[k] = index
			}
			last[k] = index
		}
		if firstIndex, claimed := first[k]; leaseOK(event) && (event.Kind == "work_order.claimed" || (claimed && index > firstIndex)) {
			lease[k] = index
		}
	}
	var out []core.Event
	for index, event := range events {
		if !lifecycle(event) {
			out = append(out, event)
			continue
		}
		k := key(event)
		if k == "" {
			continue
		}
		if f, ok := first[k]; ok && f == index {
			out = append(out, event)
		} else if l, ok := last[k]; ok && l == index {
			out = append(out, event)
		} else if s, ok := lease[k]; ok && s == index {
			out = append(out, event)
		}
	}
	return out
}

func TestLeaseLifecycleReductionKeepsReviewDiagnostics(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 123456789, time.UTC)
	leases := []string{
		"", // absent
		`null`, `"not a time"`, `5`, `"0001-01-01T00:00:00Z"`,
		`"` + now.Add(-time.Hour).Format(time.RFC3339Nano) + `"`,
		`"` + now.Add(-time.Minute).Format(time.RFC3339Nano) + `"`,
		`"` + now.Add(time.Minute).Format(time.RFC3339Nano) + `"`,
		`"` + now.Add(time.Hour).In(time.FixedZone("east", 5*3600+1800)).Format(time.RFC3339Nano) + `"`,
	}
	payload := func(fields ...string) json.RawMessage {
		body := "{"
		for i, field := range fields {
			if i > 0 {
				body += ","
			}
			body += field
		}
		return json.RawMessage(body + "}")
	}
	rng := rand.New(rand.NewPCG(20261117, 2))
	for trial := range 4000 {
		idOnly := rng.IntN(5) == 0
		orders := []core.WorkOrder{
			{ID: "review-0", JobID: "job-0", Stage: core.StageReview, State: core.WorkOrderQueued},
			{ID: "review-1", JobID: "job-1", Stage: core.StageReview, State: core.WorkOrderClaimed},
		}
		if rng.IntN(2) == 0 {
			orders[0].State = core.WorkOrderClaimed
		}
		var events []core.Event
		for index := range 1 + rng.IntN(9) {
			which := rng.IntN(len(orders))
			event := core.Event{
				ID: int64(index + 1), JobID: orders[which].JobID,
				At: now.Add(-3 * time.Hour).Add(time.Duration(index) * time.Minute),
			}
			// A trial names its orders by job ID (the payload id optional) or by
			// payload id alone, never a mix: the backends pair an order's claims
			// and renewals by one identity per row.
			if idOnly {
				event.JobID = ""
			}
			fields := []string{}
			if idOnly || rng.IntN(2) == 0 {
				fields = append(fields, fmt.Sprintf(`"id":%q`, orders[which].ID))
			}
			lease := leases[rng.IntN(len(leases))]
			if lease != "" {
				fields = append(fields, `"lease_expires_at":`+lease)
			}
			switch rng.IntN(7) {
			case 0, 1, 2:
				event.Kind = "work_order.claimed"
				fields = append(fields, `"stage":"review"`)
			case 3, 4:
				event.Kind = "work_order.lease_renewed"
			case 5:
				event.Kind = "work_order.released"
			default:
				event.Kind, fields = "review.completed", append(fields, fmt.Sprintf(`"review_work_order_id":%q`, orders[which].ID))
			}
			event.Payload = payload(fields...)
			events = append(events, event)
		}
		full := ReviewVerdictDiagnostics(orders, events, now)
		reduced := ReviewVerdictDiagnostics(orders, reduceLeaseLifecycleEvents(events), now)
		wantJSON, _ := json.Marshal(full)
		gotJSON, _ := json.Marshal(reduced)
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("trial %d: reduced diagnostics = %s, want %s\nevents: %s", trial, gotJSON, wantJSON, leaseTestJSON(events))
		}
	}
}

func leaseTestJSON(value any) string {
	body, _ := json.Marshal(value)
	return string(body)
}
