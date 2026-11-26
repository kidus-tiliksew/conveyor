package storetest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// GitHub deployment reports on every backend (component-production-feedback
// invariants 1, 6, and 10; feature-production-feedback-live FB-4;
// req-production-feedback AC-1.5 and AC-11.1; req-environments AC-1.9).

func (e feedbackEnvironment) github(offset time.Duration, repository, commit string) store.FeedbackGitHubDeploy {
	return store.FeedbackGitHubDeploy{EnvironmentID: e.id, Repository: repository, Commit: commit, ReportedAt: feedbackTime(offset)}
}

func (e feedbackEnvironment) report(t *testing.T, deploy store.FeedbackGitHubDeploy, want bool) {
	t.Helper()
	recorded, err := e.x.Backend.RecordFeedbackGitHubDeploy(e.x.Context, deploy)
	requireOK(t, err)
	if recorded != want {
		t.Fatalf("recorded=%v, want %v for %+v", recorded, want, deploy)
	}
}

// runFeedbackGitHubDeploy records a report that replaces only the running
// versions of the service named after the repository, at the report's time,
// with the source GitHub, and leaves the newest signal and every key alone.
func runFeedbackGitHubDeploy(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	app := feedbackService("app", "@1", "@2")
	app.Repository = "app"
	e.send(t, 0, app, feedbackService("web", "@3"))
	newest, ok := e.newest(t)
	if !ok {
		t.Fatal("no newest signal")
	}
	keys := environmentState(t, x)

	e.report(t, e.github(20*time.Second, "app", strings.ToUpper(feedbackCommit("a"))), true)
	e.requireRunning(t, "app="+feedbackCommit("a")+"+20", "web=@3+0")
	e.requireDeploys(t, "app=@1+0-20", "app=@2+0-20", "app="+feedbackCommit("a")+"+20", "web=@3+0")
	github := store.FeedbackOrigin{Source: store.FeedbackSourceGitHub, Actor: core.SystemActorID}
	for _, deploy := range e.deploys(t) {
		switch deploy.Service {
		case "app":
			if deploy.Commit == feedbackCommit("a") {
				if unnumbered(deploy.Origin) != github || deploy.Repository != "app" || deploy.Version != "" || !deploy.DeployedAt.Equal(feedbackTime(20*time.Second)) {
					t.Fatalf("GitHub deploy=%+v", deploy)
				}
				continue
			}
			if deploy.StopOrigin == nil || unnumbered(*deploy.StopOrigin) != github {
				t.Fatalf("replaced deploy=%+v, want a stop from GitHub", deploy)
			}
		case "web":
			if deploy.StoppedAt != nil {
				t.Fatalf("another service's deploy=%+v changed", deploy)
			}
		}
	}
	if after, ok := e.newest(t); !ok || after != newest {
		t.Fatalf("newest signal=%+v, want %+v unchanged", after, newest)
	}
	if after := environmentState(t, x); after != keys {
		t.Fatalf("a GitHub report changed an environment or key:\nbefore %s\nafter  %s", keys, after)
	}

	// The same report again writes nothing.
	before := e.state(t)
	e.report(t, e.github(20*time.Second, "app", strings.ToUpper(feedbackCommit("a"))), false)
	if after := e.state(t); after != before {
		t.Fatalf("a repeated report changed the stored state:\nbefore %s\nafter  %s", before, after)
	}
	// A later report of the same commit is a distinct redeploy at its own
	// time.
	e.report(t, e.github(30*time.Second, "app", feedbackCommit("a")), true)
	e.requireRunning(t, "app="+feedbackCommit("a")+"+30", "web=@3+0")
	e.requireDeploys(t, "app=@1+0-20", "app=@2+0-20", "app="+feedbackCommit("a")+"+20-30", "app="+feedbackCommit("a")+"+30", "web=@3+0")

	// A new commit replaces the GitHub deploy.
	e.report(t, e.github(40*time.Second, "app", feedbackCommit("b")), true)
	e.requireRunning(t, "app="+feedbackCommit("b")+"+40", "web=@3+0")
	e.requireDeploys(t, "app=@1+0-20", "app=@2+0-20", "app="+feedbackCommit("a")+"+20-30", "app="+feedbackCommit("a")+"+30-40", "app="+feedbackCommit("b")+"+40", "web=@3+0")
}

