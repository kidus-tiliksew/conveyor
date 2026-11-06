package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// TestChecksCompletedPollsOnlyForAMonitoredBase: a delivery polls once when
// its branches include the monitored repository's configured base, whichever
// branch that is, and otherwise polls nothing (component-runtime, GitHub
// webhooks).
func TestChecksCompletedPollsOnlyForAMonitoredBase(t *testing.T) {
	for _, test := range []struct {
		name       string
		repository string
		base       string
		enabled    bool
		branches   []string
		want       int32
	}{
		{name: "base branch", repository: "acme/api", base: "main", enabled: true, branches: []string{"main"}, want: 1},
		{name: "base among other branches", repository: "acme/api", base: "main", enabled: true, branches: []string{"feature/x", "main"}, want: 1},
		{name: "repository name in any case", repository: "ACME/api", base: "main", enabled: true, branches: []string{"main"}, want: 1},
		{name: "unset base means main", repository: "acme/api", base: "", enabled: true, branches: []string{"main"}, want: 1},
		{name: "configured base other than main", repository: "acme/api", base: "release/2", enabled: true, branches: []string{"release/2"}, want: 1},
		{name: "main when the base is elsewhere", repository: "acme/api", base: "release/2", enabled: true, branches: []string{"main"}},
		{name: "task branch", repository: "acme/api", base: "main", enabled: true, branches: []string{"conveyor/task-a"}},
		{name: "no branches", repository: "acme/api", base: "main", enabled: true},
		{name: "unmonitored repository", repository: "acme/other", base: "main", enabled: true, branches: []string{"main"}},
		{name: "monitor disabled", repository: "acme/api", base: "main", branches: []string{"main"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := store.NewVolatileBackend()
			defer st.Close()
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "api", GitHub: "acme/api", Base: test.base}}}
			cfg.Monitor.Enabled = test.enabled
			cfg.Monitor.Repositories = []string{"api"}
			cfg.Monitor.PollInterval = time.Minute
			cfg.Monitor.StartupWindow = time.Hour
			var polls atomic.Int32
			var request atomic.Value
			poller := &monitorPoller{Store: st, Dispatcher: dispatch.New(st, cfg, nil),
				Service: &monitor.Service{Store: st, WorkspaceID: "demo", Enabled: test.enabled, Repositories: map[string]struct{}{"api": {}}},
				Config:  func(context.Context) (*config.Config, error) { return cfg, nil },
				Runner: func(context.Context, string, string) (func(context.Context, ...string) ([]byte, error), error) {
					polls.Add(1)
					return func(_ context.Context, args ...string) ([]byte, error) {
						request.Store(strings.Join(args, " "))
						return quietMonitorRead(args...), nil
					}, nil
				},
			}
			if err := poller.ChecksCompleted(ctx, test.repository, test.branches); err != nil {
				t.Fatal(err)
			}
			if polls.Load() != test.want {
				t.Fatalf("polls=%d want %d", polls.Load(), test.want)
			}
			if test.want == 1 {
				base := test.base
				if base == "" {
					base = "main"
				}
				if got, _ := request.Load().(string); !strings.Contains(got, "-f sha="+base+" ") {
					t.Fatalf("poll read %q, want the configured base", got)
				}
			}
		})
	}
}

type recordingChecksDispatcher struct {
	gitHubWebhookDispatcher
	calls int
	err   error
}

func (d *recordingChecksDispatcher) ApplyChecksDelivery(context.Context, string, string, []string) error {
	d.calls++
	return d.err
}

