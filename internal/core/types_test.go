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

func TestJSONPayloadUsesStableFallback(t *testing.T) {
	payload := JSONPayload(make(chan int))
	if string(payload) != `{"marshal_error":true}` {
		t.Fatalf("payload = %s", payload)
	}
}
