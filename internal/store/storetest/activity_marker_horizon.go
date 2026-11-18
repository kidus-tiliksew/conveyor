package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// horizonSeed builds one task's review lifecycle events at fixed offsets from
// a read instant that carries nanoseconds.
type horizonSeed struct {
	now    time.Time
	taskID string
	orders []core.WorkOrder
	other  string
}

// at returns the JSON string of the instant d after the read, with the full
// nanosecond precision RFC3339Nano keeps.
func (h *horizonSeed) at(d time.Duration) string {
	return quoteJSON(h.now.Add(d).Format(time.RFC3339Nano))
}

// zone returns the same instant written with a fixed UTC offset.
func (h *horizonSeed) zone(d time.Duration, offsetSeconds int) string {
	return quoteJSON(h.now.Add(d).In(time.FixedZone("offset", offsetSeconds)).Format(time.RFC3339Nano))
}

func quoteJSON(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}

func (h *horizonSeed) event(order int, kind string, second int, fields ...string) core.Event {
	return core.Event{
		JobID: h.orders[order].JobID, Kind: kind,
		At:      h.now.Add(-3 * time.Hour).Add(time.Duration(second) * time.Second).UTC(),
		Payload: json.RawMessage("{" + strings.Join(fields, ",") + "}"),
	}
}

func withLease(lease string, fields ...string) []string {
	if lease != "" {
		fields = append(fields, `"lease_expires_at":`+lease)
	}
	return fields
}

// claim is the claim event the stores write for a review order.
func (h *horizonSeed) claim(order, second int, lease string) core.Event {
	return h.event(order, "work_order.claimed", second, withLease(lease, fmt.Sprintf(`"id":%q`, h.orders[order].ID), `"stage":"review"`, `"state":"claimed"`)...)
}

// renew is the renewal event the stores write: no payload id, only the job ID.
func (h *horizonSeed) renew(order, second int, lease string) core.Event {
	return h.event(order, "work_order.lease_renewed", second, withLease(lease, `"attempt_id":"attempt-1"`)...)
}

func (h *horizonSeed) release(order, second int) core.Event {
	return h.event(order, "work_order.released", second, `"attempt_id":"attempt-1"`)
}

func (h *horizonSeed) complete(order, second int) core.Event {
	return h.event(order, "review.completed", second, fmt.Sprintf(`"review_work_order_id":%q`, h.orders[order].ID))
}

type horizonOrder struct {
	state core.WorkOrderState
	// job, when set, is a job ID different from the order ID.
	separateJob bool
}

type horizonCase struct {
	name   string
	orders []horizonOrder
	events func(h *horizonSeed) []core.Event
	// horizon, when set, is the horizon the case is built to produce, asserted
	// before the backend is compared with the Go fold. It guards the case
	// against passing because both sides were zero.
	horizon func(h *horizonSeed) time.Time
}

func offset(d time.Duration) func(h *horizonSeed) time.Time {
	return func(h *horizonSeed) time.Time { return h.now.Add(d) }
}

func zeroHorizon(*horizonSeed) time.Time { return time.Time{} }

var oneOpen = []horizonOrder{{state: core.WorkOrderClaimed}}
var oneQueued = []horizonOrder{{state: core.WorkOrderQueued}}

