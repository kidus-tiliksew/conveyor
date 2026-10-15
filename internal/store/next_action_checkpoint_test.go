package store

import (
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The agent question reads the checkpoint of the latest queued order only,
// so an older order's request never attaches to the order the next action
// selects (component-work-orders, Next action).
func TestMarkersReadTheLatestQueuedOrdersCheckpoint(t *testing.T) {
	citations := []core.WorkOrderAuthorityConflictCitation{{DocumentID: "design-a", CitedVersion: 3, StatementOrSectionID: "Section"}}
	checkpoint := &core.WorkOrderCheckpoint{DecisionRequest: "Confirm design-a v4.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: citations}
	older := core.WorkOrder{ID: "older", State: core.WorkOrderQueued, Checkpoint: checkpoint}
	claimed := core.WorkOrder{ID: "claimed", State: core.WorkOrderClaimed, Checkpoint: checkpoint}
	latest := core.WorkOrder{ID: "latest", State: core.WorkOrderQueued}
	for name, orders := range map[string][]core.WorkOrder{
		"older queued":  {older, latest},
		"older claimed": {claimed, latest},
	} {
		if got := markersFrom(ActivityMarker{}, nil, orders); got.CheckpointDecision || got.Checkpoint != "" || got.CheckpointClass != "" || got.Citations != nil {
			t.Fatalf("%s: markers = %+v, want no checkpoint on the latest queued order", name, got)
		}
	}
	got := markersFrom(ActivityMarker{}, nil, []core.WorkOrder{latest, {ID: "released", State: core.WorkOrderQueued, Checkpoint: checkpoint}})
	if !got.CheckpointDecision || got.Checkpoint != checkpoint.DecisionRequest || got.CheckpointClass != checkpoint.Class || !reflect.DeepEqual(got.Citations, citations) {
		t.Fatalf("markers = %+v, want the latest queued order's checkpoint", got)
	}
}
