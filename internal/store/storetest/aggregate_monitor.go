package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
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
	requireOK(t, st.RecordMonitorFailure(ctx, "conveyor", "forge_status", "fixture failure", now.Add(time.Minute)))
	status, err := st.MonitorStatus(ctx, true, now)
	requireOK(t, err)
	if status.CurrentError != "fixture failure" || status.DriftCount != 1 {
		t.Fatal("monitor failure or drift count missing")
	}
	requireOK(t, st.RecordMonitorSuccess(ctx, "conveyor", now))
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
	t.Run("merge commit state", func(t *testing.T) { runMonitorMergeCommitState(t, x) })
	t.Run("merge deferral poll", func(t *testing.T) { runMonitorMergeDeferral(t, x) })
	t.Run("tip and per-repository status", func(t *testing.T) { runMonitorTip(t, x) })
	t.Run("task origins", func(t *testing.T) { runMonitorOrigins(t, x) })
	t.Run("recovery link", func(t *testing.T) { runMonitorRecoveryLink(t, x) })
}

// recoveryLinkFault fails LinkRecoveryTask while err is set, as a store failure
// on a pass would.
type recoveryLinkFault struct {
	monitor.Store
	err error
}

func (f *recoveryLinkFault) LinkRecoveryTask(ctx context.Context, identity, taskID string, taskPayload, activityPayload map[string]any) (monitor.ObservationRecord, error) {
	if f.err != nil {
		return monitor.ObservationRecord{}, f.err
	}
	return f.Store.LinkRecoveryTask(ctx, identity, taskID, taskPayload, activityPayload)
}