// runFeedbackGitHubRedeploy covers two successful reports of one commit at two
// times with no signal between them. Each keeps its own deploy record and
// time, both come from GitHub, and a repeat of either writes nothing.
func runFeedbackGitHubRedeploy(t *testing.T, x Fixture) {
	// The same reports give the same records in every arrival order.
	a := feedbackCommit("a")
	for _, c := range []struct {
		name    string
		order   []time.Duration
		deploys []string
	}{
		{"OldestFirst", []time.Duration{20 * time.Second, 50 * time.Second}, []string{"app=" + a + "+20-50", "app=" + a + "+50"}},
		{"NewestFirst", []time.Duration{50 * time.Second, 20 * time.Second}, []string{"app=" + a + "+20-50", "app=" + a + "+50"}},
		{"ThreeInOrder", []time.Duration{10 * time.Second, 20 * time.Second, 50 * time.Second}, []string{"app=" + a + "+10-20", "app=" + a + "+20-50", "app=" + a + "+50"}},
		{"ThreeMiddleLast", []time.Duration{10 * time.Second, 50 * time.Second, 20 * time.Second}, []string{"app=" + a + "+10-20", "app=" + a + "+20-50", "app=" + a + "+50"}},
		{"ThreeNewestFirst", []time.Duration{50 * time.Second, 20 * time.Second, 10 * time.Second}, []string{"app=" + a + "+10-20", "app=" + a + "+20-50", "app=" + a + "+50"}},
		{"ThreeOldestLast", []time.Duration{20 * time.Second, 10 * time.Second, 50 * time.Second}, []string{"app=" + a + "+10-20", "app=" + a + "+20-50", "app=" + a + "+50"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newFeedbackEnvironment(t, x, "Production "+c.name, "watcher")
			for _, at := range c.order {
				e.report(t, e.github(at, "app", a), true)
			}
			e.requireRunning(t, "app="+a+"+50")
			e.requireDeploys(t, c.deploys...)
			github := store.FeedbackOrigin{Source: store.FeedbackSourceGitHub, Actor: core.SystemActorID}
			for _, deploy := range e.deploys(t) {
				if unnumbered(deploy.Origin) != github || (deploy.StopOrigin != nil && unnumbered(*deploy.StopOrigin) != github) {
					t.Fatalf("deploy=%+v, want GitHub origins", deploy)
				}
				if deploy.StoppedAt == nil && !deploy.DeployedAt.Equal(feedbackTime(50*time.Second)) {
					t.Fatalf("running deploy=%+v, want it to keep the newest report time", deploy)
				}
			}
			if _, ok := e.newest(t); ok {
				t.Fatal("a GitHub report recorded a newest signal")
			}
			before := e.state(t)
			for _, at := range c.order {
				e.report(t, e.github(at, "app", a), false)
			}
			if after := e.state(t); after != before {
				t.Fatalf("a repeated report changed the stored state:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
	// A repeat writes nothing even when a report of another commit at the
	// same time came between them.
	t.Run("RepeatAfterAnotherCommit", func(t *testing.T) {
		e := newFeedbackEnvironment(t, x, "Production repeat", "watcher")
		b := feedbackCommit("b")
		e.report(t, e.github(20*time.Second, "app", a), true)
		e.report(t, e.github(20*time.Second, "app", b), true)
		e.requireRunning(t, "app="+b+"+20")
		before := e.state(t)
		e.report(t, e.github(20*time.Second, "app", a), false)
		if after := e.state(t); after != before {
			t.Fatalf("a repeated report changed the stored state:\nbefore %s\nafter  %s", before, after)
		}
	})
}

// runFeedbackGitHubBoundaries proves that a GitHub report is a boundary of
// the deploy history: a signal that arrives later with an earlier time ends
// at the GitHub report, also when that report was a redeploy of the running
// commit or when an earlier report of the same commit arrived after it. A
// GitHub deploy keeps its time and origin in every case.
func runFeedbackGitHubBoundaries(t *testing.T, x Fixture) {
	t.Run("RepeatedCommit", func(t *testing.T) {
		e := newFeedbackEnvironment(t, x, "Production", "watcher")
		e.report(t, e.github(20*time.Second, "app", feedbackCommit("a")), true)
		e.report(t, e.github(30*time.Second, "app", feedbackCommit("a")), true)
		// The watcher reports another version at 25, after both reports were
		// stored. GitHub reported the commit at 30, so it runs again from 30.
		e.send(t, 25*time.Second, feedbackService("app", "@9"))
		e.requireRunning(t, "app="+feedbackCommit("a")+"+30")
		e.requireDeploys(t, "app="+feedbackCommit("a")+"+20-25", "app=@9+25-30", "app="+feedbackCommit("a")+"+30")
		for _, deploy := range e.deploys(t) {
			if deploy.StoppedAt == nil && unnumbered(deploy.Origin) != (store.FeedbackOrigin{Source: store.FeedbackSourceGitHub, Actor: core.SystemActorID}) {
				t.Fatalf("running deploy=%+v, want it dated by the GitHub report", deploy)
			}
		}
	})
	t.Run("MovedStart", func(t *testing.T) {
		e := newFeedbackEnvironment(t, x, "Staging", "watcher")
		e.report(t, e.github(20*time.Second, "app", feedbackCommit("a")), true)
		// A late signal at 10 names the same version. The GitHub deploy keeps
		// its time and origin, and the span from 10 is its own deploy that
		// stops at the GitHub report.
		same := feedbackService("app", feedbackCommit("a"))
		same.Repository = "app"
		e.send(t, 10*time.Second, same)
		e.requireRunning(t, "app="+feedbackCommit("a")+"+20")
		e.requireDeploys(t, "app="+feedbackCommit("a")+"+10-20", "app="+feedbackCommit("a")+"+20")
		requireGitHubDeployAt := func(at time.Duration) {
			t.Helper()
			github := store.FeedbackOrigin{Source: store.FeedbackSourceGitHub, Actor: core.SystemActorID}
			for _, deploy := range e.deploys(t) {
				if deploy.StoppedAt == nil && (unnumbered(deploy.Origin) != github || !deploy.DeployedAt.Equal(feedbackTime(at))) {
					t.Fatalf("running deploy=%+v, want the GitHub deploy at %s", deploy, at)
				}
			}
		}
		requireGitHubDeployAt(20 * time.Second)
		for _, deploy := range e.deploys(t) {
			if deploy.DeployedAt.Equal(feedbackTime(10*time.Second)) && (deploy.Origin.Source != store.FeedbackSourceSignal || deploy.StopOrigin == nil || deploy.StopOrigin.Source != store.FeedbackSourceGitHub) {
				t.Fatalf("earlier span=%+v, want a signal deploy that the GitHub report stops", deploy)
			}
		}
		// A later signal at 15 names another version. It ends at the GitHub
		// report at 20.
		e.send(t, 15*time.Second, feedbackService("app", "@2"))
		e.requireRunning(t, "app="+feedbackCommit("a")+"+20")
		e.requireDeploys(t, "app="+feedbackCommit("a")+"+10-15", "app=@2+15-20", "app="+feedbackCommit("a")+"+20")
		requireGitHubDeployAt(20 * time.Second)
		for _, deploy := range e.deploys(t) {
			if stop, running := deploy.Stop(); !running && stop.Compare(deploy.Start()) <= 0 {
				t.Fatalf("deploy=%+v stops before it starts", deploy)
			}
		}
	})
}

// runFeedbackGitHubReportNumbers proves that signals and GitHub reports never
// share a report number.
func runFeedbackGitHubReportNumbers(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	// A GitHub report can come before the first signal.
	e.report(t, e.github(0, "app", feedbackCommit("a")), true)
	if _, ok := e.newest(t); ok {
		t.Fatal("a GitHub report recorded a newest signal")
	}
	e.send(t, 10*time.Second, feedbackService("app", "@1"))
	e.report(t, e.github(20*time.Second, "app", feedbackCommit("b")), true)
	e.send(t, 30*time.Second, feedbackService("app", "@2"))
	e.requireDeploys(t, "app="+feedbackCommit("a")+"+0-10", "app=@1+10-20", "app="+feedbackCommit("b")+"+20-30", "app=@2+30")
	starts := map[int64]string{}
	for _, deploy := range e.deploys(t) {
		if other, taken := starts[deploy.Origin.Report]; taken || deploy.Origin.Report < 1 {
			t.Fatalf("deploy %s holds report %d, which %s holds", deploy.ID, deploy.Origin.Report, other)
		}
		starts[deploy.Origin.Report] = deploy.ID
		if deploy.StopOrigin != nil && deploy.StopOrigin.Report <= deploy.Origin.Report {
			t.Fatalf("deploy=%+v stops at a report before its start", deploy)
		}
	}
	if len(starts) != 4 {
		t.Fatalf("report numbers=%v, want four", starts)
	}
}

// runFeedbackGitHubRecordsNothing covers the reports that record nothing: an
// archived environment, a missing one, a repository outside the workspace
// configuration, and a refused value. A restored environment records again.
func runFeedbackGitHubRecordsNothing(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	before := e.state(t)
	e.report(t, e.github(0, "ghost", feedbackCommit("a")), false)
	e.report(t, e.github(0, "App", feedbackCommit("a")), false)
	missing := e
	missing.id = "env-000000-000000"
	missing.report(t, missing.github(0, "app", feedbackCommit("a")), false)
	for field, deploy := range map[string]store.FeedbackGitHubDeploy{
		"commit":         e.github(0, "app", "abcdef0"),
		"reported_at":    {EnvironmentID: e.id, Repository: "app", Commit: feedbackCommit("a")},
		"repository":     e.github(0, "", feedbackCommit("a")),
		"environment_id": {Repository: "app", Commit: feedbackCommit("a"), ReportedAt: feedbackTime(0)},
	} {
		recorded, err := x.Backend.RecordFeedbackGitHubDeploy(x.Context, deploy)
		requireFeedbackField(t, err, field)
		if recorded {
			t.Fatalf("a refused report of %s recorded", field)
		}
	}
	if after := e.state(t); after != before {
		t.Fatalf("a report that records nothing changed the stored state:\nbefore %s\nafter  %s", before, after)
	}

	archived := true
	_, err := x.Backend.UpdateEnvironment(x.Context, e.id, store.EnvironmentPatch{Archived: &archived})
	requireOK(t, err)
	archivedState := e.state(t)
	e.report(t, e.github(0, "app", feedbackCommit("a")), false)
	if after := e.state(t); after != archivedState {
		t.Fatalf("an archived environment recorded a GitHub report:\nbefore %s\nafter  %s", archivedState, after)
	}
	archived = false
	_, err = x.Backend.UpdateEnvironment(x.Context, e.id, store.EnvironmentPatch{Archived: &archived})
	requireOK(t, err)
	e.report(t, e.github(10*time.Second, "app", feedbackCommit("a")), true)
	e.requireRunning(t, "app="+feedbackCommit("a")+"+10")
}

// runFeedbackGitHubObservesOnly proves that a GitHub report changes no task,
// work order, or event.
func runFeedbackGitHubObservesOnly(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	requireOK(t, x.Backend.CreateTask(x.Context, core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "app", Title: "Task", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}))
	observe := func() string {
		t.Helper()
		tasks, err := x.Backend.ListTasks(x.Context)
		requireOK(t, err)
		orders, err := x.Backend.ListWorkOrders(x.Context)
		requireOK(t, err)
		events, err := x.WorkspaceEvents(x.Context, "")
		requireOK(t, err)
		var taskEvents []core.Event
		for _, task := range tasks {
			listed, err := x.Backend.ListEvents(x.Context, task.ID)
			requireOK(t, err)
			taskEvents = append(taskEvents, listed...)
		}
		raw, err := json.Marshal([]any{tasks, orders, events, taskEvents})
		requireOK(t, err)
		return string(raw)
	}
	before := observe()
	e.report(t, e.github(0, "app", feedbackCommit("a")), true)
	e.report(t, e.github(10*time.Second, "app", feedbackCommit("b")), true)
	if after := observe(); after != before {
		t.Fatalf("a GitHub report changed an observed record:\nbefore %s\nafter  %s", before, after)
	}
}
