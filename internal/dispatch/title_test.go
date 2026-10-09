package dispatch

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type titleAgent struct {
	result inprocess.Result
	err    error
	model  string
	input  inprocess.Input
}

func (agent *titleAgent) Run(_ context.Context, model string, input inprocess.Input) (inprocess.Result, error) {
	agent.model, agent.input = model, input
	return agent.result, agent.err
}

// clearControlPlaneModelOverrides isolates a test from the process's model
// overrides; t.Setenv also forbids t.Parallel for the test.
func clearControlPlaneModelOverrides(t *testing.T) {
	t.Helper()
	t.Setenv(config.ControlPlaneModelEnv, "")
	t.Setenv(config.TriageModelEnv, "")
	t.Setenv(config.PlanningModelEnv, "")
}

func TestGenerateTaskTitleUsesTrustedTriageRoute(t *testing.T) {
	clearControlPlaneModelOverrides(t)
	agent := &titleAgent{result: inprocess.Result{Output: "Remove required task titles"}}
	d := New(store.NewMemory(), &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Model: "gpt-title", Timeout: time.Second},
	}}}, agent)
	title, err := d.GenerateTaskTitle(t.Context(), core.Task{Repo: "conveyor", Source: "dashboard", Body: "Let AI generate task titles"})
	if err != nil {
		t.Fatal(err)
	}
	if title != "Remove required task titles" || agent.model != "gpt-title" || !strings.Contains(agent.input.Prompt, "Let AI generate task titles") || len(agent.input.Attachments) != 0 {
		t.Fatalf("title=%q model=%q input=%+v", title, agent.model, agent.input)
	}
}

func TestGenerateTaskTitleIgnoresFrozenTaskPolicy(t *testing.T) {
	clearControlPlaneModelOverrides(t)
	agent := &titleAgent{result: inprocess.Result{Output: "Keep intake on control-plane model"}}
	d := New(store.NewMemory(), &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Model: "deployment-title-model", Timeout: time.Second},
	}}}, agent)
	policy := config.ExecutionSetup{
		MaxBounces: 3,
		ExecutionSettings: config.ContextualExecutionSettings{
			Implementation: config.ImplementationSettings{TimeoutText: "2h"},
		},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
	}
	title, err := d.GenerateTaskTitle(t.Context(), core.Task{Body: "Create a normal task", SetupContract: policy})
	if err != nil {
		t.Fatal(err)
	}
	if title != "Keep intake on control-plane model" || agent.model != "deployment-title-model" {
		t.Fatalf("title=%q model=%q", title, agent.model)
	}
}

// TestGenerateTaskTitleModelOverrides proves title generation resolves its
// model through the shared triage-stage precedence: trimmed
// CONVEYOR_TRIAGE_MODEL, then CONVEYOR_CONTROL_PLANE_MODEL, then the configured
// triage route. Planning overrides never apply, and neither the configuration
// nor the frozen task policy changes the outcome (component-triage).
func TestGenerateTaskTitleModelOverrides(t *testing.T) {
	for _, test := range []struct {
		name                      string
		general, triage, planning string
		want                      string
	}{
		{name: "unset uses deployment route", want: "deployment-triage"},
		{name: "whitespace-only overrides are unset", general: " \t ", triage: "  ", want: "deployment-triage"},
		{name: "triage override", triage: "triage-override", want: "triage-override"},
		{name: "triage override is trimmed", triage: "  triage-override\n", want: "triage-override"},
		{name: "triage override wins over general", general: "general-override", triage: "triage-override", want: "triage-override"},
		{name: "general override is the fallback", general: " general-override ", want: "general-override"},
		{name: "general fallback under whitespace triage", general: "general-override", triage: " ", want: "general-override"},
		{name: "planning override is ignored", planning: "planning-override", want: "deployment-triage"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(config.ControlPlaneModelEnv, test.general)
			t.Setenv(config.TriageModelEnv, test.triage)
			t.Setenv(config.PlanningModelEnv, test.planning)
			cfg := &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
				"triage": {Model: "deployment-triage", Effort: "low", Timeout: time.Minute, TimeoutText: "1m", Execution: config.ExecutionInProcess},
			}}}
			before := cfg.Routing.Stages["triage"]
			frozen := (&config.Config{MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
				"triage": {Execution: config.ExecutionInProcess, Model: "frozen-triage", TimeoutText: "5m"},
			}}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}}).FreezePolicy()
			agent := &titleAgent{result: inprocess.Result{Output: "Honor the triage override"}}
			d := New(store.NewMemory(), nil, agent)
			d.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
			body := "Keep the submitted body\nexactly as written"
			title, err := d.GenerateTaskTitle(t.Context(), core.Task{Repo: "conveyor", Source: "mcp", Body: body, SetupContract: frozen})
			if err != nil {
				t.Fatal(err)
			}
			if title != "Honor the triage override" || agent.model != test.want {
				t.Fatalf("title=%q model=%q, want model %q", title, agent.model, test.want)
			}
			if !strings.HasPrefix(agent.input.Prompt, "Generate one concise task title") || !strings.Contains(agent.input.Prompt, "Task description:\n"+body) {
				t.Fatalf("prompt changed: %q", agent.input.Prompt)
			}
			if after := cfg.Routing.Stages["triage"]; !reflect.DeepEqual(after, before) || len(cfg.Routing.Stages) != 1 {
				t.Fatalf("configuration changed: before=%+v after=%+v", before, cfg.Routing.Stages)
			}
		})
	}
}

