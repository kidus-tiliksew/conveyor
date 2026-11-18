package store

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
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
// (component-task-lifecycle invariant 51).
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

// IsTaskContextEventKind reports whether kind is one the context fold reads.
func IsTaskContextEventKind(kind string) bool {
	for _, candidate := range TaskContextEventKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// ListTaskContextEvents is the volatile backend's context read: the compact
// ledger copy narrowed to the kinds TaskContextFromEvents folds.
func (m *memory) ListTaskContextEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	events, err := m.ListCompactEvents(ctx, taskID)
	if err != nil {
		return nil, err
	}
	contextEvents := make([]core.Event, 0, len(events))
	for _, event := range events {
		if IsTaskContextEventKind(event.Kind) {
			contextEvents = append(contextEvents, event)
		}
	}
	return contextEvents, nil
}

// ListTaskDesignContextEvents is the volatile backend's design provenance
// read. It selects the task's design attachment and removal events under the
// read lock and sorts only those, as the durable backends' kind predicate
// does, so it never copies, sorts, or compacts the rest of the timeline. The
// two design kinds carry no snapshot body, so no payload needs compaction.
// Another workspace's task reads as a task without events, as the durable
// backends' workspace predicate answers.
func (m *memory) ListTaskDesignContextEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	m.mu.RLock()
	if selected, present := WorkspaceFromContext(ctx); present && selected != "" && m.tasks[taskID].Workspace != selected {
		m.mu.RUnlock()
		return []core.Event{}, nil
	}
	design := []core.Event{}
	for _, event := range m.events[taskID] {
		if event.Kind == TaskContextDesignAdded || event.Kind == TaskContextDesignRemoved {
			design = append(design, event)
		}
	}
	m.mu.RUnlock()
	sort.Slice(design, func(i, j int) bool {
		if design[i].At.Equal(design[j].At) {
			return design[i].ID < design[j].ID
		}
		return design[i].At.Before(design[j].At)
	})
	return design, nil
}
