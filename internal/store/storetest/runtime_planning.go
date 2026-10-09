package storetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// runtimePlanningRevision is the fixed commit the fake snapshot API pins.
const runtimePlanningRevision = "0123456789abcdef0123456789abcdef01234567"

// runtimePlanningDeployment is a deployment whose control-plane values differ
// from everything a workspace document could carry (component-runtime).
func runtimePlanningDeployment(t *testing.T, workspace string) *config.Config {
	t.Helper()
	deployment, err := config.ParseDeployment([]byte(`workspace: `+workspace+`
database: {url: "postgres://db.invalid/conveyor"}
execution_settings:
  control_plane:
    triage: {model: deploy-triage, effort: low, timeout: 7m}
    planning:
      model: deploy-planner
      effort: high
      timeout: 9m
      exploration_output_tokens: 1234
planning_models: [deploy-planner, deploy-alternate]
repos: []
`), "runtime-planning.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	return deployment
}

// runtimePlanningSnapshots is a deterministic gitx.SnapshotAPI: it resolves
// every base to one fixed commit and serves nothing else, so no host Git,
// forge, or network is reached.
type runtimePlanningSnapshots struct{ resolves atomic.Int64 }

func (s *runtimePlanningSnapshots) ResolveCommit(context.Context, string, string) (string, error) {
	s.resolves.Add(1)
	return runtimePlanningRevision, nil
}

func (s *runtimePlanningSnapshots) OpenArchive(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("runtime planning fixture serves no archive")
}

func (s *runtimePlanningSnapshots) History(context.Context, string, string, string, int) ([]github.SnapshotCommit, error) {
	return nil, errors.New("runtime planning fixture serves no history")
}

func (s *runtimePlanningSnapshots) CommitDetail(context.Context, string, string) (github.SnapshotCommit, error) {
	return github.SnapshotCommit{}, errors.New("runtime planning fixture serves no commit detail")
}

type runtimePlanningHarness struct {
	service     *planning.Service
	snapshots   *runtimePlanningSnapshots
	credentials *atomic.Int64
}

func newRuntimePlanningHarness(x Fixture, deployment *config.Config) runtimePlanningHarness {
	snapshots := &runtimePlanningSnapshots{}
	credentials := &atomic.Int64{}
	service := &planning.Service{
		Store: x.Backend,
		Git:   gitx.NewManager(snapshots, 0),
		ConfigProvider: func(ctx context.Context) (*config.Config, error) {
			return x.Backend.RuntimeConfig(ctx, deployment)
		},
		CredentialContext: func(ctx context.Context, _ string) (context.Context, error) {
			credentials.Add(1)
			return ctx, nil
		},
	}
	return runtimePlanningHarness{service: service, snapshots: snapshots, credentials: credentials}
}

// prepareRuntimePlanningWorkspace stores GitHub repositories through the
// public configuration write, starting from a composed runtime value, and
// proves the stored document stays policy-only.
func prepareRuntimePlanningWorkspace(t *testing.T, x Fixture, deployment *config.Config) {
	t.Helper()
	st, ctx := x.Backend, x.Context
	version, err := st.WorkspaceConfig(ctx)
	requireOK(t, err)
	runtime, err := st.RuntimeConfig(ctx, deployment)
	requireOK(t, err)
	next := *runtime
	next.Repos = make([]config.Repo, len(runtime.Repos))
	for i, repo := range runtime.Repos {
		repo.URL = fmt.Sprintf("https://github.com/conveyor-conformance/%s", repo.Name)
		next.Repos[i] = repo
	}
	if len(next.Repos) == 0 {
		t.Fatal("fixture workspace has no repository to plan in")
	}
	_, err = st.UpdateWorkspaceConfig(ctx, version.Version, &next)
	requireOK(t, err)
	requirePolicyOnlyWorkspace(t, x)
}

func requirePolicyOnlyWorkspace(t *testing.T, x Fixture) config.VersionedDocument {
	t.Helper()
	stored, err := x.Backend.WorkspaceConfig(x.Context)
	requireOK(t, err)
	document := stored.Document
	if document.ExecutionSettings != nil || len(document.PlanningModels) != 0 || len(document.Setups) != 0 || document.DefaultSetup != "" || len(document.Routing.Stages) != 0 {
		t.Fatalf("stored workspace document is not policy-only: %+v", document)
	}
	return stored
}

// runPolicyOnlyPlanningUsesDeploymentControlPlane holds every backend to the
// runtime composition: a policy-only workspace opens a real planning session
// with the deployment's planning settings (component-planning;
// component-runtime; DEC-56(3)).
func runPolicyOnlyPlanningUsesDeploymentControlPlane(t *testing.T, x Fixture) {
	t.Setenv(config.ControlPlaneModelEnv, "")
	t.Setenv(config.PlanningModelEnv, "")
	st, ctx := x.Backend, x.Context
	deployment := runtimePlanningDeployment(t, x.Workspace)
	prepareRuntimePlanningWorkspace(t, x, deployment)
	before := requirePolicyOnlyWorkspace(t, x)

	runtime, err := st.RuntimeConfig(ctx, deployment)
	requireOK(t, err)
	if runtime.ExecutionSettings == nil || runtime.ExecutionSettings.ControlPlane.Planning.Model != "deploy-planner" ||
		runtime.Routing.Stages["triage"].Model != "deploy-triage" || runtime.Routing.Stages["triage"].Effort != "low" {
		t.Fatalf("runtime configuration lacks deployment control plane: settings=%+v triage=%+v", runtime.ExecutionSettings, runtime.Routing.Stages["triage"])
	}
	harness := newRuntimePlanningHarness(x, deployment)
	session, err := harness.service.CreateSession(ctx, planning.CreateSessionInput{})
	requireOK(t, err)
	stored, err := st.GetPlanningSession(ctx, session.ID)
	requireOK(t, err)
	primary := runtime.Repos[0].Name
	if stored.Model != "deploy-planner" || stored.Effort != "high" || stored.ExplorationOutputTokens != 1234 ||
		stored.PrimaryRepo != primary || stored.PinnedRevisions[primary] != runtimePlanningRevision || len(stored.PinnedRevisions) != 1 {
		t.Fatalf("planning session provenance = model %q effort %q cap %d primary %q pins %v", stored.Model, stored.Effort, stored.ExplorationOutputTokens, stored.PrimaryRepo, stored.PinnedRevisions)
	}
	if harness.snapshots.resolves.Load() != 1 || harness.credentials.Load() != 1 {
		t.Fatalf("pin calls=%d credential calls=%d", harness.snapshots.resolves.Load(), harness.credentials.Load())
	}
	alternate, err := harness.service.CreateSession(ctx, planning.CreateSessionInput{ModelOverride: "deploy-alternate"})
	requireOK(t, err)
	if alternate.Model != "deploy-alternate" {
		t.Fatalf("allowlisted alternate model = %q", alternate.Model)
	}
	if _, err := harness.service.CreateSession(ctx, planning.CreateSessionInput{ModelOverride: "workspace-model"}); err == nil || !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("unlisted model error = %v", err)
	}
	after := requirePolicyOnlyWorkspace(t, x)
	if after.Version != before.Version {
		t.Fatalf("runtime reads and planning advanced the configuration version %d -> %d", before.Version, after.Version)
	}
	updated := *runtime
	updated.MaxBounces = runtime.MaxBounces + 1
	receipt, err := st.UpdateWorkspaceConfig(ctx, after.Version, &updated)
	requireOK(t, err)
	if receipt.Version != after.Version+1 || receipt.Document.ExecutionSettings != nil || len(receipt.Document.PlanningModels) != 0 {
		t.Fatalf("update from a runtime value returned %+v", receipt)
	}
	requirePolicyOnlyWorkspace(t, x)
	if again, err := st.RuntimeConfig(ctx, deployment); err != nil || again.MaxBounces != updated.MaxBounces || again.ExecutionSettings.ControlPlane.Planning.Model != "deploy-planner" {
		t.Fatalf("runtime after update = %+v err=%v", again, err)
	}
}

