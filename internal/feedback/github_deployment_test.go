package feedback

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// deploymentFixture applies deployment reports against the memory backend
// with a stubbed forge (component-production-feedback invariants 10 and 11;
// feature-production-feedback-live FB-4).
type deploymentFixture struct {
	t        *testing.T
	st       store.Backend
	ctx      context.Context
	forge    *stubForge
	mu       sync.Mutex
	recorded []store.FeedbackSignal
	logs     []string
}

const (
	deployHead     = "1111111111111111111111111111111111111111"
	deployAncestor = "2222222222222222222222222222222222222222"
	deployOff      = "3333333333333333333333333333333333333333"
	deployAhead    = "4444444444444444444444444444444444444444"
)

var deployTime = time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC)

func newDeploymentFixture(t *testing.T) *deploymentFixture {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	workspace := "deploy-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor("github-webhook"))
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{
		{Name: "app", URL: "https://example.test/app", Base: "main", GitHub: "acme/app"},
		{Name: "web", URL: "https://example.test/web", Base: "main", GitHub: "acme/web"},
		{Name: "local", URL: "https://example.test/local", Base: "main"},
	}}); err != nil {
		t.Fatal(err)
	}
	forge := newStubForge()
	// acme/app: ancestor <- head on main, and off on another line. ahead is
	// a child of head that main does not hold yet.
	forge.chain("acme/app", "", deployAncestor, deployHead, deployAhead)
	forge.chain("acme/app", deployAncestor, deployOff)
	forge.refs["acme/app|heads/main"] = deployHead
	return &deploymentFixture{t: t, st: st, ctx: ctx, forge: forge}
}

func (f *deploymentFixture) environment(input store.EnvironmentInput) store.Environment {
	f.t.Helper()
	record, err := f.st.CreateEnvironment(f.ctx, input)
	if err != nil {
		f.t.Fatal(err)
	}
	return record.Environment
}

func (f *deploymentFixture) reports() DeploymentReports {
	return DeploymentReports{
		Store: f.st,
		Repositories: func(ctx context.Context) ([]Repository, error) {
			return []Repository{{Name: "app", Slug: "acme/app"}, {Name: "web", Slug: "acme/web"}, {Name: "local"}}, nil
		},
		Forge: func(context.Context, string) Forge { return f.forge },
		Recorded: func(workspace string, signal store.FeedbackSignal) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if want, _ := store.WorkspaceFromContext(f.ctx); workspace != want {
				f.t.Errorf("recorded workspace=%q, want %q", workspace, want)
			}
			f.recorded = append(f.recorded, signal)
		},
		Logf: func(format string, args ...any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.logs = append(f.logs, fmt.Sprintf(format, args...))
		},
	}
}

func (f *deploymentFixture) apply(slug, state, environment, commit string, at time.Time) error {
	return f.reports().Apply(f.ctx, slug, github.DeploymentStatus{State: state, Environment: environment, Commit: commit, ReportedAt: at})
}

func (f *deploymentFixture) mustApply(slug, state, environment, commit string, at time.Time) {
	f.t.Helper()
	if err := f.apply(slug, state, environment, commit, at); err != nil {
		f.t.Fatalf("apply %s %s %s: %v", state, environment, commit, err)
	}
}

func (f *deploymentFixture) deploys(environmentID string) []store.FeedbackDeploy {
	f.t.Helper()
	deploys, err := f.st.ListFeedbackDeploys(f.ctx, environmentID)
	if err != nil {
		f.t.Fatal(err)
	}
	return deploys
}

func (f *deploymentFixture) running(environmentID string) []string {
	f.t.Helper()
	running, err := f.st.ListFeedbackRunningVersions(f.ctx, environmentID)
	if err != nil {
		f.t.Fatal(err)
	}
	out := []string{}
	for _, version := range running {
		out = append(out, version.Service+"="+version.Repository+"@"+version.Commit+version.Version)
	}
	return out
}

func (f *deploymentFixture) suggestions() []store.EnvironmentSuggestion {
	f.t.Helper()
	suggestions, err := f.st.ListEnvironmentSuggestions(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return suggestions
}

func (f *deploymentFixture) recordedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recorded)
}