// TestGenerateTaskTitlePreservesRouteValidationAndTimeout proves an override
// does not stand in for a missing or blank triage route, and the configured
// route's timeout still bounds the call.
func TestGenerateTaskTitlePreservesRouteValidationAndTimeout(t *testing.T) {
	t.Setenv(config.ControlPlaneModelEnv, "general-override")
	t.Setenv(config.TriageModelEnv, "triage-override")
	t.Setenv(config.PlanningModelEnv, "")
	for name, stages := range map[string]map[string]config.StageRoute{
		"missing route": {"review": {Model: "reviewer"}},
		"blank model":   {"triage": {Model: "  ", Timeout: time.Minute}},
	} {
		t.Run(name, func(t *testing.T) {
			agent := &deadlineTitleAgent{}
			d := New(store.NewMemory(), &config.Config{Routing: config.Routing{Stages: stages}}, agent)
			if _, err := d.GenerateTaskTitle(t.Context(), core.Task{Body: "context"}); err == nil || !strings.Contains(err.Error(), "triage route with a model is not configured") {
				t.Fatalf("error = %v", err)
			}
			if agent.calls != 0 {
				t.Fatalf("agent called %d times without a configured route", agent.calls)
			}
		})
	}
	t.Run("route timeout", func(t *testing.T) {
		agent := &deadlineTitleAgent{}
		d := New(store.NewMemory(), &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{
			"triage": {Model: "deployment-triage", Timeout: 7 * time.Minute},
		}}}, agent)
		started := time.Now()
		if _, err := d.GenerateTaskTitle(t.Context(), core.Task{Body: "context"}); err != nil {
			t.Fatal(err)
		}
		finished := time.Now()
		if agent.calls != 1 || agent.model != "triage-override" || !agent.hasDeadline ||
			agent.deadline.Before(started.Add(7*time.Minute)) || agent.deadline.After(finished.Add(7*time.Minute)) {
			t.Fatalf("call=%+v, want triage-override with a 7m deadline in [%s, %s]", agent, started.Add(7*time.Minute), finished.Add(7*time.Minute))
		}
	})
}

type deadlineTitleAgent struct {
	calls       int
	model       string
	deadline    time.Time
	hasDeadline bool
}

func (agent *deadlineTitleAgent) Run(ctx context.Context, model string, _ inprocess.Input) (inprocess.Result, error) {
	agent.calls++
	agent.model = model
	agent.deadline, agent.hasDeadline = ctx.Deadline()
	return inprocess.Result{Output: "Bounded title"}, nil
}

// concurrentTitleAgent records every model it receives. Each call waits on a
// shared start barrier so the calls overlap deterministically.
type concurrentTitleAgent struct {
	mu      sync.Mutex
	models  []string
	start   <-chan struct{}
	arrived sync.WaitGroup
}