// horizonCases are the differential cases. Each task's markers and review
// lease horizon must equal what store.ReviewVerdictDiagnostics and
// store.ActivityReviewLeaseHorizon fold from the task's complete event list.
func horizonCases() []horizonCase {
	cases := []horizonCase{
		{name: "superseded renewal below the latest", orders: oneOpen, horizon: offset(5 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(10*time.Minute)), h.renew(0, 2, h.at(5*time.Minute)), h.renew(0, 3, h.at(20*time.Minute))}
		}},
		{name: "equal timestamps", orders: oneOpen, horizon: offset(10 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(10*time.Minute)), h.renew(0, 2, h.at(10*time.Minute)), h.renew(0, 3, h.at(10*time.Minute))}
		}},
		{name: "lease equal to now is not after now", orders: oneOpen, horizon: offset(time.Hour), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), h.renew(0, 2, h.at(0))}
		}},
		{name: "lease one nanosecond after now", orders: oneOpen, horizon: offset(time.Nanosecond), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), h.renew(0, 2, h.at(time.Nanosecond))}
		}},
		{name: "lease one nanosecond before now", orders: oneQueued, horizon: zeroHorizon, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(-time.Nanosecond))}
		}},
		{name: "mixed offsets name the same instants", orders: oneOpen, horizon: offset(20 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{
				h.claim(0, 1, h.zone(30*time.Minute, 5*3600+1800)),
				h.renew(0, 2, h.zone(20*time.Minute, -8*3600)),
				h.renew(0, 3, h.zone(25*time.Minute, 14*3600)),
				h.renew(0, 4, h.at(40*time.Minute)),
			}
		}},
		{name: "single digit hour lease", orders: oneOpen, horizon: func(h *horizonSeed) time.Time {
			return time.Date(h.now.Year(), h.now.Month(), h.now.Day()+1, 9, 10, 11, 123456789, time.UTC)
		}, events: func(h *horizonSeed) []core.Event {
			lease := h.now.AddDate(0, 0, 1).Format("2006-01-02") + "T9:10:11.123456789Z"
			return []core.Event{h.claim(0, 1, h.at(48*time.Hour)), h.renew(0, 2, quoteJSON(lease))}
		}},
		{name: "fraction digits truncate at nanoseconds", orders: oneOpen, events: func(h *horizonSeed) []core.Event {
			base := h.now.Truncate(time.Second).Add(time.Hour).Format("2006-01-02T15:04:05")
			frac := func(digits string) string { return `"` + base + "." + digits + `Z"` }
			return []core.Event{h.claim(0, 1, frac("5")), h.renew(0, 2, frac("500000000")), h.renew(0, 3, frac("5000000001")), h.renew(0, 4, frac("499999999")), h.renew(0, 5, frac("500000000000"))}
		}},
		{name: "far future leap day", orders: oneOpen, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, `"2400-02-29T00:00:00Z"`), h.renew(0, 2, `"9999-12-31T23:59:59.999999999Z"`)}
		}},
		{name: "far future invalid leap day", orders: oneOpen, horizon: zeroHorizon, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, `"2100-02-29T00:00:00Z"`), h.renew(0, 2, `"2026-02-29T00:00:00Z"`)}
		}},
		{name: "negative zero offset", orders: oneOpen, horizon: offset(10 * time.Minute), events: func(h *horizonSeed) []core.Event {
			utc := h.now.Add(10*time.Minute).Format("2006-01-02T15:04:05.999999999") + "-00:00"
			return []core.Event{h.claim(0, 1, h.at(3*time.Hour)), h.renew(0, 2, quoteJSON(utc))}
		}},
		{name: "renewal and claim history across a release", orders: oneOpen, horizon: offset(5 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(10*time.Minute)), h.renew(0, 2, h.at(5*time.Minute)), h.release(0, 3), h.claim(0, 4, h.at(30*time.Minute)), h.renew(0, 5, h.at(25*time.Minute))}
		}},
		{name: "latest claim without a lease keeps the earlier lease", orders: oneQueued, horizon: zeroHorizon, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(-10*time.Minute)), h.renew(0, 2, h.at(-5*time.Minute)), h.claim(0, 3, "")}
		}},
		{name: "latest claim with the zero time keeps the earlier lease", orders: oneOpen, horizon: offset(time.Hour), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), h.claim(0, 2, `"0001-01-01T05:30:00+05:30"`)}
		}},
		{name: "latest renewal malformed keeps the previous renewal", orders: oneQueued, horizon: zeroHorizon, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(-30*time.Minute)), h.renew(0, 2, h.at(-20*time.Minute)), h.renew(0, 3, `"not a time"`), h.renew(0, 4, "")}
		}},
		{name: "renewal before the first claim", orders: oneOpen, horizon: offset(2 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.renew(0, 1, h.at(2*time.Minute)), h.claim(0, 2, h.at(time.Hour))}
		}},
		{name: "late events append out of order", orders: oneOpen, horizon: offset(10 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 2, h.at(30*time.Minute)), h.renew(0, 3, h.at(20*time.Minute)), h.renew(0, 1, h.at(10*time.Minute))}
		}},
		{name: "same event time orders by id", orders: oneOpen, horizon: offset(5 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), h.renew(0, 1, h.at(5*time.Minute)), h.renew(0, 1, h.at(20*time.Minute))}
		}},
		{name: "release after the lapsed claim", orders: oneQueued, horizon: zeroHorizon, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(-10*time.Minute)), h.release(0, 2)}
		}},
		{name: "verdict after the lapsed claim", orders: oneQueued, horizon: zeroHorizon, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(-10*time.Minute)), h.complete(0, 2)}
		}},
		{name: "only open orders count", orders: []horizonOrder{{state: core.WorkOrderQueued}, {state: core.WorkOrderClaimed}, {state: core.WorkOrderSubmitted}}, horizon: offset(35 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(-time.Minute)), h.claim(1, 2, h.at(40*time.Minute)), h.renew(1, 3, h.at(35*time.Minute)), h.claim(2, 4, h.at(time.Minute))}
		}},
		{name: "order id differs from job id", orders: []horizonOrder{{state: core.WorkOrderClaimed, separateJob: true}}, horizon: offset(8 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(10*time.Minute)), h.renew(0, 2, h.at(8*time.Minute))}
		}},
		{name: "events name the order by payload id or review_work_order_id", orders: oneOpen, horizon: offset(2 * time.Minute), events: func(h *horizonSeed) []core.Event {
			byID := core.Event{Kind: "work_order.lease_renewed", At: h.now.Add(-3 * time.Hour).Add(3 * time.Second).UTC(),
				Payload: json.RawMessage(fmt.Sprintf(`{"id":%q,"lease_expires_at":%s}`, h.orders[0].ID, h.at(2*time.Minute)))}
			byReview := core.Event{JobID: h.other, Kind: "work_order.lease_renewed", At: h.now.Add(-3 * time.Hour).Add(4 * time.Second).UTC(),
				Payload: json.RawMessage(fmt.Sprintf(`{"review_work_order_id":%q,"lease_expires_at":%s}`, h.orders[0].ID, h.at(3*time.Minute)))}
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), byID, byReview}
		}},
		{name: "events of another job or a non-string id do not match", orders: oneOpen, horizon: offset(time.Hour), events: func(h *horizonSeed) []core.Event {
			otherJob := core.Event{JobID: h.other, Kind: "work_order.lease_renewed", At: h.now.Add(-3 * time.Hour).Add(2 * time.Second).UTC(),
				Payload: json.RawMessage(`{"lease_expires_at":` + h.at(time.Minute) + `}`)}
			numericID := core.Event{JobID: h.other, Kind: "work_order.claimed", At: h.now.Add(-3 * time.Hour).Add(3 * time.Second).UTC(),
				Payload: json.RawMessage(`{"id":5,"lease_expires_at":` + h.at(2*time.Minute) + `}`)}
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), otherJob, numericID}
		}},
		{name: "a numeric payload id still matches by job id", orders: oneOpen, horizon: offset(2 * time.Minute), events: func(h *horizonSeed) []core.Event {
			numericID := h.event(0, "work_order.lease_renewed", 2, `"id":5`, `"lease_expires_at":`+h.at(2*time.Minute))
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), numericID}
		}},
		{name: "case insensitive lease key", orders: oneOpen, horizon: offset(time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), h.event(0, "work_order.lease_renewed", 2, `"LEASE_EXPIRES_AT":`+h.at(time.Minute))}
		}},
		{name: "claim invalid unrelated field", orders: oneOpen, horizon: offset(time.Hour), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), h.event(0, "work_order.claimed", 2, `"id":5`, `"lease_expires_at":`+h.at(time.Minute))}
		}},
		{name: "renewal ignores invalid unrelated claim field", orders: oneOpen, horizon: offset(time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), h.event(0, "work_order.lease_renewed", 2, `"stage":7`, `"lease_expires_at":`+h.at(time.Minute))}
		}},
		{name: "payload id matches despite different job", orders: oneOpen, horizon: offset(time.Minute), events: func(h *horizonSeed) []core.Event {
			e := h.event(0, "work_order.lease_renewed", 2, `"ID":`+quoteJSON(h.orders[0].ID), `"lease_expires_at":`+h.at(time.Minute))
			e.JobID = h.other
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), e}
		}},
		{name: "matching job overrides conflicting payload id", orders: oneOpen, horizon: offset(time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), h.event(0, "work_order.lease_renewed", 2, `"id":"other"`, `"lease_expires_at":`+h.at(time.Minute))}
		}},
		{name: "escaped lease string uses stored Go representation", orders: oneOpen, events: func(h *horizonSeed) []core.Event {
			lease := strings.Replace(h.at(time.Minute), "T", `\u0054`, 1)
			return []core.Event{h.claim(0, 1, h.at(time.Hour)), h.renew(0, 2, lease)}
		}},
		// The claim reduction keeps one claim per order only where the folds
		// read nothing from the older claims.
		{name: "an older claim holds the earliest future lease", orders: oneOpen, horizon: offset(10 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(10*time.Minute)), h.claim(0, 2, h.at(time.Hour))}
		}},
		{name: "a newest claim that does not decode keeps the older lease", orders: oneQueued, horizon: zeroHorizon, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(-10*time.Minute)), h.event(0, "work_order.claimed", 2, `"id":5`, `"lease_expires_at":`+h.at(-5*time.Minute))}
		}},
		{name: "an older claim of one order names another open order", orders: []horizonOrder{{state: core.WorkOrderClaimed}, {state: core.WorkOrderQueued}}, horizon: offset(time.Hour), events: func(h *horizonSeed) []core.Event {
			return []core.Event{
				h.event(0, "work_order.claimed", 1, fmt.Sprintf(`"review_work_order_id":%q`, h.orders[1].ID), `"stage":"review"`, `"lease_expires_at":`+h.at(-10*time.Minute)),
				h.claim(0, 2, h.at(time.Hour)),
			}
		}},
		{name: "claims at one instant order by id", orders: oneQueued, horizon: zeroHorizon, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(-20*time.Minute)), h.claim(0, 1, h.at(-10*time.Minute))}
		}},
		{name: "a renewal between older claims and a newest claim without a lease", orders: oneQueued, horizon: zeroHorizon, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(-40*time.Minute)), h.claim(0, 2, h.at(-30*time.Minute)), h.renew(0, 3, h.at(-20*time.Minute)), h.claim(0, 4, "")}
		}},
		// A projected claim keeps only the keys the folds read, so a field the
		// folds never read must still decode for the claim's lease to count.
		{name: "a claim with an invalid unread time does not decode", orders: oneOpen, horizon: offset(time.Hour), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)),
				h.event(0, "work_order.claimed", 2, fmt.Sprintf(`"id":%q`, h.orders[0].ID), `"stage":"review"`, `"created_at":"2027-02-29T00:00:00Z"`, `"lease_expires_at":`+h.at(time.Minute))}
		}},
		{name: "a claim with a leap day unread time decodes", orders: oneOpen, horizon: offset(time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)),
				h.event(0, "work_order.claimed", 2, fmt.Sprintf(`"id":%q`, h.orders[0].ID), `"stage":"review"`, `"created_at":"2028-02-29T23:59:59.5+05:30"`, `"tokens_in":7`, `"claimable":true`, `"lease_expires_at":`+h.at(time.Minute))}
		}},
		{name: "older claims keep only the earliest future lease", orders: oneOpen, horizon: offset(10 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(20*time.Minute)), h.claim(0, 2, h.at(10*time.Minute)), h.claim(0, 3, h.at(30*time.Minute)), h.claim(0, 4, h.at(time.Hour))}
		}},
		{name: "an undecodable claim with an earlier future lease", orders: oneOpen, horizon: offset(10 * time.Minute), events: func(h *horizonSeed) []core.Event {
			return []core.Event{
				h.event(0, "work_order.claimed", 1, fmt.Sprintf(`"id":%q`, h.orders[0].ID), `"attempt_id":5`, `"lease_expires_at":`+h.at(5*time.Minute)),
				h.claim(0, 2, h.at(10*time.Minute)), h.claim(0, 3, h.at(20*time.Minute)), h.claim(0, 4, h.at(time.Hour)),
			}
		}},
		{name: "a claim with a fractional count does not decode", orders: oneOpen, horizon: offset(time.Hour), events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(time.Hour)),
				h.event(0, "work_order.claimed", 2, fmt.Sprintf(`"id":%q`, h.orders[0].ID), `"tokens_in":7.5`, `"lease_expires_at":`+h.at(time.Minute))}
		}},
	}
	// Malformed and unusual lease values never become the horizon: the claim's
	// lease is the only one Go decodes, so the horizon stays at the claim's.
	ok := time.Date(2026, 11, 17, 12, 10, 0, 0, time.UTC)
	text := ok.Format(time.RFC3339Nano)
	malformed := map[string]string{
		"not a time": `"not a time"`, "empty": `""`, "null": `null`, "number": `5`, "bool": `true`, "object": `{}`, "array": `[]`,
		"lowercase t and z": quoteJSON(strings.ToLower(text)), "space separator": quoteJSON(strings.Replace(text, "T", " ", 1)),
		"comma fraction": quoteJSON("2026-11-17T12:10:00,5Z"), "dot without digits": quoteJSON("2026-11-17T12:10:00.Z"),
		"missing zone": quoteJSON("2026-11-17T12:10:00"), "trailing space": quoteJSON(text + " "), "leading space": quoteJSON(" " + text),
		"trailing newline": quoteJSON(text + "\n"), "hour 24": quoteJSON("2026-11-17T24:00:00Z"), "minute 60": quoteJSON("2026-11-17T12:60:00Z"),
		"second 60": quoteJSON("2026-11-17T12:10:60Z"), "month 13": quoteJSON("2026-13-17T12:10:00Z"), "month 00": quoteJSON("2026-00-17T12:10:00Z"),
		"day 00": quoteJSON("2026-11-00T12:10:00Z"), "february 30": quoteJSON("2027-02-30T12:10:00Z"), "april 31": quoteJSON("2027-04-31T12:10:00Z"),
		"zone +24:00": quoteJSON("2026-11-17T12:10:00+24:00"), "zone minute 60": quoteJSON("2026-11-17T12:10:00+05:60"), "zone without colon": quoteJSON("2026-11-17T12:10:00+0530"),
		"doubled Z": quoteJSON(text + "Z"), "five digit year": quoteJSON("12026-11-17T12:10:00Z"), "short fields": quoteJSON("2026-11-17T12:10:0Z"),
		"date only": quoteJSON("2026-11-17"), "zero time": quoteJSON("0001-01-01T00:00:00Z"), "zero instant at an offset": quoteJSON("0001-01-01T05:30:00+05:30"),
	}
	for name, lease := range malformed {
		cases = append(cases, horizonCase{name: "malformed lease: " + name, orders: oneOpen, events: func(h *horizonSeed) []core.Event {
			return []core.Event{h.claim(0, 1, h.at(3*time.Hour)), h.renew(0, 2, lease), h.renew(0, 3, "")}
		}})
	}
	return cases
}

