package storetest

import (
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunRequirementEventReads checks scoped batches against the existing event read.
func RunRequirementEventReads(t *testing.T, x Fixture) {
	st := x.Backend
	ctx := store.WithActor(x.Context, store.SystemActor())
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "scope-event-" + core.NewTaskID(), Title: "Scoped event"}, core.RequirementVersion{Content: "# Scoped event\n\nIntent", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve intent."}}, Origin: core.RequirementOriginOperator})
	requireOK(t, err)
	other, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "scope-noise-" + core.NewTaskID(), Title: "Other event"}, core.RequirementVersion{Content: "# Other event\n\nOther intent", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve other intent."}}, Origin: core.RequirementOriginOperator})
	requireOK(t, err)
	want, err := st.ListRequirementEvents(ctx, requirement.ID)
	requireOK(t, err)
	got, err := st.ListRequirementEventsForRequirements(ctx, []string{requirement.ID, requirement.ID, "missing", "MISSING"})
	requireOK(t, err)
	if !reflect.DeepEqual(got[requirement.ID], want) || len(got) != 3 || got["missing"] == nil || got["MISSING"] == nil {
		t.Fatalf("scoped requirement events = %#v, want selected events %#v and explicit empty IDs", got, want)
	}
	if _, ok := got[other.ID]; ok {
		t.Fatalf("unrequested events returned: %#v", got[other.ID])
	}
	empty, err := st.ListRequirementEventsForRequirements(ctx, nil)
	requireOK(t, err)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty requirement selection = %#v", empty)
	}
	// Unknown or out-of-workspace task IDs, and known tasks with no delivery
	// events, are absent from the batch on every backend
	// (runRequirementDeliveryEvents covers the known and foreign tasks).
	delivery, err := st.ListRequirementDeliveryEventsForTasks(ctx, []string{"missing", "MISSING", "missing"})
	requireOK(t, err)
	if len(delivery) != 0 {
		t.Fatalf("missing delivery events = %#v, want empty map", delivery)
	}
}
