package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runtimeControlPlaneAgent records the model, effort, and deadline of each
// in-process call; it answers title prompts with a title and every other
// prompt with a triage verdict.
type runtimeControlPlaneAgent struct {
	calls []runtimeControlPlaneCall
	title inprocess.Result
	err   error
}

type runtimeControlPlaneCall struct {
	model    string
	effort   string
	deadline time.Time
	title    bool
}

func (agent *runtimeControlPlaneAgent) Run(ctx context.Context, model string, input inprocess.Input) (inprocess.Result, error) {
	deadline, _ := ctx.Deadline()
	title := strings.HasPrefix(input.Prompt, "Generate one concise task title")
	agent.calls = append(agent.calls, runtimeControlPlaneCall{model: model, effort: input.Effort, deadline: deadline, title: title})
	if title {
		return agent.title, agent.err
	}
	return inprocess.Result{Output: "```conveyor:triage\n{\"class\":\"chore\",\"route\":\"proceed\",\"summary\":\"runtime route\"}\n```"}, nil
}

// TestPolicyOnlyRuntimePreservesTriageAndTitle pins the already-working
// readers: a policy-only workspace read through RuntimeConfig keeps the
// deployment's triage route for triage and title generation, and a frozen
// task policy cannot replace it (component-triage; component-runtime).
func TestPolicyOnlyRuntimePreservesTriageAndTitle(t *testing.T) {
	t.Setenv(config.ControlPlaneModelEnv, "")
	t.Setenv(config.TriageModelEnv, "")
	deployment, err := config.ParseDeployment([]byte(`workspace: demo
database: {url: "postgres://db.invalid/conveyor"}
execution_settings:
  control_plane:
    triage: {model: deploy-triage, effort: low, timeout: 7m}
    planning: {model: deploy-planner, timeout: 9m}
repos: []
`), "triage-runtime.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	backend := store.NewVolatileBackend()
	t.Cleanup(backend.Close)
	ctx := store.WithWorkspace(t.Context(), "demo")
	workspace := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "api", URL: "https://github.com/example/api", Base: "main"}}}
	if _, err = backend.BootstrapWorkspaceConfig(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	stored, err := backend.WorkspaceConfig(ctx)
	if err != nil || stored.Document.ExecutionSettings != nil || len(stored.Document.Routing.Stages) != 0 {
		t.Fatalf("fixture workspace is not policy-only: %+v err=%v", stored.Document, err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &runtimeControlPlaneAgent{title: inprocess.Result{Output: "Keep the deployment triage route"}}
	d := New(backend, nil, agent)
	d.Pack = bundle
	d.ConfigProvider = func(ctx context.Context) (*config.Config, error) { return backend.RuntimeConfig(ctx, deployment) }

	// A frozen policy from a workspace with different stage timeouts.
	frozen := (&config.Config{MaxBounces: 4, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage":    {Execution: config.ExecutionInProcess, Model: "frozen-triage", TimeoutText: "1m"},
		"spec":      {Execution: config.ExecutionMCP, TimeoutText: "10m"},
		"implement": {Execution: config.ExecutionMCP, TimeoutText: "1h"},
		"review":    {Execution: config.ExecutionMCP, TimeoutText: "20m"},
	}}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}}).FreezePolicy()
	task := core.Task{ID: "runtime-triage", Workspace: "demo", Repo: "api", Title: "Runtime triage", Body: "Keep the deployment triage route",
		State: core.TaskQueued, NextStage: core.StageTriage, SetupContract: frozen, PolicyVersion: 1, CreatedAt: time.Now().UTC()}
	if err = backend.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	finished := time.Now()
	if len(agent.calls) != 1 || agent.calls[0].title {
		t.Fatalf("triage calls = %+v", agent.calls)
	}
	triage := agent.calls[0]
	if triage.model != "deploy-triage" || triage.effort != "low" ||
		triage.deadline.Before(started.Add(7*time.Minute)) || triage.deadline.After(finished.Add(7*time.Minute)) {
		t.Fatalf("triage call = %+v, want deploy-triage/low with a 7m deadline in [%s, %s]", triage, started.Add(7*time.Minute), finished.Add(7*time.Minute))
	}
	jobs, err := backend.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 || jobs[0].Stage != core.StageTriage || jobs[0].ModelTier != "deploy-triage" {
		t.Fatalf("triage jobs = %+v err=%v", jobs, err)
	}

	started = time.Now()
	title, err := d.GenerateTaskTitle(ctx, task)
	finished = time.Now()
	if err != nil || title != "Keep the deployment triage route" {
		t.Fatalf("title = %q err=%v", title, err)
	}
	generated := agent.calls[len(agent.calls)-1]
	if !generated.title || generated.model != "deploy-triage" ||
		generated.deadline.Before(started.Add(7*time.Minute)) || generated.deadline.After(finished.Add(7*time.Minute)) {
		t.Fatalf("title call = %+v", generated)
	}

	// The current title semantics stay: invalid output and provider failures
	// refuse, and no process override applies to title generation.
	t.Setenv(config.TriageModelEnv, "triage-override")
	if _, err = d.GenerateTaskTitle(ctx, task); err != nil || agent.calls[len(agent.calls)-1].model != "deploy-triage" {
		t.Fatalf("title model under a triage override = %q err=%v", agent.calls[len(agent.calls)-1].model, err)
	}
	agent.title = inprocess.Result{Output: "Title\nCommentary"}
	if _, err = d.GenerateTaskTitle(ctx, task); err == nil || !strings.Contains(err.Error(), "invalid title") {
		t.Fatalf("multiline title error = %v", err)
	}
	agent.title, agent.err = inprocess.Result{}, errors.New("provider unavailable")
	if _, err = d.GenerateTaskTitle(ctx, task); err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("provider error = %v", err)
	}
}
