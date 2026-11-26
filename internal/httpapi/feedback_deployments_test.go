package httpapi

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/feedback"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// TestFeedbackDeploymentReportLiveStatus applies a GitHub deployment report
// through the server's handler. The tracked-branch check reads through the
// server's forge, the recorded deploy goes to the background work, which
// resolves and compares it, and a merged task the deployed commit holds reads
// as live under the repository's service at the report's time
// (feature-production-feedback-live FB-4; component-production-feedback
// invariants 10, 16, and 17; req-production-feedback AC-1.5).
func TestFeedbackDeploymentReportLiveStatus(t *testing.T) {
	merge, deployed := readsCommit("b2b2b2b2b2b2"), readsCommit("c3c3c3c3c3c3")
	h := newReadsHarness(t, merge, deployed)
	task := h.merged(h.ctx, merge, "")
	environment, err := h.st.CreateEnvironment(h.ctx, store.EnvironmentInput{Name: "Staging", GitHubEnvironments: []string{"staging"}, TrackedBranches: map[string]string{"app": "main"}})
	if err != nil {
		t.Fatal(err)
	}
	h.forge.setBranch("acme/app", "main", deployed)
	apply := h.server.FeedbackDeploymentReports()
	if apply == nil {
		t.Fatal("the server offers no deployment report handler")
	}
	reportedAt := time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC)
	if err := apply(h.ctx, "Acme/App", github.DeploymentStatus{State: "success", Environment: "STAGING", Commit: deployed, ReportedAt: reportedAt}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	deploys, err := h.st.ListFeedbackDeploys(h.ctx, environment.ID)
	if err != nil || len(deploys) != 1 || deploys[0].Service != "app" || deploys[0].Origin.Source != store.FeedbackSourceGitHub {
		t.Fatalf("deploys=%+v err=%v", deploys, err)
	}
	var staging *feedback.TaskStatus
	for _, status := range statusesOf(t, h.taskRead(task.ID)["environments"]) {
		if status.Environment.ID == environment.ID {
			copied := status
			staging = &copied
		}
	}
	if staging == nil || staging.Status != feedback.StatusLive || !slices.Equal(staging.Services, []string{"app"}) || staging.LiveSince == nil || !staging.LiveSince.Equal(reportedAt) || staging.AsOf == nil || !staging.AsOf.Equal(reportedAt) {
		t.Fatalf("staging status=%+v, want live under app since the report", staging)
	}
}

// TestFeedbackSignalJobsOfOneTimeStayApart proves that two accepted reports of
// one environment with one report time each reach the background work. Two
// GitHub deployment reports of different repositories can share a time, and
// the second must not be taken for the first while the first job runs.
func TestFeedbackSignalJobsOfOneTimeStayApart(t *testing.T) {
	deployed := readsCommit("c3c3c3c3c3c3")
	h := newReadsHarness(t, deployed)
	release := h.forge.hold(t)
	at := time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC)
	first := store.FeedbackSignal{EnvironmentID: h.environment.ID, ArrivedAt: at, Services: []store.FeedbackSignalService{{Name: "app", Repository: "app", Versions: []store.FeedbackSignalVersion{{Commit: deployed}}}}}
	second := store.FeedbackSignal{EnvironmentID: h.environment.ID, ArrivedAt: at, Services: []store.FeedbackSignalService{{Name: "web", Versions: []store.FeedbackSignalVersion{{Version: "1.0"}}}}}
	h.server.enqueueFeedbackSignal("alpha", first)
	select {
	case <-h.forge.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first job made no forge call")
	}
	// The first job waits in the forge, so its key is still pending.
	h.server.enqueueFeedbackSignal("alpha", second)
	release()
	h.settle()
	inputs, err := h.st.ReadFeedbackInputs(h.ctx, []string{h.environment.ID})
	if err != nil {
		t.Fatal(err)
	}
	services := []string{}
	for _, resolution := range inputs.Resolutions {
		services = append(services, resolution.Service)
	}
	slices.Sort(services)
	if !slices.Equal(services, []string{"app", "web"}) {
		t.Fatalf("resolved services=%v, want both reports processed", services)
	}
}