// RunActivityMarkerHorizonConformance proves the review lease horizon and the
// review markers a backend reads equal the Go folds over each task's complete
// event history, for the lease shapes the database must decode itself: equal
// instants, instants at, just before, and just after the read, mixed zone
// offsets, nanosecond precision, malformed values, several claims, renewals
// before the first claim, late events, and orders that are no longer open.
// The read instant carries nanoseconds so a database that rounds to
// microseconds fails.
func RunActivityMarkerHorizonConformance(t *testing.T, x Fixture) {
	st, ws := x.Backend, x.Workspace
	ctx := store.WithActor(x.Context, store.SystemActor())
	now := time.Now().UTC().Truncate(time.Second).Add(123456789 * time.Nanosecond)
	for _, c := range horizonCases() {
		t.Run(c.name, func(t *testing.T) {
			h := seedHorizonTask(t, ctx, st, ws, now, c)
			events, err := st.ListEvents(ctx, h.taskID)
			requireOK(t, err)
			inputs, err := st.ReadActivityInputs(ctx, []string{h.taskID}, now)
			requireOK(t, err)
			wantHorizon := store.ActivityReviewLeaseHorizon(inputs.Orders, events, now)
			if c.horizon != nil && !wantHorizon.Equal(c.horizon(h)) {
				t.Fatalf("case is built for horizon %s but the Go fold reads %s", c.horizon(h), wantHorizon)
			}
			if !inputs.ValidUntil.Equal(wantHorizon) {
				t.Fatalf("review lease horizon = %s, want %s", inputs.ValidUntil, wantHorizon)
			}
			if len(inputs.Markers) != 1 {
				t.Fatalf("markers = %+v, want one", inputs.Markers)
			}
			task, err := st.GetTask(ctx, h.taskID)
			requireOK(t, err)
			got, want := inputs.Markers[0], store.ActivityMarker{
				ForgeFailure:              store.LatestForgeFailure(events),
				ReviewDiagnostics:         store.ReviewVerdictDiagnostics(inputs.Orders, events, now),
				ReviewRecovery:            store.ReviewRecoveryNeeded(inputs.Orders, events),
				InterruptedReviewRecovery: store.InterruptedReviewRecoveryNeeded(task, store.CurrentReviewOrders(inputs.Orders, events), events),
				Stalled:                   store.StalledTask(inputs.Orders),
				UserChangesRequested:      store.UserRequestChangesPending(events),
			}
			// A malformed lease payload is kept exactly as written, but Go can
			// parse a "+24:00" zone that RFC3339 marshalling refuses. Compare
			// the diagnostics by instant in UTC so the intentionally malformed
			// payload still exercises the fold without breaking the comparison.
			utcDiagnostics := func(in []store.ReviewVerdictDiagnostic) []store.ReviewVerdictDiagnostic {
				out := make([]store.ReviewVerdictDiagnostic, len(in))
				copy(out, in)
				for index := range out {
					out[index].ClaimedAt = out[index].ClaimedAt.UTC()
					out[index].LeaseExpiresAt = out[index].LeaseExpiresAt.UTC()
				}
				return out
			}
			requireSameJSON(t, "review diagnostics", utcDiagnostics(got.ReviewDiagnostics), utcDiagnostics(want.ReviewDiagnostics))
			requireSameJSON(t, "review recovery", got.ReviewRecovery, want.ReviewRecovery)
			requireSameJSON(t, "interrupted review recovery", got.InterruptedReviewRecovery, want.InterruptedReviewRecovery)
			requireSameJSON(t, "forge failure", got.ForgeFailure, want.ForgeFailure)
			requireSameJSON(t, "stalled", got.Stalled, want.Stalled)
			if got.UserChangesRequested != want.UserChangesRequested {
				t.Fatalf("user changes requested = %t, want %t", got.UserChangesRequested, want.UserChangesRequested)
			}
		})
	}
	t.Run("long claim history matches Go", func(t *testing.T) { runHorizonLongHistory(t, x, ctx, now) })
}

