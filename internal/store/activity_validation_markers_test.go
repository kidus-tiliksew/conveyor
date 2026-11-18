package store

import (
	"encoding/json"
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
			want := 0
			if c.want {
				want = 1
			}
			if len(got) != want {
				t.Fatalf("backfill = %+v, want %d partitions", got, want)
			}
			if c.want && got[0] != (MarkerClaimPartition{TaskID: "task", JobID: c.row.Event.JobID}) {
				t.Fatalf("backfill partition = %+v", got[0])
			}
		})
	}
}