// TestFeedbackDeploymentRedeployAsOf applies two successful GitHub reports of
// one commit at two times with no signal between them. The environment's
// live status rests on the later report (component-production-feedback invariants 10 and 17; req-production-feedback
// AC-1.5 and AC-3.6).
func TestFeedbackDeploymentRedeployAsOf(t *testing.T) {
	merge, deployed := readsCommit("b2b2b2b2b2b2"), readsCommit("c3c3c3c3c3c3")
	h := newReadsHarness(t, merge, deployed)
	task := h.merged(h.ctx, merge, "")
	environment, err := h.st.CreateEnvironment(h.ctx, store.EnvironmentInput{Name: "Staging", GitHubEnvironments: []string{"staging"}})
	if err != nil {
		t.Fatal(err)
	}
	apply := h.server.FeedbackDeploymentReports()
	first, second := time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC), time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{first, second} {
		if err := apply(h.ctx, "acme/app", github.DeploymentStatus{State: "success", Environment: "staging", Commit: deployed, ReportedAt: at}); err != nil {
			t.Fatal(err)
		}
		h.settle()
	}
	deploys, err := h.st.ListFeedbackDeploys(h.ctx, environment.ID)
	if err != nil || len(deploys) != 2 || !deploys[0].DeployedAt.Equal(first) || !deploys[1].DeployedAt.Equal(second) || deploys[1].StoppedAt != nil {
		t.Fatalf("deploys=%+v err=%v, want one deploy for each report time", deploys, err)
	}
	var staging *feedback.TaskStatus
	for _, status := range statusesOf(t, h.taskRead(task.ID)["environments"]) {
		if status.Environment.ID == environment.ID {
			copied := status
			staging = &copied
		}
	}
	if staging == nil || staging.Status != feedback.StatusLive || staging.AsOf == nil || !staging.AsOf.Equal(second) {
		t.Fatalf("staging status=%+v, want live as of the later report", staging)
	}
}