// runMonitorRecoveryLink pins the recovery_observed outcome and its once-only
// annotation on every backend (component-monitor-drift invariants 4 and 17;
// component-persistence invariant 5). Each scenario runs in its own workspace,
// because the open post-merge task is found per workspace and repository.
func runMonitorRecoveryLink(t *testing.T, x Fixture) {
	st := x.Backend
	ms := st.(monitor.Store)
	scoped := func(t *testing.T) Fixture {
		t.Helper()
		foreign := x.Workspace + "-recovery-" + core.NewTaskID()
		away := store.WithWorkspace(x.Context, foreign)
		_, err := st.BootstrapWorkspaceConfig(away, &config.Config{Workspace: foreign, Repos: x.Config.Repos})
		requireOK(t, err)
		fixture := x
		fixture.Context, fixture.Workspace = away, foreign
		return fixture
	}
	recovery := func(sha string) monitor.Observation {
		return monitor.Observation{Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "recovery:commit:" + sha, SourceURL: "https://example.test/commit/" + sha, CommitSHA: sha, RecoveryObserved: true}
	}
	failure := func(sha string) monitor.Observation {
		return monitor.Observation{Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "commit:" + sha + ":attempt:1", SourceURL: "https://example.test/check/" + sha, CommitSHA: sha, Attempt: 1}
	}
	// annotations counts the observation's task events on the task and its
	// activity rows, and reads its stored row without counting a repeat.
	annotations := func(t *testing.T, fixture Fixture, taskID, identity string) (int, int, monitor.ObservationRecord) {
		t.Helper()
		events, err := st.ListEvents(fixture.Context, taskID)
		requireOK(t, err)
		taskEvents := 0
		for _, event := range events {
			if event.Kind == monitor.EventRecoveryObserved {
				taskEvents++
			}
		}
		status, err := ms.MonitorStatus(fixture.Context, true, time.Now().UTC())
		requireOK(t, err)
		activity := 0
		for _, row := range status.Activity {
			if row.Kind == monitor.EventRecoveryObserved && row.Payload["identity"] == identity {
				activity++
			}
		}
		for _, row := range status.Observations {
			if row.Identity() == identity {
				return taskEvents, activity, row
			}
		}
		t.Fatalf("observation %s missing from status %+v", identity, status.Observations)
		return 0, 0, monitor.ObservationRecord{}
	}
	requireAnnotated := func(t *testing.T, fixture Fixture, taskID, identity string, want int) {
		t.Helper()
		taskEvents, activity, row := annotations(t, fixture, taskID, identity)
		if taskEvents != want || activity != want {
			t.Fatalf("task events=%d activity=%d, want %d of each", taskEvents, activity, want)
		}
		if want > 0 && (row.TaskID != taskID || row.TaskOutcome != monitor.OutcomeRecoveryObserved) {
			t.Fatalf("stored link = %q outcome %q, want %q with %q", row.TaskID, row.TaskOutcome, taskID, monitor.OutcomeRecoveryObserved)
		}
		if want == 0 && (row.TaskID != "" || row.TaskOutcome != "") {
			t.Fatalf("stored link = %q outcome %q, want none", row.TaskID, row.TaskOutcome)
		}
	}
	observed := func(t *testing.T, fixture Fixture, occurrence string) monitor.Observation {
		t.Helper()
		observation := monitor.Observation{WorkspaceID: fixture.Workspace, Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: occurrence, SourceURL: "https://example.test/commit/" + occurrence, CommitSHA: occurrence, ObservedAt: time.Now().UTC().Truncate(time.Microsecond)}
		_, fresh, err := ms.Observe(fixture.Context, observation)
		requireOK(t, err)
		if !fresh {
			t.Fatalf("observation %s was not fresh", occurrence)
		}
		return observation
	}

	t.Run("task link stores the outcome", func(t *testing.T) {
		fixture := scoped(t)
		task := newAggregateTask(t, fixture)
		observation := observed(t, fixture, "recovery:commit:outcome")
		linked, err := ms.LinkTask(fixture.Context, observation.Identity(), task.ID, monitor.OutcomeRecoveryObserved)
		requireOK(t, err)
		if linked.TaskID != task.ID || linked.TaskOutcome != monitor.OutcomeRecoveryObserved || linked.State != "task_linked" {
			t.Fatalf("linked = %+v", linked)
		}
		_, _, row := annotations(t, fixture, task.ID, observation.Identity())
		if row.TaskID != task.ID || row.TaskOutcome != monitor.OutcomeRecoveryObserved {
			t.Fatalf("stored = %+v", row)
		}
	})

	t.Run("link and events commit together once", func(t *testing.T) {
		fixture := scoped(t)
		task, other := newAggregateTask(t, fixture), newAggregateTask(t, fixture)
		identity := observed(t, fixture, "recovery:commit:atomic").Identity()
		payload := map[string]any{"identity": identity}

		if _, err := ms.LinkRecoveryTask(fixture.Context, identity, "missing-task", payload, payload); err == nil {
			t.Fatal("a link to a missing task was accepted")
		}
		requireAnnotated(t, fixture, task.ID, identity, 0)

		// The last write of the transaction fails, after the link and the task
		// event, and neither of them may survive it.
		if _, err := ms.LinkRecoveryTask(fixture.Context, identity, task.ID, payload, map[string]any{"unmarshalable": make(chan int)}); err == nil {
			t.Fatal("a failed activity write was accepted")
		}
		requireAnnotated(t, fixture, task.ID, identity, 0)

		linked, err := ms.LinkRecoveryTask(fixture.Context, identity, task.ID, payload, payload)
		requireOK(t, err)
		if linked.TaskID != task.ID || linked.TaskOutcome != monitor.OutcomeRecoveryObserved {
			t.Fatalf("linked = %+v", linked)
		}
		requireAnnotated(t, fixture, task.ID, identity, 1)

		repeated, err := ms.LinkRecoveryTask(fixture.Context, identity, task.ID, payload, payload)
		requireOK(t, err)
		if repeated.TaskID != task.ID {
			t.Fatalf("repeated = %+v", repeated)
		}
		requireAnnotated(t, fixture, task.ID, identity, 1)

		if _, err = ms.LinkRecoveryTask(fixture.Context, identity, other.ID, payload, payload); err == nil {
			t.Fatal("a link to a second task was accepted")
		}
		requireAnnotated(t, fixture, task.ID, identity, 1)
		if taskEvents, _, _ := annotations(t, fixture, other.ID, identity); taskEvents != 0 {
			t.Fatalf("refused task gained %d events", taskEvents)
		}
	})

	t.Run("a later pass completes a failed link", func(t *testing.T) {
		fixture := scoped(t)
		service := mergeDeferralService(t, fixture, func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) })
		fault := &recoveryLinkFault{Store: service.Store, err: errors.New("link failed")}
		service.Store = fault
		filed, err := service.Process(fixture.Context, failure("recovery-a"))
		requireOK(t, err)
		observation := recovery("recovery-b")
		if _, err = service.Process(fixture.Context, observation); !errors.Is(err, fault.err) {
			t.Fatalf("first pass err = %v", err)
		}
		requireAnnotated(t, fixture, filed.TaskID, observation.Identity(), 0)
		fault.err = nil
		for range 2 {
			record, processErr := service.Process(fixture.Context, observation)
			requireOK(t, processErr)
			if record.TaskID != filed.TaskID {
				t.Fatalf("record = %+v, want task %s", record, filed.TaskID)
			}
			requireAnnotated(t, fixture, filed.TaskID, observation.Identity(), 1)
		}
		task, err := st.GetTask(fixture.Context, filed.TaskID)
		requireOK(t, err)
		if task.State != core.TaskQueued {
			t.Fatalf("recovery changed task state to %s", task.State)
		}
	})

	t.Run("a repeat with nothing to link never annotates a newer task", func(t *testing.T) {
		fixture := scoped(t)
		service := mergeDeferralService(t, fixture, func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) })
		observation := recovery("recovery-c")
		first, err := service.Process(fixture.Context, observation)
		requireOK(t, err)
		if first.TaskID != "" {
			t.Fatalf("first = %+v, want no task", first)
		}
		filed, err := service.Process(fixture.Context, failure("recovery-d"))
		requireOK(t, err)
		repeat, err := service.Process(fixture.Context, observation)
		requireOK(t, err)
		if repeat.TaskID != "" {
			t.Fatalf("repeat = %+v, want no task", repeat)
		}
		requireAnnotated(t, fixture, filed.TaskID, observation.Identity(), 0)
	})

	t.Run("a task that ended is not replaced by a newer one", func(t *testing.T) {
		fixture := scoped(t)
		service := mergeDeferralService(t, fixture, func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) })
		fault := &recoveryLinkFault{Store: service.Store, err: errors.New("link failed")}
		service.Store = fault
		ended, err := service.Process(fixture.Context, failure("recovery-e"))
		requireOK(t, err)
		observation := recovery("recovery-f")
		if _, err = service.Process(fixture.Context, observation); !errors.Is(err, fault.err) {
			t.Fatalf("first pass err = %v", err)
		}
		_, err = taskops.New(st).Cancel(fixture.Context, core.Intervention{TaskID: ended.TaskID, Action: core.InterventionCancel, ReasonCode: "operator-resolved"})
		requireOK(t, err)
		replacement, err := service.Process(fixture.Context, failure("recovery-g"))
		requireOK(t, err)
		if replacement.TaskID == ended.TaskID {
			t.Fatal("the failure reused the ended task")
		}
		fault.err = nil
		record, err := service.Process(fixture.Context, observation)
		requireOK(t, err)
		if record.TaskID != "" {
			t.Fatalf("record = %+v, want no task", record)
		}
		requireAnnotated(t, fixture, ended.TaskID, observation.Identity(), 0)
		requireAnnotated(t, fixture, replacement.TaskID, observation.Identity(), 0)
	})
}

