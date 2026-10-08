package planning

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// policyOnlyRuntimeDeployment is a deployment file whose planning settings
// reach a policy-only workspace only through RuntimeConfig composition
// (component-planning; component-runtime; DEC-56(3)).
const policyOnlyRuntimeDeployment = `workspace: demo
database: {url: "postgres://db.invalid/conveyor"}
execution_settings:
  control_plane:
    triage: {model: deploy-triage, effort: low, timeout: 7m}
    planning:
      model: deploy-planner
      effort: high
      timeout: %TIMEOUT%
      exploration_output_tokens: 40
planning_models: [deploy-planner, deploy-alternate]
repos: []
`

type policyOnlyRuntime struct {
	ctx         context.Context
	backend     store.Backend
	deployment  *config.Config
	service     *Service
	credentials *atomic.Int64
}

// newPolicyOnlyRuntime stores a policy-only workspace in a volatile backend
// and wires planning to its RuntimeConfig, exactly as conveyord does for a
// durable store.
func newPolicyOnlyRuntime(t *testing.T, timeout string) *policyOnlyRuntime {
	t.Helper()
	t.Setenv(config.ControlPlaneModelEnv, "")
	t.Setenv(config.PlanningModelEnv, "")
	deployment, err := config.ParseDeployment([]byte(strings.Replace(policyOnlyRuntimeDeployment, "%TIMEOUT%", timeout, 1)), "planning-runtime.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	primary := createPlanningRepo(t, filepath.Join(tmp, "primary"), "README.md", strings.Repeat("planning runtime limits line\n", 400))
	seed := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "primary", URL: "file://" + primary, Base: "main"}}}
	manager := planningSnapshotManager(t, seed)
	backend := store.NewVolatileBackend()
	t.Cleanup(backend.Close)
	ctx := store.WithWorkspace(t.Context(), "demo")
	if _, err = backend.BootstrapWorkspaceConfig(ctx, seed); err != nil {
		t.Fatal(err)
	}
	stored, err := backend.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Document.ExecutionSettings != nil || len(stored.Document.PlanningModels) != 0 {
		t.Fatalf("fixture workspace is not policy-only: %+v", stored.Document)
	}
	credentials := &atomic.Int64{}
	runtime := &policyOnlyRuntime{ctx: ctx, backend: backend, deployment: deployment, credentials: credentials}
	runtime.service = &Service{
		Store: backend, Git: manager, Prompt: testPlanningPrompt,
		CredentialContext: func(ctx context.Context, repository string) (context.Context, error) {
			credentials.Add(1)
			return planningTestCredential(ctx, repository)
		},
		ConfigProvider: func(ctx context.Context) (*config.Config, error) {
			return backend.RuntimeConfig(ctx, runtime.deployment)
		},
	}
	return runtime
}

