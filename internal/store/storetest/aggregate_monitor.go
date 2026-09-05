package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/monitor"
)

func runMonitor(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
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
