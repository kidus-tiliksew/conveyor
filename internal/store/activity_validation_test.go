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

// TestReviewRenewalBelongsToItsJob proves a lease renewal reaches the review
// folds only through the job it is recorded on. Every store records a
// renewal on the job of the order it renews and writes no order ID in its
// payload, so a renewal on another job, or on none, extends no review order
// whatever its payload names. A claim still matches by payload, because a
// claim payload is the work order itself. A backend's marker read relies on
// this to read only the renewals of the open review orders' jobs
// (MarkerReviewRenewalJobs).
func TestReviewRenewalBelongsToItsJob(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	lease := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	event := func(id int64, job, kind, payload string) core.Event {
		return core.Event{ID: id, JobID: job, Kind: kind, At: now.Add(-time.Hour).Add(time.Duration(id) * time.Minute), Payload: json.RawMessage(payload)}
	}
	review := core.WorkOrder{ID: "review-1", JobID: "job-review-1", Stage: core.StageReview, State: core.WorkOrderQueued}
	claim := event(1, "job-review-1", "work_order.claimed", `{"id":"review-1","stage":"review","lease_expires_at":"`+lease(-10*time.Minute)+`"}`)
	for _, tc := range []struct {
		name    string
		renewal core.Event
		// extends says the renewal's lease, 30 minutes from now, replaces the
		// lapsed claim's.
		extends bool
	}{
		{"the order's job", event(2, "job-review-1", "work_order.lease_renewed", `{"attempt_id":"a","lease_expires_at":"`+lease(30*time.Minute)+`"}`), true},
		{"the order's job with another order's id", event(2, "job-review-1", "work_order.lease_renewed", `{"id":"review-9","lease_expires_at":"`+lease(30*time.Minute)+`"}`), true},
		{"another job", event(2, "job-verify-1", "work_order.lease_renewed", `{"attempt_id":"a","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"another job naming the order by id", event(2, "job-verify-1", "work_order.lease_renewed", `{"id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"another job naming the order by review_work_order_id", event(2, "job-verify-1", "work_order.lease_renewed", `{"review_work_order_id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"no job naming the order by id", event(2, "", "work_order.lease_renewed", `{"id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
		{"no job naming the order by a case-variant id", event(2, "", "work_order.lease_renewed", `{"ID":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []core.Event{claim, tc.renewal}
			horizon := ActivityReviewLeaseHorizon([]core.WorkOrder{review}, events, now)
			diagnostics := ReviewVerdictDiagnostics([]core.WorkOrder{review}, events, now)
			if tc.extends {
				if !horizon.Equal(now.Add(30*time.Minute)) || len(diagnostics) != 0 {
					t.Fatalf("horizon = %s diagnostics = %+v, want the renewal's lease and no expired claim", horizon, diagnostics)
				}
				return
			}
			if !horizon.IsZero() {
				t.Fatalf("horizon = %s, want zero: the renewal belongs to no open review order", horizon)
			}
			if len(diagnostics) != 1 || diagnostics[0].Status != ReviewExpiredWithoutVerdict || !diagnostics[0].LeaseExpiresAt.Equal(now.Add(-10*time.Minute)) {
				t.Fatalf("diagnostics = %+v, want the claim's lapsed lease", diagnostics)
			}
		})
	}
	// A claim on another job, or on none, still matches by its payload.
	for name, named := range map[string]core.Event{
		"claim by id":                   event(2, "", "work_order.claimed", `{"id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`),
		"claim by review_work_order_id": event(2, "job-verify-1", "work_order.claimed", `{"review_work_order_id":"review-1","lease_expires_at":"`+lease(30*time.Minute)+`"}`),
	} {
		if got := ActivityReviewLeaseHorizon([]core.WorkOrder{review}, []core.Event{claim, named}, now); !got.Equal(now.Add(30 * time.Minute)) {
			t.Fatalf("%s: horizon = %s, want the named claim's lease", name, got)
		}
	}
}

// TestMarkerReviewRenewalJobs proves which jobs' renewals a marker read
// selects: the job of each open review order once, in order, and none for an
// order without a job.
func TestMarkerReviewRenewalJobs(t *testing.T) {
	jobs := MarkerReviewRenewalJobs([]MarkerReviewOrder{
		{TaskID: "b", ID: "b-1", JobID: "job-b-1"},
		{TaskID: "a", ID: "a-1", JobID: "job-a-1"},
		{TaskID: "b", ID: "b-2", JobID: "job-b-1"},
		{TaskID: "c", ID: "c-1"},
	})
	if len(jobs) != 2 || jobs[0] != "job-b-1" || jobs[1] != "job-a-1" {
		t.Fatalf("renewal jobs = %v, want job-b-1 and job-a-1", jobs)
	}
	if jobs := MarkerReviewRenewalJobs(nil); len(jobs) != 0 {
		t.Fatalf("no open review order gives jobs %v", jobs)
	}
	// The selection is the folds' own rule: a renewal on a selected job
	// matches its order, and one on any other job matches none.
	order := core.WorkOrder{ID: "b-1", JobID: "job-b-1", Stage: core.StageReview, State: core.WorkOrderClaimed}
	onJob := core.Event{JobID: "job-b-1", Kind: "work_order.lease_renewed", Payload: json.RawMessage(`{}`)}
	offJob := core.Event{JobID: "job-b-9", Kind: "work_order.lease_renewed", Payload: json.RawMessage(`{"id":"b-1"}`)}
	if !reviewLifecycleEventMatches(order, onJob) || reviewLifecycleEventMatches(order, offJob) {
		t.Fatal("the review folds do not match renewals by job alone")
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
