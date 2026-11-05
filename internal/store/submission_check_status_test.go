package store

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

func checkEvent(kind, payload string) core.Event {
	return core.Event{Kind: kind, Payload: []byte(payload)}
}

func TestFoldCheckStatus(t *testing.T) {
	hold := checkEvent(EventCheckHold, `{"head_sha":"a","reason":"failed","checks":[{"name":"e2e","state":"failed"}]}`)
	cleared := checkEvent(EventCheckHoldCleared, `{"head_sha":"a"}`)
	pending := checkEvent(EventCheckPending, `{"head_sha":"a","checks":[{"name":"deploy","state":"pending"}]}`)
	failedRead := checkEvent(EventCheckPending, `{"head_sha":"a","read_failed":true,"read_failure":"The read failed."}`)
	if got := FoldCheckStatus([]core.Event{hold}, ""); got.Hold != nil {
		t.Fatalf("empty head = %+v", got)
	}
	if got := FoldCheckStatus([]core.Event{hold}, "b"); got.Hold != nil {
		t.Fatalf("a hold of another head stood: %+v", got)
	}
	if got := FoldCheckStatus([]core.Event{hold}, "a"); got.Hold == nil || got.Hold.Reason != "failed" || got.Hold.Checks[0].Name != "e2e" {
		t.Fatalf("hold = %+v", got)
	}
	if got := FoldCheckStatus([]core.Event{hold, cleared}, "a"); got.Hold != nil {
		t.Fatalf("cleared hold stood: %+v", got)
	}
	if got := FoldCheckStatus([]core.Event{hold, cleared, hold}, "a"); got.Hold == nil {
		t.Fatal("a later hold did not stand")
	}
	if got := FoldCheckStatus([]core.Event{pending}, "a"); len(got.Waiting) != 1 || got.ReadFailure != "" {
		t.Fatalf("waiting = %+v", got)
	}
	if got := FoldCheckStatus([]core.Event{pending, failedRead}, "a"); len(got.Waiting) != 0 || got.ReadFailure == "" {
		t.Fatalf("failed read = %+v", got)
	}
	exhausted := checkEvent(EventCheckPending, `{"head_sha":"a","read_failed":true,"read_failure":"The read failed.","retries_exhausted":true}`)
	if got := FoldCheckStatus([]core.Event{failedRead, exhausted}, "a"); !got.RetriesExhausted || got.ReadFailure == "" {
		t.Fatalf("exhausted = %+v", got)
	}
	if got := FoldCheckStatus([]core.Event{exhausted, pending}, "a"); got.RetriesExhausted || got.ReadFailure != "" || len(got.Waiting) != 1 {
		t.Fatalf("a later answered read = %+v", got)
	}
	bad := core.Event{Kind: EventCheckHold, Payload: []byte(`not json`)}
	if got := FoldCheckStatus([]core.Event{bad}, "a"); got.Hold != nil {
		t.Fatalf("undecodable = %+v", got)
	}
}

func TestHoldsSilenceFallback(t *testing.T) {
	for reason, want := range map[string]bool{nextaction.CheckHoldFailed: true, nextaction.CheckHoldStuck: true, nextaction.CheckHoldPermission: false} {
		if got := HoldsSilenceFallback(nextaction.CheckStatus{Hold: &nextaction.CheckHold{Reason: reason}}); got != want {
			t.Errorf("%s = %t", reason, got)
		}
	}
	if HoldsSilenceFallback(nextaction.CheckStatus{}) {
		t.Error("no hold excluded a task")
	}
}