// runPolicyOnlyPlanningRejectsMissingDeploymentControlPlane holds every
// backend to the actionable refusal: without deployment planning settings, or
// with an empty planning model, nothing is pinned, no credential is minted,
// and no session row exists (component-planning).
func runPolicyOnlyPlanningRejectsMissingDeploymentControlPlane(t *testing.T, x Fixture) {
	t.Setenv(config.ControlPlaneModelEnv, "")
	t.Setenv(config.PlanningModelEnv, "")
	st, ctx := x.Backend, x.Context
	deployment := runtimePlanningDeployment(t, x.Workspace)
	prepareRuntimePlanningWorkspace(t, x, deployment)
	before := requirePolicyOnlyWorkspace(t, x)
	sessions, err := st.ListPlanningSessions(ctx)
	requireOK(t, err)

	absent := *deployment
	absent.ExecutionSettings, absent.PlanningModels = nil, nil
	settings := *deployment.ExecutionSettings
	settings.ControlPlane.Planning.Model = ""
	blank := *deployment
	blank.ExecutionSettings = &settings
	for name, candidate := range map[string]*config.Config{"absent settings": &absent, "blank model": &blank} {
		harness := newRuntimePlanningHarness(x, candidate)
		_, err := harness.service.CreateSession(ctx, planning.CreateSessionInput{})
		if err == nil || !strings.Contains(err.Error(), config.PlanningControlPlaneRemedy) {
			t.Fatalf("%s: refusal = %v", name, err)
		}
		if harness.snapshots.resolves.Load() != 0 || harness.credentials.Load() != 0 {
			t.Fatalf("%s: refused creation pinned %d times and minted %d credentials", name, harness.snapshots.resolves.Load(), harness.credentials.Load())
		}
	}
	// A process override replaces a model but never supplies absent settings.
	t.Setenv(config.PlanningModelEnv, "override-planner")
	harness := newRuntimePlanningHarness(x, &absent)
	if _, err := harness.service.CreateSession(ctx, planning.CreateSessionInput{}); err == nil || !strings.Contains(err.Error(), config.PlanningControlPlaneRemedy) {
		t.Fatalf("override with absent settings: refusal = %v", err)
	}
	after, err := st.ListPlanningSessions(ctx)
	requireOK(t, err)
	if len(after) != len(sessions) {
		t.Fatalf("refused creation wrote planning sessions: before=%d after=%d", len(sessions), len(after))
	}
	if stored := requirePolicyOnlyWorkspace(t, x); stored.Version != before.Version {
		t.Fatalf("refusal advanced the configuration version %d -> %d", before.Version, stored.Version)
	}
}
