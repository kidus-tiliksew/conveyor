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
	"gopkg.in/yaml.v3"
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
	if document.ExecutionSettings != nil || len(document.PlanningModels) != 0 || len(document.Routing.Stages) != 0 {
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

// legacyWorkspaceRowKeys are the members an earlier release wrote into a
// stored workspace document: a harness registry whose argv the retired
// validation refused, two execution setups with executor detail, the
// default setup that names one, a stale top-level projection and routing,
// and review seat contents (DEC-104(1), DEC-104(2)).
const legacyWorkspaceRowKeys = `harnesses:
  - {name: codex, mcp_transport: toml_override, command: [codex, exec], probe_command: [codex, --version], probe_timeout: 10s}
setups:
  - name: other
    execution_settings:
      implementation: {harness: codex, model: other-impl, timeout: 12m}
      review: {execution: mcp, timeout: 13m}
    review: {seats: [{model: other-seat}]}
  - name: fast
    refresh_review: full
    execution_settings:
      spec: {harness: codex, model: setup-spec, model_policy: explicit, effort: high, timeout: 45m}
      implementation: {harness: codex, model_policy: bogus, effort: high, timeout: 5h}
      verify: {harness: codex, model: setup-verify, timeout: 90m}
      review: {execution: mcp, timeout: 2h, fallback_harness: codex, fallback_model: setup-fallback}
    review:
      seats:
        - {model: seat-a, harness: codex}
        - {model: seat-b}
        - {model: seat-c, effort: high}
default_setup: fast
execution_settings:
  implementation: {harness: codex, model: stale-impl, timeout: 1h}
  review: {execution: mcp, timeout: 20m}
routing:
  stages:
    implement: {model: stale-routing, harness: codex, timeout: 9m, execution: mcp}
review: {seats: [{model: stale-seat}]}
`

// runLegacyWorkspaceRowLoads holds every backend to the stored-row contract:
// a workspace row carrying legacy harness, setup, and default-setup keys
// loads through WorkspaceConfig and RuntimeConfig without validation, yields
// the stage timeouts and review seat count its named default setup declared
// with no executor detail, and is neither rewritten nor versioned by the
// reads (component-persistence; component-runtime; DEC-104(1)).
func runLegacyWorkspaceRowLoads(t *testing.T, x Fixture) {
	if x.SeedWorkspaceConfigYAML == nil || x.WorkspaceConfigYAML == nil {
		t.Fatal("conformance fixture must provide SeedWorkspaceConfigYAML and WorkspaceConfigYAML for the legacy-row case (component-persistence)")
	}
	st, ctx := x.Backend, x.Context
	before, err := st.WorkspaceConfig(ctx)
	requireOK(t, err)
	original := x.WorkspaceConfigYAML(t, ctx)
	t.Cleanup(func() { x.SeedWorkspaceConfigYAML(t, ctx, original) })
	var document map[string]any
	if err := yaml.Unmarshal([]byte(original), &document); err != nil {
		t.Fatal(err)
	}
	var legacyKeys map[string]any
	if err := yaml.Unmarshal([]byte(legacyWorkspaceRowKeys), &legacyKeys); err != nil {
		t.Fatal(err)
	}
	delete(document, "stage_timeouts")
	for key, value := range legacyKeys {
		document[key] = value
	}
	encoded, err := yaml.Marshal(document)
	requireOK(t, err)
	legacy := string(encoded)
	x.SeedWorkspaceConfigYAML(t, ctx, legacy)

	stored, err := st.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatalf("legacy row failed to load: %v", err)
	}
	if stored.Version != before.Version || stored.Document.Workspace != before.Document.Workspace || len(stored.Document.Repos) != len(before.Document.Repos) {
		t.Fatalf("legacy row read = version %d workspace %q repos %d, want version %d workspace %q repos %d",
			stored.Version, stored.Document.Workspace, len(stored.Document.Repos), before.Version, before.Document.Workspace, len(before.Document.Repos))
	}
	if len(stored.Document.Review.Seats) != 3 {
		t.Fatalf("legacy row review seats = %+v, want the default setup's three", stored.Document.Review.Seats)
	}
	for i, seat := range stored.Document.Review.Seats {
		if seat != (config.ReviewSeat{}) {
			t.Fatalf("legacy row kept review seat %d contents: %+v", i, seat)
		}
	}
	runtime, err := st.RuntimeConfig(ctx, x.Config)
	if err != nil {
		t.Fatalf("legacy row failed to compose: %v", err)
	}
	for stage, timeout := range map[string]string{"spec": "45m", "implement": "5h", "review": "2h", "verify": "90m"} {
		route := runtime.Routing.Stages[stage]
		if route.TimeoutText != timeout || route.Model != "" || route.Harness != "" || route.Effort != "" || route.ModelPolicy != "" {
			t.Fatalf("legacy row %s route = %+v, want only the default setup's %s timeout", stage, route, timeout)
		}
	}
	if len(runtime.Review.Seats) != 3 || runtime.MaxBounces != before.Document.MaxBounces {
		t.Fatalf("legacy row runtime policy = seats %d bounces %d", len(runtime.Review.Seats), runtime.MaxBounces)
	}
	if frozen := runtime.FreezePolicy(); frozen.StageTimeouts != (config.PolicyStageTimeouts{Spec: "45m", Implement: "5h", Review: "2h", Verify: "90m"}) || len(frozen.Review.Seats) != 3 {
		t.Fatalf("legacy row froze %+v", frozen)
	}
	if settings := runtime.ExecutionSettings; settings != nil {
		for _, stage := range []config.ImplementationSettings{settings.Spec, settings.Implementation, settings.Verify} {
			if stage.Harness != "" || stage.Model != "" || stage.Effort != "" || stage.ModelPolicy != "" {
				t.Fatalf("legacy row composed executor settings: %+v", settings)
			}
		}
	}
	if after := x.WorkspaceConfigYAML(t, ctx); after != legacy {
		t.Fatalf("reads rewrote the legacy row:\n%s\nwant:\n%s", after, legacy)
	}
	again, err := st.WorkspaceConfig(ctx)
	requireOK(t, err)
	if again.Version != before.Version {
		t.Fatalf("reads advanced the configuration version %d -> %d", before.Version, again.Version)
	}
}
