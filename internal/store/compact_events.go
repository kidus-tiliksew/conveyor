package store

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// compactEventKindPrefix selects the work-order lifecycle events whose
// historical payloads embed claim-time snapshot bodies (component-work-orders,
// Compact event projections).
const compactEventKindPrefix = "work_order."

// compactEventOmittedKeys are the top-level work-order payload keys the
// compact read leaves out. WorkOrderEventPayload omits the same two keys
// and names the digest; historical rows may still embed the bodies.
var compactEventOmittedKeys = [...]string{"governance_snapshot", "served_requirement_snapshot"}

// CompactEventPayload returns payload as ListCompactEvents serves it for an
// event of kind: a work_order.* JSON object without its top-level
// governance_snapshot and served_requirement_snapshot keys. It returns the
// stored bytes unchanged for another kind, a payload that is not an object,
// a malformed payload, or an object holding neither key. Nested keys stay
// (component-persistence, Compact task event reads).
func CompactEventPayload(kind string, payload json.RawMessage) json.RawMessage {
	if !strings.HasPrefix(kind, compactEventKindPrefix) {
		return payload
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return payload
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(trimmed, &fields) != nil {
		return payload
	}
	omitted := false
	for _, key := range compactEventOmittedKeys {
		if _, ok := fields[key]; ok {
			delete(fields, key)
			omitted = true
		}
	}
	if !omitted {
		return payload
	}
	compact, err := json.Marshal(fields)
	if err != nil {
		return payload
	}
	return compact
}

// ListCompactEvents is the volatile backend's compact ledger read: the
// ListEvents copy with each payload passed through CompactEventPayload.
func (m *memory) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	events, err := m.ListEvents(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for i := range events {
		events[i].Payload = CompactEventPayload(events[i].Kind, events[i].Payload)
	}
	return events, nil
}
