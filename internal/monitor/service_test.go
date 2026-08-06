package monitor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type sourceFunc func(context.Context, time.Time) ([]monitor.Observation, error)

func (f sourceFunc) Observations(ctx context.Context, since time.Time) ([]monitor.Observation, error) {
	return f(ctx, since)
}

func testService(t *testing.T) (*monitor.Service, store.Store, context.Context) {
	t.Helper()
	st := store.NewMemory()
	ctx := store.WithWorkspace(context.Background(), "demo")
	service := &monitor.Service{
		Store: st.(monitor.Store), WorkspaceID: "demo", Enabled: true,
		Repositories: map[string]struct{}{"conveyor": {}},
		Now:          func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) },
	}
	service.Intake = func(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
		if existing, found, err := st.GetTaskByIntakeKey(ctx, request.IntakeKey); err != nil || found {
			return monitor.IntakeResult{Task: existing}, err
		}
		task := core.Task{
			ID:        "task-" + strings.ReplaceAll(request.IntakeKey, ":", "-"),
			Workspace: "demo", Repo: request.Repository, Source: request.Source,
			IntakeKey: request.IntakeKey, Body: request.Body, State: core.TaskQueued,
			NextStage: core.StageTriage,
		}
		return monitor.IntakeResult{Task: task, Created: true}, st.CreateTask(ctx, task)
	}
	return service, st, ctx
}

