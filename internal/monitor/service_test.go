package monitor_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// req-260811-228be6 AC-4.3; req-delivery-and-forge AC-4.1;
// component-monitor-drift v3. The fixture records the exact README occurrence
// reconciled by task 260915-00ccb2, including its GitHub first parent.
func TestReadmeDirectPushPreservesGovernedDriftOnRedelivery(t *testing.T) {
	const sha = "6c76388faf5fe52a8a0421c60ee9d5f407c654b8"
	const parent = "4864149e29d8db9957eac3b8295875585de606e7"
	const sourceURL = "https://github.com/kidus-tiliksew/conveyor/commit/" + sha
	for _, governedPath := range []string{"README.md", "internal/monitor/**"} {
		t.Run(governedPath, func(t *testing.T) {
			service, st, ctx := testService(t)
			content := fmt.Sprintf("# Runtime fixture\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - %s\n```", governedPath)
			design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "runtime-fixture", Title: "Runtime fixture", Category: "Component design"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
				t.Fatal(err)
			}
			source := monitor.GitHubSource{
				WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "kidus-tiliksew/conveyor",
				Run: func(_ context.Context, args ...string) ([]byte, error) {
					request := strings.Join(args, " ")
					switch {
					case strings.Contains(request, "/commits -f"):
						return []byte(fmt.Sprintf(`[{"sha":%q,"html_url":%q,"parents":[{"sha":%q}],"commit":{"message":"docs(readme): clarify Conveyor description","committer":{"date":"2026-09-15T14:08:33Z"}}}]`, sha, sourceURL, parent)), nil
					case strings.Contains(request, "/commits/"+sha+"/pulls"):
						return []byte(`[]`), nil
					case strings.Contains(request, "/compare/"+parent+"..."+sha):
						return []byte(`{"files":[{"filename":"README.md","status":"modified","additions":14,"deletions":14,"changes":28}]}`), nil
					default:
						return nil, fmt.Errorf("unexpected GitHub request: %s", request)
					}
				},
			}
			for attempt := 0; attempt < 2; attempt++ {
				// A new poller models restart: the store owns deduplication.
				poller := monitor.Poller{Service: service, Source: source, StartupWindow: 24 * time.Hour}
				if err := poller.Poll(ctx); err != nil {
					t.Fatal(err)
				}
			}
			status, err := service.Status(ctx)
			if err != nil || len(status.Observations) != 1 {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			observation := status.Observations[0]
			if observation.WorkspaceID != "demo" || observation.Repository != "conveyor" || observation.Kind != monitor.DirectPush || observation.OccurrenceID != sha || observation.CommitSHA != sha || observation.SourceURL != sourceURL || observation.DeduplicatedCount != 1 {
				t.Fatalf("lost occurrence provenance or redelivery identity: %+v", observation)
			}
			wantDrift := 1 // Repository drift remains until an audited outcome.
			if governedPath == "README.md" {
				wantDrift++
			}
			if len(status.Drift) != wantDrift {
				t.Fatalf("drift=%+v want %d records for scope %s", status.Drift, wantDrift, governedPath)
			}
			for _, drift := range status.Drift {
				if drift.SystemDesignID != "" && (drift.SystemDesignID != design.ID || drift.SystemDesignVersion != version.Version || strings.Join(drift.MatchingPaths, ",") != "README.md") {
					t.Fatalf("incorrect governed drift: %+v", drift)
				}
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil || len(tasks) != 1 || tasks[0].ID != observation.TaskID || tasks[0].State != core.TaskQueued || tasks[0].NextStage != core.StageTriage || !strings.Contains(tasks[0].Body, sourceURL) {
				t.Fatalf("ordinary task intake changed: %+v err=%v", tasks, err)
			}
			events, err := st.ListEvents(ctx, observation.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, event := range events {
				counts[event.Kind]++
			}
			if counts["monitor.occurrence_observed"] != 1 || counts["monitor.observation_deduplicated"] != 1 {
				t.Fatalf("missing occurrence audit: %+v", counts)
			}
		})
	}
}

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

func TestPostMergeFailuresShareOpenTaskAcrossCommitsAndKeepDistinctObservations(t *testing.T) {
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
	observation.OccurrenceID = "commit:def:attempt:2"
	observation.CommitSHA = "def"
	observation.Attempt = 2
	third, err := service.Process(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	if third.TaskID != first.TaskID || third.TaskOutcome != "reused" {
		t.Fatalf("distinct attempt did not reuse task: first=%+v third=%+v", first, third)
	}
	tasks, _ := st.ListTasks(ctx)
	if len(tasks) != 1 || tasks[0].NextStage != core.StageTriage {
		t.Fatalf("normal intake tasks=%+v", tasks)
	}
	status, err := service.Status(ctx)
	if err != nil || len(status.Observations) != 2 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	for _, record := range status.Observations {
		if record.TaskID != first.TaskID {
			t.Fatalf("observation not linked to shared task: %+v", record)
		}
	}
}

func TestPostMergeFailureAfterClosedTaskCreatesFreshTask(t *testing.T) {
	service, st, ctx := testService(t)
	first, err := service.Process(ctx, monitor.Observation{
		Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "commit:abc:attempt:1",
		SourceURL: "https://github.example/check/1", CommitSHA: "abc", Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).CancelTask(ctx, core.Intervention{TaskID: first.TaskID, Action: core.InterventionCancel, ReasonCode: "operator-resolved"}); err != nil {
		t.Fatal(err)
	}
	second, err := service.Process(ctx, monitor.Observation{
		Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "commit:def:attempt:1",
		SourceURL: "https://github.example/check/2", CommitSHA: "def", Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.TaskID == "" || second.TaskID == first.TaskID {
		t.Fatalf("closed task was reused: first=%+v second=%+v", first, second)
	}
}

func TestPostMergeRecoveryAnnotatesOpenTaskOnceWithoutClosing(t *testing.T) {
	service, st, ctx := testService(t)
	failure, err := service.Process(ctx, monitor.Observation{
		Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "commit:abc:attempt:1",
		SourceURL: "https://github.example/check/1", CommitSHA: "abc", Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	recovery := monitor.Observation{
		Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "recovery:commit:def",
		SourceURL: "https://github.example/commit/def", CommitSHA: "def", RecoveryObserved: true,
	}
	first, err := service.Process(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Process(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	if first.TaskID != failure.TaskID || second.TaskID != failure.TaskID {
		t.Fatalf("recovery linkage failure=%+v first=%+v second=%+v", failure, first, second)
	}
	task, err := st.GetTask(ctx, failure.TaskID)
	if err != nil || task.State != core.TaskQueued {
		t.Fatalf("recovery changed task state: task=%+v err=%v", task, err)
	}
	events, err := st.ListEvents(ctx, failure.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "monitor.recovery_observed" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("recovery event count=%d events=%+v", count, events)
	}
}

func TestObservationIntakeKeyPreservesOccurrenceIdentity(t *testing.T) {
	tests := []struct {
		name string
		one  monitor.Observation
		two  monitor.Observation
		same bool
	}{
		{
			name: "post merge attempts remain distinct",
			one:  monitor.Observation{Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "commit:abc:attempt:1", CommitSHA: "abc"},
			two:  monitor.Observation{Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "commit:abc:attempt:2", CommitSHA: "abc"},
		},
		{
			name: "exact redelivery",
			one:  monitor.Observation{Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "commit:abc:attempt:1", CommitSHA: "abc"},
			two:  monitor.Observation{Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "commit:abc:attempt:1", CommitSHA: "abc"},
			same: true,
		},
		{
			name: "post merge failures without commit",
			one:  monitor.Observation{Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "run:1"},
			two:  monitor.Observation{Repository: "conveyor", Kind: monitor.PostMergeFailure, OccurrenceID: "run:2"},
		},
		{
			name: "direct pushes",
			one:  monitor.Observation{Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "push:1", CommitSHA: "abc"},
			two:  monitor.Observation{Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "push:2", CommitSHA: "abc"},
		},
		{
			name: "external pull request merges",
			one:  monitor.Observation{Repository: "conveyor", Kind: monitor.ExternalPRMerge, OccurrenceID: "pr:1", CommitSHA: "abc"},
			two:  monitor.Observation{Repository: "conveyor", Kind: monitor.ExternalPRMerge, OccurrenceID: "pr:2", CommitSHA: "abc"},
		},
		{
			name: "reverts",
			one:  monitor.Observation{Repository: "conveyor", Kind: monitor.Revert, OccurrenceID: "revert:1", CommitSHA: "abc"},
			two:  monitor.Observation{Repository: "conveyor", Kind: monitor.Revert, OccurrenceID: "revert:2", CommitSHA: "abc"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.one.IntakeKey() == test.two.IntakeKey(); got != test.same {
				t.Fatalf("first=%q second=%q same=%t want=%t", test.one.IntakeKey(), test.two.IntakeKey(), got, test.same)
			}
		})
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
			if _, err = service.Resolve(ctx, status.Drift[0].ID, "operator_said_ok", ""); err == nil {
				t.Fatal("unsupported outcome cleared drift")
			}
			if _, err = service.Resolve(ctx, status.Drift[0].ID, "conflict_resolved", ""); err != nil {
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

func TestListUnresolvedDriftResolvesScopeAndFailsClosed(t *testing.T) {
	service, _, ctx := testService(t)
	if _, err := service.Process(ctx, monitor.Observation{Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "scope-drift", SourceURL: "https://example.test/commit/scope"}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("scope unavailable")
	service.ResolveScope = func(context.Context) (string, bool, map[string]struct{}, error) {
		return "", false, nil, sentinel
	}
	if _, err := service.ListUnresolvedDrift(ctx); !errors.Is(err, sentinel) {
		t.Fatalf("scope failure=%v", err)
	}
	called := false
	service.ResolveScope = func(context.Context) (string, bool, map[string]struct{}, error) {
		called = true
		return "demo", false, nil, nil
	}
	// Disabled polling does not hide persisted attention signals, just as Status
	// keeps drift visible when the resolved enabled flag is false.
	drift, err := service.ListUnresolvedDrift(ctx)
	if err != nil || !called || len(drift) != 1 {
		t.Fatalf("drift=%v scope called=%v err=%v", drift, called, err)
	}
}
