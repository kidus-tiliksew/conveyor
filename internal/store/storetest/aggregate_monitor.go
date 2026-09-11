package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/monitor"
)

func runMonitor(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	assertDrift := func(ctx context.Context, wantIDs ...string) {
		t.Helper()
		narrow, err := st.ListUnresolvedDrift(ctx)
		requireOK(t, err)
		status, err := st.MonitorStatus(ctx, true, time.Now().UTC())
		requireOK(t, err)
		a, err := json.Marshal(narrow)
		requireOK(t, err)
		b, err := json.Marshal(status.Drift)
		requireOK(t, err)
		if !bytes.Equal(a, b) || len(narrow) != len(wantIDs) || status.DriftCount != len(wantIDs) {
			t.Fatalf("narrow=%s status=%s want IDs=%v", a, b, wantIDs)
		}
		for i, id := range wantIDs {
			if narrow[i].ID != id || narrow[i].WorkspaceID != x.Workspace || !narrow[i].ResolvedAt.IsZero() {
				t.Fatalf("unexpected drift at %d: %+v", i, narrow[i])
			}
		}
	}
	assertDrift(ctx)
	task := newAggregateTask(t, x)
	now := time.Now().UTC().Truncate(time.Microsecond)
	observation := monitor.Observation{WorkspaceID: x.Workspace, Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "push-one", SourceURL: "https://example.test/commit", CommitSHA: "fixture-sha", ObservedAt: now}
	_, created, err := st.Observe(ctx, observation)
	requireOK(t, err)
	if !created {
		t.Fatal("first observation deduplicated")
	}
	repeated, created, err := st.Observe(ctx, observation)
	requireOK(t, err)
	if created || repeated.DeduplicatedCount != 1 {
		t.Fatal("repeat observation did not deduplicate")
	}
	linked, err := st.LinkTask(ctx, observation.Identity(), task.ID, "created")
	requireOK(t, err)
	if linked.TaskID != task.ID {
		t.Fatal("observation task link differs")
	}
	drift, created, err := st.RecordDrift(ctx, monitor.Drift{ID: "drift-one", WorkspaceID: x.Workspace, Repository: "conveyor", Kind: monitor.DirectPush, SourceURL: observation.SourceURL, CommitSHA: observation.CommitSHA, TaskID: task.ID, DetectedAt: now})
	requireOK(t, err)
	if !created {
		t.Fatal("first drift deduplicated")
	}
	_, created, err = st.RecordDrift(ctx, drift)
	requireOK(t, err)
	if created {
		t.Fatal("repeat drift created a second record")
	}
	requireOK(t, st.RecordMonitorFailure(ctx, "forge_status", "fixture failure", now.Add(time.Minute)))
	status, err := st.MonitorStatus(ctx, true, now)
	requireOK(t, err)
	if status.CurrentError != "fixture failure" || status.DriftCount != 1 {
		t.Fatal("monitor failure or drift count missing")
	}
	requireOK(t, st.RecordMonitorSuccess(ctx, now))
	requireOK(t, st.AuditMonitor(ctx, "monitor.observed", map[string]any{"repository": "conveyor"}))
	status, err = st.MonitorStatus(ctx, true, now)
	requireOK(t, err)
	if status.CurrentError != "" || !status.LastSuccessfulAt.Equal(now) || len(status.Activity) == 0 {
		t.Fatal("monitor success failed to clear error or preserve activity")
	}
	if _, err := st.ResolveDrift(ctx, drift.ID, "unrecognized", ""); err == nil {
		t.Fatal("unknown drift outcome accepted")
	}
	resolved, err := st.ResolveDrift(ctx, drift.ID, "conflict_resolved", "")
	requireOK(t, err)
	if resolved.ResolvedAt.IsZero() || resolved.Outcome != "conflict_resolved" {
		t.Fatal("drift resolution missing")
	}
	status, err = st.MonitorStatus(ctx, true, now)
	requireOK(t, err)
	if status.DriftCount != 0 {
		t.Fatal("resolved drift remains active")
	}
	assertDrift(ctx)
	// Insert out of timestamp and ID order, including a timestamp tie.
	for i, id := range []string{"later", "tie-b", "tie-a"} {
		at := now.Add(-time.Hour)
		if i == 0 {
			at = now
		}
		_, _, err := st.RecordDrift(ctx, monitor.Drift{ID: id, WorkspaceID: x.Workspace, Repository: "conveyor", Kind: monitor.DirectPush, TaskID: task.ID, DetectedAt: at, MatchingPaths: []string{"internal/example.go"}})
		requireOK(t, err)
	}
	assertDrift(ctx, "tie-a", "tie-b", "later")
	assertDrift(store.WithWorkspace(ctx, x.Workspace+"-foreign"))
	_, err = st.ResolveDrift(ctx, "tie-b", "change_reverted", "")
	requireOK(t, err)
	assertDrift(ctx, "tie-a", "later")
	sentinel := errors.New("callback failure")
	for range 2 {
		err := st.WithMonitorSignalClassLock(ctx, "conveyor", monitor.DirectPush, func(context.Context) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatal("signal lock did not return callback error or release")
		}
	}
	_, found, err := st.FindOpenMonitorTask(ctx, "conveyor", monitor.DirectPush)
	requireOK(t, err)
	if found {
		t.Fatal("ordinary task appeared as monitor task")
	}
	requireOK(t, st.AuditTask(ctx, task.ID, "task.updated", map[string]any{"fixture": true}))
}
