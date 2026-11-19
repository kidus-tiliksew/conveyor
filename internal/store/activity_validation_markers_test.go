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

// TestMarkerEventFoldable pins the exact row tests a backend applies after a
// text selection: a bounce counts only when UserRequestChangesPending reads it
// as a user change request, an update only when LatestForgeFailure reads it
// as an implement submission with a head, and every other kind counts.
func TestMarkerEventFoldable(t *testing.T) {
	for _, tc := range []struct {
		kind, payload string
		want          bool
	}{
		{"pipeline.bounced", `{"source":"user-request-changes"}`, true},
		{"pipeline.bounced", `{"Source":"user-request-changes"}`, true},
		{"pipeline.bounced", `{"source":"operator","note":"user-request-changes"}`, false},
		{"pipeline.bounced", `{"source":"user-request-changes","Source":"operator"}`, false},
		{"pipeline.bounced", `{"source":5}`, false},
		{"work_order.updated", `{"id":"o","stage":"implement","state":"submitted","head_sha":"h"}`, true},
		{"work_order.updated", `{"id":"o","Stage":"implement","state":"submitted","head_sha":"h"}`, true},
		{"work_order.updated", `{"id":"o","stage":"implement","state":"claimed","head_sha":"submitted"}`, false},
		{"work_order.updated", `{"id":"o","stage":"review","state":"submitted","head_sha":"implement"}`, false},
		{"work_order.updated", `{"id":"o","stage":"implement","state":"submitted"}`, false},
		{"work_order.claimed", `not json`, true},
		{"merge.failed", `{}`, true},
	} {
		event := core.Event{Kind: tc.kind, Payload: json.RawMessage(tc.payload)}
		if got := MarkerEventFoldable(event); got != tc.want {
			t.Errorf("MarkerEventFoldable(%s %s) = %t, want %t", tc.kind, tc.payload, got, tc.want)
		}
		// The filter agrees with the folds it serves.
		switch tc.kind {
		case "pipeline.bounced":
			if UserRequestChangesPending([]core.Event{event}) != tc.want {
				t.Errorf("bounce %s: filter and fold disagree", tc.payload)
			}
		case "work_order.updated":
			if _, _, ok := implementationSubmission(event); ok != tc.want {
				t.Errorf("update %s: filter and fold disagree", tc.payload)
			}
		}
	}
}