// TestChecksDeliveryRunsReadinessAndPollWhateverTheOtherDoes: neither half
// hides the other, and either failure returns so the applier retries the
// delivery (component-runtime invariant 31).
func TestChecksDeliveryRunsReadinessAndPollWhateverTheOtherDoes(t *testing.T) {
	readinessFailure, pollFailure := errors.New("readiness failed"), errors.New("poll failed")
	for _, test := range []struct {
		name            string
		readiness, poll error
	}{
		{name: "both succeed"},
		{name: "readiness fails", readiness: readinessFailure},
		{name: "poll fails", poll: pollFailure},
		{name: "both fail", readiness: readinessFailure, poll: pollFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := store.NewVolatileBackend()
			defer st.Close()
			dispatcher := &recordingChecksDispatcher{err: test.readiness}
			polls := 0
			applier := newGitHubWebhookApplier(st, dispatcher, nil, func(_ context.Context, repository string, branches []string) error {
				polls++
				if repository != "acme/api" || len(branches) != 1 || branches[0] != "main" {
					t.Errorf("poll for %s %v", repository, branches)
				}
				return test.poll
			}, func(string) {}, t.Logf)
			err := applier.Handlers.Checks(t.Context(), "acme/api", "landed", []string{"main"})
			if dispatcher.calls != 1 || polls != 1 {
				t.Fatalf("readiness calls=%d polls=%d", dispatcher.calls, polls)
			}
			if !errors.Is(err, test.readiness) && test.readiness != nil || !errors.Is(err, test.poll) && test.poll != nil {
				t.Fatalf("err=%v", err)
			}
			if test.readiness == nil && test.poll == nil && err != nil {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// postMergeFixture is a merged task, a monitored repository, and the webhook
// applier the receiver hands each accepted delivery to, with a scripted forge
// whose merge commit's check runs the test sets.
type postMergeFixture struct {
	t        *testing.T
	ctx      context.Context
	st       store.Backend
	applier  *githubtrigger.WebhookApplier
	checks   atomic.Value
	failNext atomic.Int32
	reads    atomic.Int32
	intakes  atomic.Int32
	lines    atomic.Int32
}

func newPostMergeFixture(t *testing.T) *postMergeFixture {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(func() { st.Close() })
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "app", GitHub: "acme/app", Base: "main"}}}
	cfg.Monitor.Enabled = true
	cfg.Monitor.Repositories = []string{"app"}
	cfg.Monitor.PollInterval = time.Minute
	cfg.Monitor.StartupWindow = time.Hour
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "merged-task", Workspace: "demo", Repo: "app", BaseBranch: "main", Branch: "conveyor/task-merged", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "number": 41, "head_sha": "merged-head"})},
		{TaskID: task.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "pull_request": 41, "head_sha": "merged-head", "merge_commit_sha": "landed"})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	f := &postMergeFixture{t: t, ctx: ctx, st: st}
	f.checks.Store(`{"check_runs":[{"id":7,"name":"ci","conclusion":""}]}`)
	pull := fmt.Sprintf(`{"number":41,"html_url":"https://github.com/acme/app/pull/41","merged_at":%q,"merged_by":{"login":"operator"},"merge_commit_sha":"landed","head":{"ref":"conveyor/task-merged","sha":"merged-head"}}`, time.Now().UTC().Format(time.RFC3339))
	landedCommit := fmt.Sprintf(`{"sha":"landed","html_url":"https://github.com/acme/app/commit/landed","parents":[{"sha":"before"},{"sha":"merged-head"}],"commit":{"message":"Merge pull request #41","committer":{"date":%q}}}`, time.Now().UTC().Format(time.RFC3339))
	branchCommit := fmt.Sprintf(`{"sha":"merged-head","html_url":"https://github.com/acme/app/commit/merged-head","parents":[{"sha":"before"}],"commit":{"message":"work","committer":{"date":%q}}}`, time.Now().UTC().Format(time.RFC3339))
	commits := "[" + landedCommit + "," + branchCommit + "]"
	run := func(_ context.Context, args ...string) ([]byte, error) {
		request := strings.Join(args, " ")
		f.reads.Add(1)
		if f.failNext.Load() > 0 {
			f.failNext.Add(-1)
			return nil, githubtrigger.StatusError(http.StatusBadGateway, errors.New("bad gateway"))
		}
		switch {
		case strings.HasSuffix(args[3], "/commits/main"):
			return []byte(`{"sha":"landed"}`), nil
		case strings.Contains(request, "/compare/before...main"):
			// The tip stays behind the lineaged merge, and a comparison answers
			// oldest first.
			return []byte(fmt.Sprintf(`{"status":"ahead","total_commits":2,"commits":[%s,%s]}`, branchCommit, landedCommit)), nil
		case strings.Contains(request, "/commits -f"):
			return []byte(commits), nil
		case strings.Contains(request, "/commits/merged-head/check-runs"):
			t.Error("read the branch commit's check runs")
			return []byte(`{"check_runs":[]}`), nil
		case strings.Contains(request, "/check-runs"):
			return []byte(f.checks.Load().(string)), nil
		case strings.Contains(request, "/pulls/41"):
			return []byte(pull), nil
		case strings.Contains(request, "/pulls"):
			return []byte("[" + pull + "]"), nil
		case strings.Contains(request, "/contents/"):
			return nil, errors.New("HTTP 404: not found")
		}
		return nil, fmt.Errorf("unscripted request: %s", request)
	}
	service := &monitor.Service{Store: st, WorkspaceID: "demo", Enabled: true, Repositories: map[string]struct{}{"app": {}},
		Intake: func(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
			f.intakes.Add(1)
			filed := core.Task{ID: "post-merge-task", Workspace: "demo", Repo: request.Repository, Source: request.Source, IntakeKey: request.IntakeKey, Body: request.Body, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}
			return monitor.IntakeResult{Task: filed, Created: true}, st.CreateTask(ctx, filed)
		},
	}
	d := dispatch.New(st, cfg, nil)
	polls := &monitorPoller{Store: st, Dispatcher: d, Service: service,
		Config: func(context.Context) (*config.Config, error) { return cfg, nil },
		Runner: func(context.Context, string, string) (func(context.Context, ...string) ([]byte, error), error) {
			return run, nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	f.applier = newGitHubWebhookApplier(st, d, polls.PushedRef, polls.ChecksCompleted, func(string) {}, func(format string, args ...any) {
		f.lines.Add(1)
		t.Logf(format, args...)
	})
	f.applier.Sleep = func(context.Context, time.Duration) error { return nil }
	f.applier.Start(t.Context())
	return f
}

func (f *postMergeFixture) deliver(id, event, body string) {
	f.t.Helper()
	f.applier.Submit(githubtrigger.WebhookDelivery{Workspace: "demo", ID: id, Event: event, Body: []byte(body), ReceivedAt: time.Now()})
	f.applier.Wait()
}

func (f *postMergeFixture) postMergeObservations() int {
	f.t.Helper()
	status, err := (&monitor.Service{Store: f.st, WorkspaceID: "demo", Enabled: true}).Status(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	count := 0
	for _, observation := range status.Observations {
		if observation.Kind == monitor.PostMergeFailure && observation.OccurrenceID == "commit:landed:attempt:1" {
			count++
		}
	}
	return count
}

const (
	failedRunRecord = `{"check_runs":[{"id":7,"name":"ci","html_url":"https://github.com/acme/app/runs/7","conclusion":"failure","run_attempt":1}]}`
	basePushBody    = `{"ref":"refs/heads/main","after":"landed","deleted":false,"repository":{"full_name":"acme/app"}}`
	baseCheckRun    = `{"action":"completed","check_run":{"id":7,"head_sha":"landed","status":"completed","conclusion":"failure","check_suite":{"head_branch":"main"},"pull_requests":[]},"repository":{"full_name":"acme/app"}}`
)

// TestCompletedBaseCheckFilesPostMergeFailure is the case this poll exists
// for. The push's poll runs while the merge commit's check is unfinished and
// files nothing, and with no other trigger only the completed check's delivery
// reads the failure. A second delivery of the same failure files nothing more
// (req-delivery-and-forge REQ-4, AC-4.1; component-runtime, GitHub webhooks).
func TestCompletedBaseCheckFilesPostMergeFailure(t *testing.T) {
	f := newPostMergeFixture(t)
	f.deliver("push", "push", basePushBody)
	if f.postMergeObservations() != 0 || f.intakes.Load() != 0 {
		t.Fatalf("the unfinished check filed: observations=%d intakes=%d", f.postMergeObservations(), f.intakes.Load())
	}
	f.checks.Store(failedRunRecord)
	f.deliver("check-1", "check_run", baseCheckRun)
	if f.postMergeObservations() != 1 || f.intakes.Load() != 1 {
		t.Fatalf("completed check: observations=%d intakes=%d", f.postMergeObservations(), f.intakes.Load())
	}
	f.deliver("check-2", "check_run", baseCheckRun)
	if f.postMergeObservations() != 1 || f.intakes.Load() != 1 {
		t.Fatalf("repeat delivery: observations=%d intakes=%d", f.postMergeObservations(), f.intakes.Load())
	}
}

// TestCompletedBaseCheckPollFailureRetriesTheDelivery: a forge failure during
// the check's poll doesn't lose the post-merge failure, because the applier
// retries the delivery and the retried poll files it.
func TestCompletedBaseCheckPollFailureRetriesTheDelivery(t *testing.T) {
	f := newPostMergeFixture(t)
	f.checks.Store(failedRunRecord)
	// Fails all three attempts of the delivery's first poll.
	f.failNext.Store(3)
	f.deliver("check-1", "check_run", baseCheckRun)
	if f.postMergeObservations() != 1 || f.intakes.Load() != 1 || f.lines.Load() != 0 {
		t.Fatalf("after retry: observations=%d intakes=%d error lines=%d", f.postMergeObservations(), f.intakes.Load(), f.lines.Load())
	}
}

// TestExhaustedCheckPollIsLoggedAndRecoversOnRedelivery: when every retry
// fails, the failure is written as an error line rather than reported as
// applied, status keeps the error, and a redelivery files the failure
// (component-runtime invariant 31; component-monitor-drift invariant 15).
func TestExhaustedCheckPollIsLoggedAndRecoversOnRedelivery(t *testing.T) {
	f := newPostMergeFixture(t)
	f.checks.Store(failedRunRecord)
	f.failNext.Store(9)
	f.deliver("check-1", "check_run", baseCheckRun)
	if f.postMergeObservations() != 0 || f.lines.Load() == 0 {
		t.Fatalf("exhausted: observations=%d error lines=%d", f.postMergeObservations(), f.lines.Load())
	}
	status, err := (&monitor.Service{Store: f.st, WorkspaceID: "demo", Enabled: true}).Status(f.ctx)
	if err != nil || status.CurrentError == "" {
		t.Fatalf("status kept no error: %+v err=%v", status, err)
	}
	f.deliver("check-1", "check_run", baseCheckRun)
	if f.postMergeObservations() != 1 || f.intakes.Load() != 1 {
		t.Fatalf("redelivery: observations=%d intakes=%d", f.postMergeObservations(), f.intakes.Load())
	}
}

// TestTaskBranchCheckDeliveryReadsNoMonitor: a completed check on a task
// branch, the common delivery, costs the monitor no poll.
func TestTaskBranchCheckDeliveryReadsNoMonitor(t *testing.T) {
	f := newPostMergeFixture(t)
	f.deliver("branch-check", "check_run", `{"action":"completed","check_run":{"id":8,"head_sha":"elsewhere","check_suite":{"head_branch":"conveyor/task-other"},"pull_requests":[{"head":{"ref":"conveyor/task-other"}}]},"repository":{"full_name":"acme/app"}}`)
	if f.reads.Load() != 0 {
		t.Fatalf("forge reads=%d", f.reads.Load())
	}
}

// quietMonitorRead answers a monitor read of a repository with no commit in its
// window: a branch head for the read that precedes a window listing, an
// unchanged comparison from the tip that pass stores, and an empty listing.
func quietMonitorRead(args ...string) []byte {
	switch {
	case len(args) > 3 && strings.Contains(args[3], "/commits/"):
		return []byte(`{"sha":"quiet-head"}`)
	case len(args) > 3 && strings.Contains(args[3], "/compare/"):
		return []byte(`{"status":"identical","total_commits":0,"commits":[]}`)
	default:
		return []byte(`[]`)
	}
}