// runMonitorOrigins pins MonitorOriginsForTasks on every backend: per
// monitor-filed task, the earliest linked observation of the task's source
// kind by created_at and then identity; unlinked observations, observations
// of another kind, non-monitor tasks, and another workspace's records never
// answer (component-monitor-drift; feature-operator-next-action NA-3).
func runMonitorOrigins(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	empty, err := st.MonitorOriginsForTasks(ctx, nil)
	requireOK(t, err)
	if len(empty) != 0 {
		t.Fatalf("empty request = %+v", empty)
	}
	suffix := core.NewTaskID()[:8]
	at := time.Now().UTC().Truncate(time.Second)
	sourced := func(fixture Fixture, source string) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: fixture.Workspace, Repo: "conveyor", Title: id, Source: source, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: at}
		requireOK(t, fixture.Backend.CreateTask(fixture.Context, task))
		return task
	}
	observe := func(fixture Fixture, kind monitor.SignalKind, occurrence string, observedAt time.Time, taskID string) monitor.Observation {
		t.Helper()
		observation := monitor.Observation{WorkspaceID: fixture.Workspace, Repository: "conveyor", Kind: kind, OccurrenceID: occurrence + "-" + suffix, SourceURL: "https://example.test/runs/" + occurrence, CommitSHA: "origin-sha", ObservedAt: observedAt}
		_, _, err := st.Observe(fixture.Context, observation)
		requireOK(t, err)
		if taskID != "" {
			_, err = st.LinkTask(fixture.Context, observation.Identity(), taskID, "created")
			requireOK(t, err)
		}
		return observation
	}

	filed := sourced(x, "monitor:post_merge_failure")
	ordinary := sourced(x, "mcp")
	observe(x, monitor.PostMergeFailure, "unlinked-earliest", at.Add(-3*time.Hour), "")
	observe(x, monitor.DirectPush, "other-kind", at.Add(-2*time.Hour), filed.ID)
	observe(x, monitor.PostMergeFailure, "tie-b", at, filed.ID)
	tieA := observe(x, monitor.PostMergeFailure, "tie-a", at, filed.ID)
	observe(x, monitor.PostMergeFailure, "later", at.Add(time.Hour), filed.ID)
	observe(x, monitor.PostMergeFailure, "ordinary", at.Add(-time.Hour), ordinary.ID)

	foreign := x.Workspace + "-origin-" + core.NewTaskID()[:6]
	away := store.WithWorkspace(x.Context, foreign)
	_, err = st.BootstrapWorkspaceConfig(away, &config.Config{Workspace: foreign, Repos: x.Config.Repos})
	requireOK(t, err)
	awayFixture := x
	awayFixture.Context, awayFixture.Workspace = away, foreign
	awayTask := sourced(awayFixture, "monitor:post_merge_failure")
	awayObservation := observe(awayFixture, monitor.PostMergeFailure, "away", at.Add(-4*time.Hour), awayTask.ID)

	origins, err := st.MonitorOriginsForTasks(ctx, []string{filed.ID, ordinary.ID, awayTask.ID, "missing-" + suffix})
	requireOK(t, err)
	if len(origins) != 1 {
		t.Fatalf("origins = %+v, want only the monitor-filed task", origins)
	}
	got, ok := origins[filed.ID]
	if !ok || got.Identity() != tieA.Identity() || got.TaskID != filed.ID || got.SourceURL != tieA.SourceURL || !got.ObservedAt.Equal(at) || got.WorkspaceID != x.Workspace {
		t.Fatalf("origin = %+v, want the earliest matching observation %s with the created_at tie broken by identity", got, tieA.Identity())
	}
	awayOrigins, err := st.MonitorOriginsForTasks(away, []string{awayTask.ID, filed.ID})
	requireOK(t, err)
	if len(awayOrigins) != 1 || awayOrigins[awayTask.ID].Identity() != awayObservation.Identity() {
		t.Fatalf("away origins = %+v, want only the away task's observation", awayOrigins)
	}
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

	unbound := store.WithActor(t.Context(), store.SystemActor())
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

