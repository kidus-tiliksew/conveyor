package store

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestUserRequestChangesPendingClearsOnNextImplementClaim(t *testing.T) {
	bounce := core.Event{Kind: "pipeline.bounced", Payload: core.JSONPayload(map[string]string{"source": UserRequestChangesSource})}
	reviewClaim := core.Event{Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{Stage: core.StageReview, ClaimantID: "reviewer"})}
	workerClaim := core.Event{Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{Stage: core.StageImplement, ClaimantID: "worker-1"})}
	runClaim := core.Event{Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{Stage: core.StageImplement, ClaimantID: core.TaskRunClaimantID("user-1")})}

	if !UserRequestChangesPending([]core.Event{bounce, reviewClaim}) {
		t.Fatal("review claims must not clear the bounced implement marker")
	}
	// Whoever claims the re-queued implement order clears the marker: a user
	// bounce re-queues an ordinary eligible order (DEC-74(3)).
	if UserRequestChangesPending([]core.Event{bounce, reviewClaim, workerClaim}) {
		t.Fatal("the next worker implement claim must clear the marker")
	}
	if UserRequestChangesPending([]core.Event{runClaim, bounce, runClaim}) {
		t.Fatal("the next run implement claim must clear the marker")
	}
}
