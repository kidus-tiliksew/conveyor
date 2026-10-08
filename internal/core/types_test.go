package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestJobJSONOmitsZeroEndedAt(t *testing.T) {
	pending, err := json.Marshal(Job{ID: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pending), "started_at") {
		t.Fatalf("pending job contains started_at: %s", pending)
	}
	running, err := json.Marshal(Job{ID: "running", StartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(running), "ended_at") {
		t.Fatalf("running job contains ended_at: %s", running)
	}
	endedAt := time.Now().UTC()
	finished, err := json.Marshal(Job{ID: "done", StartedAt: endedAt.Add(-time.Second), EndedAt: endedAt})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(finished), "ended_at") {
		t.Fatalf("finished job omitted ended_at: %s", finished)
	}
}

func TestJobJSONKeepsMissingCostDistinctFromReportedZero(t *testing.T) {
	inProcess, err := json.Marshal(Job{ID: "in-process", Runner: "in-process", TokensIn: 17, TokensOut: 3})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inProcess), "cost_usd") || !strings.Contains(string(inProcess), `"tokens_in":17`) || !strings.Contains(string(inProcess), `"tokens_out":3`) {
		t.Fatalf("in-process job wire contract = %s", inProcess)
	}
	reportedZero := 0.0
	worker, err := json.Marshal(Job{ID: "worker", Runner: "external", CostUSD: &reportedZero})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(worker), `"cost_usd":0`) {
		t.Fatalf("worker job omitted reported cost: %s", worker)
	}
}

// A work order exposes cost only when it retains a historical reported value
// (req-usage-telemetry AC-2.1).
func TestWorkOrderJSONExposesOnlyHistoricalCost(t *testing.T) {
	fresh, err := json.Marshal(WorkOrder{ID: "fresh", TokensIn: 5, UsageReported: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fresh), "cost_usd") || !strings.Contains(string(fresh), `"tokens_in":5`) {
		t.Fatalf("fresh work-order wire contract = %s", fresh)
	}
	if (WorkOrder{}).HistoricalCostUSD() != nil {
		t.Fatal("zero order cost produced a job cost")
	}
	historical, err := json.Marshal(WorkOrder{ID: "historical", CostUSD: 1.25})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(historical), `"cost_usd":1.25`) {
		t.Fatalf("historical work order omitted cost: %s", historical)
	}
	var decoded WorkOrder
	if err = json.Unmarshal(historical, &decoded); err != nil || decoded.CostUSD != 1.25 {
		t.Fatalf("historical decode cost=%v err=%v", decoded.CostUSD, err)
	}
	if cost := decoded.HistoricalCostUSD(); cost == nil || *cost != 1.25 {
		t.Fatalf("historical job cost = %v", cost)
	}
}

func TestQueuedWorkOrderJSONOmitsExecutionAndLeaseClocks(t *testing.T) {
	now := time.Now().UTC()
	data, err := json.Marshal(WorkOrder{ID: "queued", State: WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	for _, field := range []string{"execution_started_at", "execution_deadline", "lease_expires_at", "last_failure_at", "next_retry_at"} {
		if strings.Contains(encoded, field) {
			t.Fatalf("queued work order contains %s: %s", field, encoded)
		}
	}
	if !strings.Contains(encoded, `"claimable":true`) || !strings.Contains(encoded, "queue_deadline") {
		t.Fatalf("queued work order omitted queue state: %s", encoded)
	}
}

func TestWorkOrderActiveForConflictDispatchLifecycleConformance(t *testing.T) {
	tests := []struct {
		name  string
		order WorkOrder
		want  bool
	}{
		{name: "queued", order: WorkOrder{State: WorkOrderQueued}, want: true},
		{name: "queued held task remains active", order: WorkOrder{State: WorkOrderQueued, Claimable: false}, want: true},
		{name: "claimed", order: WorkOrder{State: WorkOrderClaimed}, want: true},
		{name: "expired attempt projection", order: WorkOrder{State: WorkOrderQueued, RetrySuppressed: true, LastAttemptOutcome: WorkOrderOutcomeExpired}},
		{name: "cancelled", order: WorkOrder{State: WorkOrderCancelled}},
		{name: "submitted", order: WorkOrder{State: WorkOrderSubmitted}},
		{name: "completed", order: WorkOrder{State: WorkOrderCompleted}},
		{name: "stale", order: WorkOrder{State: WorkOrderStale}},
		{name: "timed out", order: WorkOrder{State: WorkOrderTimedOut}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := WorkOrderActiveForConflictDispatch(test.order); got != test.want {
				t.Fatalf("active=%t, want %t for %+v", got, test.want, test.order)
			}
		})
	}
}

func TestJSONPayloadUsesStableFallback(t *testing.T) {
	payload := JSONPayload(make(chan int))
	if string(payload) != `{"marshal_error":true}` {
		t.Fatalf("payload = %s", payload)
	}
}

func TestVerificationEvidencePolicyNormalizesAndRejectsIneligibleMedia(t *testing.T) {
	normalized, err := NormalizeVerificationEvidenceContentType(" IMAGE/PNG; charset=binary ", MaxVerificationScreenshotBytes)
	if err != nil || normalized != "image/png" {
		t.Fatalf("normalized=%q err=%v", normalized, err)
	}
	for _, test := range []struct {
		name        string
		contentType string
		size        int64
	}{
		{name: "unsupported", contentType: "image/gif", size: 10},
		{name: "empty", contentType: "image/png", size: 0},
		{name: "oversized screenshot", contentType: "image/jpeg", size: MaxVerificationScreenshotBytes + 1},
		{name: "oversized recording", contentType: "video/mp4", size: MaxVerificationRecordingBytes + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NormalizeVerificationEvidenceContentType(test.contentType, test.size); err == nil {
				t.Fatalf("accepted %s at %d bytes", test.contentType, test.size)
			}
		})
	}
	if (Artifact{Role: ArtifactRoleTaskContext, TaskID: "task", ContentType: "image/png", SizeBytes: 1}).EligibleVerificationEvidence() {
		t.Fatal("wrong role satisfied evidence eligibility")
	}
	if !(Artifact{Role: ArtifactRoleVerificationEvidence, TaskID: "task", ContentType: "video/webm", SizeBytes: 1}).EligibleVerificationEvidence() {
		t.Fatal("eligible recording was rejected")
	}
}