// TestDeploySuccessRecordsAGitHubDeploy covers a success report whose name
// an unarchived environment lists in another letter case, from a repository
// slug in another letter case. The deploy is dated at the status time, named
// after the repository, from GitHub, and replaces only that service.
func TestDeploySuccessRecordsAGitHubDeploy(t *testing.T) {
	f := newDeploymentFixture(t)
	environment := f.environment(store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"production"}})
	key, err := f.st.IssueEnvironmentKey(f.ctx, environment.ID, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	signalAt := deployTime.Add(-time.Hour)
	if err := f.st.RecordFeedbackSignal(f.ctx, store.FeedbackSignal{EnvironmentID: environment.ID, KeyID: key.ID, ArrivedAt: signalAt, NextSignalSeconds: 60, Services: []store.FeedbackSignalService{
		{Name: "app", Versions: []store.FeedbackSignalVersion{{Version: "1.0"}}},
		{Name: "worker", Versions: []store.FeedbackSignalVersion{{Version: "2.0"}}},
	}}); err != nil {
		t.Fatal(err)
	}

	f.mustApply("ACME/App", "success", "PRODUCTION", deployHead, deployTime)

	if got, want := f.running(environment.ID), []string{"app=app@" + deployHead, "worker=@2.0"}; !slices.Equal(got, want) {
		t.Fatalf("running=%v, want %v", got, want)
	}
	var github *store.FeedbackDeploy
	for _, deploy := range f.deploys(environment.ID) {
		switch {
		case deploy.Commit == deployHead:
			copied := deploy
			github = &copied
		case deploy.Service == "app":
			if deploy.StoppedAt == nil || !deploy.StoppedAt.Equal(deployTime) || deploy.StopOrigin.Source != store.FeedbackSourceGitHub {
				t.Fatalf("replaced deploy=%+v, want stopped by GitHub at the status time", deploy)
			}
		case deploy.Service == "worker":
			if deploy.StoppedAt != nil {
				t.Fatalf("another service's deploy=%+v stopped", deploy)
			}
		}
	}
	if github == nil || github.Service != "app" || github.Repository != "app" || !github.DeployedAt.Equal(deployTime) || github.Origin.Source != store.FeedbackSourceGitHub || github.Origin.Actor != core.SystemActorID || github.StoppedAt != nil {
		t.Fatalf("GitHub deploy=%+v", github)
	}
	signals, err := f.st.ListFeedbackSignals(f.ctx, []string{environment.ID})
	if err != nil || !signals[environment.ID].LastSignalAt.Equal(signalAt) {
		t.Fatalf("newest signal=%+v err=%v, want the watcher's signal time kept", signals, err)
	}
	if f.recordedCount() != 1 || f.recorded[0].Services[0].Name != "app" || f.recorded[0].Services[0].Versions[0].Commit != deployHead || !f.recorded[0].ArrivedAt.Equal(deployTime) {
		t.Fatalf("recorded=%+v, want the deploy handed to the background work", f.recorded)
	}
	if len(f.suggestions()) != 0 {
		t.Fatalf("suggestions=%+v, want none for a listed name", f.suggestions())
	}

	// A duplicate report makes no second write and hands nothing on.
	before := f.deploys(environment.ID)
	f.mustApply("acme/app", "success", "production", deployHead, deployTime)
	if after := f.deploys(environment.ID); len(after) != len(before) || f.recordedCount() != 1 {
		t.Fatalf("deploys=%d recorded=%d after a duplicate, want %d and 1", len(after), f.recordedCount(), len(before))
	}
}

// TestDeployRedeployOfTheRunningCommit covers two successes for one service
// and commit at two times with no watcher signal between them. Each keeps its
// own deploy record at its status time, and a duplicate of either writes
// nothing and hands nothing on.
func TestDeployRedeployOfTheRunningCommit(t *testing.T) {
	f := newDeploymentFixture(t)
	environment := f.environment(store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"production"}})
	later := deployTime.Add(time.Hour)
	f.mustApply("acme/app", "success", "production", deployHead, deployTime)
	f.mustApply("acme/app", "success", "production", deployHead, later)
	deploys := f.deploys(environment.ID)
	if len(deploys) != 2 || !deploys[0].DeployedAt.Equal(deployTime) || deploys[0].StoppedAt == nil || !deploys[0].StoppedAt.Equal(later) || !deploys[1].DeployedAt.Equal(later) || deploys[1].StoppedAt != nil || deploys[1].Commit != deployHead || deploys[1].Origin.Source != store.FeedbackSourceGitHub {
		t.Fatalf("deploys=%+v, want one deploy for each report time", deploys)
	}
	if f.recordedCount() != 2 {
		t.Fatalf("recorded=%d, want both reports handed to the background work", f.recordedCount())
	}
	f.mustApply("acme/app", "success", "production", deployHead, later)
	f.mustApply("acme/app", "success", "production", deployHead, deployTime)
	if after := f.deploys(environment.ID); len(after) != 2 || f.recordedCount() != 2 {
		t.Fatalf("deploys=%+v recorded=%d after duplicates, want no second write", after, f.recordedCount())
	}
}