func TestCreateSessionPolicyOnlyRuntimeModelSelection(t *testing.T) {
	runtime := newPolicyOnlyRuntime(t, "9m")
	ctx, service := runtime.ctx, runtime.service
	session, err := service.CreateSession(ctx, CreateSessionInput{})
	if err != nil {
		t.Fatal(err)
	}
	if session.Model != "deploy-planner" || session.Effort != "high" || session.ExplorationOutputTokens != 40 || session.PinnedRevisions["primary"] == "" {
		t.Fatalf("default session = %+v", session)
	}
	alternate, err := service.CreateSession(ctx, CreateSessionInput{ModelOverride: "deploy-alternate"})
	if err != nil || alternate.Model != "deploy-alternate" {
		t.Fatalf("allowlisted alternate = %+v err=%v", alternate, err)
	}
	if _, err = service.CreateSession(ctx, CreateSessionInput{ModelOverride: "workspace-model"}); err == nil ||
		!strings.Contains(err.Error(), "configured models: deploy-planner, deploy-alternate") {
		t.Fatalf("unlisted model error = %v", err)
	}

	// Stage-specific override beats the general override, and either may name
	// a model outside the allowlist; neither changes the deployment.
	t.Setenv(config.ControlPlaneModelEnv, "general-override")
	general, err := service.CreateSession(ctx, CreateSessionInput{})
	if err != nil || general.Model != "general-override" {
		t.Fatalf("general override session = %+v err=%v", general, err)
	}
	t.Setenv(config.PlanningModelEnv, "planning-override")
	specific, err := service.CreateSession(ctx, CreateSessionInput{})
	if err != nil || specific.Model != "planning-override" {
		t.Fatalf("planning override session = %+v err=%v", specific, err)
	}
	if _, err = service.CreateSession(ctx, CreateSessionInput{ModelOverride: "general-override"}); err == nil || !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("inactive general override accepted while the planning override is active: %v", err)
	}
	if runtime.deployment.ExecutionSettings.ControlPlane.Planning.Model != "deploy-planner" ||
		!slices.Equal(runtime.deployment.PlanningModels, []string{"deploy-planner", "deploy-alternate"}) {
		t.Fatalf("deployment mutated: planning=%+v allowlist=%v", runtime.deployment.ExecutionSettings.ControlPlane.Planning, runtime.deployment.PlanningModels)
	}

	sessions, err := runtime.backend.ListPlanningSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credentialsBefore := runtime.credentials.Load()
	absent := *runtime.deployment
	absent.ExecutionSettings, absent.PlanningModels = nil, nil
	settings := *runtime.deployment.ExecutionSettings
	settings.ControlPlane.Planning.Model = ""
	blank := *runtime.deployment
	blank.ExecutionSettings = &settings
	for _, refusal := range []struct {
		name       string
		deployment *config.Config
		override   string
	}{
		{name: "absent settings", deployment: &absent},
		{name: "absent settings with override", deployment: &absent, override: "planning-override"},
		{name: "blank model", deployment: &blank},
	} {
		t.Setenv(config.ControlPlaneModelEnv, "")
		t.Setenv(config.PlanningModelEnv, refusal.override)
		runtime.deployment = refusal.deployment
		_, err := service.CreateSession(ctx, CreateSessionInput{})
		if err == nil || !strings.Contains(err.Error(), config.PlanningControlPlaneRemedy) {
			t.Fatalf("%s: refusal = %v", refusal.name, err)
		}
	}
	after, err := runtime.backend.ListPlanningSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(sessions) || runtime.credentials.Load() != credentialsBefore {
		t.Fatalf("refusals wrote sessions %d -> %d or minted credentials %d -> %d", len(sessions), len(after), credentialsBefore, runtime.credentials.Load())
	}
}

// deadlineAgent records the deadline each model call observes and ends the
// turn with one text decision.
type deadlineAgent struct {
	deadlines []time.Time
	models    []string
	output    string
}

func (a *deadlineAgent) Run(ctx context.Context, model string, _ inprocess.Input) (inprocess.Result, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Time{}
	}
	a.deadlines = append(a.deadlines, deadline)
	a.models = append(a.models, model)
	return inprocess.Result{Output: a.output, Model: model}, nil
}