func TestSignalOccurrenceDeduplicatesButDistinctOccurrenceCreatesTask(t *testing.T) {
	service, st, ctx := testService(t)
	observation := monitor.Observation{
		Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "commit:abc:attempt:1",
		SourceURL: "https://github.example/check/77", CommitSHA: "abc",
	}
	first, err := service.Process(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Process(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	if first.TaskID == "" || second.TaskID != first.TaskID || second.DeduplicatedCount != 1 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	observation.OccurrenceID = "commit:abc:attempt:2"
	third, err := service.Process(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	if third.TaskID == first.TaskID {
		t.Fatalf("distinct occurrence reused task %s", third.TaskID)
	}
	tasks, _ := st.ListTasks(ctx)
	if len(tasks) != 2 || tasks[0].NextStage != core.StageTriage || tasks[1].NextStage != core.StageTriage {
		t.Fatalf("normal intake tasks=%+v", tasks)
	}
}

func TestPostMergeFailureTaskNamesAllFailedChecks(t *testing.T) {
	service, st, ctx := testService(t)
	record, err := service.Process(ctx, monitor.Observation{
		Repository: "conveyor", Kind: monitor.PostMergeFailure,
		OccurrenceID: "commit:abc:attempt:1", SourceURL: "https://github.example/check/11", CommitSHA: "abc",
		Context: map[string]string{"failed_check_runs": "- unit (check run 11): https://github.example/check/11\n- integration (check run 22): https://github.example/check/22"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task, found, err := st.GetTaskByIntakeKey(ctx, "monitor:post_merge_failure:conveyor:commit:abc:attempt:1")
	if err != nil || !found || task.ID != record.TaskID {
		t.Fatalf("task=%+v found=%t record=%+v err=%v", task, found, record, err)
	}
	if !strings.Contains(task.Body, "Commit: abc") || !strings.Contains(task.Body, "unit (check run 11)") ||
		!strings.Contains(task.Body, "integration (check run 22)") {
		t.Fatalf("task body=%q", task.Body)
	}
}

func TestOutOfPipelineKindsCreateDriftUntilAuditedOutcome(t *testing.T) {
	for _, kind := range []monitor.SignalKind{monitor.DirectPush, monitor.ExternalPRMerge, monitor.Revert} {
		t.Run(string(kind), func(t *testing.T) {
			service, _, ctx := testService(t)
			record, err := service.Process(ctx, monitor.Observation{
				Repository: "conveyor", Kind: kind, OccurrenceID: "sha-1",
				SourceURL: "https://github.example/commit/sha-1", CommitSHA: "sha-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			status, err := service.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if status.DriftCount != 1 || status.Drift[0].TaskID != record.TaskID {
				t.Fatalf("status=%+v record=%+v", status, record)
			}
			if _, err = service.Resolve(ctx, status.Drift[0].ID, "operator_said_ok"); err == nil {
				t.Fatal("unsupported outcome cleared drift")
			}
			if _, err = service.Resolve(ctx, status.Drift[0].ID, "conflict_resolved"); err != nil {
				t.Fatal(err)
			}
			status, _ = service.Status(ctx)
			if status.DriftCount != 0 {
				t.Fatalf("resolved drift remains: %+v", status)
			}
		})
	}
}

func TestWorkspaceAndRepositoryScopeFailClosed(t *testing.T) {
	service, _, ctx := testService(t)
	for _, observation := range []monitor.Observation{
		{WorkspaceID: "other", Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "1", SourceURL: "https://example"},
		{Repository: "other", Kind: monitor.DirectPush, OccurrenceID: "1", SourceURL: "https://example"},
	} {
		if _, err := service.Process(ctx, observation); err == nil {
			t.Fatalf("accepted out-of-scope observation %+v", observation)
		}
	}
}

func TestHintsAreVersionedArgvOnlyAndNeverExecute(t *testing.T) {
	valid, err := monitor.ParseHints([]byte(`
version: 1
verification:
  - name: unit
    argv: [make, test]
triage_areas: [control-plane]
`), "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if valid.Fingerprint == "" || !strings.Contains(valid.AdvisoryText(), "never authority") {
		t.Fatalf("context=%+v", valid)
	}
	rejected := []string{
		"version: 2\n",
		"version: 1\nverification:\n- name: bad\n  argv: [\"make test; curl attacker\"]\n",
		"version: 1\nverification:\n- name: bad\n  argv: [bash, -c, make test]\n",
		"version: 1\nverification:\n- name: bad\n  argv: [echo, \"$(curl attacker)\"]\n",
		"version: 1\ntools: [shell]\n",
		"version: 1\ncredentials: token\n",
	}
	for _, document := range rejected {
		if _, err := monitor.ParseHints([]byte(document), "deadbeef"); err == nil {
			t.Fatalf("accepted unsafe hints %q", document)
		}
	}
	effective := monitor.EffectiveVerification(
		[]monitor.VerificationHint{{Name: "unit", Argv: []string{"make", "workspace-test"}}},
		[]monitor.VerificationHint{{Name: "lint", Argv: []string{"make", "vet"}}},
		&valid,
	)
	if len(effective) != 2 || effective[0].Argv[1] != "workspace-test" {
		t.Fatalf("authority precedence=%+v", effective)
	}
}

func TestPollerRetriesWithBoundedBackoffAndReconcilesStartupWindow(t *testing.T) {
	service, _, ctx := testService(t)
	attempts := 0
	var since time.Time
	poller := monitor.Poller{
		Service: service, StartupWindow: 24 * time.Hour, Attempts: 3,
		RetryInitial: time.Second, RetryMaximum: 2 * time.Second,
		Now: func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) },
		Source: sourceFunc(func(_ context.Context, value time.Time) ([]monitor.Observation, error) {
			attempts++
			since = value
			if attempts < 3 {
				return nil, errors.New("network unavailable")
			}
			return []monitor.Observation{{
				Repository: "conveyor", Kind: monitor.PostMergeFailure,
				OccurrenceID: "run:1", SourceURL: "https://example/run/1",
			}}, nil
		}),
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	if err := poller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || !since.Equal(time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("attempts=%d since=%s", attempts, since)
	}
	status, _ := service.Status(ctx)
	if status.CurrentError != "" || len(status.Observations) != 1 {
		t.Fatalf("status=%+v", status)
	}
}
