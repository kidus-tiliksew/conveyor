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
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// req-260811-228be6 AC-4.3; req-delivery-and-forge AC-4.1;
// component-monitor-drift. The fixture records the exact README occurrence
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
				WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "kidus-tiliksew/conveyor", Base: "main",
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

// External pull-request merges at an unapproved head and reverts that touch a
// governed path raise drift on that System Design document regardless of task
// context, through the poller, source, and service (req-260811-228be6 AC-4.3;
// component-monitor-drift). A repeated observation records no second drift.
func TestExternalMergeAndRevertRaiseGovernedDocumentDrift(t *testing.T) {
	for _, test := range []struct {
		name, message, pulls, occurrence string
		kind                             monitor.SignalKind
	}{
		{name: "external merge", message: "Merge pull request #41 from contributor/feature", kind: monitor.ExternalPRMerge, occurrence: "pr:41",
			pulls: `[{"number":41,"html_url":"https://github.com/kidus-tiliksew/conveyor/pull/41","merged_at":"2026-09-15T14:08:00Z","head":{"ref":"contributor/feature","sha":"feature-head"}}]`},
		{name: "revert", message: "Revert \"feature\"", kind: monitor.Revert, occurrence: "observed-sha", pulls: `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, st, ctx := testService(t)
			content := "# Monitor fixture\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/monitor/**\n```"
			design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "monitor-fixture", Title: "Monitor fixture", Category: "Component design"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
				t.Fatal(err)
			}
			compares := 0
			source := monitor.GitHubSource{
				WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "kidus-tiliksew/conveyor", Base: "main",
				ResolveTask: func(context.Context, string, int) (string, bool, error) { return "", false, nil },
				Run: func(_ context.Context, args ...string) ([]byte, error) {
					request := strings.Join(args, " ")
					switch {
					case strings.Contains(request, "/commits -f"):
						return []byte(fmt.Sprintf(`[{"sha":"observed-sha","html_url":"https://github.com/kidus-tiliksew/conveyor/commit/observed-sha","parents":[{"sha":"first-parent"},{"sha":"feature-head"}],"commit":{"message":%q,"committer":{"date":"2026-09-15T14:08:33Z"}}}]`, test.message)), nil
					case strings.Contains(request, "/commits/observed-sha/pulls"):
						return []byte(test.pulls), nil
					case strings.Contains(request, "/compare/first-parent...observed-sha"):
						compares++
						return []byte(`{"files":[{"filename":"internal/monitor/types.go"},{"filename":"web/src/app.tsx"}]}`), nil
					default:
						return nil, fmt.Errorf("unexpected GitHub request: %s", request)
					}
				},
			}
			for attempt := 0; attempt < 2; attempt++ {
				poller := monitor.Poller{Service: service, Source: source, StartupWindow: 24 * time.Hour}
				if err := poller.Poll(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if compares != 2 {
				t.Fatalf("first-parent comparisons=%d", compares)
			}
			status, err := service.Status(ctx)
			if err != nil || len(status.Observations) != 1 {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			observation := status.Observations[0]
			if observation.Kind != test.kind || observation.OccurrenceID != test.occurrence || observation.DeduplicatedCount != 1 {
				t.Fatalf("observation=%+v", observation)
			}
			repositoryDrift, documentDrift := 0, 0
			for _, drift := range status.Drift {
				switch drift.SystemDesignID {
				case "":
					repositoryDrift++
				case design.ID:
					documentDrift++
					if drift.SystemDesignVersion != version.Version || drift.Kind != test.kind || drift.TaskID != observation.TaskID ||
						strings.Join(drift.MatchingPaths, ",") != "internal/monitor/types.go" {
						t.Fatalf("document drift=%+v", drift)
					}
				default:
					t.Fatalf("unexpected drift=%+v", drift)
				}
			}
			if repositoryDrift != 1 || documentDrift != 1 || status.DriftCount != 2 {
				t.Fatalf("drift=%+v count=%d", status.Drift, status.DriftCount)
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
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
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
			service, st, ctx := testService(t)
			record, err := service.Process(ctx, monitor.Observation{
				Repository: "conveyor", Kind: kind, OccurrenceID: "sha-1",
				SourceURL: "https://github.example/commit/sha-1", CommitSHA: "sha-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			// The drift task asks for a requirement revision; the retired
			// amendment wording must not return (req-delivery-and-forge AC-4.3;
			// component-monitor-drift; DEC-48).
			task, err := st.GetTask(ctx, record.TaskID)
			if err != nil || !strings.Contains(task.Body, "propose a requirement revision") || strings.Contains(task.Body, "amendment") {
				t.Fatalf("drift task body=%q err=%v", task.Body, err)
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

// failingStatusStore fails the status write for a failed pass.
type failingStatusStore struct {
	monitor.Store
	failureErr error
}

func (s *failingStatusStore) RecordMonitorFailure(ctx context.Context, category, detail string, backoff time.Time) error {
	if s.failureErr != nil {
		return s.failureErr
	}
	return s.Store.RecordMonitorFailure(ctx, category, detail, backoff)
}

func directPush(sha string) monitor.Observation {
	return monitor.Observation{
		Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: sha, CommitSHA: sha,
		SourceURL: "https://example/commit/" + sha,
	}
}

func staticSource(observations ...monitor.Observation) monitor.Source {
	return sourceFunc(func(context.Context, time.Time) ([]monitor.Observation, error) {
		return observations, nil
	})
}

// failIntakeFor fails intake for the named intake keys while the returned
// switch holds true.
func failIntakeFor(service *monitor.Service, failure error, intakeKeys ...string) *bool {
	failing := true
	intake := service.Intake
	service.Intake = func(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
		for _, key := range intakeKeys {
			if failing && request.IntakeKey == key {
				return monitor.IntakeResult{}, failure
			}
		}
		return intake(ctx, request)
	}
	return &failing
}

// A failed observation strands no newer one, the pass records and returns the
// failure, and only a later clean pass records success
// (component-monitor-drift, State and failure; req-delivery-and-forge AC-4.5).
func TestPollContinuesPastFailedObservationAndRecordsFailure(t *testing.T) {
	service, st, ctx := testService(t)
	older, newer := directPush("older"), directPush("newer")
	errIntake := errors.New("intake unavailable")
	failing := failIntakeFor(service, errIntake, "monitor:direct_push:conveyor:older")
	poller := monitor.Poller{Service: service, Source: staticSource(older, newer), StartupWindow: time.Hour}

	err := poller.Poll(ctx)
	if !errors.Is(err, errIntake) || !strings.Contains(err.Error(), "direct_push:conveyor:older") {
		t.Fatalf("poll err=%v", err)
	}
	status, statusErr := service.Status(ctx)
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if !strings.Contains(status.CurrentError, "direct_push:conveyor:older") || !strings.Contains(status.CurrentError, errIntake.Error()) {
		t.Fatalf("current_error=%q", status.CurrentError)
	}
	if status.ForgeErrorCategory != "" || status.BackoffUntil.IsZero() {
		t.Fatalf("category=%q backoff=%s", status.ForgeErrorCategory, status.BackoffUntil)
	}
	if !status.LastSuccessfulAt.IsZero() {
		t.Fatalf("a failed pass advanced last_successful_observation to %s", status.LastSuccessfulAt)
	}
	linked := map[string]string{}
	for _, observation := range status.Observations {
		linked[observation.OccurrenceID] = observation.TaskID
	}
	if linked["newer"] == "" {
		t.Fatalf("the newer observation was stranded: %+v", status.Observations)
	}
	if taskID, observed := linked["older"]; !observed || taskID != "" {
		t.Fatalf("the failed observation must stay recorded with no task: %+v", status.Observations)
	}
	if status.DriftCount != 1 || status.Drift[0].ID != newer.Identity() {
		t.Fatalf("drift=%+v", status.Drift)
	}

	// Outside a pass, an observation neither clears the failure nor counts as
	// a pass success.
	if _, err = service.Process(ctx, directPush("submitted")); err != nil {
		t.Fatal(err)
	}
	if status, err = service.Status(ctx); err != nil || status.CurrentError == "" || !status.LastSuccessfulAt.IsZero() {
		t.Fatalf("a lone observation changed pass status: %+v err=%v", status, err)
	}

	*failing = false
	if err = poller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if status, err = service.Status(ctx); err != nil || status.CurrentError != "" || status.LastSuccessfulAt.IsZero() {
		t.Fatalf("a clean pass did not clear the failure: %+v err=%v", status, err)
	}
	if status.DriftCount != 3 {
		t.Fatalf("the retried observation recorded no drift: %+v", status.Drift)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
}

// Every failed observation in a pass is named, not only the first, and a
// credential in a failure never reaches status or the returned text.
func TestPollJoinsEveryFailureAndRedactsItsText(t *testing.T) {
	service, _, ctx := testService(t)
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	errFirst, errSecond := errors.New("first failed "+secret), errors.New("second failed")
	intake := service.Intake
	service.Intake = func(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
		switch request.IntakeKey {
		case "monitor:revert:conveyor:one":
			return monitor.IntakeResult{}, errFirst
		case "monitor:revert:conveyor:two":
			return monitor.IntakeResult{}, errSecond
		}
		return intake(ctx, request)
	}
	revert := func(sha string) monitor.Observation {
		observation := directPush(sha)
		observation.Kind = monitor.Revert
		return observation
	}
	poller := monitor.Poller{Service: service, Source: staticSource(revert("one"), revert("two"), revert("three")), StartupWindow: time.Hour}
	err := poller.Poll(ctx)
	if !errors.Is(err, errFirst) || !errors.Is(err, errSecond) {
		t.Fatalf("poll err=%v", err)
	}
	status, statusErr := service.Status(ctx)
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	for _, text := range []string{err.Error(), status.CurrentError} {
		if strings.Contains(text, secret) || strings.Contains(text, "\n") {
			t.Fatalf("failure text not redacted to one line: %q", text)
		}
		if !strings.Contains(text, "revert:conveyor:one") || !strings.Contains(text, "revert:conveyor:two") {
			t.Fatalf("failure text does not name each observation: %q", text)
		}
	}
}

type secretSource struct {
	secrets []string
	err     error
}

func (s secretSource) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s.secrets, s.err
}

// A configured secret that matches no built-in pattern is redacted from a
// failed pass's status and returned text, and a failed secret lookup still
// leaves a record that carries no failure text.
func TestPollRedactsConfiguredSecretsFromPassFailure(t *testing.T) {
	const opaque = "plain-words-no-pattern-matches"
	errIntake := errors.New("intake refused " + opaque)
	t.Run("configured secret", func(t *testing.T) {
		service, _, ctx := testService(t)
		service.RedactionSecrets = secretSource{secrets: []string{opaque}}
		failIntakeFor(service, errIntake, "monitor:direct_push:conveyor:only")
		poller := monitor.Poller{Service: service, Source: staticSource(directPush("only")), StartupWindow: time.Hour}
		err := poller.Poll(ctx)
		if !errors.Is(err, errIntake) {
			t.Fatalf("poll err=%v", err)
		}
		status, statusErr := service.Status(ctx)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.CurrentError == "" || strings.Contains(status.CurrentError, opaque) || strings.Contains(err.Error(), opaque) {
			t.Fatalf("configured secret reached the failure text: status=%q err=%q", status.CurrentError, err)
		}
	})
	t.Run("secret lookup fails", func(t *testing.T) {
		service, _, ctx := testService(t)
		errLookup := errors.New("secrets unavailable")
		service.RedactionSecrets = secretSource{err: errLookup}
		failIntakeFor(service, errIntake, "monitor:direct_push:conveyor:only")
		poller := monitor.Poller{Service: service, Source: staticSource(directPush("only")), StartupWindow: time.Hour}
		err := poller.Poll(ctx)
		if !errors.Is(err, errIntake) || !errors.Is(err, errLookup) {
			t.Fatalf("poll err=%v", err)
		}
		status, statusErr := service.Status(ctx)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.CurrentError == "" || strings.Contains(status.CurrentError, opaque) || strings.Contains(err.Error(), opaque) {
			t.Fatalf("unredacted failure text: status=%q err=%q", status.CurrentError, err)
		}
	})
}

// A repository drift write that fails after the link fails the pass with the
// observation linked, and the next pass records it with no second task
// (req-delivery-and-forge AC-4.10).
func TestPollRetriesDriftWriteThatFailedAfterLinking(t *testing.T) {
	service, st, ctx := testService(t)
	errWrite := errors.New("drift write unavailable")
	failing := &failingDriftWrites{Store: service.Store, err: errWrite, identity: directPush("linked").Identity()}
	service.Store = failing
	newer := directPush("after-linked")
	poller := monitor.Poller{Service: service, Source: staticSource(directPush("linked"), newer), StartupWindow: time.Hour}
	if err := poller.Poll(ctx); !errors.Is(err, errWrite) {
		t.Fatalf("poll err=%v", err)
	}
	status, err := service.Status(ctx)
	if err != nil || status.DriftCount != 1 || status.Drift[0].ID != newer.Identity() || status.CurrentError == "" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	failing.err = nil
	if err = poller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if status, err = service.Status(ctx); err != nil || status.DriftCount != 2 || status.CurrentError != "" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if tasks, listErr := st.ListTasks(ctx); listErr != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, listErr)
	}
}

type failingDriftWrites struct {
	monitor.Store
	err      error
	identity string
}

func (s *failingDriftWrites) RecordDrift(ctx context.Context, drift monitor.Drift) (monitor.Drift, bool, error) {
	if s.err != nil && drift.ID == s.identity {
		return monitor.Drift{}, false, s.err
	}
	return s.Store.RecordDrift(ctx, drift)
}

// A canceled poll stops, keeps its record of the failure, and records no
// success for the observations it never processed.
func TestPollCanceledMidPassRecordsFailureNotSuccess(t *testing.T) {
	service, _, ctx := testService(t)
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	intake := service.Intake
	service.Intake = func(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
		result, err := intake(ctx, request)
		cancel()
		return result, err
	}
	poller := monitor.Poller{Service: service, Source: staticSource(directPush("first"), directPush("second")), StartupWindow: time.Hour}
	if err := poller.Poll(pollCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("poll err=%v", err)
	}
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentError == "" || !status.LastSuccessfulAt.IsZero() {
		t.Fatalf("status=%+v", status)
	}
	for _, observation := range status.Observations {
		if observation.OccurrenceID == "second" {
			t.Fatalf("a canceled pass processed a later observation: %+v", status.Observations)
		}
	}
}

// A status write that fails joins the returned error, so the failed pass is
// never reported as only its first cause or as nothing at all.
func TestPollReturnsFailureToRecordPassFailure(t *testing.T) {
	service, _, ctx := testService(t)
	errIntake, errStatus := errors.New("intake unavailable"), errors.New("status unavailable")
	failIntakeFor(service, errIntake, "monitor:direct_push:conveyor:only")
	service.Store = &failingStatusStore{Store: service.Store, failureErr: errStatus}
	poller := monitor.Poller{Service: service, Source: staticSource(directPush("only")), StartupWindow: time.Hour}
	err := poller.Poll(ctx)
	if !errors.Is(err, errIntake) || !errors.Is(err, errStatus) {
		t.Fatalf("poll err=%v", err)
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

// A lagging commit-to-pull-request lookup on a recorded merge's second parent
// files no task and records no drift through the poller, source, and service,
// and repeated polls keep it that way, while a direct push in the same window
// still files its task (req-260811-228be6 AC-4.1, AC-4.3; task 261104-c2a830).
func TestLaggingSecondParentLookupFilesNoTaskOrDrift(t *testing.T) {
	service, st, ctx := testService(t)
	content := "# Governed\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/monitor/**\n```"
	design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "governed", Title: "Governed", Category: "Component design"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	commit := func(sha, parents string) string {
		return fmt.Sprintf(`{"sha":%q,"html_url":"https://github.com/acme/conveyor/commit/%s","parents":[%s],"commit":{"message":"work","committer":{"date":"2026-07-28T11:00:00Z"}}}`, sha, sha, parents)
	}
	listing := commit("merge", `{"sha":"before"},{"sha":"branch-head"}`) + "," + commit("branch-head", `{"sha":"before"}`)
	source := monitor.GitHubSource{
		WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "acme/conveyor", Base: "main",
		MergeCommitState: func(_ context.Context, _, sha string, _ time.Time) (monitor.MergeState, string, error) {
			if sha == "merge" {
				return monitor.MergeRecorded, "delivering-task", nil
			}
			return monitor.MergeNone, "", nil
		},
		OnSuppressed: func(ctx context.Context, payload map[string]any) error {
			return service.Store.AuditMonitor(ctx, "monitor.suppressed", payload)
		},
		Run: func(_ context.Context, args ...string) ([]byte, error) {
			request := strings.Join(args, " ")
			switch {
			case strings.Contains(request, "/commits -f"):
				return []byte("[" + listing + "]"), nil
			case strings.Contains(request, "/pulls"):
				return []byte(`[]`), nil
			case strings.Contains(request, "/compare/"):
				return []byte(`{"files":[{"filename":"internal/monitor/types.go"}]}`), nil
			default:
				return nil, fmt.Errorf("unexpected GitHub request: %s", request)
			}
		},
	}
	poll := func() {
		t.Helper()
		poller := monitor.Poller{Service: service, Source: source, StartupWindow: 24 * time.Hour}
		if err := poller.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	poll()
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 || len(status.Observations) != 0 || len(status.Drift) != 0 {
		t.Fatalf("tasks=%+v observations=%+v drift=%+v", tasks, status.Observations, status.Drift)
	}
	audited := false
	for _, activity := range status.Activity {
		if activity.Kind == "monitor.suppressed" && activity.Payload["reason"] == "outside_first_parent" && activity.Payload["commit_sha"] == "branch-head" {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("no outside_first_parent audit for the branch commit: %+v", status.Activity)
	}

	// The same window with a real direct push on first-parent history files
	// its one task, so the assertions above aren't vacuous.
	listing = commit("direct", `{"sha":"merge"}`) + "," + listing
	poll()
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err = st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || len(status.Observations) != 1 || status.Observations[0].OccurrenceID != "direct" || len(status.Drift) != 2 {
		t.Fatalf("direct push: tasks=%+v observations=%+v drift=%+v", tasks, status.Observations, status.Drift)
	}
}

// The review's graph through the poller, source, and service. M has parents
// [X, B], X and B share the parent D, and X is older than the window. While the
// ancestry read fails, the pass files nothing and status states the failure.
// The next pass files D's direct push with its governed drift and nothing for
// B (req-260811-228be6 AC-4.3; req-delivery-and-forge REQ-4, AC-4.4, AC-4.5).
func TestSharedAncestryDirectPushFilesTaskAndDriftAfterFailedRead(t *testing.T) {
	service, st, ctx := testService(t)
	content := "# Governed\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/monitor/**\n```"
	design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "governed", Title: "Governed", Category: "Component design"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	commit := func(sha, date, parents string) string {
		return fmt.Sprintf(`{"sha":%q,"html_url":"https://github.com/acme/conveyor/commit/%s","parents":[%s],"commit":{"message":"work","committer":{"date":%q}}}`, sha, sha, parents, date)
	}
	listing := "[" + commit("M", "2026-07-28T11:00:00Z", `{"sha":"X"},{"sha":"B"}`) + "," +
		commit("B", "2026-07-28T10:00:00Z", `{"sha":"D"}`) + "," +
		commit("D", "2026-07-28T09:00:00Z", `{"sha":"A"}`) + "]"
	readFails := true
	source := monitor.GitHubSource{
		WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "acme/conveyor", Base: "main",
		MergeCommitState: func(_ context.Context, _, sha string, _ time.Time) (monitor.MergeState, string, error) {
			if sha == "M" {
				return monitor.MergeRecorded, "delivering-task", nil
			}
			return monitor.MergeNone, "", nil
		},
		OnSuppressed: func(ctx context.Context, payload map[string]any) error {
			return service.Store.AuditMonitor(ctx, "monitor.suppressed", payload)
		},
		Run: func(_ context.Context, args ...string) ([]byte, error) {
			request := strings.Join(args, " ")
			switch {
			case strings.Contains(request, "/commits -f"):
				return []byte(listing), nil
			case strings.Contains(request, "/pulls"):
				return []byte(`[]`), nil
			case strings.Contains(request, "/compare/X..."):
				if readFails {
					return nil, githubtrigger.StatusError(502, errors.New("bad gateway"))
				}
				if strings.Contains(request, "/compare/X...D") {
					return []byte(`{"status":"behind"}`), nil
				}
				return []byte(`{"status":"diverged"}`), nil
			case strings.Contains(request, "/compare/A...D"):
				return []byte(`{"files":[{"filename":"internal/monitor/types.go"}]}`), nil
			case strings.HasSuffix(args[3], "/commits/X"):
				return []byte(`{"sha":"X","parents":[{"sha":"D"}]}`), nil
			default:
				return nil, fmt.Errorf("unexpected GitHub request: %s", request)
			}
		},
	}
	poller := monitor.Poller{Service: service, Source: source, StartupWindow: 24 * time.Hour, Attempts: 1,
		Now: func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) }}
	if err := poller.Poll(ctx); err == nil {
		t.Fatal("a failed ancestry read reported a successful poll")
	}
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentError == "" || !status.LastSuccessfulAt.IsZero() || len(tasks) != 0 || len(status.Observations) != 0 || len(status.Drift) != 0 {
		t.Fatalf("failed read: status=%+v tasks=%+v", status, tasks)
	}
	for _, activity := range status.Activity {
		if activity.Kind == "monitor.suppressed" {
			t.Fatalf("a commit was classified from a failed read: %+v", activity)
		}
	}

	readFails = false
	if err := poller.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err = st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentError != "" || status.LastSuccessfulAt.IsZero() {
		t.Fatalf("the next pass did not clear the failure: %+v", status)
	}
	if len(tasks) != 1 || len(status.Observations) != 1 || status.Observations[0].Kind != monitor.DirectPush || status.Observations[0].OccurrenceID != "D" {
		t.Fatalf("tasks=%+v observations=%+v", tasks, status.Observations)
	}
	governed := false
	for _, drift := range status.Drift {
		if drift.SystemDesignID == design.ID && strings.Join(drift.MatchingPaths, ",") == "internal/monitor/types.go" {
			governed = true
		}
	}
	if !governed || len(status.Drift) != 2 {
		t.Fatalf("D lost its governed drift: %+v", status.Drift)
	}
	audited := map[string]bool{}
	for _, activity := range status.Activity {
		if activity.Kind == "monitor.suppressed" && activity.Payload["reason"] == "outside_first_parent" {
			sha, _ := activity.Payload["commit_sha"].(string)
			audited[sha] = true
		}
	}
	if !audited["B"] || audited["D"] {
		t.Fatalf("outside_first_parent audits=%v", audited)
	}
}
