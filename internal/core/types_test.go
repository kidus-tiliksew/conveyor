package core

import (
	"encoding/json"
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
