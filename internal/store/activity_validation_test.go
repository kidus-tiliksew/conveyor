package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestFoldActivityValidationTablesBindsEveryField(t *testing.T) {
	base := []ActivityTableSignature{{Table: "tasks", Rows: 2, Sum: "17"}, {Table: "work_orders", Rows: 1, Sum: "-4"}}
	fold := FoldActivityValidationTables(base)
	if again := FoldActivityValidationTables(append([]ActivityTableSignature(nil), base...)); again != fold {
		t.Fatal("fold is not deterministic")
	}
	for name, changed := range map[string][]ActivityTableSignature{
		"rows":  {{Table: "tasks", Rows: 3, Sum: "17"}, base[1]},
		"sum":   {{Table: "tasks", Rows: 2, Sum: "18"}, base[1]},
		"table": {{Table: "jobs", Rows: 2, Sum: "17"}, base[1]},
		"order": {base[1], base[0]},
		// Without separators, "1"+"7" and "17"+"" would collide.
		"boundary": {{Table: "tasks", Rows: 2, Sum: "1"}, {Table: "7work_orders", Rows: 1, Sum: "-4"}},
	} {
		if FoldActivityValidationTables(changed) == fold {
			t.Fatalf("changing %s left the signature unchanged", name)
		}
	}
}

func TestActivityReviewLeaseHorizonDecodesLifecycleLeases(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	payload := func(value any) json.RawMessage { body, _ := json.Marshal(value); return body }
	review := core.WorkOrder{ID: "review-1", JobID: "review-1", Stage: core.StageReview, State: core.WorkOrderQueued}
	events := []core.Event{
		{JobID: "review-1", Kind: "work_order.claimed", Payload: payload(map[string]any{"lease_expires_at": now.Add(time.Minute)})},
		{JobID: "review-1", Kind: "work_order.lease_renewed", Payload: payload(map[string]any{"lease_expires_at": now.Add(30 * time.Second)})},
		{JobID: "review-1", Kind: "work_order.lease_renewed", Payload: payload(map[string]any{"lease_expires_at": now.Add(-time.Second)})},
		{JobID: "review-1", Kind: "work_order.lease_renewed", Payload: json.RawMessage(`{"lease_expires_at":"not a time"}`)},
		{JobID: "other", Kind: "work_order.claimed", Payload: payload(map[string]any{"lease_expires_at": now.Add(time.Second)})},
	}
	if got := ActivityReviewLeaseHorizon([]core.WorkOrder{review}, events, now); !got.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("horizon = %s, want the earliest future lease", got)
	}
	for name, order := range map[string]core.WorkOrder{
		"implement order": {ID: "review-1", JobID: "review-1", Stage: core.StageImplement, State: core.WorkOrderQueued},
		"terminal review": {ID: "review-1", JobID: "review-1", Stage: core.StageReview, State: core.WorkOrderCompleted},
	} {
		if got := ActivityReviewLeaseHorizon([]core.WorkOrder{order}, events, now); !got.IsZero() {
			t.Fatalf("%s horizon = %s, want zero", name, got)
		}
	}
	if got := ActivityReviewLeaseHorizon([]core.WorkOrder{review}, events[2:4], now); !got.IsZero() {
		t.Fatalf("past and malformed leases horizon = %s, want zero", got)
	}
}