func TestWorkOrderContinuationValidationAndEligibility(t *testing.T) {
	capture, err := NormalizeWorkOrderContinuation(WorkOrderContinuation{
		SessionID: " native-session ", AttemptID: " attempt-1 ", Harness: " codex ", LaunchEnvironment: " worker-a ",
	})
	if err != nil || capture.SessionID != "native-session" || capture.LaunchEnvironment != "worker-a" {
		t.Fatalf("normalized capture=%+v err=%v", capture, err)
	}
	if _, err = NormalizeWorkOrderContinuation(WorkOrderContinuation{
		SessionID: strings.Repeat("x", MaxWorkOrderContinuationSessionIDRunes+1), AttemptID: "attempt-1", Harness: "codex", LaunchEnvironment: "worker-a",
	}); err == nil || !strings.Contains(err.Error(), "continuation_session_id") {
		t.Fatalf("oversized session error=%v", err)
	}

	base := WorkOrder{
		Stage: StageImplement, LastAttemptID: "attempt-1", ContinuationSessionID: "native-session",
		ContinuationAttemptID: "attempt-1", ContinuationHarness: "codex", ContinuationLaunchEnvironment: "worker-a",
	}
	for _, test := range []struct {
		name     string
		mutate   func(*WorkOrder)
		eligible bool
	}{
		{name: "checkpoint", mutate: func(order *WorkOrder) { order.LastFailureMessage = WorkOrderReleaseReasonOperatorCheckpointReached }, eligible: true},
		{name: "plan revision declined recovery", mutate: func(order *WorkOrder) { order.LastFailureMessage = WorkOrderReleaseReasonPlanRevisionRequested }, eligible: true},
		{name: "crash", mutate: func(order *WorkOrder) { order.LastFailureMessage = "harness exited" }},
		{name: "stall", mutate: func(order *WorkOrder) { order.LastFailureMessage = "agent progress stalled" }},
		{name: "preemption", mutate: func(order *WorkOrder) { order.LastFailureMessage = "work order was preempted by an operator" }},
		{name: "claim loss", mutate: func(order *WorkOrder) { order.LastFailureMessage = "claim lease expired" }},
		{name: "ordinary release", mutate: func(order *WorkOrder) { order.LastFailureMessage = "session exited" }},
		{name: "review", mutate: func(order *WorkOrder) {
			order.Stage = StageReview
			order.LastFailureMessage = WorkOrderReleaseReasonOperatorCheckpointReached
		}},
		{name: "post-bounce successor", mutate: func(order *WorkOrder) {
			order.LastAttemptID = "bounce-attempt"
			order.LastFailureMessage = WorkOrderReleaseReasonOperatorCheckpointReached
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			order := base
			test.mutate(&order)
			if got := order.CanResumeContinuation(); got != test.eligible {
				t.Fatalf("eligible=%v want=%v order=%+v", got, test.eligible, order)
			}
			data, marshalErr := json.Marshal(order)
			if marshalErr != nil || !strings.Contains(string(data), fmt.Sprintf(`"continuation_resume_eligible":%t`, test.eligible)) {
				t.Fatalf("json=%s err=%v", data, marshalErr)
			}
			var decoded WorkOrder
			if err := json.Unmarshal(data, &decoded); err != nil || decoded.ContinuationResumeEligible != test.eligible {
				t.Fatalf("decoded eligibility=%v err=%v json=%s", decoded.ContinuationResumeEligible, err, data)
			}
		})
	}
}

