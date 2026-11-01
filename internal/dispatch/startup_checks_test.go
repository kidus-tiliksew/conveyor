package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// driftStore wraps the volatile backend. Each store pass reports one repair
// for its task on its first successful call and nothing afterwards; failures
// makes a pass fail that many times first. appendErr fails drift.repaired
// appends.
type driftStore struct {
	store.Backend
	mu        sync.Mutex
	calls     map[string]int
	failures  map[string]int
	tasks     map[string]string
	appendErr error
}

func (s *driftStore) pass(ctx context.Context, name string) (int, error) {
	s.mu.Lock()
	s.calls[name]++
	if s.failures[name] > 0 {
		s.failures[name]--
		s.mu.Unlock()
		return 0, fmt.Errorf("%s: database unavailable", name)
	}
	repaired := 0
	taskID, drifted := s.tasks[name]
	if drifted {
		delete(s.tasks, name)
		repaired = 1
	}
	s.mu.Unlock()
	if drifted {
		store.ReportDriftRepair(ctx, store.DriftRepair{TaskID: taskID, Pass: name, Mismatch: name + " mismatch", Repair: name + " repair"})
	}
	return repaired, nil
}

func (s *driftStore) ReconcileGitHubLifecycles(ctx context.Context) (int, error) {
	return s.pass(ctx, store.DriftPassGitHubIssuePublications)
}
func (s *driftStore) ReconcileQueuedTasks(ctx context.Context) (int, error) {
	return s.pass(ctx, store.DriftPassQueuedTasks)
}
func (s *driftStore) ReconcileBlueprintClosures(ctx context.Context) (int, error) {
	return s.pass(ctx, store.DriftPassBlueprintClosures)
}
func (s *driftStore) ReconcileReviewPublications(ctx context.Context) (int, error) {
	return s.pass(ctx, store.DriftPassReviewPublications)
}
func (s *driftStore) ReconcileVerificationDeliveries(ctx context.Context) error {
	if store.ActorFromContext(ctx).ID != "verification-publication" {
		return errors.New("verification deliveries reconciled without the verification-publication actor")
	}
	_, err := s.pass(ctx, store.DriftPassVerificationDeliveries)
	return err
}
func (s *driftStore) AppendEvent(ctx context.Context, event core.Event) error {
	if event.Kind == store.DriftRepairedEvent && s.appendErr != nil {
		return s.appendErr
	}
	return s.Backend.AppendEvent(ctx, event)
}