func runMonitorMergeCommitState(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	ms := st.(monitor.Store)
	committed := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	bound := 10 * time.Minute
	assertState := func(t *testing.T, sha string, now time.Time, want monitor.MergeState, wantTask string) {
		t.Helper()
		state, taskID, err := ms.MonitorMergeCommitState(ctx, "acme/conveyor", sha, committed, now, bound)
		requireOK(t, err)
		if state != want || (wantTask == "" && taskID != "") || (wantTask != "" && taskID != wantTask) {
			t.Fatalf("state=%v task=%s want %v %s", state, taskID, want, wantTask)
		}
	}
	assertState(t, "none-sha", committed.Add(time.Minute), monitor.MergeNone, "")

	inFlight := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: inFlight.ID, Kind: "merge.requested", At: committed, Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 1})}))
	assertState(t, "other-sha", committed.Add(time.Minute), monitor.MergeInFlight, inFlight.ID)
	assertState(t, "other-sha", committed.Add(bound), monitor.MergeNone, "")

	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: inFlight.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})}))
	assertState(t, "other-sha", committed.Add(time.Minute), monitor.MergeNone, "")

	unconfirmed := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: unconfirmed.ID, Kind: "merge.requested", At: committed, Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 11})}))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: unconfirmed.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "merge_unconfirmed"})}))
	assertState(t, "unconfirmed-sha", committed.Add(time.Minute), monitor.MergeInFlight, unconfirmed.ID)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: unconfirmed.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})}))
	assertState(t, "unconfirmed-sha", committed.Add(time.Minute), monitor.MergeNone, "")

	first := newAggregateTask(t, x)
	second := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: first.ID, Kind: "merge.requested", At: committed, Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 21})}))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: second.ID, Kind: "merge.requested", At: committed.Add(time.Second), Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 22})}))
	assertState(t, "two-task-sha", committed.Add(time.Minute), monitor.MergeInFlight, first.ID)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: first.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})}))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: second.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})}))

	stuck := newAggregateTask(t, x)
	now := committed.Add(time.Minute)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: stuck.ID, Kind: "merge.requested", At: now.Add(-bound), Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 31})}))
	futureState, futureTask, err := ms.MonitorMergeCommitState(ctx, "acme/conveyor", "future-sha", now.Add(time.Hour), now, bound)
	requireOK(t, err)
	if futureState != monitor.MergeNone || futureTask != "" {
		t.Fatalf("future-dated commit state=%v task=%s", futureState, futureTask)
	}
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: stuck.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})}))
	staleReq := newAggregateTask(t, x)
	freshReq := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: staleReq.ID, Kind: "merge.requested", At: now.Add(-20 * time.Minute), Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 32})}))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: freshReq.ID, Kind: "merge.requested", At: now.Add(-time.Minute), Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 33})}))
	staleThenFresh, staleThenFreshTask, err := ms.MonitorMergeCommitState(ctx, "acme/conveyor", "stale-then-fresh-sha", now.Add(-30*time.Second), now, bound)
	requireOK(t, err)
	if staleThenFresh != monitor.MergeInFlight || staleThenFreshTask != freshReq.ID {
		t.Fatalf("stale then fresh state=%v task=%s want in-flight %s", staleThenFresh, staleThenFreshTask, freshReq.ID)
	}
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: staleReq.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})}))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: freshReq.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})}))

	otherRepo := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: otherRepo.ID, Kind: "merge.requested", At: committed, Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "pull_request": 41})}))
	assertState(t, "cross-repo-inflight-sha", committed.Add(time.Minute), monitor.MergeNone, "")

	recorded := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: recorded.ID, Kind: "merge.requested", Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 2})}))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: recorded.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 2, "head_sha": "head-sha", "merge_commit_sha": "landed-sha"})}))
	assertState(t, "landed-sha", committed.Add(time.Minute), monitor.MergeRecorded, recorded.ID)
	assertState(t, "head-sha", committed.Add(time.Minute), monitor.MergeNone, "")

	reconciled := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: reconciled.ID, Kind: "merge.reconciled", Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 3, "head_sha": "recon-head", "merge_commit_sha": "recon-landed"})}))
	assertState(t, "recon-landed", committed.Add(time.Minute), monitor.MergeRecorded, reconciled.ID)

	stale := core.Task{
		ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "stale",
		BaseBranch: "main", Branch: "conveyor/task-stale", State: core.TaskRunning,
		NextStage: core.StageImplement, ApprovedHeadSHA: "stale-head",
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	requireOK(t, st.CreateTask(ctx, stale))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: stale.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "number": 9, "head_sha": "stale-head"})}))
	assertState(t, "stale-head", committed.Add(time.Minute), monitor.MergeNone, "")

	foreign := core.Task{
		ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "app", Title: "foreign",
		BaseBranch: "main", Branch: "conveyor/task-foreign", State: core.TaskRunning,
		NextStage: core.StageImplement, CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	requireOK(t, st.CreateTask(ctx, foreign))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: foreign.ID, Kind: "merge.requested", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "pull_request": 4})}))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: foreign.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "pull_request": 4, "merge_commit_sha": "foreign-sha"})}))
	assertState(t, "foreign-sha", committed.Add(time.Minute), monitor.MergeNone, "")

	state, _, err := ms.MonitorMergeCommitState(ctx, "acme/conveyor", "", committed, committed.Add(time.Minute), bound)
	requireOK(t, err)
	if state != monitor.MergeNone {
		t.Fatalf("empty sha state=%v", state)
	}
}