// TestDeployOtherStatesRecordNoDeploy covers every state other than success
// under a listed name.
func TestDeployOtherStatesRecordNoDeploy(t *testing.T) {
	f := newDeploymentFixture(t)
	environment := f.environment(store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"production"}})
	for _, state := range []string{"error", "failure", "inactive", "in_progress", "queued", "pending", "Success"} {
		f.mustApply("acme/app", state, "production", deployHead, deployTime)
	}
	if deploys := f.deploys(environment.ID); len(deploys) != 0 {
		t.Fatalf("deploys=%+v, want none", deploys)
	}
	if len(f.suggestions()) != 0 || f.recordedCount() != 0 {
		t.Fatalf("suggestions=%+v recorded=%d, want none", f.suggestions(), f.recordedCount())
	}
	f.forge.mu.Lock()
	defer f.forge.mu.Unlock()
	if len(f.forge.reads)+len(f.forge.compare) != 0 {
		t.Fatalf("forge reads=%v compares=%v, want none", f.forge.reads, f.forge.compare)
	}
}

// TestDeployUnlistedNameRecordsASuggestion covers an unlisted name from any
// state, the newest report time, an ignored name, and an archived match.
func TestDeployUnlistedNameRecordsASuggestion(t *testing.T) {
	f := newDeploymentFixture(t)
	f.environment(store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"production"}})
	f.mustApply("acme/app", "failure", "staging", deployHead, deployTime)
	f.mustApply("acme/web", "in_progress", "Staging", deployHead, deployTime.Add(time.Minute))
	// An older report cannot replace the newest suggestion.
	f.mustApply("acme/app", "success", "staging", deployHead, deployTime.Add(-time.Hour))
	suggestions := f.suggestions()
	if len(suggestions) != 1 || suggestions[0].GitHubEnvironment != "Staging" || suggestions[0].Repository != "web" || !suggestions[0].LastReportedAt.Equal(deployTime.Add(time.Minute)) {
		t.Fatalf("suggestions=%+v, want Staging in web at the newest report", suggestions)
	}

	if err := f.st.IgnoreEnvironmentSuggestion(f.ctx, "staging"); err != nil {
		t.Fatal(err)
	}
	f.mustApply("acme/app", "success", "STAGING", deployHead, deployTime.Add(time.Hour))
	if suggestions := f.suggestions(); len(suggestions) != 0 {
		t.Fatalf("suggestions=%+v, want an ignored name to stay ignored", suggestions)
	}

	// An archived environment records no deploy. Its name can become a
	// suggestion while no unarchived environment lists it.
	archived := f.environment(store.EnvironmentInput{Name: "Preview", GitHubEnvironments: []string{"preview"}})
	yes := true
	if _, err := f.st.UpdateEnvironment(f.ctx, archived.ID, store.EnvironmentPatch{Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	f.mustApply("acme/app", "success", "preview", deployHead, deployTime)
	if deploys := f.deploys(archived.ID); len(deploys) != 0 {
		t.Fatalf("archived deploys=%+v, want none", deploys)
	}
	if suggestions := f.suggestions(); len(suggestions) != 1 || suggestions[0].GitHubEnvironment != "preview" {
		t.Fatalf("suggestions=%+v, want the archived environment's name suggested", suggestions)
	}
	if f.recordedCount() != 0 {
		t.Fatalf("recorded=%d, want none", f.recordedCount())
	}
}

// TestDeployRepositoryOutsideTheWorkspace records nothing for a repository
// that no configured repository names by its slug.
func TestDeployRepositoryOutsideTheWorkspace(t *testing.T) {
	f := newDeploymentFixture(t)
	environment := f.environment(store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"production"}})
	for _, slug := range []string{"other/app", "acme/app2", "", "local"} {
		f.mustApply(slug, "success", "production", deployHead, deployTime)
		f.mustApply(slug, "success", "unlisted", deployHead, deployTime)
	}
	if deploys := f.deploys(environment.ID); len(deploys) != 0 || len(f.suggestions()) != 0 {
		t.Fatalf("deploys=%+v suggestions=%+v, want none", deploys, f.suggestions())
	}
}

