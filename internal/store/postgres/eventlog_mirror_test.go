package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func TestLegacyStreamMapping(t *testing.T) {
	task := pgtype.Text{String: "260903-ebf5ec", Valid: true}
	cases := []struct {
		name string
		row  db.Event
		want eventlog.StreamID
	}{
		{"task-bound", db.Event{Kind: "task.state_changed", TaskID: task, WorkspaceID: "demo"}, eventlog.TaskStream("260903-ebf5ec")},
		{"review on task", db.Event{Kind: "review.revise", TaskID: task, PayloadJson: []byte(`{"round":1}`)}, eventlog.TaskStream("260903-ebf5ec")},
		{"work order by work_order_id", db.Event{Kind: "work_order.claimed", TaskID: task, PayloadJson: []byte(`{"work_order_id":"wo-1"}`)}, eventlog.WorkOrderStream("wo-1")},
		{"work order by id", db.Event{Kind: "work_order.created", TaskID: task, PayloadJson: []byte(`{"id":"wo-2","task_id":"260903-ebf5ec"}`)}, eventlog.WorkOrderStream("wo-2")},
		{"work order without id falls back to task", db.Event{Kind: "work_order.stale", TaskID: task, PayloadJson: []byte(`{}`)}, eventlog.TaskStream("260903-ebf5ec")},
		{"requirement", db.Event{Kind: "requirement.version_confirmed", PayloadJson: []byte(`{"requirement_id":"req-1","version":2}`), WorkspaceID: "demo"}, eventlog.RequirementStream("req-1")},
		{"design", db.Event{Kind: "system_design.version_proposed", PayloadJson: []byte(`{"document_id":"design-database"}`)}, eventlog.DesignStream("design-database")},
		{"reference document", db.Event{Kind: "reference_document.created", PayloadJson: []byte(`{"document_id":"ref-1"}`)}, eventlog.ReferenceDocumentStream("ref-1")},
		{"decision", db.Event{Kind: "decision.confirmed", PayloadJson: []byte(`{"decision_id":"DEC-6"}`)}, eventlog.DecisionStream("DEC-6")},
		{"planning session", db.Event{Kind: "planning_session.created", PayloadJson: []byte(`{"session_id":"ps-1"}`)}, eventlog.PlanningSessionStream("ps-1")},
		{"planning bundle", db.Event{Kind: "planning_bundle.approved", PayloadJson: []byte(`{"bundle_id":"pb-1"}`)}, eventlog.PlanningBundleStream("pb-1")},
		{"worker", db.Event{Kind: "worker.enrolled", PayloadJson: []byte(`{"worker_id":"worker-1"}`)}, eventlog.WorkerStream("worker-1")},
		{"identity with user", db.Event{Kind: "identity.forge_token_deleted", PayloadJson: []byte(`{"user_id":"usr_1"}`)}, eventlog.UserStream("usr_1")},
		{"identity without user", db.Event{Kind: "identity.legacy_token_rotated", PayloadJson: []byte(`{}`), WorkspaceID: "demo"}, eventlog.WorkspaceStream("demo")},
		{"workspace", db.Event{Kind: "workspace.created", PayloadJson: []byte(`{"id":"demo"}`), WorkspaceID: "demo"}, eventlog.WorkspaceStream("demo")},
		{"config", db.Event{Kind: "config.updated", PayloadJson: []byte(`{"from_version":1}`), WorkspaceID: "demo"}, eventlog.WorkspaceStream("demo")},
		{"numeric id ignored", db.Event{Kind: "decision.proposed", PayloadJson: []byte(`{"decision_id":12}`), WorkspaceID: "demo"}, eventlog.WorkspaceStream("demo")},
		{"malformed payload", db.Event{Kind: "requirement.created", PayloadJson: []byte(`not json`), WorkspaceID: "demo"}, eventlog.WorkspaceStream("demo")},
		{"blank task id", db.Event{Kind: "task.created", TaskID: pgtype.Text{String: "  ", Valid: true}, WorkspaceID: "demo"}, eventlog.WorkspaceStream("demo")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyStream(tc.row); got != tc.want {
				t.Fatalf("legacyStream=%q want %q", got, tc.want)
			}
		})
	}
}
