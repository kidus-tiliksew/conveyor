package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestMarkerClaimBackfill pins when the read takes back the claims the
// reduction left out: whenever the newest kept claim does not supersede them
// for the change-request fold or for an open review order's lease.
func TestMarkerClaimBackfill(t *testing.T) {
	lease := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	claim := func(stage core.Stage, lease time.Time) json.RawMessage {
		return WorkOrderEventPayload(core.WorkOrder{ID: "order", JobID: "job", Stage: stage, State: core.WorkOrderClaimed, LeaseExpiresAt: lease})
	}
	open := []MarkerReviewOrder{{TaskID: "task", ID: "review-order", JobID: "review-job"}}
	row := func(job string, payload json.RawMessage, dropped, droppedImplement int64) MarkerFoldRow {
		return MarkerFoldRow{Event: core.Event{TaskID: "task", JobID: job, Kind: "work_order.claimed", Payload: payload}, Dropped: dropped, DroppedImplement: droppedImplement}
	}
	for _, c := range []struct {
		name string
		row  MarkerFoldRow
		want bool
	}{
		{"nothing left out", row("job", json.RawMessage(`{"id":5}`), 0, 0), false},
		{"an implement claim supersedes older implement claims", row("job", claim(core.StageImplement, time.Time{}), 4, 4), false},
		{"a review claim supersedes claims that cannot name the implement stage", row("job", claim(core.StageReview, time.Time{}), 4, 0), false},
		{"a review claim does not supersede implement claims", row("job", claim(core.StageReview, lease), 4, 1), true},
		{"a claim that does not decode supersedes nothing", row("job", json.RawMessage(`{"stage":"implement","attempt_id":5}`), 2, 2), true},
		{"an open review order's newest claim needs a lease", row("review-job", claim(core.StageReview, time.Time{}), 2, 0), true},
		{"an open review order's newest claim with a lease", row("review-job", claim(core.StageReview, lease), 2, 0), false},
		{"an earliest future claim that decodes", MarkerFoldRow{Event: core.Event{TaskID: "task", JobID: "review-job", Kind: "work_order.claimed", Payload: claim(core.StageReview, lease)}, FutureDropped: 3}, false},
		{"an earliest future claim that does not decode", MarkerFoldRow{Event: core.Event{TaskID: "task", JobID: "review-job", Kind: "work_order.claimed", Payload: json.RawMessage(`{"id":5}`)}, FutureDropped: 3}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := MarkerClaimBackfill([]MarkerFoldRow{c.row}, open)
			if (len(got) == 1) != c.want {
				t.Fatalf("backfill = %+v, want backfill %t", got, c.want)
			}
			if c.want && got[0] != (MarkerClaimPartition{TaskID: "task", JobID: c.row.Event.JobID}) {
				t.Fatalf("backfill partition = %+v", got[0])
			}
		})
	}
}

// TestMergeMarkerFoldRows pins that backfilled claims join once each, in the
// ledger's (at, id) order.
func TestMergeMarkerFoldRows(t *testing.T) {
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	event := func(id int64, second int) MarkerFoldRow {
		return MarkerFoldRow{Event: core.Event{ID: id, At: at.Add(time.Duration(second) * time.Second)}}
	}
	merged := MergeMarkerFoldRows([]MarkerFoldRow{event(3, 2), event(9, 5)}, []MarkerFoldRow{event(1, 1), event(3, 2), event(7, 2), event(4, 2)})
	var ids []int64
	for _, row := range merged {
		ids = append(ids, row.Event.ID)
	}
	if want := []int64{1, 3, 4, 7, 9}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("merged ids = %v, want %v", ids, want)
	}
}

// TestMarkerFoldSQLShape pins what the shared statement must never hold: a
// question mark outside a SingleStore bind placeholder, and a snapshot body.
func TestMarkerFoldSQLShape(t *testing.T) {
	dialect := MarkerSQLDialect{
		Lease:       SingleStoreLeaseSQL,
		Placeholder: func(int) string { return "?" },
		Scope: func(b *MarkerSQLBinder, ws string, page []string) string {
			return "FROM events e WHERE e.workspace_id=" + b.Bind(ws)
		},
		Compact: "e.payload_json",
		InList: func(b *MarkerSQLBinder, col string, values []string) string {
			return col + " IN (" + b.Bind(values) + ")"
		},
		Int:       func(p string) string { return p },
		Text:      func(j string) string { return j },
		Keys:      func(j string) string { return "K(" + j + ")" },
		Field:     func(j, k string) string { return "F(" + j + ",'" + k + "')" },
		FieldText: func(j, k string) string { return "T(" + j + ",'" + k + "')" },
		IsString:  func(v string) string { return "S(" + v + ")" },
		Object:    func(pairs []string) string { return "B(" + strings.Join(pairs, ",") + ")" },
		Contains:  func(text, literal string) string { return "C(" + text + "," + literal + ")" },
		Lower:     func(text string) string { return "L(" + text + ")" },
		Stripped:  func(text, literal string) string { return "E(" + text + "," + literal + ")" },
		ASCII:     func(text string) string { return "A(" + text + ")" },
	}
	in := MarkerFoldInput{Workspace: "ws", Page: []string{"task"}, ReviewTasks: []string{"task"}, LiveReviewTasks: []string{"task"},
		OpenReviewOrders: []MarkerReviewOrder{{TaskID: "task", ID: "order-1", JobID: "job-1"}, {TaskID: "task", ID: `odd"id`, JobID: "job-2"}}, Now: time.Unix(10, 5)}
	foldQuery, foldArgs := MarkerFoldSQL(dialect, in)
	historyQuery, historyArgs := MarkerClaimHistorySQL(dialect, in, []MarkerClaimPartition{{TaskID: "task", JobID: "job-1"}})
	for name, statement := range map[string]struct {
		query string
		args  []any
	}{"fold": {foldQuery, foldArgs}, "history": {historyQuery, historyArgs}} {
		query, args := statement.query, statement.args
		if strings.Count(query, "?") != len(args) {
			t.Errorf("%s statement holds %d question marks for %d arguments; the SingleStore driver reads each as a bind placeholder", name, strings.Count(query, "?"), len(args))
		}
		if strings.Contains(query, "'governance_snapshot'") || strings.Contains(query, "'served_requirement_snapshot'") {
			t.Errorf("%s statement names a snapshot body key", name)
		}
	}
	// An order ID outside the safe alphabet keeps every claim of its task.
	query, args := MarkerFoldSQL(dialect, in)
	if !strings.Contains(query, "AND NOT (task_id IN (?))") {
		t.Fatal("a task with an order ID outside the safe alphabet is not excluded from the claim reduction")
	}
	if !containsArg(args, []string{"task"}) {
		t.Fatalf("args = %v, want the unsafe task bound", args)
	}
}

func containsArg(args []any, want any) bool {
	for _, arg := range args {
		if reflect.DeepEqual(arg, want) {
			return true
		}
	}
	return false
}