func runMonitorMergeDeferral(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	ms := st.(monitor.Store)
	committed := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	bound := 10 * time.Minute
	now := committed.Add(time.Minute)
	clock := now
	const sha = "inflight-then-confirmed"
	task := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.requested", At: now, Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 12})}))

	var suppressed []string
	source := mergeDeferralGitHubSource(x, sha, committed, func() time.Time { return clock }, bound, "", &suppressed)
	service := mergeDeferralService(t, x, func() time.Time { return clock })
	poll := func(t *testing.T) {
		t.Helper()
		poller := monitor.Poller{Service: service, Repository: "conveyor", Source: source, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
		if err := poller.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	observed := func(commit string) bool {
		status, err := ms.MonitorStatus(ctx, true, clock)
		requireOK(t, err)
		for _, observation := range status.Observations {
			if observation.CommitSHA == commit {
				return true
			}
		}
		return false
	}

	poll(t)
	if _, found, err := ms.FindOpenMonitorTask(ctx, "conveyor", monitor.DirectPush); err != nil || found {
		t.Fatalf("in-flight opened a direct_push task found=%v err=%v", found, err)
	}
	if observed(sha) || len(suppressed) != 1 || suppressed[0] != "merge_in_flight" {
		t.Fatalf("in-flight observed=%v suppressed=%v", observed(sha), suppressed)
	}

	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 12, "head_sha": "head", "merge_commit_sha": sha})}))
	poll(t)
	if _, found, err := ms.FindOpenMonitorTask(ctx, "conveyor", monitor.DirectPush); err != nil || found {
		t.Fatalf("recorded merge opened a direct_push task found=%v err=%v", found, err)
	}
	if observed(sha) || len(suppressed) != 2 || suppressed[1] != "conveyor_merge_recorded" {
		t.Fatalf("recorded observed=%v suppressed=%v", observed(sha), suppressed)
	}

	const real = "real-direct-push"
	realSource := mergeDeferralGitHubSource(x, real, committed, func() time.Time { return clock }, bound, "", &suppressed)
	realPoller := monitor.Poller{Service: service, Repository: "conveyor", Source: realSource, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
	if err := realPoller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ms.FindOpenMonitorTask(ctx, "conveyor", monitor.DirectPush); err != nil || !found {
		t.Fatalf("real direct push found=%v err=%v", found, err)
	}
	if !observed(real) {
		t.Fatal("real direct push produced no observation")
	}

	const delayed = "stuck-merge-push"
	stuck := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: stuck.ID, Kind: "merge.requested", At: committed, Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 13})}))
	before := committed.Add(bound - time.Second)
	clock = before
	delayedSuppressed := []string{}
	delayedSource := mergeDeferralGitHubSource(x, delayed, committed, func() time.Time { return clock }, bound, "", &delayedSuppressed)
	delayedPoller := monitor.Poller{Service: service, Repository: "conveyor", Source: delayedSource, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
	if err := delayedPoller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if observed(delayed) || len(delayedSuppressed) != 1 || delayedSuppressed[0] != "merge_in_flight" {
		t.Fatalf("inside bound observed=%v suppressed=%v", observed(delayed), delayedSuppressed)
	}
	clock = committed.Add(bound)
	if err := delayedPoller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !observed(delayed) {
		t.Fatal("commit at the deferral bound was not reported")
	}
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: stuck.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})}))
	const failedSHA = "failed-merge-push"
	failed := newAggregateTask(t, x)
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: failed.ID, Kind: "merge.requested", Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 14})}))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: failed.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "forge_merge_failed"})}))
	clock = committed.Add(time.Minute)
	failedSource := mergeDeferralGitHubSource(x, failedSHA, committed, func() time.Time { return clock }, bound, "", new([]string))
	failedPoller := monitor.Poller{Service: service, Repository: "conveyor", Source: failedSource, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
	if err := failedPoller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !observed(failedSHA) {
		t.Fatal("merge.failed still deferred the commit")
	}

	const staleSHA = "stale-approval-head"
	stalePulls := fmt.Sprintf(`[{"number":20,"html_url":"https://github.com/acme/conveyor/pull/20","merged_at":%q,"head":{"ref":"feature/stale","sha":%q}}]`, committed.Format(time.RFC3339), staleSHA)
	staleSource := mergeDeferralGitHubSource(x, staleSHA, committed, func() time.Time { return clock }, bound, stalePulls, new([]string))
	stalePoller := monitor.Poller{Service: service, Repository: "conveyor", Source: staleSource, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
	if err := stalePoller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := ms.MonitorStatus(ctx, true, clock)
	requireOK(t, err)
	foundExternal := false
	for _, observation := range status.Observations {
		if observation.CommitSHA == staleSHA && observation.Kind == monitor.ExternalPRMerge {
			foundExternal = true
		}
	}
	if !foundExternal {
		t.Fatal("stale-approval GitHub merge was not reported as external_pr_merge")
	}

	// The repository has a tip now, so a commit dated older than the window is
	// read all the same (component-monitor-drift invariant 4).
	const oldDatedSHA = "outside-window-sha"
	oldDated := clock.Add(-16 * time.Minute)
	oldDatedSource := mergeDeferralGitHubSource(x, oldDatedSHA, oldDated, func() time.Time { return clock }, bound, "", new([]string))
	oldDatedPoller := monitor.Poller{Service: service, Repository: "conveyor", Source: oldDatedSource, StartupWindow: 15 * time.Minute, Now: func() time.Time { return clock }}
	if err := oldDatedPoller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !observed(oldDatedSHA) {
		t.Fatal("commit older than the startup window was lost despite a stored tip")
	}
}

// runMonitorTip proves the tip read, its conditional advance, and that one
// repository's status and tip never touch another's on every backend
// (component-monitor-drift invariants 4 and 15; req-delivery-and-forge AC-4.5).
func runMonitorTip(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	const one, two = "conveyor-tip-one", "conveyor-tip-two"
	tip := func(repository string) string {
		t.Helper()
		value, err := st.MonitorTip(ctx, repository)
		requireOK(t, err)
		return value
	}
	advance := func(repository, expected, next string, want bool) {
		t.Helper()
		advanced, err := st.AdvanceMonitorTip(ctx, repository, expected, next)
		requireOK(t, err)
		if advanced != want {
			t.Fatalf("advance %s from %q to %q = %v, want %v", repository, expected, next, advanced, want)
		}
	}
	if tip(one) != "" {
		t.Fatalf("a repository with no row has tip %q", tip(one))
	}
	// A non-empty expected value never creates the row.
	advance(one, "stale", "next", false)
	if tip(one) != "" {
		t.Fatalf("a refused advance stored tip %q", tip(one))
	}
	advance(one, "", "first", true)
	if tip(one) != "first" || tip(two) != "" {
		t.Fatalf("tips one=%q two=%q", tip(one), tip(two))
	}
	// A slower pass holding an old value can't move the tip backward.
	advance(one, "", "slower", false)
	advance(one, "stale", "slower", false)
	advance(one, "first", "second", true)
	advance(one, "first", "slower", false)
	if tip(one) != "second" {
		t.Fatalf("tip=%q after refused advances", tip(one))
	}
	if value, err := st.MonitorTip(store.WithWorkspace(ctx, x.Workspace+"-foreign"), one); err != nil || value != "" {
		t.Fatalf("tip crossed workspaces: %q err=%v", value, err)
	}
	if _, err := st.AdvanceMonitorTip(ctx, one, "second", ""); !errors.Is(err, monitor.ErrTipRequired) {
		t.Fatalf("an empty tip err=%v", err)
	}
	if _, err := st.AdvanceMonitorTip(ctx, "", "", "next"); !errors.Is(err, monitor.ErrRepositoryRequired) {
		t.Fatalf("an unnamed repository's advance err=%v", err)
	}
	if err := st.RecordMonitorSuccess(ctx, "", time.Now()); !errors.Is(err, monitor.ErrRepositoryRequired) {
		t.Fatalf("an unnamed repository's success err=%v", err)
	}
	if err := st.RecordMonitorFailure(ctx, "", "forge_status", "failure", time.Now()); !errors.Is(err, monitor.ErrRepositoryRequired) {
		t.Fatalf("an unnamed repository's failure err=%v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	row := func(repository string) monitor.RepositoryStatus {
		t.Helper()
		status, err := st.MonitorStatus(ctx, true, now)
		requireOK(t, err)
		for _, candidate := range status.Repositories {
			if candidate.Repository == repository {
				return candidate
			}
		}
		t.Fatalf("no status row for %s: %+v", repository, status.Repositories)
		return monitor.RepositoryStatus{}
	}
	requireOK(t, st.RecordMonitorSuccess(ctx, one, now.Add(-time.Hour)))
	requireOK(t, st.RecordMonitorSuccess(ctx, two, now))
	requireOK(t, st.RecordMonitorFailure(ctx, one, "forge_status", "one failed", now.Add(time.Minute)))
	requireOK(t, st.RecordMonitorFailure(ctx, two, "forge_rate_limited", "two failed", now.Add(2*time.Minute)))
	// A pass over one repository clears only that repository's error.
	requireOK(t, st.RecordMonitorSuccess(ctx, two, now))
	cleared, failing := row(two), row(one)
	if cleared.CurrentError != "" || cleared.ForgeErrorCategory != "" || !cleared.BackoffUntil.IsZero() || !cleared.LastSuccessfulAt.Equal(now) {
		t.Fatalf("cleared row=%+v", cleared)
	}
	if failing.CurrentError != "one failed" || failing.ForgeErrorCategory != "forge_status" || !failing.BackoffUntil.Equal(now.Add(time.Minute)) || !failing.LastSuccessfulAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("failing row=%+v", failing)
	}
	status, err := st.MonitorStatus(ctx, true, now)
	requireOK(t, err)
	if status.CurrentError != "one failed" || status.ForgeErrorCategory != "forge_status" || !status.BackoffUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("summary error=%q category=%q backoff=%s", status.CurrentError, status.ForgeErrorCategory, status.BackoffUntil)
	}
	if status.LastSuccessfulAt.After(now.Add(-time.Hour)) {
		t.Fatalf("summary success=%s is not the oldest", status.LastSuccessfulAt)
	}
	// Status writes leave the tip alone, and the tip write leaves status alone.
	if tip(one) != "second" {
		t.Fatalf("status writes moved the tip to %q", tip(one))
	}
	advance(one, "second", "third", true)
	if row(one).CurrentError != "one failed" {
		t.Fatalf("a tip write changed status: %+v", row(one))
	}
	foreign, err := st.MonitorStatus(store.WithWorkspace(ctx, x.Workspace+"-foreign"), true, now)
	requireOK(t, err)
	if len(foreign.Repositories) != 0 || foreign.CurrentError != "" {
		t.Fatalf("status crossed workspaces: %+v", foreign.Repositories)
	}
	// Later cases read the workspace summary, so this case leaves no error.
	requireOK(t, st.RecordMonitorSuccess(ctx, one, now))
}

