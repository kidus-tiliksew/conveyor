package postgres

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// legacyMirror appends inside whatever executor the caller passes; it never
// opens its own transaction, so it needs no pool.
var legacyMirror = pglog.New(nil)

// telemetryKinds are liveness signals, not facts about an entity: a lease
// belongs in state, not in the log. They stay in the legacy events table and
// never reach the log, from the mirror or from the import. Their columns
// are excluded from snapshot hashes for the same reason. Operator decision,
// 2026-09-04: together they were 84% of the demo workspace's log.
var telemetryKinds = map[string]bool{
	"worker.heartbeat":         true,
	"work_order.lease_renewed": true,
}

// IsTelemetryKind reports whether kind is excluded from the log.
func IsTelemetryKind(kind string) bool { return telemetryKinds[kind] }

// telemetryKindList is the exclusion in a form SQL can take.
func telemetryKindList() []string {
	out := make([]string, 0, len(telemetryKinds))
	for kind := range telemetryKinds {
		out = append(out, kind)
	}
	return out
}

// mirrorLegacyEvent appends a just-inserted legacy events row to the event
// log using the same executor, so the row and its log entry commit or roll
// back together. Legacy writers do not track stream versions, so the append
// is ExpectAny; the per-stream serialization they already rely on (advisory
// locks) keeps concurrent mirrors ordered. When the executor is a store
// transaction, the stream is registered so the entity's final rows are
// recorded at commit (see eventlog_bridge.go).
func mirrorLegacyEvent(ctx context.Context, exec db.DBTX, inserted db.Event) error {
	if telemetryKinds[inserted.Kind] {
		return nil
	}
	stream := legacyStream(inserted)
	_, err := legacyMirror.AppendWith(ctx, exec, inserted.WorkspaceID, stream, eventlog.ExpectAny, []eventlog.NewEvent{{
		Kind:      inserted.Kind,
		ActorID:   inserted.ActorID,
		ActorRole: inserted.ActorRole,
		Payload:   json.RawMessage(inserted.PayloadJson),
		At:        inserted.At.Time,
		LegacyID:  inserted.ID,
	}})
	if err != nil {
		return err
	}
	if tx, ok := exec.(*stateTx); ok {
		tx.touch(inserted.WorkspaceID, stream, inserted.Kind)
	}
	return nil
}

// legacyStream maps a legacy events row onto the stream that owns it in the
// log core. The mapping is by event family and payload id, falling back to
// the task the row was bound to and finally to the workspace stream. The
// genesis import uses the same function so mirrored and imported history
// land on identical streams.
func legacyStream(inserted db.Event) eventlog.StreamID {
	family, _, _ := strings.Cut(inserted.Kind, ".")
	payload := payloadIDs(inserted.PayloadJson)
	taskID := ""
	if inserted.TaskID.Valid {
		taskID = strings.TrimSpace(inserted.TaskID.String)
	}
	switch family {
	case "work_order":
		if id := payload.first("work_order_id", "id"); id != "" {
			return eventlog.WorkOrderStream(id)
		}
	case "requirement":
		if id := payload.first("requirement_id"); id != "" {
			return eventlog.RequirementStream(id)
		}
	case "system_design":
		if id := payload.first("document_id"); id != "" {
			return eventlog.DesignStream(id)
		}
	case "reference_document":
		if id := payload.first("document_id"); id != "" {
			return eventlog.ReferenceDocumentStream(id)
		}
	case "decision":
		if id := payload.first("decision_id"); id != "" {
			return eventlog.DecisionStream(id)
		}
	case "planning_session":
		if id := payload.first("session_id"); id != "" {
			return eventlog.PlanningSessionStream(id)
		}
	case "planning_bundle":
		if id := payload.first("bundle_id"); id != "" {
			return eventlog.PlanningBundleStream(id)
		}
	case "worker":
		if id := payload.first("worker_id"); id != "" {
			return eventlog.WorkerStream(id)
		}
	case "identity":
		if id := payload.first("user_id"); id != "" {
			return eventlog.UserStream(id)
		}
	}
	if taskID != "" {
		return eventlog.TaskStream(taskID)
	}
	return eventlog.WorkspaceStream(inserted.WorkspaceID)
}

type payloadIDSet map[string]string

// payloadIDs extracts the string-valued top-level fields of a payload. Only
// strings count: an id is never a number or an object in Conveyor's events.
func payloadIDs(raw []byte) payloadIDSet {
	out := payloadIDSet{}
	if len(raw) == 0 {
		return out
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return out
	}
	for key, value := range fields {
		var s string
		if err := json.Unmarshal(value, &s); err == nil && strings.TrimSpace(s) != "" {
			out[key] = strings.TrimSpace(s)
		}
	}
	return out
}

func (p payloadIDSet) first(keys ...string) string {
	for _, key := range keys {
		if v, ok := p[key]; ok {
			return v
		}
	}
	return ""
}
