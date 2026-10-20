package workorder

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Review round summaries in rework context (req-review-gates-evidence
// AC-10.6; DEC-79; feature-review-round-summary SU-5).

// observingSummaryStore exposes the memory store's context observations
// through the summary fixture's authority wrapper, so freshness comparisons
// run as in production.
type observingSummaryStore struct{ *summaryAuthorityStore }

func (s observingSummaryStore) RecordContextObservation(ctx context.Context, lease taskops.TaskLease, observation core.ContextObservation) (core.ContextObservation, error) {
	return s.Store.(store.ContextObservationStore).RecordContextObservation(ctx, lease, observation)
}

func (s observingSummaryStore) RecordInProcessContextObservation(ctx context.Context, lease taskops.TaskLease, observation core.ContextObservation) (core.ContextObservation, error) {
	return s.Store.(store.ContextObservationStore).RecordInProcessContextObservation(ctx, lease, observation)
}

// newReworkSummaryFixture extends the settled round-1 fixture with the bounce's
// fresh implementation order, claimed live, and a service that can assemble
// its context.
func newReworkSummaryFixture(t *testing.T) (*summaryFixture, string) {
	t.Helper()
	f := newSummaryFixture(t)
	bundle, err := pack.Load("../../pack")
	f.ok(err)
	f.service.Pack = bundle
	f.service.Store = observingSummaryStore{f.st}
	f.st.claimWork["usr_executor"] = true
	rework := f.order(f.taskID, f.taskID+"-implement-2", core.StageImplement, 0, 0)
	f.claim(rework, "rework-session")
	return f, rework
}

func (f *summaryFixture) contextFor(orderID, session string) Context {
	f.t.Helper()
	result, err := f.service.Get(f.ownerContext(), orderID, session)
	f.ok(err)
	return result
}

// ownerContext is the claimant's own user context, which the refresh tool's
// claim validation requires.
func (f *summaryFixture) ownerContext() context.Context {
	return store.WithActor(f.base, store.Actor{ID: store.UserActorID("usr_owner"), Role: core.ActorUser})
}

func (f *summaryFixture) record(requestID, summary string) store.ReviewSummaryResult {
	f.t.Helper()
	input := f.input(requestID)
	input.Summary = summary
	result, err := f.service.RecordReviewSummary(userSummaryContext(f.base, "usr_executor"), input)
	f.ok(err)
	return result
}

func decodeContextSummaries(t *testing.T, raw []json.RawMessage) []store.ReviewSummaryPayload {
	t.Helper()
	out := make([]store.ReviewSummaryPayload, 0, len(raw))
	for _, item := range raw {
		var payload store.ReviewSummaryPayload
		if err := json.Unmarshal(item, &payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, payload)
	}
	return out
}

func rawEqual(a, b []json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestWorkOrderContextReviewSummaries(t *testing.T) {
	t.Run("a bounced round carries its latest summary beside unchanged bounce history", func(t *testing.T) {
		f, rework := newReworkSummaryFixture(t)
		before := f.contextFor(rework, "rework-session")
		if len(before.BounceHistory) != 1 {
			t.Fatalf("bounce_history=%s, want the round-1 bounce", before.BounceHistory)
		}
		if round, ok := eventReviewRound(before.BounceHistory[0]); !ok || round != 1 {
			t.Fatalf("bounce payload %s has no review_round 1", before.BounceHistory[0])
		}
		if !bytes.Contains(before.BounceHistory[0], []byte("seat feedback")) {
			t.Fatalf("bounce payload %s lacks the merged seat feedback", before.BounceHistory[0])
		}
		first := f.record("summary-v1", "Seat 1 found a defect.")
		second := f.record("summary-v2", "Seat 1 found a defect; the fix is narrow.")
		// A summary for a round that never bounced does not attach.
		f.ok(f.st.AppendEvent(f.base, core.Event{TaskID: f.taskID, Kind: store.ReviewSummaryRecordedEvent, Payload: core.JSONPayload(store.ReviewSummaryPayload{ReviewRound: 7, Version: 1, Summary: "unrelated round"})}))
		after := f.contextFor(rework, "rework-session")
		if !rawEqual(after.BounceHistory, before.BounceHistory) {
			t.Fatalf("bounce_history changed:\nbefore=%s\nafter=%s", before.BounceHistory, after.BounceHistory)
		}
		got := decodeContextSummaries(t, after.ReviewSummaries)
		if len(got) != 1 || got[0].ReviewRound != 1 || got[0].Version != second.Version || got[0].Summary != "Seat 1 found a defect; the fix is narrow." {
			t.Fatalf("review_summaries=%s (first version %d, second %d)", after.ReviewSummaries, first.Version, second.Version)
		}
		if !bytes.Equal(after.ReviewSummaries[0], f.eventPayload(second.EventID)) {
			t.Fatalf("summary payload is not the recorded event payload verbatim: %s", after.ReviewSummaries[0])
		}
	})

	t.Run("selection follows (at, id) and the bounce order, not slice order", func(t *testing.T) {
		t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
		ev := func(id int64, seconds int, kind, payload string) core.Event {
			return core.Event{ID: id, Kind: kind, At: t0.Add(time.Duration(seconds) * time.Second), Payload: json.RawMessage(payload)}
		}
		events := []core.Event{
			ev(1, 0, "pipeline.bounced", `{"review_round":2,"count":1}`),
			ev(2, 1, "pipeline.bounced", `{"from":"verify","to":"implement","count":2}`),
			ev(3, 2, "pipeline.bounced", `{"review_round":1,"count":3}`),
			ev(4, 3, "pipeline.bounced", `{"review_round":3,"count":4}`),
			// Round 1: a lower ID committed later at the same instant sorts first,
			// so the higher ID is the latest whatever the slice order.
			ev(12, 10, store.ReviewSummaryRecordedEvent, `{"review_round":1,"version":2,"summary":"r1 latest"}`),
			ev(11, 10, store.ReviewSummaryRecordedEvent, `{"review_round":1,"version":1,"summary":"r1 earlier"}`),
			// Round 2: the later timestamp wins over a higher ID.
			ev(30, 20, store.ReviewSummaryRecordedEvent, `{"review_round":2,"version":1,"summary":"r2 earlier"}`),
			ev(21, 21, store.ReviewSummaryRecordedEvent, `{"review_round":2,"version":2,"summary":"r2 latest"}`),
			// Not attached: a round that never bounced, round 0, and a non-integer round.
			ev(40, 30, store.ReviewSummaryRecordedEvent, `{"review_round":9,"version":1,"summary":"unbounced"}`),
			ev(41, 31, store.ReviewSummaryRecordedEvent, `{"review_round":0,"version":1,"summary":"round zero"}`),
			ev(42, 32, store.ReviewSummaryRecordedEvent, `{"review_round":"1","version":1,"summary":"string round"}`),
		}
		got := reviewSummariesForBounces(events)
		want := []string{
			`{"review_round":2,"version":2,"summary":"r2 latest"}`,
			`{"review_round":1,"version":2,"summary":"r1 latest"}`,
		}
		if len(got) != len(want) {
			t.Fatalf("summaries=%s, want %v", got, want)
		}
		for i := range want {
			if string(got[i]) != want[i] {
				t.Fatalf("summary %d=%s, want %s", i, got[i], want[i])
			}
		}
		if got := reviewSummariesForBounces(events[:4]); got != nil {
			t.Fatalf("no summary events produced %s, want nil", got)
		}
	})
}

func (f *summaryFixture) eventPayload(eventID int64) []byte {
	f.t.Helper()
	for _, event := range f.events(f.taskID) {
		if event.ID == eventID {
			return event.Payload
		}
	}
	f.t.Fatalf("event %d not found", eventID)
	return nil
}

// TestWorkOrderContextReviewSummariesRefresh: context is rebuilt on every
// read, so a summary recorded after the implementer's claim appears on its
// next read without a reclaim, and a newer version replaces it. The
// refresh_work_order_context tool returns only context_freshness, so the
// refresh step proves it succeeds unchanged and the following get_work_order
// read carries the summary.
func TestWorkOrderContextReviewSummariesRefresh(t *testing.T) {
	f, rework := newReworkSummaryFixture(t)
	claimed, err := f.st.GetWorkOrder(f.base, rework)
	f.ok(err)
	initial := f.contextFor(rework, "rework-session")
	if initial.ReviewSummaries != nil {
		t.Fatalf("review_summaries before any summary=%s", initial.ReviewSummaries)
	}
	f.record("after-claim-v1", "Recorded after the claim.")
	refreshed, err := f.service.RefreshContext(f.ownerContext(), rework, "rework-session", initial.ContextFreshness.SelectionRevision)
	f.ok(err)
	if refreshed.ComparisonStatus != "unchanged" {
		t.Fatalf("refresh after a summary=%+v, want unchanged", refreshed)
	}
	next := f.contextFor(rework, "rework-session")
	if got := decodeContextSummaries(t, next.ReviewSummaries); len(got) != 1 || got[0].Version != 1 || got[0].Summary != "Recorded after the claim." {
		t.Fatalf("next read review_summaries=%s", next.ReviewSummaries)
	}
	f.record("after-claim-v2", "A newer version.")
	latest := f.contextFor(rework, "rework-session")
	if got := decodeContextSummaries(t, latest.ReviewSummaries); len(got) != 1 || got[0].Version != 2 || got[0].Summary != "A newer version." {
		t.Fatalf("replaced review_summaries=%s", latest.ReviewSummaries)
	}
	current, err := f.st.GetWorkOrder(f.base, rework)
	f.ok(err)
	if current.State != core.WorkOrderClaimed || current.SessionID != claimed.SessionID || current.AttemptID != claimed.AttemptID {
		t.Fatalf("order after reads=%+v, want the same live claim", current)
	}
}

// legacyContextJSON serializes c through a struct type identical to Context
// without its ReviewSummaries field: the pre-change wire shape.
func legacyContextJSON(t *testing.T, c Context) []byte {
	t.Helper()
	value := reflect.ValueOf(c)
	var fields []reflect.StructField
	var indexes []int
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.Name == "ReviewSummaries" {
			continue
		}
		fields = append(fields, field)
		indexes = append(indexes, i)
	}
	if len(fields) != value.NumField()-1 {
		t.Fatal("Context has no ReviewSummaries field")
	}
	legacy := reflect.New(reflect.StructOf(fields)).Elem()
	for target, source := range indexes {
		legacy.Field(target).Set(value.Field(source))
	}
	data, err := json.Marshal(legacy.Interface())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWorkOrderContextWithoutReviewSummariesUnchanged(t *testing.T) {
	f, rework := newReworkSummaryFixture(t)
	result := f.contextFor(rework, "rework-session")
	if result.ReviewSummaries != nil || len(result.BounceHistory) == 0 {
		t.Fatalf("review_summaries=%s bounce_history=%s", result.ReviewSummaries, result.BounceHistory)
	}
	data, err := json.Marshal(result)
	f.ok(err)
	if bytes.Contains(data, []byte(`"review_summaries"`)) {
		t.Fatal("serialized context names review_summaries without a summary")
	}
	if legacy := legacyContextJSON(t, result); !bytes.Equal(data, legacy) {
		t.Fatalf("context without summaries differs from the pre-change serialization:\n%s\n%s", data, legacy)
	}
}

func TestWorkOrderContextReviewSummaryDoesNotChangeFreshness(t *testing.T) {
	f, rework := newReworkSummaryFixture(t)
	before := f.contextFor(rework, "rework-session").ContextFreshness
	f.record("freshness-v1", "Recording a summary marks nothing stale.")
	after := f.contextFor(rework, "rework-session").ContextFreshness
	if !before.ObservationRecorded || !after.ObservationRecorded {
		t.Fatalf("observations not recorded: before=%+v after=%+v", before, after)
	}
	if after.ComparisonStatus != "unchanged" {
		t.Fatalf("read after a summary compared %q, want unchanged", after.ComparisonStatus)
	}
	if before.SelectionRevision == "" || after.SelectionRevision != before.SelectionRevision {
		t.Fatalf("selection revision before=%q after=%q", before.SelectionRevision, after.SelectionRevision)
	}
	if after.Snapshot.Revision != before.Snapshot.Revision || after.Snapshot.ItemsDigest != before.Snapshot.ItemsDigest || after.Snapshot.AuthorityDigest != before.Snapshot.AuthorityDigest {
		t.Fatalf("snapshot changed: before=%+v after=%+v", before.Snapshot, after.Snapshot)
	}
	refreshed, err := f.service.RefreshContext(f.ownerContext(), rework, "rework-session", before.SelectionRevision)
	f.ok(err)
	if refreshed.ComparisonStatus != "unchanged" || refreshed.Additions.Count != 0 || refreshed.Removals.Count != 0 || refreshed.Changes.Count != 0 {
		t.Fatalf("refresh after a summary=%+v", refreshed)
	}
}
