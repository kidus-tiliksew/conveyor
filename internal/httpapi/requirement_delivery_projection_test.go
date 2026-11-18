package httpapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestRequirementDeliveryClassificationMatchesProjectedPayloads proves that
// the delivery event payload projection leaves every classification value
// unchanged: signal IDs, reasons, labels, URLs, and versions
// (req-260811-228be6 AC-1.1 to AC-1.4; component-lineage).
func TestRequirementDeliveryClassificationMatchesProjectedPayloads(t *testing.T) {
	const requirementID = "req-delivery"
	big := strings.Repeat("unrelated-payload-", 2048)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }
	versions := []core.RequirementVersion{
		{RequirementID: requirementID, Version: 1, Confirmed: true, ConfirmedAt: at(0)},
		{RequirementID: requirementID, Version: 2, Confirmed: true, ConfirmedAt: at(20)},
	}
	raw := func(value string) json.RawMessage { return json.RawMessage(value) }
	payload := func(fields map[string]any) json.RawMessage { return core.JSONPayload(fields) }
	attach := func(id int64, minute int, kind string, fields map[string]any) core.Event {
		fields["id"], fields["requirement_title"] = requirementID, big
		return core.Event{ID: id, Kind: kind, At: at(minute), Payload: payload(fields)}
	}
	round := func(id int64, minute int, fields map[string]any) core.Event {
		fields["summary"] = big
		return core.Event{ID: id, Kind: "review.round_completed", At: at(minute), Payload: payload(fields)}
	}
	review := func(verdict, head string) map[string]any {
		return map[string]any{"verdict": verdict, "reviewed_commit_sha": head, "feedback": big, "review_seat": 1}
	}
	merge := func(id int64, minute int, kind string, fields map[string]any) core.Event {
		fields["body"] = big
		return core.Event{ID: id, Kind: kind, At: at(minute), Payload: payload(fields)}
	}
	cases := []struct {
		name            string
		events          []core.Event
		directlyServing bool
		wantAttention   bool
	}{
		{
			name: "historical reconciliation proven by aggregate seat reviews",
			events: []core.Event{
				attach(1, 5, store.TaskContextRequirementAdded, map[string]any{"version": 1}),
				round(2, 25, map[string]any{"verdict": "approve", "reviews": []any{review("approve", "HEAD-A"), review("approve", "head-a")}}),
				merge(3, 30, "merge.reconciled", map[string]any{"head_sha": "head-a", "url": "https://example.test/pull/3", "title": "Reconciled"}),
			},
			directlyServing: true,
			wantAttention:   true,
		},
		{
			name: "historical reconciliation with a dissenting seat",
			events: []core.Event{
				attach(1, 5, store.TaskContextRequirementAdded, map[string]any{"version": 2}),
				round(2, 25, map[string]any{"verdict": "approve", "reviews": []any{review("approve", "head-a"), review("request_changes", "head-a")}}),
				merge(3, 30, "merge.reconciled", map[string]any{"head_sha": "head-a"}),
			},
			directlyServing: true,
			wantAttention:   true,
		},
		{
			name: "explicit factory review provenance",
			events: []core.Event{
				attach(1, 25, store.TaskContextRequirementAdded, map[string]any{"version": 2}),
				merge(2, 30, "merge.reconciled", map[string]any{"head_sha": "head-a", "approved_head_sha": "HEAD-A", "factory_review_validated": true, "task_title": "Task title"}),
				merge(3, 31, "merge.reconciled", map[string]any{"head_sha": "head-a", "reviewed_head_sha": "head-a", "factory_review_validated": false}),
			},
			directlyServing: true,
			wantAttention:   true,
		},
		{
			name: "malformed values stay malformed",
			events: []core.Event{
				attach(1, 25, store.TaskContextRequirementAdded, map[string]any{"version": 2}),
				merge(2, 30, "merge.reconciled", map[string]any{"head_sha": "head-a", "approved_head_sha": "head-a", "factory_review_validated": "yes"}),
				round(3, 31, map[string]any{"verdict": "approve", "reviews": map[string]any{"seat": "approve"}}),
				merge(4, 32, "merge.reconciled", map[string]any{"head_sha": "head-b"}),
				round(5, 33, map[string]any{"verdict": "approve", "reviews": []any{review("approve", "head-c"), "malformed-seat"}}),
				merge(6, 34, "merge.reconciled", map[string]any{"head_sha": "head-c"}),
				round(7, 35, map[string]any{"verdict": "approve", "reviews": []any{nil, review("approve", "head-d")}}),
				merge(8, 36, "merge.reconciled", map[string]any{"head_sha": "head-d"}),
				round(9, 37, map[string]any{"verdict": "approve", "reviews": []any{review("approve", "head-e"), map[string]any{"feedback": big}}}),
				merge(10, 38, "merge.reconciled", map[string]any{"head_sha": "head-e"}),
				round(11, 39, map[string]any{"verdict": 7, "approved_head_sha": "head-f"}),
				merge(12, 40, "merge.reconciled", map[string]any{"head_sha": "head-f"}),
				{ID: 13, Kind: "merge.reconciled", At: at(41), Payload: raw(`["not","an","object"]`)},
				attach(14, 42, store.TaskContextRequirementActive, map[string]any{"version": "3"}),
				merge(15, 43, "merge.confirmed", map[string]any{"url": 9, "title": 4}),
			},
			directlyServing: true,
			wantAttention:   true,
		},
		{
			name: "tied timestamps resolve by event ID",
			events: []core.Event{
				attach(1, 5, store.TaskContextRequirementAdded, map[string]any{"version": 1}),
				merge(2, 30, "merge.confirmed", map[string]any{"url": "https://example.test/pull/2"}),
				attach(3, 30, store.TaskContextRequirementActive, map[string]any{"version": 2}),
				merge(4, 30, "merge.confirmed", map[string]any{"url": "https://example.test/pull/4"}),
			},
			directlyServing: true,
			wantAttention:   true,
		},
		{
			name: "incomplete context stays neutral",
			events: []core.Event{
				merge(1, 30, "merge.reconciled", map[string]any{"head_sha": "head-a"}),
			},
			directlyServing: true,
		},
		{
			name: "attachment activation and removal",
			events: []core.Event{
				attach(1, 5, store.TaskContextRequirementAdded, map[string]any{"version": 1, "unconfirmed": true}),
				merge(2, 10, "merge.confirmed", map[string]any{"title": "  ", "task_title": "Unconfirmed attachment"}),
				attach(3, 11, store.TaskContextRequirementActive, map[string]any{"unconfirmed": false}),
				merge(4, 12, "merge.confirmed", map[string]any{}),
				attach(5, 13, store.TaskContextRequirementRemoved, map[string]any{}),
				merge(6, 30, "merge.confirmed", map[string]any{"title": "After removal"}),
			},
			directlyServing: true,
		},
		{
			name: "related work without serving the requirement",
			events: []core.Event{
				merge(1, 30, "merge.confirmed", map[string]any{"task_title": "Related work"}),
			},
			wantAttention: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			watermark := requirementDeliveryWatermark{At: t0}
			full := classifyRequirementDeliveries("task-delivery", tc.events, versions, requirementID, watermark, tc.directlyServing)
			if len(full) == 0 {
				t.Fatal("case classifies no delivery")
			}
			attention := false
			for _, delivery := range full {
				attention = attention || delivery.NeedsAttention
			}
			if attention != tc.wantAttention {
				t.Fatalf("needs attention = %v, want %v: %#v", attention, tc.wantAttention, full)
			}
			projected := make([]core.Event, len(tc.events))
			nullFilled := make([]core.Event, len(tc.events))
			for i, event := range tc.events {
				projected[i], nullFilled[i] = event, event
				projected[i].Payload = store.ProjectRequirementDeliveryPayload(event.Kind, event.Payload)
				if strings.Contains(string(projected[i].Payload), "unrelated-payload-") {
					t.Fatalf("projection kept an unrelated key: %s", projected[i].Payload)
				}
				nullFilled[i].Payload = withAbsentKeysAsNull(t, event.Kind, projected[i].Payload)
			}
			for name, events := range map[string][]core.Event{"projected": projected, "absent keys as null": nullFilled} {
				got := classifyRequirementDeliveries("task-delivery", events, versions, requirementID, watermark, tc.directlyServing)
				if !reflect.DeepEqual(got, full) {
					t.Fatalf("%s classification = %#v, want %#v", name, got, full)
				}
			}
		})
	}
}