// TestFeedbackDeploymentLateReportsKeepAsOf applies a GitHub report of a
// commit at t2, then an earlier report of the same commit that arrives later:
// first a GitHub report at t1, then a watcher signal at t1. The GitHub deploy
// at t2 keeps its record and origin after each, and the environment's live
// status stays as of t2 (component-production-feedback invariants 10 and 17;
// req-production-feedback AC-1.5 and AC-3.6).
func TestFeedbackDeploymentLateReportsKeepAsOf(t *testing.T) {
	merge, deployed := readsCommit("b2b2b2b2b2b2"), readsCommit("c3c3c3c3c3c3")
	h := newReadsHarness(t, merge, deployed)
	task := h.merged(h.ctx, merge, "")
	environment, err := h.st.CreateEnvironment(h.ctx, store.EnvironmentInput{Name: "Staging", GitHubEnvironments: []string{"staging"}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := h.st.IssueEnvironmentKey(h.ctx, environment.ID, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	apply := h.server.FeedbackDeploymentReports()
	early, late := time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC), time.Date(2026, 11, 26, 10, 30, 0, 0, time.UTC)
	report := func(at time.Time) {
		t.Helper()
		if err := apply(h.ctx, "acme/app", github.DeploymentStatus{State: "success", Environment: "staging", Commit: deployed, ReportedAt: at}); err != nil {
			t.Fatal(err)
		}
		h.settle()
	}
	requireAsOfLate := func(step string) {
		t.Helper()
		deploys, err := h.st.ListFeedbackDeploys(h.ctx, environment.ID)
		if err != nil {
			t.Fatal(err)
		}
		kept := false
		for _, deploy := range deploys {
			if deploy.StoppedAt == nil {
				kept = deploy.DeployedAt.Equal(late) && deploy.Origin.Source == store.FeedbackSourceGitHub
			}
		}
		if !kept {
			t.Fatalf("%s: deploys=%+v, want the GitHub deploy at the later report to keep running", step, deploys)
		}
		var staging *feedback.TaskStatus
		for _, status := range statusesOf(t, h.taskRead(task.ID)["environments"]) {
			if status.Environment.ID == environment.ID {
				copied := status
				staging = &copied
			}
		}
		if staging == nil || staging.Status != feedback.StatusLive || staging.AsOf == nil || !staging.AsOf.Equal(late) {
			t.Fatalf("%s: staging status=%+v, want live as of the later GitHub report", step, staging)
		}
	}
	report(late)
	requireAsOfLate("GitHub report")
	report(early)
	requireAsOfLate("late earlier GitHub report")
	if err := h.st.RecordFeedbackSignal(h.ctx, store.FeedbackSignal{EnvironmentID: environment.ID, KeyID: key.ID, ArrivedAt: early.Add(time.Minute), NextSignalSeconds: 60, Services: []store.FeedbackSignalService{{Name: "app", Repository: "app", Versions: []store.FeedbackSignalVersion{{Commit: deployed}}}}}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	requireAsOfLate("late earlier watcher signal")
}

// TestFeedbackDeploymentLongAliasLiveStatus applies a GitHub report for a
// configured repository whose alias has 256 characters. The deploy keeps the
// whole alias as its service, the background work resolves and compares it,
// and a merged task of that repository reads as live under the alias
// (component-production-feedback invariants 7, 10, and 16;
// req-production-feedback AC-1.5 and AC-3.2).
func TestFeedbackDeploymentLongAliasLiveStatus(t *testing.T) {
	merge, deployed := readsCommit("b2b2b2b2b2b2"), readsCommit("c3c3c3c3c3c3")
	h := newReadsHarness(t, merge, deployed)
	alias := "app-" + strings.Repeat("x", 252)
	current, err := h.st.WorkspaceConfig(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.UpdateWorkspaceConfig(h.ctx, current.Version, &config.Config{Workspace: "alpha", Repos: []config.Repo{{Name: alias, URL: "https://example.test/app", Base: "main", GitHub: "acme/app"}}}); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: core.NewTaskID() + "-long", Workspace: "alpha", Repo: alias, Title: "Merged", Body: "Merged.", State: core.TaskMerged, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := h.st.CreateTask(h.ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := h.st.AppendEvent(h.ctx, core.Event{TaskID: task.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "acme/app", "merge_commit_sha": merge})}); err != nil {
		t.Fatal(err)
	}
	environment, err := h.st.CreateEnvironment(h.ctx, store.EnvironmentInput{Name: "Staging", GitHubEnvironments: []string{"staging"}})
	if err != nil {
		t.Fatal(err)
	}
	reportedAt := time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC)
	if err := h.server.FeedbackDeploymentReports()(h.ctx, "acme/app", github.DeploymentStatus{State: "success", Environment: "staging", Commit: deployed, ReportedAt: reportedAt}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	deploys, err := h.st.ListFeedbackDeploys(h.ctx, environment.ID)
	if err != nil || len(alias) != 256 || len(deploys) != 1 || deploys[0].Service != alias || deploys[0].Repository != alias {
		t.Fatalf("deploys=%+v err=%v, want one deploy named after the whole alias", deploys, err)
	}
	inputs, err := h.st.ReadFeedbackInputs(h.ctx, []string{environment.ID})
	if err != nil {
		t.Fatal(err)
	}
	resolved := false
	for _, resolution := range inputs.Resolutions {
		resolved = resolved || (resolution.Service == alias && resolution.ResolvedRepository == alias && resolution.ResolvedCommit == deployed)
	}
	if !resolved {
		t.Fatalf("resolutions=%+v, want the deploy resolved under the whole alias", inputs.Resolutions)
	}
	var staging *feedback.TaskStatus
	for _, status := range statusesOf(t, h.taskRead(task.ID)["environments"]) {
		if status.Environment.ID == environment.ID {
			copied := status
			staging = &copied
		}
	}
	if staging == nil || staging.Status != feedback.StatusLive || !slices.Equal(staging.Services, []string{alias}) || staging.AsOf == nil || !staging.AsOf.Equal(reportedAt) {
		t.Fatalf("staging status=%+v, want live under the whole alias as of the report", staging)
	}
}