func TestPlanningPolicyOnlyRuntimeLimits(t *testing.T) {
	for _, limit := range []struct {
		name    string
		timeout string
		want    time.Duration
	}{
		{name: "deployment timeout shorter than the maximum", timeout: "9m", want: 9 * time.Minute},
		{name: "existing maximum below the deployment timeout", timeout: "45m", want: DefaultMaxDuration},
	} {
		t.Run(limit.name, func(t *testing.T) {
			runtime := newPolicyOnlyRuntime(t, limit.timeout)
			session, err := runtime.service.CreateSession(runtime.ctx, CreateSessionInput{})
			if err != nil {
				t.Fatal(err)
			}
			configured, err := time.ParseDuration(limit.timeout)
			if err != nil {
				t.Fatal(err)
			}
			model, effort, timeout, err := runtime.service.modelSettings(runtime.ctx, session)
			if err != nil || model != "deploy-planner" || effort != "high" || timeout != configured {
				t.Fatalf("modelSettings = %q %q %s err=%v", model, effort, timeout, err)
			}
			agent := &deadlineAgent{output: decisionJSON(t, "Which repository?", nil)}
			runtime.service.Agent = agent
			started := time.Now()
			if err = runtime.service.Run(runtime.ctx, session.ID, UserMessage{Content: "Plan."}, func(map[string]any) error { return nil }); err != nil {
				t.Fatal(err)
			}
			finished := time.Now()
			if len(agent.deadlines) != 1 || agent.models[0] != "deploy-planner" {
				t.Fatalf("agent calls = %d models=%v", len(agent.deadlines), agent.models)
			}
			deadline := agent.deadlines[0]
			if deadline.IsZero() || deadline.Before(started.Add(limit.want)) || deadline.After(finished.Add(limit.want)) {
				t.Fatalf("turn deadline %s outside [%s, %s]", deadline, started.Add(limit.want), finished.Add(limit.want))
			}
		})
	}

	t.Run("malformed timeout names its key", func(t *testing.T) {
		runtime := newPolicyOnlyRuntime(t, "9m")
		session, err := runtime.service.CreateSession(runtime.ctx, CreateSessionInput{})
		if err != nil {
			t.Fatal(err)
		}
		malformed := *runtime.deployment
		settings := *runtime.deployment.ExecutionSettings
		settings.ControlPlane.Planning.TimeoutText = "soon"
		malformed.ExecutionSettings = &settings
		runtime.deployment = &malformed
		if _, _, _, err = runtime.service.modelSettings(runtime.ctx, session); err == nil || !strings.Contains(err.Error(), "execution_settings.control_plane.planning.timeout") {
			t.Fatalf("malformed timeout error = %v", err)
		}
	})

	t.Run("exploration cap and low-budget halving", func(t *testing.T) {
		runtime := newPolicyOnlyRuntime(t, "9m")
		ctx := runtime.ctx
		session, err := runtime.service.CreateSession(ctx, CreateSessionInput{})
		if err != nil {
			t.Fatal(err)
		}
		exploration, err := runtime.service.resolveExploration(ctx, session, "")
		if err != nil {
			t.Fatal(err)
		}
		if exploration.capTokens != 40 || exploration.lowBudget {
			t.Fatalf("deployment exploration cap = %d low=%v", exploration.capTokens, exploration.lowBudget)
		}
		read, err := runtime.service.explorationTool(ctx, session, toolCall{
			Name: "read_file", ArgumentsJSON: `{"repo":"primary","path":"README.md","offset":1,"limit":400}`,
		})
		// A 40-token cap (about 160 bytes) pages a 400-line file after its
		// first line; the default 10,000-token cap would return every line.
		if err != nil || !strings.Contains(read.Output.(string), "lines 1–1 of 400") ||
			!strings.Contains(read.Output.(string), "call again with offset=2") || len(read.Output.(string)) > 40*4 {
			t.Fatalf("capped read = %d bytes err=%v output=%v", len(read.Output.(string)), err, read.Output)
		}
		// Fifteen caps of usage is the existing degradation threshold.
		used, err := runtime.backend.GetPlanningSession(ctx, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if remaining := 15*40 - used.ExplorationTokensUsed; remaining > 0 {
			if _, err = runtime.backend.RecordPlanningExplorationTokens(ctx, session.ID, remaining); err != nil {
				t.Fatal(err)
			}
		}
		degraded, err := runtime.service.resolveExploration(ctx, session, "")
		if err != nil {
			t.Fatal(err)
		}
		if degraded.capTokens != 20 || !degraded.lowBudget {
			t.Fatalf("degraded exploration cap = %d low=%v", degraded.capTokens, degraded.lowBudget)
		}
	})
}
