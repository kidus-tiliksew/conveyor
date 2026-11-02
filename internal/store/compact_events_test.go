package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestCompactEventPayload(t *testing.T) {
	decode := func(t *testing.T, payload json.RawMessage) map[string]any {
		t.Helper()
		var fields map[string]any
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatalf("decode %s: %v", payload, err)
		}
		return fields
	}
	t.Run("removes both top-level snapshot keys from work-order objects", func(t *testing.T) {
		stored := json.RawMessage(`{"id":"wo-1","attempt_id":"a1","governance_snapshot":{"designs":[{"content":"body"}]},"served_requirement_snapshot":[{"id":"req"}],"nested":{"governance_snapshot":"kept"}}`)
		got := decode(t, CompactEventPayload("work_order.claimed", stored))
		want := map[string]any{"id": "wo-1", "attempt_id": "a1", "nested": map[string]any{"governance_snapshot": "kept"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("compact payload=%v, want %v", got, want)
		}
	})
	t.Run("removes one present key", func(t *testing.T) {
		got := decode(t, CompactEventPayload("work_order.updated", json.RawMessage(`{"id":"wo-1","served_requirement_snapshot":[]}`)))
		if !reflect.DeepEqual(got, map[string]any{"id": "wo-1"}) {
			t.Fatalf("compact payload=%v", got)
		}
	})
	for name, tc := range map[string]struct {
		kind    string
		payload string
	}{
		"object without either key":          {"work_order.claimed", `{"id":"wo-1", "progress":"spaced"}`},
		"another kind":                       {"review.completed", `{"governance_snapshot":"kept","verdict":"approve"}`},
		"array payload":                      {"work_order.updated", `[1,"governance_snapshot"]`},
		"scalar payload":                     {"work_order.updated", `"governance_snapshot"`},
		"malformed payload":                  {"work_order.updated", `{"governance_snapshot":`},
		"empty payload":                      {"work_order.updated", ``},
		"kind sharing only part of a prefix": {"work_order", `{"governance_snapshot":"kept"}`},
	} {
		t.Run(name+" keeps the stored bytes", func(t *testing.T) {
			stored := json.RawMessage(tc.payload)
			if got := CompactEventPayload(tc.kind, stored); !bytes.Equal(got, stored) {
				t.Fatalf("CompactEventPayload(%q, %s)=%s, want the stored bytes", tc.kind, stored, got)
			}
		})
	}
}