func (agent *concurrentTitleAgent) Run(_ context.Context, model string, input inprocess.Input) (inprocess.Result, error) {
	agent.arrived.Done()
	<-agent.start
	agent.mu.Lock()
	agent.models = append(agent.models, model)
	agent.mu.Unlock()
	return inprocess.Result{Output: "Concurrent title"}, nil
}

// TestGenerateTaskTitleConcurrentCallsKeepConfigImmutable runs overlapping
// title calls against one shared configuration under a fixed override. Every
// call resolves the override, and the shared route is unchanged; the focused
// race target runs it under -race.
func TestGenerateTaskTitleConcurrentCallsKeepConfigImmutable(t *testing.T) {
	t.Setenv(config.ControlPlaneModelEnv, "general-override")
	t.Setenv(config.TriageModelEnv, "triage-override")
	t.Setenv(config.PlanningModelEnv, "planning-override")
	cfg := &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Model: "deployment-triage", Timeout: time.Minute, TimeoutText: "1m"},
	}}}
	before := cfg.Routing.Stages["triage"]
	const calls = 16
	start := make(chan struct{})
	agent := &concurrentTitleAgent{start: start}
	agent.arrived.Add(calls)
	d := New(store.NewMemory(), cfg, agent)
	errs := make(chan error, calls)
	var done sync.WaitGroup
	for range calls {
		done.Add(1)
		go func() {
			defer done.Done()
			_, err := d.GenerateTaskTitle(t.Context(), core.Task{Body: "context"})
			errs <- err
		}()
	}
	// Every call has resolved its model and reached the agent before any returns.
	agent.arrived.Wait()
	close(start)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(agent.models) != calls {
		t.Fatalf("models=%v", agent.models)
	}
	for _, model := range agent.models {
		if model != "triage-override" {
			t.Fatalf("models=%v, want every call on triage-override", agent.models)
		}
	}
	if after := cfg.Routing.Stages["triage"]; !reflect.DeepEqual(after, before) || len(cfg.Routing.Stages) != 1 {
		t.Fatalf("configuration changed: before=%+v after=%+v", before, cfg.Routing.Stages)
	}
}

// TestGenerateTaskTitleRejectsInvalidOutputAndProviderFailure keeps the title
// rules and fail-closed provider handling unchanged under an override (DEC-8).
func TestGenerateTaskTitleRejectsInvalidOutputAndProviderFailure(t *testing.T) {
	t.Setenv(config.ControlPlaneModelEnv, "")
	t.Setenv(config.TriageModelEnv, "triage-override")
	t.Setenv(config.PlanningModelEnv, "")
	cfg := &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt"}}}}
	maxTitle := strings.Repeat("a", 200)
	for _, test := range []struct {
		name    string
		result  inprocess.Result
		err     error
		want    string
		wantErr string
	}{
		{name: "empty", result: inprocess.Result{Output: "  \n "}, wantErr: "invalid title"},
		{name: "multiline", result: inprocess.Result{Output: "Title\nCommentary"}, wantErr: "invalid title"},
		{name: "carriage return", result: inprocess.Result{Output: "Title\rCommentary"}, wantErr: "invalid title"},
		{name: "200 bytes accepted", result: inprocess.Result{Output: maxTitle}, want: maxTitle},
		{name: "201 bytes refused", result: inprocess.Result{Output: maxTitle + "a"}, wantErr: "invalid title"},
		{name: "provider", err: errors.New("provider unavailable"), wantErr: "provider unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := &titleAgent{result: test.result, err: test.err}
			d := New(store.NewMemory(), cfg, agent)
			title, err := d.GenerateTaskTitle(t.Context(), core.Task{Body: "context"})
			if agent.model != "triage-override" {
				t.Fatalf("model=%q, want triage-override", agent.model)
			}
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || title != "" {
					t.Fatalf("title=%q err=%v, want error containing %q", title, err, test.wantErr)
				}
				return
			}
			if err != nil || title != test.want {
				t.Fatalf("title=%q err=%v, want %q", title, err, test.want)
			}
		})
	}
}
