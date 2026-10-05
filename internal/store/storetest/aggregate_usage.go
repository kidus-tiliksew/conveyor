package storetest

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runUsageTokenUpdates applies the token-only metadata and job updates that
// report_usage performs. Tokens and provenance persist, a historical order or
// job cost stays unchanged, a fresh job keeps an absent cost, and prior event
// payloads are not rewritten (req-usage-telemetry REQ-2, AC-2.1; DEC-1).
func runUsageTokenUpdates(t *testing.T, x Fixture) {
	for _, tc := range []struct {
		name       string
		historical float64
	}{
		{name: "fresh"},
		{name: "historical", historical: 1.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx := x.Backend, x.Context
			order := newAggregateOrder(t, x)
			_, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: "usage-session", ClientToken: "usage-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
			requireOK(t, err)
			if tc.historical != 0 {
				job, found, jobErr := st.GetLatestJob(ctx, order.TaskID)
				requireOK(t, jobErr)
				if !found || job.ID != order.JobID {
					t.Fatal("usage fixture job missing")
				}
				historical := tc.historical
				job.CostUSD = &historical
				requireOK(t, st.UpdateJob(ctx, job))
				seeded, getErr := st.GetWorkOrder(ctx, order.ID)
				requireOK(t, getErr)
				seeded.CostUSD = tc.historical
				requireOK(t, UpdateWorkOrder(ctx, st, seeded, taskops.WorkOrderMetadataCommand))
			}
			before, err := st.ListEvents(ctx, order.TaskID)
			requireOK(t, err)

			reported, err := st.GetWorkOrder(ctx, order.ID)
			requireOK(t, err)
			reported.TokensIn, reported.TokensOut = 40, 9
			reported.UsageReported, reported.SelfReported = true, true
			requireOK(t, UpdateWorkOrder(ctx, st, reported, taskops.WorkOrderMetadataCommand))
			job, found, err := st.GetLatestJob(ctx, order.TaskID)
			requireOK(t, err)
			if !found || job.ID != order.JobID {
				t.Fatal("usage job missing before token update")
			}
			job.TokensIn, job.TokensOut = 40, 9
			requireOK(t, st.UpdateJob(ctx, job))

			stored, err := st.GetWorkOrder(ctx, order.ID)
			requireOK(t, err)
			if stored.TokensIn != 40 || stored.TokensOut != 9 || !stored.UsageReported || !stored.SelfReported {
				t.Fatalf("order tokens or provenance lost: %+v", stored)
			}
			if stored.CostUSD != tc.historical {
				t.Fatalf("order cost = %v, want %v", stored.CostUSD, tc.historical)
			}
			jobs, err := st.ListJobs(ctx, order.TaskID)
			requireOK(t, err)
			if len(jobs) != 1 || jobs[0].TokensIn != 40 || jobs[0].TokensOut != 9 {
				t.Fatalf("job tokens lost: %+v", jobs)
			}
			switch {
			case tc.historical == 0 && jobs[0].CostUSD != nil:
				t.Fatalf("fresh job gained cost %v", *jobs[0].CostUSD)
			case tc.historical != 0 && (jobs[0].CostUSD == nil || *jobs[0].CostUSD != tc.historical):
				t.Fatalf("historical job cost changed: %v", jobs[0].CostUSD)
			}

			after, err := st.ListEvents(ctx, order.TaskID)
			requireOK(t, err)
			retained := make(map[int64]core.Event, len(after))
			for _, event := range after {
				retained[event.ID] = event
			}
			for _, prior := range before {
				event, ok := retained[prior.ID]
				if !ok || event.Kind != prior.Kind || !sameJSON(t, event.Payload, prior.Payload) {
					t.Fatalf("token update rewrote prior event %d (%s)", prior.ID, prior.Kind)
				}
			}
		})
	}
}

func sameJSON(t *testing.T, left, right []byte) bool {
	t.Helper()
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return bytes.Equal(left, right)
	}
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(encodedA, encodedB)
}
