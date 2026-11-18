package store

import (
	"encoding/json"
	"reflect"
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