type lines struct {
	mu    sync.Mutex
	items []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	l.items = append(l.items, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *lines) matching(substrings ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.items {
		ok := true
		for _, s := range substrings {
			ok = ok && strings.Contains(line, s)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

func newDriftFixture(t *testing.T, drifted ...string) (context.Context, *driftStore, *StartupChecks, *lines) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
	st := &driftStore{Backend: store.NewVolatileBackend(), calls: map[string]int{}, failures: map[string]int{}, tasks: map[string]string{}}
	for _, pass := range drifted {
		taskID := "task-" + strings.ReplaceAll(pass, "_", "-")
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "test", Repo: "app", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		st.tasks[pass] = taskID
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	log := &lines{}
	checks := &StartupChecks{Dispatcher: d, Store: st, Logf: log.logf, Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
	return ctx, st, checks, log
}

func driftEvents(t *testing.T, st store.Store, ctx context.Context, taskID string) []core.Event {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []core.Event
	for _, event := range events {
		if event.Kind == store.DriftRepairedEvent {
			out = append(out, event)
		}
	}
	return out
}

var storeDriftPasses = []string{
	store.DriftPassGitHubIssuePublications, store.DriftPassQueuedTasks, store.DriftPassBlueprintClosures,
	store.DriftPassReviewPublications, store.DriftPassVerificationDeliveries,
}

// TestStartupChecksRecordEachRepairOnce: every drift pass's repair appends
// one system drift.repaired event naming the pass, mismatch, and repair, and
// writes one error-level line naming the workspace, task, and pass; a second
// start repairs, appends, and writes nothing (req-quiet-idle-runtime AC-4.1
// to AC-4.3; component-runtime, Start-up checks and background loops).
func TestStartupChecksRecordEachRepairOnce(t *testing.T) {
	ctx, st, checks, log := newDriftFixture(t, storeDriftPasses...)
	checks.RunDrift(ctx, []string{"test"})
	checks.Wait()
	for _, pass := range storeDriftPasses {
		taskID := "task-" + strings.ReplaceAll(pass, "_", "-")
		events := driftEvents(t, st, ctx, taskID)
		if len(events) != 1 || events[0].ActorRole != core.ActorSystem || !strings.Contains(string(events[0].Payload), `"pass":"`+pass+`"`) ||
			!strings.Contains(string(events[0].Payload), pass+" mismatch") || !strings.Contains(string(events[0].Payload), pass+" repair") {
			t.Fatalf("pass %s events=%+v", pass, events)
		}
		if got := log.matching("level=error", "start-up check repaired drift", "workspace=test", "task="+taskID, "pass="+pass); len(got) != 1 {
			t.Fatalf("pass %s error lines=%v", pass, got)
		}
	}
	if acts, err := st.ListFactoryActs(ctx, time.Now().Add(-time.Hour), 100); err != nil || len(acts) != len(storeDriftPasses) {
		t.Fatalf("factory acts=%+v err=%v, want one per repair", acts, err)
	}
	before := len(log.matching(""))
	checks.RunDrift(ctx, []string{"test"})
	checks.Wait()
	if after := len(log.matching("")); after != before {
		t.Fatalf("second start wrote %v", log.matching("")[before:])
	}
	for _, pass := range storeDriftPasses {
		if events := driftEvents(t, st, ctx, "task-"+strings.ReplaceAll(pass, "_", "-")); len(events) != 1 {
			t.Fatalf("second start appended for %s: %+v", pass, events)
		}
	}
}

// TestStartupChecksNoDriftWritesNothing: a start with no drift appends no
// event and writes no line.
func TestStartupChecksNoDriftWritesNothing(t *testing.T) {
	ctx, st, checks, log := newDriftFixture(t)
	if err := st.CreateTask(ctx, core.Task{ID: "quiet", Workspace: "test", Repo: "app", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	checks.RunDrift(ctx, []string{"test"})
	checks.RunGitHub(ctx, []string{"test"})
	checks.Wait()
	if got := log.matching(""); len(got) != 0 {
		t.Fatalf("lines=%v", got)
	}
	if events := driftEvents(t, st, ctx, "quiet"); len(events) != 0 {
		t.Fatalf("events=%+v", events)
	}
	for _, pass := range storeDriftPasses {
		if st.calls[pass] != 1 {
			t.Fatalf("pass %s ran %d times", pass, st.calls[pass])
		}
	}
}

// TestStartupChecksRetryFailedPass: a failed pass writes an error line, never
// skips the later passes, and is retried after the doubling delay until it
// repairs and reports; no retry runs once it completes.
func TestStartupChecksRetryFailedPass(t *testing.T) {
	ctx, st, checks, log := newDriftFixture(t, store.DriftPassQueuedTasks, store.DriftPassReviewPublications)
	st.failures[store.DriftPassQueuedTasks] = 2
	var delays []time.Duration
	var mu sync.Mutex
	checks.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return ctx.Err()
	}
	checks.RunDrift(ctx, []string{"test"})
	checks.Wait()
	if got := log.matching("level=error", "start-up check failed", "workspace=test", "pass=queued_tasks", "database unavailable"); len(got) != 2 ||
		!strings.Contains(got[0], "attempt=1 retry_in=5s") || !strings.Contains(got[1], "attempt=2 retry_in=10s") {
		t.Fatalf("failure lines=%v", got)
	}
	if len(delays) != 2 || delays[0] != 5*time.Second || delays[1] != 10*time.Second {
		t.Fatalf("retry delays=%v", delays)
	}
	if st.calls[store.DriftPassQueuedTasks] != 3 || st.calls[store.DriftPassReviewPublications] != 1 || st.calls[store.DriftPassVerificationDeliveries] != 1 {
		t.Fatalf("calls=%v", st.calls)
	}
	for _, taskID := range []string{"task-queued-tasks", "task-review-publications"} {
		if events := driftEvents(t, st, ctx, taskID); len(events) != 1 {
			t.Fatalf("task %s events=%+v", taskID, events)
		}
		if got := log.matching("start-up check repaired drift", "task="+taskID); len(got) != 1 {
			t.Fatalf("task %s repair lines=%v", taskID, got)
		}
	}
}

// TestStartupChecksRetryStopsWithService: a pass that keeps failing stops
// retrying when the service context ends.
func TestStartupChecksRetryStopsWithService(t *testing.T) {
	ctx, st, checks, _ := newDriftFixture(t)
	st.failures[store.DriftPassBlueprintClosures] = 1 << 20
	service, cancel := context.WithCancel(ctx)
	sleeps := 0
	checks.Sleep = func(ctx context.Context, _ time.Duration) error {
		sleeps++
		if sleeps == 3 {
			cancel()
		}
		return ctx.Err()
	}
	checks.RunDrift(service, []string{"test"})
	checks.Wait()
	if st.calls[store.DriftPassBlueprintClosures] != 3 {
		t.Fatalf("blueprint pass ran %d times, want three attempts before the service stopped", st.calls[store.DriftPassBlueprintClosures])
	}
}

// TestStartupChecksReportFailedAppend: a repair whose drift.repaired append
// fails writes the repair line and a second error line naming the task.
func TestStartupChecksReportFailedAppend(t *testing.T) {
	ctx, st, checks, log := newDriftFixture(t, store.DriftPassBlueprintClosures)
	st.appendErr = errors.New("events table unavailable")
	checks.RunDrift(ctx, []string{"test"})
	checks.Wait()
	if got := log.matching("level=error", "could not record drift.repaired", "task=task-blueprint-closures", "pass=blueprint_closures", "events table unavailable"); len(got) != 1 {
		t.Fatalf("lines=%v", log.matching(""))
	}
	if got := log.matching("start-up check repaired drift", "task=task-blueprint-closures"); len(got) != 1 {
		t.Fatalf("repair lines=%v", got)
	}
}

// TestStartupGitHubReadAppliesMergedPullRequest: the start-up GitHub read
// reads a task waiting on GitHub against a fake forge that reports its
// pull request merged, and the task ends merged with no drift.repaired event
// (req-quiet-idle-runtime AC-3.4).
func TestStartupGitHubReadAppliesMergedPullRequest(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	views := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return githubtrigger.PullRequest{Number: 41, State: "closed", Merged: true, Mergeable: "UNKNOWN", HeadSHA: "approved-head", MergeCommitSHA: "landed", MergedBy: "github-operator"}, nil
	}
	forbidMergeCalls(t, d)
	log := &lines{}
	checks := &StartupChecks{Dispatcher: d, Logf: log.logf}
	checks.RunGitHub(ctx, []string{"test"})
	checks.Wait()
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || views != 1 {
		t.Fatalf("task=%+v views=%d err=%v", current, views, err)
	}
	if events := driftEvents(t, st, ctx, task.ID); len(events) != 0 {
		t.Fatalf("drift events=%+v", events)
	}
	if got := log.matching(""); len(got) != 0 {
		t.Fatalf("lines=%v", got)
	}
}