// TestReviewClaimAndRenewalBelongToTheirJob proves a claim or a lease renewal
// reaches the review folds only through the job it is recorded on. Every
// store records a claim on the job of the order it claims, with that order
// as its payload, and a renewal on the job of the order it renews, with no
// order ID in its payload. So a claim or a renewal on another job, or on
// none, extends no review order whatever its payload names. A release and
// the review kinds still match by payload. A backend's marker read relies on
// this to read only the claims and renewals of the open review orders' jobs
// (MarkerReviewJobs).
func TestReviewClaimAndRenewalBelongToTheirJob(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	lease := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	event := func(id int64, job, kind, payload string) core.Event {
		return core.Event{ID: id, JobID: job, Kind: kind, At: now.Add(-time.Hour).Add(time.Duration(id) * time.Minute), Payload: json.RawMessage(payload)}
	}
	review := core.WorkOrder{ID: "review-1", JobID: "job-review-1", Stage: core.StageReview, State: core.WorkOrderQueued}
	claim := event(1, "job-review-1", "work_order.claimed", `{"id":"review-1","stage":"review","lease_expires_at":"`+lease(-10*time.Minute)+`"}`)
	for _, tc := range []struct {
		name  string
		later core.Event
		// extends says the later event's lease, 30 minutes from now,
		// replaces the lapsed claim's.
		extends bool
	}{
		{"a renewal on the order's job", event(2, "job-review-1", "work_order.lease_renewed", `{"attempt_id":"a","lease_expires_at":"`+lease(30*time.Minute)+`"}`), true},
		{"a renewal on the order's job with another order's id", event(2, "job-review-1", "work_order.lease_renewed", `{"id":"review-9","lease_expires_at":"`+lease(30*time.Minute)+`"}`), true},
		{"a renewal on another job", event(2, "job-verify-1", "work_order.lease_renewed", `{"attempt_id":"a","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"a renewal on another job naming the order by id", event(2, "job-verify-1", "work_order.lease_renewed", `{"id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"a renewal on another job naming the order by review_work_order_id", event(2, "job-verify-1", "work_order.lease_renewed", `{"review_work_order_id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"a renewal on no job naming the order by id", event(2, "", "work_order.lease_renewed", `{"id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"a renewal on no job naming the order by a case-variant id", event(2, "", "work_order.lease_renewed", `{"ID":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"a claim on the order's job", event(2, "job-review-1", "work_order.claimed", `{"id":"review-1","stage":"review","lease_expires_at":"`+lease(30*time.Minute)+`"}`), true},
		{"a claim on the order's job with another order's id", event(2, "job-review-1", "work_order.claimed", `{"id":"review-9","stage":"review","lease_expires_at":"`+lease(30*time.Minute)+`"}`), true},
		{"a claim on another job naming the order by id", event(2, "job-verify-1", "work_order.claimed", `{"id":"review-1","stage":"review","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"a claim on another job naming the order by review_work_order_id", event(2, "job-verify-1", "work_order.claimed", `{"review_work_order_id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"a claim on no job naming the order by id", event(2, "", "work_order.claimed", `{"id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"a claim on no job naming the order by review_work_order_id", event(2, "", "work_order.claimed", `{"review_work_order_id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []core.Event{claim, tc.later}
			horizon := ActivityReviewLeaseHorizon([]core.WorkOrder{review}, events, now)
			diagnostics := ReviewVerdictDiagnostics([]core.WorkOrder{review}, events, now)
			if tc.extends {
				if !horizon.Equal(now.Add(30*time.Minute)) || len(diagnostics) != 0 {
					t.Fatalf("horizon = %s diagnostics = %+v, want the later lease and no expired claim", horizon, diagnostics)
				}
				return
			}
			if !horizon.IsZero() {
				t.Fatalf("horizon = %s, want zero: the event belongs to no open review order", horizon)
			}
			if len(diagnostics) != 1 || diagnostics[0].Status != ReviewExpiredWithoutVerdict || !diagnostics[0].LeaseExpiresAt.Equal(now.Add(-10*time.Minute)) {
				t.Fatalf("diagnostics = %+v, want the claim's lapsed lease", diagnostics)
			}
		})
	}
	// A release and a verdict still match by payload: on another job or on
	// none, one that names the order ends its lapsed claim.
	for name, named := range map[string]core.Event{
		"a release on another job by id":                   event(2, "job-verify-1", "work_order.released", `{"id":"review-1"}`),
		"a release on no job by review_work_order_id":      event(2, "", "work_order.released", `{"review_work_order_id":"review-1"}`),
		"a verdict on another job by review_work_order_id": event(2, "job-verify-1", "review.completed", `{"review_work_order_id":"review-1"}`),
		"an acceptance on no job by review_work_order_id":  event(2, "", "review.accepted", `{"review_work_order_id":"review-1"}`),
	} {
		if got := ReviewVerdictDiagnostics([]core.WorkOrder{review}, []core.Event{claim, named}, now); len(got) != 0 {
			t.Fatalf("%s: diagnostics = %+v, want none: the payload names the order", name, got)
		}
	}
}

// TestMarkerReviewJobs proves which jobs' claims and renewals a marker read
// selects for the review folds: the job of each open review order once, in
// order, and none for an order without a job.
func TestMarkerReviewJobs(t *testing.T) {
	jobs := MarkerReviewJobs([]MarkerReviewOrder{
		{TaskID: "b", ID: "b-1", JobID: "job-b-1"},
		{TaskID: "a", ID: "a-1", JobID: "job-a-1"},
		{TaskID: "b", ID: "b-2", JobID: "job-b-1"},
		{TaskID: "c", ID: "c-1"},
	})
	if len(jobs) != 2 || jobs[0] != "job-b-1" || jobs[1] != "job-a-1" {
		t.Fatalf("review jobs = %v, want job-b-1 and job-a-1", jobs)
	}
	if jobs := MarkerReviewJobs(nil); len(jobs) != 0 {
		t.Fatalf("no open review order gives jobs %v", jobs)
	}
	// The selection is the folds' own rule: a claim or renewal on a selected
	// job matches its order, and one on any other job matches none.
	order := core.WorkOrder{ID: "b-1", JobID: "job-b-1", Stage: core.StageReview, State: core.WorkOrderClaimed}
	for _, kind := range []string{"work_order.claimed", "work_order.lease_renewed"} {
		onJob := core.Event{JobID: "job-b-1", Kind: kind, Payload: json.RawMessage(`{}`)}
		offJob := core.Event{JobID: "job-b-9", Kind: kind, Payload: json.RawMessage(`{"id":"b-1"}`)}
		noJob := core.Event{Kind: kind, Payload: json.RawMessage(`{"review_work_order_id":"b-1"}`)}
		if !reviewLifecycleEventMatches(order, onJob) || reviewLifecycleEventMatches(order, offJob) || reviewLifecycleEventMatches(order, noJob) {
			t.Fatalf("the review folds do not match %s by job alone", kind)
		}
	}
}

func TestPassedWorkOrderDeadlinesCountsEveryTimeField(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	order := core.WorkOrder{
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, QueueDeadline: now.Add(time.Second),
		LeaseExpiresAt: now.Add(-time.Second), NextRetryAt: now.Add(time.Minute), NextAutomaticRecoveryAt: now.Add(-time.Minute),
	}
	if got := passedWorkOrderDeadlines(order, now); got != 4 {
		t.Fatalf("passed = %d, want created, updated (at now), lease, and recovery", got)
	}
	if got := passedWorkOrderDeadlines(order, now.Add(time.Minute)); got != 6 {
		t.Fatalf("passed a minute later = %d, want 6", got)
	}
	if got := passedWorkOrderDeadlines(core.WorkOrder{}, now); got != 0 {
		t.Fatalf("zero times passed = %d", got)
	}
}
