package singlestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestMonitorIndependentIntegration(t *testing.T) {
	s, ctx, _ := ownedIdentityFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	o := monitor.Observation{WorkspaceID: "identity-fixture", Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "fixture-sha", SourceURL: "https://example.test/commit", CommitSHA: "fixture-sha", ObservedAt: now, ChangedPaths: []string{"owned.go"}}
	r, fresh, err := s.Observe(ctx, o)
	if err != nil || !fresh || r.State != "observed" {
		t.Fatalf("observe fresh=%v err=%v", fresh, err)
	}
	o.CausalEventID = 42
	r, fresh, err = s.Observe(ctx, o)
	if err != nil || fresh || r.DeduplicatedCount != 1 || r.CausalEventID != 42 {
		t.Fatal("observation did not deduplicate")
	}
	if err = s.RecordMonitorFailure(ctx, "forge_status", "fixture failure", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, err := s.MonitorStatus(ctx, true, now)
	if err != nil || status.CurrentError != "fixture failure" || len(status.Observations) != 1 {
		t.Fatal("monitor failure missing")
	}
	if err = s.RecordMonitorSuccess(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err = s.AuditMonitor(ctx, "monitor.observed", map[string]any{"repository": "conveyor"}); err != nil {
		t.Fatal(err)
	}
	status, err = s.MonitorStatus(ctx, true, now)
	if err != nil || status.CurrentError != "" || !status.BackoffUntil.IsZero() || !status.LastSuccessfulAt.Equal(now) || len(status.Activity) != 1 {
		t.Fatal("monitor recovery missing")
	}
	foreign := store.WithWorkspace(ctx, "foreign")
	status, err = s.MonitorStatus(foreign, true, now)
	if err != nil || len(status.Observations) != 0 || len(status.Activity) != 0 {
		t.Fatal("cross-workspace monitor data leaked")
	}
	if _, _, err = s.Observe(foreign, o); err == nil {
		t.Fatal("cross-workspace observation accepted")
	}
	sentinel := errors.New("callback failure")
	for range 2 {
		if err = s.WithMonitorSignalClassLock(ctx, "conveyor", monitor.DirectPush, func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatal("callback failure lost or lock not released")
		}
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithMonitorSignalClassLock(ctx, "conveyor", monitor.DirectPush, func(context.Context) error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first lock did not enter")
	}
	timeout, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err = s.WithMonitorSignalClassLock(timeout, "conveyor", monitor.DirectPush, func(context.Context) error { t.Error("same signal class entered concurrently"); return nil })
	close(release)
	if err == nil {
		t.Fatal("contended lock did not cancel")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	// Task-dependent conformance stays deferred without seeding substitute tasks.
	if _, _, err = s.RecordDrift(ctx, monitor.Drift{ID: "missing-task", WorkspaceID: "identity-fixture", Repository: "conveyor", Kind: monitor.DirectPush, TaskID: "missing", DetectedAt: now}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("missing drift parent accepted")
	}
}
