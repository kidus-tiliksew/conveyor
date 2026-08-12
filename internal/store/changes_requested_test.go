package store

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestUserRequestChangesHoldUsesLatestImplementClaim(t *testing.T) {
	workerClaim := core.Event{Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{Stage: core.StageImplement, ClaimantID: "worker-1"})}
	runClaim := core.Event{Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{Stage: core.StageImplement, ClaimantID: core.TaskRunClaimantID("user-1")})}
	reviewClaim := core.Event{Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{Stage: core.StageReview, ClaimantID: "reviewer"})}

	if UserRequestChangesHold([]core.Event{workerClaim, reviewClaim}) {
		t.Fatal("worker implementation must not be held")
	}
	if !UserRequestChangesHold([]core.Event{workerClaim, runClaim, reviewClaim}) {
		t.Fatal("latest explicitly run implementation must be held")
	}
}

func TestUserRequestChangesPendingClearsOnNextImplementClaim(t *testing.T) {
	bounce := core.Event{Kind: "pipeline.bounced", Payload: core.JSONPayload(map[string]string{"source": UserRequestChangesSource})}
	reviewClaim := core.Event{Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{Stage: core.StageReview, ClaimantID: "reviewer"})}
	workerClaim := core.Event{Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{Stage: core.StageImplement, ClaimantID: "worker-1"})}

	if !UserRequestChangesPending([]core.Event{bounce, reviewClaim}) {
		t.Fatal("review claims must not clear the bounced implement marker")
	}
	if UserRequestChangesPending([]core.Event{bounce, reviewClaim, workerClaim}) {
		t.Fatal("the next implement claim must clear the marker")
	}
}