// TestWorkOrderAttemptEndingReason pins which persisted rows name an
// attempt's ending (req-260820-221be8 AC-2.1, AC-2.3; DEC-26). Rows whose
// last_failure_message may belong to an earlier attempt never yield a reason.
func TestWorkOrderAttemptEndingReason(t *testing.T) {
	const attempt, older, successor = "att-own", "att-older", "att-next"
	for _, tc := range []struct {
		name   string
		order  WorkOrder
		reason string
		ok     bool
	}{
		{"empty attempt", WorkOrder{State: WorkOrderQueued}, "", false},
		{"still active", WorkOrder{State: WorkOrderClaimed, AttemptID: attempt}, "", false},
		{"active with stale history", WorkOrder{State: WorkOrderClaimed, AttemptID: attempt, LastAttemptID: older, LastAttemptOutcome: WorkOrderOutcomeChildFailure, LastFailureMessage: "older failure"}, "", false},
		{"implementation handoff", WorkOrder{State: WorkOrderSubmitted, AttemptID: attempt, LastAttemptID: older, LastAttemptOutcome: WorkOrderOutcomeStalled, LastFailureMessage: "older stall"}, "work order submitted", true},
		{"stage completion", WorkOrder{State: WorkOrderCompleted, AttemptID: attempt}, "work order completed", true},
		{"submitted then stale", WorkOrder{State: WorkOrderStale, AttemptID: attempt}, "", false},
		{"child failure release", WorkOrder{State: WorkOrderQueued, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeChildFailure, LastFailureMessage: " harness exited with status 1 "}, "harness exited with status 1", true},
		{"stall release", WorkOrder{State: WorkOrderQueued, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeStalled, LastFailureMessage: "no output"}, "no output", true},
		{"checkpoint release", WorkOrder{State: WorkOrderQueued, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeReleased, LastFailureMessage: WorkOrderReleaseReasonOperatorCheckpointReached}, WorkOrderReleaseReasonOperatorCheckpointReached, true},
		{"plan revision release", WorkOrder{State: WorkOrderQueued, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeReleased, LastFailureMessage: WorkOrderReleaseReasonPlanRevisionRequested}, WorkOrderReleaseReasonPlanRevisionRequested, true},
		{"worker shutdown release", WorkOrder{State: WorkOrderQueued, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeCancelled, LastFailureMessage: "worker shutting down"}, "worker shutting down", true},
		{"release without reason", WorkOrder{State: WorkOrderQueued, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeReleased}, WorkOrderOutcomeReleased, true},
		{"preempted", WorkOrder{State: WorkOrderQueued, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomePreempted}, WorkOrderOutcomePreempted, true},
		{"released then successor claimed", WorkOrder{State: WorkOrderClaimed, AttemptID: successor, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeChildFailure, LastFailureMessage: "own failure"}, "own failure", true},
		{"released then successor completed", WorkOrder{State: WorkOrderCompleted, AttemptID: successor, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeStalled, LastFailureMessage: "own stall"}, "own stall", true},
		{"released then stale queue", WorkOrder{State: WorkOrderStale, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeReleased, LastFailureMessage: "own release"}, "own release", true},
		{"successor released", WorkOrder{State: WorkOrderQueued, LastAttemptID: successor, LastAttemptOutcome: WorkOrderOutcomeChildFailure, LastFailureMessage: "successor failure"}, "", false},
		{"lease expired with stale message", WorkOrder{State: WorkOrderQueued, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeExpired, LastFailureMessage: WorkOrderReleaseReasonOperatorCheckpointReached}, "", false},
		{"recovered with kept message", WorkOrder{State: WorkOrderQueued, LastAttemptID: attempt, LastFailureMessage: "older failure"}, "", false},
		{"execution timeout with stale outcome", WorkOrder{State: WorkOrderTimedOut, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeChildFailure, LastFailureMessage: "older failure"}, "", false},
		{"task cancellation with stale message", WorkOrder{State: WorkOrderCancelled, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeCancelled, LastFailureMessage: "older failure"}, "", false},
		{"sibling retirement after release", WorkOrder{State: WorkOrderCancelled, LastAttemptID: attempt, LastAttemptOutcome: WorkOrderOutcomeCancelled, LastFailureMessage: "worker shutting down"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := attempt
			if tc.name == "empty attempt" {
				id = "  "
			}
			reason, ok := tc.order.AttemptEndingReason(" " + id + " ")
			if reason != tc.reason || ok != tc.ok {
				t.Fatalf("AttemptEndingReason=%q,%v want %q,%v", reason, ok, tc.reason, tc.ok)
			}
		})
	}
}
