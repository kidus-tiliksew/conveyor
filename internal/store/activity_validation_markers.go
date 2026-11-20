package store

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The activity-marker fold read is written by each backend in its own SQL
// (component-persistence invariant 2). This file holds only the pure Go
// facts both backends share: which payload keys the folds decode, the
// read's inputs and rows, and the decision to read a claim history back
// after a backend's claim reduction (component-web-task-surfaces invariant 19).

// MarkerReviewOrder names an open review order of a page task: one whose
// claim and renewal events the review folds read.
type MarkerReviewOrder struct {
	TaskID string
	ID     string
	JobID  string
}

// MarkerFoldInput is the page an activity-marker fold read covers.
type MarkerFoldInput struct {
	Workspace string
	// Page lists the read's tasks. On PostgreSQL a nil page reads every task
	// of the workspace; SingleStore reads in task chunks and refuses a nil
	// page.
	Page []string
	// ReviewTasks have at least one review order; their review lifecycle
	// kinds feed the review folds.
	ReviewTasks []string
	// LiveReviewTasks have an open (claimed or queued) review order. The
	// review folds read their claims, and of their renewals only those
	// recorded on the job of an open review order (MarkerReviewRenewalJobs).
	LiveReviewTasks []string
	// OpenReviewOrders are the open review orders of LiveReviewTasks.
	OpenReviewOrders []MarkerReviewOrder
	Now              time.Time
	// KeepAllClaims turns the claim reduction off. A backend sets it to read
	// a page again, in one statement, after MarkerClaimBackfill finds that a
	// kept claim does not supersede the claims the reduction left out.
	KeepAllClaims bool
}

// markerForgeKeys are the keys LatestForgeFailure decodes from a forge
// failure kind.
var markerForgeKeys = []string{"review_work_order_id", "work_order_id", "forge_error_category", "last_error", "error", "automatic", "head_sha", "expected_head_sha"}

// MarkerPayloadProjection is one group of event kinds and the payload keys
// every fold that reads those kinds decodes.
type MarkerPayloadProjection struct {
	Kinds []string
	Keys  []string
}

// MarkerReviewRoundKinds are the review-round kinds the review recovery folds
// read: ReviewRecoveryNeeded reads review.round_completed, and
// AutomaticReviewAct, which ReviewRecoveryNeeded and
// InterruptedReviewRecoveryNeeded call, reads review.round_retried and
// review.round_recovered. A marker read selects them for every task with a
// review order (component-web-task-surfaces invariant 19).
var MarkerReviewRoundKinds = []string{"review.round_completed", "review.round_retried", "review.round_recovered"}

// MarkerPayloadProjections lists, per group of event kinds, the keys every fold that
// reads the kinds decodes. Each backend's marker read sends a payload as these
// keys only when its own SQL proves Go matches the keys exactly
// (component-web-task-surfaces invariant 19). The decoders are store.UserRequestChangesPending (bounce),
// store.LatestForgeFailure (forge kinds and the implement submission),
// the review folds' reviewLifecycleEventMatches, ReviewVerdictDiagnostics,
// ActivityReviewLeaseHorizon, completedReviewWorkOrderIDs, and
// SupersededReviewWorkOrders (review lifecycle kinds), and the review
// recovery folds' ReviewRecoveryNeeded (review.round_completed) and
// AutomaticReviewAct (review.round_retried and review.round_recovered). Both
// round decoders keep every key of their payload struct: a key the final
// condition does not use can still fail json.Unmarshal. A claim decodes as a
// whole work order: the database would have to prove every typed field of
// the work order decodes before leaving one out, and that proof made the
// SingleStore statement too costly to compile, so a claim keeps its compact
// payload.
var MarkerPayloadProjections = func() []MarkerPayloadProjection {
	forge := make([]string, 0, len(ForgeFailureEventKinds))
	for _, kind := range ForgeFailureEventKinds {
		if kind != "work_order.updated" {
			forge = append(forge, kind)
		}
	}
	return []MarkerPayloadProjection{
		{[]string{"pipeline.bounced"}, []string{"source"}},
		{[]string{"work_order.updated"}, []string{"id", "stage", "state", "head_sha"}},
		{forge, markerForgeKeys},
		{[]string{"work_order.lease_renewed"}, []string{"id", "review_work_order_id", "lease_expires_at"}},
		{[]string{"work_order.released", "review.accepted"}, []string{"id", "review_work_order_id"}},
		{[]string{"review.completed"}, []string{"id", "review_work_order_id", "review_round"}},
		{[]string{"task.setup.changed"}, []string{"id", "review_work_order_id", "review_transition"}},
		{[]string{"review.round_completed"}, []string{"review_round"}},
		{[]string{"review.round_retried", "review.round_recovered"}, []string{"automatic", "prior_round", "new_round", "review_round"}},
	}
}()