// withAbsentKeysAsNull adds each kept key that a projected payload lacks as
// JSON null, as the SingleStore projection returns it.
func withAbsentKeysAsNull(t *testing.T, kind string, payload json.RawMessage) json.RawMessage {
	t.Helper()
	var fields map[string]any
	if json.Unmarshal(payload, &fields) != nil || fields == nil {
		return payload
	}
	keys := map[string][]string{
		"merge.confirmed":                   {"url", "title", "task_title", "head_sha", "reviewed_head_sha", "approved_head_sha", "factory_review_validated"},
		"merge.reconciled":                  {"url", "title", "task_title", "head_sha", "reviewed_head_sha", "approved_head_sha", "factory_review_validated"},
		"review.round_completed":            {"verdict", "approved_head_sha", "reviews"},
		store.TaskContextRequirementAdded:   {"id", "version", "unconfirmed"},
		store.TaskContextRequirementActive:  {"id", "version", "unconfirmed"},
		store.TaskContextRequirementRemoved: {"id", "version", "unconfirmed"},
	}[kind]
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			fields[key] = nil
		}
	}
	if reviews, ok := fields["reviews"].([]any); ok {
		for _, review := range reviews {
			if object, ok := review.(map[string]any); ok {
				for _, key := range []string{"verdict", "reviewed_commit_sha"} {
					if _, ok := object[key]; !ok {
						object[key] = nil
					}
				}
			}
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