func mergeDeferralService(t *testing.T, x Fixture, now func() time.Time) *monitor.Service {
	t.Helper()
	st := x.Backend
	service := &monitor.Service{
		Store: st.(monitor.Store), WorkspaceID: x.Workspace, Enabled: true,
		Repositories: map[string]struct{}{"conveyor": {}},
		Now:          now,
	}
	service.Intake = func(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
		if existing, found, err := st.GetTaskByIntakeKey(ctx, request.IntakeKey); err != nil || found {
			return monitor.IntakeResult{Task: existing}, err
		}
		id := core.NewTaskID()
		task := core.Task{
			ID: id, Workspace: x.Workspace, Repo: request.Repository, BaseBranch: "main",
			Branch: "conveyor/task-" + id, Source: request.Source, IntakeKey: request.IntakeKey,
			State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: now(),
		}
		return monitor.IntakeResult{Task: task, Created: true}, st.CreateTask(ctx, task)
	}
	return service
}

func mergeDeferralGitHubSource(x Fixture, sha string, committed time.Time, now func() time.Time, bound time.Duration, pulls string, suppressed *[]string) monitor.GitHubSource {
	ms := x.Backend.(monitor.Store)
	if pulls == "" {
		pulls = "[]"
	}
	parent := "parent-" + sha
	listed := fmt.Sprintf(`{"sha":%q,"html_url":"https://example/%s","parents":[{"sha":%q}],"commit":{"message":"manual","committer":{"date":%q}}}`, sha, sha, parent, committed.UTC().Format(time.RFC3339))
	return monitor.GitHubSource{
		WorkspaceID: x.Workspace, Repository: "conveyor", GitHubSlug: "acme/conveyor", Base: "main",
		MergeCommitState: func(ctx context.Context, repository, commitSHA string, at time.Time) (monitor.MergeState, string, error) {
			return ms.MonitorMergeCommitState(ctx, repository, commitSHA, at, now(), bound)
		},
		OnSuppressed: func(_ context.Context, payload map[string]any) error {
			reason, _ := payload["reason"].(string)
			*suppressed = append(*suppressed, reason)
			return nil
		},
		Run: func(_ context.Context, args ...string) ([]byte, error) {
			path := strings.Join(args, " ")
			switch {
			case strings.HasSuffix(args[3], "/commits/main"):
				return []byte(fmt.Sprintf(`{"sha":%q}`, sha)), nil
			case strings.HasSuffix(args[3], "/compare/"+sha+"...main"):
				return []byte(`{"status":"identical","total_commits":0,"commits":[]}`), nil
			case strings.HasSuffix(args[3], "...main"):
				return []byte(`{"status":"ahead","total_commits":1,"commits":[` + listed + `]}`), nil
			case strings.Contains(path, "/commits -f"):
				var since time.Time
				for i, arg := range args {
					if arg == "-f" && i+1 < len(args) && strings.HasPrefix(args[i+1], "since=") {
						parsed, parseErr := time.Parse(time.RFC3339, strings.TrimPrefix(args[i+1], "since="))
						if parseErr == nil {
							since = parsed
						}
					}
				}
				if !since.IsZero() && committed.Before(since) {
					return []byte("[]"), nil
				}
				return []byte("[" + listed + "]"), nil
			case strings.Contains(path, "/pulls"):
				return []byte(pulls), nil
			case strings.Contains(path, "compare/"+parent+"..."+sha):
				return []byte(`{"files":[{"filename":"internal/monitor/types.go"}]}`), nil
			default:
				return nil, fmt.Errorf("unexpected args %v", args)
			}
		},
	}
}