func seedHorizonTask(t *testing.T, ctx context.Context, st store.Backend, workspace string, now time.Time, c horizonCase) *horizonSeed {
	t.Helper()
	taskID := core.NewTaskID()
	requireOK(t, st.CreateTask(ctx, core.Task{
		ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-" + taskID, Title: "Marker horizon", Body: "marker horizon",
		State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now.Add(-4 * time.Hour),
	}))
	h := &horizonSeed{now: now, taskID: taskID, other: taskID + "-other-job"}
	requireOK(t, st.CreateJob(ctx, core.Job{ID: h.other, TaskID: taskID, Stage: core.StageReview, State: core.JobPending}))
	for index, spec := range c.orders {
		orderID := fmt.Sprintf("%s-review-%d", taskID, index)
		jobID := orderID
		if spec.separateJob {
			jobID = "job-" + orderID
		}
		requireOK(t, st.CreateJob(ctx, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageReview, State: core.JobPending}))
		// The create command admits one state, queued; a claimed order is
		// seeded queued and still counts as open in the lease horizon fold.
		// A case that keeps the order out of the open set retires it through
		// the legal queue edge instead of storing a state creation refuses.
		order := core.WorkOrder{
			ID: orderID, TaskID: taskID, JobID: jobID, Stage: core.StageReview,
			QueueEnteredAt: now.Add(-3 * time.Hour), QueueDeadline: now.Add(24 * time.Hour),
		}
		requireOK(t, CreateWorkOrder(ctx, st, order))
		switch spec.state {
		case "", core.WorkOrderQueued, core.WorkOrderClaimed:
		default:
			order.State = core.WorkOrderTimedOut
			requireOK(t, UpdateWorkOrder(ctx, st, order, core.WorkOrderCmdTimeout))
		}
		h.orders = append(h.orders, order)
	}
	for _, event := range c.events(h) {
		event.TaskID = taskID
		requireOK(t, st.AppendEvent(ctx, event))
	}
	return h
}

func runHorizonLongHistory(t *testing.T, x Fixture, ctx context.Context, now time.Time) {
	st, ws := x.Backend, x.Workspace
	history := func(open bool, claims int) string {
		state := core.WorkOrderSubmitted
		if open {
			state = core.WorkOrderClaimed
		}
		h := seedHorizonTask(t, ctx, st, ws, now, horizonCase{orders: []horizonOrder{{state: state}}, events: func(h *horizonSeed) []core.Event {
			var events []core.Event
			for i := range claims {
				events = append(events,
					h.claim(0, 2*i, h.at(time.Duration(i+10)*time.Minute)),
					h.renew(0, 2*i+1, h.at(time.Duration(i+20)*time.Minute)))
			}
			return events
		}})
		return h.taskID
	}
	for _, taskID := range []string{history(true, 3), history(true, 400), history(false, 400)} {
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		inputs, err := st.ReadActivityInputs(ctx, []string{taskID}, now)
		requireOK(t, err)
		want := store.ActivityReviewLeaseHorizon(inputs.Orders, events, now)
		if !inputs.ValidUntil.Equal(want) {
			t.Fatalf("task %s horizon = %s, want %s", taskID, inputs.ValidUntil, want)
		}
	}
}