// TestDeployTrackedBranch covers a commit at the tracked head, an ancestor,
// a commit off the branch, a commit the branch does not hold yet, and a branch
// tracked for another repository only. A head that does not resolve is a
// failure, which TestDeployForgeFailureFailsTheDelivery covers.
func TestDeployTrackedBranch(t *testing.T) {
	for _, c := range []struct {
		name     string
		branches map[string]string
		commit   string
		want     bool
		reads    int
	}{
		{"Head", map[string]string{"app": "main"}, deployHead, true, 1},
		{"Ancestor", map[string]string{"app": "main"}, deployAncestor, true, 1},
		{"OffTheBranch", map[string]string{"app": "main"}, deployOff, false, 1},
		{"NotOnTheBranchYet", map[string]string{"app": "main"}, deployAhead, false, 1},
		{"NoBranchForTheRepository", map[string]string{"web": "main"}, deployOff, true, 0},
		{"NoBranch", nil, deployOff, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDeploymentFixture(t)
			environment := f.environment(store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"production"}, TrackedBranches: c.branches})
			f.mustApply("acme/app", "success", "production", c.commit, deployTime)
			deploys := f.deploys(environment.ID)
			if got := len(deploys) == 1 && deploys[0].Commit == c.commit; got != c.want || (!c.want && len(deploys) != 0) {
				t.Fatalf("deploys=%+v, want a deploy=%v", deploys, c.want)
			}
			reads, compares := f.forge.calls()
			if reads != c.reads {
				t.Fatalf("branch reads=%d, want %d", reads, c.reads)
			}
			if c.reads > 0 {
				f.forge.mu.Lock()
				compared := slices.Clone(f.forge.compare)
				f.forge.mu.Unlock()
				// The deployment commit is the base and the branch head the
				// comparison head.
				if compares != 1 || compared[0] != "acme/app|"+c.commit+"|"+deployHead {
					t.Fatalf("compares=%v, want the deployment commit compared with the branch head", compared)
				}
			}
		})
	}
}

// TestDeployForgeFailureFailsTheDelivery proves that a forge failure during
// the branch check writes no deploy and fails with its category only, and
// that the same report records once the forge answers. A tracked branch head
// that does not resolve is such a failure, not an off-branch answer.
func TestDeployForgeFailureFailsTheDelivery(t *testing.T) {
	for _, c := range []struct {
		name    string
		key     string
		failure error
	}{
		{"BranchRead", "acme/app|heads/main", github.StatusError(502, errors.New("upstream said token ghs_secretsecretsecretsecretsecretsecret1"))},
		{"MissingHead", "acme/app|heads/main", github.CommitNotFoundError("heads/main")},
		{"Comparison", "compare|acme/app|" + deployAncestor + "|" + deployHead, github.StatusError(502, errors.New("upstream said token ghs_secretsecretsecretsecretsecretsecret1"))},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDeploymentFixture(t)
			environment := f.environment(store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"production"}, TrackedBranches: map[string]string{"app": "main"}})
			f.forge.fail[c.key] = c.failure
			err := f.apply("acme/app", "success", "production", deployAncestor, deployTime)
			var failed *BranchCheckError
			if !errors.As(err, &failed) || failed.Category != github.ForgeStatus {
				t.Fatalf("err=%v, want a branch check failure with category %s", err, github.ForgeStatus)
			}
			if strings.Contains(err.Error(), "ghs_") || strings.Contains(err.Error(), "upstream") {
				t.Fatalf("err=%q repeats GitHub's response", err)
			}
			if deploys := f.deploys(environment.ID); len(deploys) != 0 || f.recordedCount() != 0 {
				t.Fatalf("deploys=%+v recorded=%d after a forge failure, want none", deploys, f.recordedCount())
			}
			delete(f.forge.fail, c.key)
			f.mustApply("acme/app", "success", "production", deployAncestor, deployTime)
			if deploys := f.deploys(environment.ID); len(deploys) != 1 || f.recordedCount() != 1 {
				t.Fatalf("deploys=%+v recorded=%d after the forge answered, want one", deploys, f.recordedCount())
			}
		})
	}
}

// TestDeployRefusedNameIsNotRetried proves that a GitHub environment name the
// suggestion rules refuse, such as one that looks like a credential, records
// nothing, fails no delivery, and is logged without the name.
func TestDeployRefusedNameIsNotRetried(t *testing.T) {
	f := newDeploymentFixture(t)
	f.environment(store.EnvironmentInput{Name: "Production", GitHubEnvironments: []string{"production"}})
	name := "ghp_" + strings.Repeat("A1b2", 9)
	if err := f.apply("acme/app", "success", name, deployHead, deployTime); err != nil {
		t.Fatalf("err=%v, want a refused name to fail no delivery", err)
	}
	if len(f.suggestions()) != 0 {
		t.Fatalf("suggestions=%+v, want none", f.suggestions())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logs) != 1 || !strings.Contains(f.logs[0], "outcome=refused") || strings.Contains(f.logs[0], name) {
		t.Fatalf("logs=%q, want one refusal without the name", f.logs)
	}
}

// TestDeployStoreFailureFailsTheDelivery proves that a store failure is
// returned, so the applier retries the delivery.
func TestDeployStoreFailureFailsTheDelivery(t *testing.T) {
	f := newDeploymentFixture(t)
	reports := f.reports()
	failure := errors.New("store unavailable")
	reports.Repositories = func(context.Context) ([]Repository, error) { return nil, failure }
	if err := reports.Apply(f.ctx, "acme/app", github.DeploymentStatus{State: "success", Environment: "production", Commit: deployHead, ReportedAt: deployTime}); !errors.Is(err, failure) {
		t.Fatalf("err=%v, want the store failure", err)
	}
}
