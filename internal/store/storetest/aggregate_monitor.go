package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
	t.Run("record workspace scoping", func(t *testing.T) { runMonitorWorkspaceScoping(t, x) })
}

// runMonitorWorkspaceScoping pins the one monitor record workspace rule on
// every backend: the context must be bound and the record must name that
// workspace, or the write refuses before any effect. The same observation
// identity and drift ID in two workspaces are independent records
// (component-monitor-drift; component-persistence).
func runMonitorWorkspaceScoping(t *testing.T, x Fixture) {
	st, home := x.Backend, x.Context
	foreign := x.Workspace + "-monitor-" + core.NewTaskID()[:6]
	away := store.WithWorkspace(x.Context, foreign)
	_, err := st.BootstrapWorkspaceConfig(away, &config.Config{Workspace: foreign, Repos: x.Config.Repos})
	requireOK(t, err)
	awayFixture := x
	awayFixture.Context, awayFixture.Workspace = away, foreign
	homeTask, awayTask := newAggregateTask(t, x), newAggregateTask(t, awayFixture)
	now := time.Now().UTC().Truncate(time.Microsecond)
	observation := func(workspace string) monitor.Observation {
		return monitor.Observation{WorkspaceID: workspace, Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "scoped-occurrence", SourceURL: "https://example.test/commit/scoped", CommitSHA: "scoped-sha", ObservedAt: now}
	}
	drift := func(workspace, taskID string) monitor.Drift {
		return monitor.Drift{ID: "scoped-drift", WorkspaceID: workspace, Repository: "conveyor", Kind: monitor.DirectPush, SourceURL: "https://example.test/commit/scoped", CommitSHA: "scoped-sha", TaskID: taskID, DetectedAt: now}
	}
	identity := observation(x.Workspace).Identity()
	findObservation := func(ctx context.Context) (monitor.ObservationRecord, bool) {
		t.Helper()
		status, err := st.MonitorStatus(ctx, true, now)
		requireOK(t, err)
		for _, record := range status.Observations {
			if record.Identity() == identity {
				return record, true
			}
		}
		return monitor.ObservationRecord{}, false
	}
	unresolved := func(ctx context.Context) []monitor.Drift {
		t.Helper()
		items, err := st.ListUnresolvedDrift(ctx)
		requireOK(t, err)
		var out []monitor.Drift
		for _, item := range items {
			if item.ID == "scoped-drift" {
				out = append(out, item)
			}
		}
		return out
	}

	unbound := t.Context()
	for name, refusal := range map[string]struct {
		ctx       context.Context
		workspace string
		want      error
	}{
		"omitted record workspace":   {home, "", store.ErrMonitorWorkspaceMismatch},
		"different record workspace": {home, foreign, store.ErrMonitorWorkspaceMismatch},
		"unbound context":            {unbound, x.Workspace, store.ErrWorkspaceRequired},
	} {
		if _, _, err := st.Observe(refusal.ctx, observation(refusal.workspace)); !errors.Is(err, refusal.want) {
			t.Fatalf("Observe with %s err=%v, want %v", name, err, refusal.want)
		}
		if _, _, err := st.RecordDrift(refusal.ctx, drift(refusal.workspace, homeTask.ID)); !errors.Is(err, refusal.want) {
			t.Fatalf("RecordDrift with %s err=%v, want %v", name, err, refusal.want)
		}
	}
	for _, ctx := range []context.Context{home, away} {
		if _, found := findObservation(ctx); found {
			t.Fatal("a refused observation was stored")
		}
		if items := unresolved(ctx); len(items) != 0 {
			t.Fatalf("a refused drift was stored: %+v", items)
		}
	}

	// The same identity in two workspaces is two records with separate
	// deduplication counts and task links.
	_, fresh, err := st.Observe(home, observation(x.Workspace))
	requireOK(t, err)
	if !fresh {
		t.Fatal("first home observation deduplicated")
	}
	repeated, fresh, err := st.Observe(home, observation(x.Workspace))
	requireOK(t, err)
	if fresh || repeated.DeduplicatedCount != 1 {
		t.Fatalf("home repeat fresh=%v count=%d", fresh, repeated.DeduplicatedCount)
	}
	awayRecord, fresh, err := st.Observe(away, observation(foreign))
	requireOK(t, err)
	if !fresh || awayRecord.DeduplicatedCount != 0 || awayRecord.WorkspaceID != foreign {
		t.Fatalf("foreign observation fresh=%v record=%+v, want an independent record", fresh, awayRecord)
	}
	_, err = st.LinkTask(home, identity, homeTask.ID, "created")
	requireOK(t, err)
	_, err = st.LinkTask(away, identity, awayTask.ID, "created")
	requireOK(t, err)
	homeRecord, found := findObservation(home)
	if !found || homeRecord.TaskID != homeTask.ID || homeRecord.DeduplicatedCount != 1 {
		t.Fatalf("home observation=%+v found=%v", homeRecord, found)
	}
	awayRecord, found = findObservation(away)
	if !found || awayRecord.TaskID != awayTask.ID || awayRecord.DeduplicatedCount != 0 {
		t.Fatalf("foreign observation=%+v found=%v", awayRecord, found)
	}

	// The same drift ID in two workspaces is two unresolved records, and
	// resolving one leaves the other open.
	_, fresh, err = st.RecordDrift(home, drift(x.Workspace, homeTask.ID))
	requireOK(t, err)
	if !fresh {
		t.Fatal("home drift deduplicated")
	}
	awayDrift, fresh, err := st.RecordDrift(away, drift(foreign, awayTask.ID))
	requireOK(t, err)
	if !fresh || awayDrift.WorkspaceID != foreign || awayDrift.TaskID != awayTask.ID {
		t.Fatalf("foreign drift fresh=%v drift=%+v", fresh, awayDrift)
	}
	requireOK(t, st.AuditMonitor(away, "monitor.observed", map[string]any{"identity": identity}))
	awayStatus, err := st.MonitorStatus(away, true, now)
	requireOK(t, err)
	if awayStatus.WorkspaceID != foreign || awayStatus.DriftCount != 1 || len(awayStatus.Activity) != 1 || len(awayStatus.Observations) != 1 {
		t.Fatalf("foreign status=%+v, want only its own observation, drift, and activity", awayStatus)
	}
	_, err = st.ResolveDrift(home, "scoped-drift", "conflict_resolved", "")
	requireOK(t, err)
	if items := unresolved(home); len(items) != 0 {
		t.Fatalf("home drift stayed open: %+v", items)
	}
	if items := unresolved(away); len(items) != 1 || items[0].TaskID != awayTask.ID || !items[0].ResolvedAt.IsZero() {
		t.Fatalf("resolving the home drift changed the foreign drift: %+v", items)
	}
}