// MarkerReviewRenewalJobs returns the job IDs whose lease renewals the review
// folds read: the jobs of the open review orders, each once, in order. A
// renewal belongs to an order only through its job
// (reviewLifecycleEventMatches), so a renewal recorded on any other job, or
// on none, reaches no review fold and a marker read may leave it out
// (component-web-task-surfaces invariant 19). An order without a job matches
// no renewal.
func MarkerReviewRenewalJobs(orders []MarkerReviewOrder) []string {
	seen := make(map[string]bool, len(orders))
	jobs := make([]string, 0, len(orders))
	for _, order := range orders {
		if order.JobID == "" || seen[order.JobID] {
			continue
		}
		seen[order.JobID] = true
		jobs = append(jobs, order.JobID)
	}
	return jobs
}

var markerSafeOrderID = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// MarkerSafeOrderID reports whether an order ID uses only characters whose
// JSON string text equals the ID between quotes on both backends, so a text
// search for the quoted ID finds every normalized payload that names it.
func MarkerSafeOrderID(id string) bool { return markerSafeOrderID.MatchString(id) }

// MarkerForgePageKinds are the forge failure kinds other than
// work_order.updated, which a marker read selects only for an implement
// submission.
func MarkerForgePageKinds() []string {
	forge := make([]string, 0, len(ForgeFailureEventKinds))
	for _, kind := range ForgeFailureEventKinds {
		if kind != "work_order.updated" {
			forge = append(forge, kind)
		}
	}
	return forge
}

// MarkerEventFoldable reports whether the folds can act on a marker row
// that a backend selected by a necessary text test. A pipeline.bounced row
// counts only when UserRequestChangesPending reads it as a user change
// request, and a work_order.updated row only when LatestForgeFailure reads it
// as an implement order submitted with a head. Both tests call the folds'
// own decoders, so leaving out a row this reports false changes no marker
// (component-web-task-surfaces invariant 19). Every other kind is foldable.
func MarkerEventFoldable(event core.Event) bool {
	switch event.Kind {
	case "pipeline.bounced":
		return userRequestChangesBounce(event)
	case "work_order.updated":
		_, _, ok := implementationSubmission(event)
		return ok
	}
	return true
}

// MarkerClaimPartition names the claims of one task and job.
type MarkerClaimPartition struct {
	TaskID string
	JobID  string
}

// MarkerFoldRow is one row of a backend's marker fold read: an event and,
// on a kept claim, how many claims of its job the backend left out.
type MarkerFoldRow struct {
	Event            core.Event
	Dropped          int64
	DroppedImplement int64
	FutureDropped    int64
}

// MarkerClaimBackfill returns the task and job claim histories whose left-out
// claims the folds may still read. The newest claim that the backend kept
// must decode as a work order, name the implement stage when a left-out
// claim may, and carry a non-zero lease when an open review order runs on
// its job; otherwise an earlier claim can still clear the user's change
// request, or give an open review order its lease. The earliest future
// claim must decode; otherwise a left-out claim's lease can be the review
// lease horizon. The caller reads such a history back whole
// (the backend reads the page again with KeepAllClaims).
func MarkerClaimBackfill(rows []MarkerFoldRow, openReviewOrders []MarkerReviewOrder) []MarkerClaimPartition {
	openJobs := map[MarkerClaimPartition]bool{}
	for _, order := range openReviewOrders {
		openJobs[MarkerClaimPartition{TaskID: order.TaskID, JobID: order.JobID}] = true
	}
	var out []MarkerClaimPartition
	seen := map[MarkerClaimPartition]bool{}
	for _, row := range rows {
		if row.Event.Kind != "work_order.claimed" || (row.Dropped == 0 && row.FutureDropped == 0) {
			continue
		}
		key := MarkerClaimPartition{TaskID: row.Event.TaskID, JobID: row.Event.JobID}
		var claimed core.WorkOrder
		decoded := json.Unmarshal(row.Event.Payload, &claimed) == nil
		// The earliest future claim stands for the later future claims left
		// out: it must decode for its lease to count.
		supersedes := decoded
		if row.Dropped == 0 {
			if !supersedes && !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
			continue
		}
		if row.DroppedImplement > 0 && claimed.Stage != core.StageImplement {
			supersedes = false
		}
		if openJobs[key] && claimed.LeaseExpiresAt.IsZero() {
			supersedes = false
		}
		if !supersedes && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}
