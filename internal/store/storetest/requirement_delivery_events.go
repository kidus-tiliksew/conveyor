package storetest

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runRequirementDeliveryEvents proves that every backend transfers only the
// payload keys requirement-delivery classification reads, keeps each kept
// value and every review member as stored, and omits a requested task that
// is unknown, outside the workspace, or without delivery events
// (component-lineage; component-task-lifecycle).
func runRequirementDeliveryEvents(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	populated, knownEmpty := newAggregateTask(t, x), newAggregateTask(t, x)
	big := strings.Repeat("unrelated-payload-", 4096)
	at := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	stored := []core.Event{
		{TaskID: populated.ID, Kind: store.TaskContextRequirementAdded, At: at, Payload: core.JSONPayload(map[string]any{
			"id": "req-delivery", "version": 2, "unconfirmed": false, "requirement_title": big,
		})},
		{TaskID: populated.ID, Kind: "review.round_completed", At: at.Add(time.Second), Payload: core.JSONPayload(map[string]any{
			"review_round": 1, "verdict": "approve", "summary": big,
			"reviews": []any{
				map[string]any{"verdict": "approve", "reviewed_commit_sha": "HEAD-A", "feedback": big, "review_seat": 1},
				nil,
				"malformed-seat",
				map[string]any{"feedback": big},
			},
		})},
		{TaskID: populated.ID, Kind: "review.round_completed", At: at.Add(2 * time.Second), Payload: core.JSONPayload(map[string]any{
			"review_round": 2, "verdict": "approve", "approved_head_sha": "head-b", "reviews": map[string]any{"seat": 1},
		})},
		{TaskID: populated.ID, Kind: "merge.reconciled", At: at.Add(3 * time.Second), Payload: core.JSONPayload(map[string]any{
			"head_sha": "head-a", "factory_review_validated": "yes", "result": "merged_outside_conveyor", "diff": big,
		})},
		// Tied timestamps keep event ID order.
		{TaskID: populated.ID, Kind: "merge.confirmed", At: at.Add(4 * time.Second), Payload: core.JSONPayload(map[string]any{
			"url": "https://example.test/pull/1", "title": "Delivery title", "task_title": "Task title",
			"head_sha": "head-a", "reviewed_head_sha": "head-a", "approved_head_sha": "head-a",
			"factory_review_validated": false, "body": big, "nested": map[string]any{"body": big},
		})},
		{TaskID: populated.ID, Kind: store.TaskContextRequirementActive, At: at.Add(4 * time.Second), Payload: core.JSONPayload(map[string]any{
			"id": "req-delivery", "version": "3",
		})},
		{TaskID: populated.ID, Kind: store.TaskContextRequirementRemoved, At: at.Add(5 * time.Second), Payload: core.JSONPayload(map[string]any{
			"id": "req-delivery", "reason": big,
		})},
		{TaskID: populated.ID, Kind: "pull_request.opened", At: at.Add(6 * time.Second), Payload: core.JSONPayload(map[string]any{"body": big})},
		{TaskID: knownEmpty.ID, Kind: "pull_request.opened", At: at, Payload: core.JSONPayload(map[string]any{"body": "not a delivery"})},
	}
	for _, event := range stored {
		requireOK(t, st.AppendEvent(ctx, event))
	}
	want := stored[:7]

	if x.DrainReadBytes != nil {
		x.DrainReadBytes()
	}
	got, err := st.ListRequirementDeliveryEventsForTasks(ctx, []string{populated.ID, knownEmpty.ID, "missing", populated.ID, "MISSING"})
	requireOK(t, err)
	if x.DrainReadBytes != nil {
		// Seven unrelated bodies of about 70 KiB each stay in the database.
		if read := x.DrainReadBytes()["ListRequirementDeliveryEventsForTasks"]; read == 0 || read > 32<<10 {
			t.Fatalf("delivery event read transferred %d bytes, want the projected payloads only", read)
		}
	}
	if len(got) != 1 {
		t.Fatalf("delivery events for tasks %v, want only the populated task", mapKeys(got))
	}
	if _, ok := got[knownEmpty.ID]; ok {
		t.Fatalf("known task without delivery events returned %#v, want it absent", got[knownEmpty.ID])
	}
	events := got[populated.ID]
	if len(events) != len(want) {
		t.Fatalf("delivery events = %d, want %d: %#v", len(events), len(want), events)
	}
	for i, event := range events {
		expected := want[i]
		if event.Kind != expected.Kind || event.TaskID != populated.ID || event.ID == 0 || !event.At.Equal(expected.At) {
			t.Fatalf("delivery event %d = %s %s at %s, want %s at %s", i, event.Kind, event.TaskID, event.At, expected.Kind, expected.At)
		}
		if i > 0 && event.At.Equal(events[i-1].At) && event.ID < events[i-1].ID {
			t.Fatalf("tied delivery events are out of ID order: %#v", events)
		}
		if bytes.Contains(event.Payload, []byte("unrelated-payload-")) {
			t.Fatalf("delivery event %s transferred an unrelated payload key: %s", event.Kind, event.Payload)
		}
		gotPayload := normalizedDeliveryPayload(t, event.Payload)
		wantPayload := normalizedDeliveryPayload(t, store.ProjectRequirementDeliveryPayload(expected.Kind, expected.Payload))
		if !reflect.DeepEqual(gotPayload, wantPayload) {
			t.Fatalf("delivery event %s payload = %s, want %v", event.Kind, event.Payload, wantPayload)
		}
	}

	foreign, err := st.ListRequirementDeliveryEventsForTasks(store.WithWorkspace(ctx, x.Workspace+"-foreign"), []string{populated.ID, knownEmpty.ID})
	requireOK(t, err)
	if len(foreign) != 0 {
		t.Fatalf("foreign workspace delivery events = %#v, want an empty map", foreign)
	}
	none, err := st.ListRequirementDeliveryEventsForTasks(ctx, nil)
	requireOK(t, err)
	if none == nil || len(none) != 0 {
		t.Fatalf("empty task selection = %#v, want an empty map", none)
	}
}

// normalizedDeliveryPayload decodes a payload and drops JSON-null keys of the
// payload object and of each object review element: a durable backend may
// return a key that the stored payload lacks as null, and every
// classification reader treats the two alike.
func normalizedDeliveryPayload(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode delivery payload %s: %v", raw, err)
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return decoded
	}
	dropNullKeys(object)
	if reviews, ok := object["reviews"].([]any); ok {
		for _, review := range reviews {
			if fields, ok := review.(map[string]any); ok {
				dropNullKeys(fields)
			}
		}
	}
	return object
}

func dropNullKeys(object map[string]any) {
	for key, value := range object {
		if value == nil {
			delete(object, key)
		}
	}
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
