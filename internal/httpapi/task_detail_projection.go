package httpapi

import (
	"bytes"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// taskDetailProjection changes only the dashboard serialization boundary.
// All operational derivations and execution reads retain complete records
// (component-http-api; req-task-centric-operations-view AC-1.2, AC-2.2).
type taskDetailProjection struct{ reviewItem }

func (detail taskDetailProjection) MarshalJSON() ([]byte, error) {
	events := make([]json.RawMessage, 0, len(detail.Events))
	for _, event := range detail.Events {
		payload, omitted, err := omitAuditSnapshots(event.Payload)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(struct {
			core.Event
			Payload        json.RawMessage `json:"payload"`
			AuditAvailable bool            `json:"audit_available,omitempty"`
		}{event, payload, omitted})
		if err != nil {
			return nil, err
		}
		events = append(events, data)
	}
	orders := make([]json.RawMessage, 0, len(detail.WorkOrders))
	for _, order := range detail.WorkOrders {
		data, err := json.Marshal(order)
		if err != nil {
			return nil, err
		}
		projected, omitted, err := omitAuditSnapshots(data)
		if err != nil {
			return nil, err
		}
		if omitted {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(projected, &fields); err != nil {
				return nil, err
			}
			fields["audit_available"] = json.RawMessage(`true`)
			projected, err = json.Marshal(fields)
			if err != nil {
				return nil, err
			}
		}
		orders = append(orders, projected)
	}
	return json.Marshal(struct {
		reviewItem
		Events     []json.RawMessage `json:"events"`
		WorkOrders []json.RawMessage `json:"work_orders"`
	}{detail.reviewItem, events, orders})
}

// RawMessage preserves integer precision and leaves every scalar untouched.
// Only newly decoded containers are changed; store-owned bytes are read-only.
func omitAuditSnapshots(data json.RawMessage) (json.RawMessage, bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return data, false, nil
	}
	omitted := false
	switch trimmed[0] {
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return nil, false, err
		}
		for key, value := range fields {
			if key == "governance_snapshot" || key == "served_requirement_snapshot" {
				delete(fields, key)
				omitted = true
				continue
			}
			child, changed, err := omitAuditSnapshots(value)
			if err != nil {
				return nil, false, err
			}
			fields[key] = child
			omitted = omitted || changed
		}
		if omitted {
			result, err := json.Marshal(fields)
			return result, true, err
		}
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, false, err
		}
		for i, value := range values {
			child, changed, err := omitAuditSnapshots(value)
			if err != nil {
				return nil, false, err
			}
			values[i] = child
			omitted = omitted || changed
		}
		if omitted {
			result, err := json.Marshal(values)
			return result, true, err
		}
	}
	return data, false, nil
}
