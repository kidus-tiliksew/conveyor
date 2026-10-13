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
	t.Run("merge commit state", func(t *testing.T) { runMonitorMergeCommitState(t, x) })
	t.Run("merge deferral poll", func(t *testing.T) { runMonitorMergeDeferral(t, x) })
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
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.requested", Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 12})}))

	var suppressed []string
	source := mergeDeferralGitHubSource(x, sha, committed, func() time.Time { return clock }, bound, "", &suppressed)
	service := mergeDeferralService(t, x, func() time.Time { return clock })
	poll := func(t *testing.T) {
		t.Helper()
		poller := monitor.Poller{Service: service, Source: source, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
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
	realPoller := monitor.Poller{Service: service, Source: realSource, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
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
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: stuck.ID, Kind: "merge.requested", Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 13})}))
	before := committed.Add(bound - time.Second)
	clock = before
	delayedSuppressed := []string{}
	delayedSource := mergeDeferralGitHubSource(x, delayed, committed, func() time.Time { return clock }, bound, "", &delayedSuppressed)
	delayedPoller := monitor.Poller{Service: service, Source: delayedSource, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
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
	failedPoller := monitor.Poller{Service: service, Source: failedSource, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
	if err := failedPoller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !observed(failedSHA) {
		t.Fatal("merge.failed still deferred the commit")
	}

	const staleSHA = "stale-approval-head"
	stalePulls := fmt.Sprintf(`[{"number":20,"html_url":"https://github.com/acme/conveyor/pull/20","merged_at":%q,"head":{"ref":"feature/stale","sha":%q}}]`, committed.Format(time.RFC3339), staleSHA)
	staleSource := mergeDeferralGitHubSource(x, staleSHA, committed, func() time.Time { return clock }, bound, stalePulls, new([]string))
	stalePoller := monitor.Poller{Service: service, Source: staleSource, StartupWindow: 24 * time.Hour, Now: func() time.Time { return clock }}
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

	const lostSHA = "outside-window-sha"
	lostCommitted := clock.Add(-16 * time.Minute)
	lostSource := mergeDeferralGitHubSource(x, lostSHA, lostCommitted, func() time.Time { return clock }, bound, "", new([]string))
	lostPoller := monitor.Poller{Service: service, Source: lostSource, StartupWindow: 15 * time.Minute, Now: func() time.Time { return clock }}
	if err := lostPoller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if observed(lostSHA) {
		t.Fatal("commit older than startup window was observed")
	}
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
	return monitor.GitHubSource{
		WorkspaceID: x.Workspace, Repository: "conveyor", GitHubSlug: "acme/conveyor",
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
				return []byte(fmt.Sprintf(`[{"sha":%q,"html_url":"https://example/%s","parents":[{"sha":%q}],"commit":{"message":"manual","committer":{"date":%q}}}]`, sha, sha, parent, committed.UTC().Format(time.RFC3339))), nil
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
