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

func TestGenerateTaskTitleUsesTrustedTriageRoute(t *testing.T) {
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

func TestGenerateTaskTitleRejectsInvalidOutputAndProviderFailure(t *testing.T) {
	cfg := &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt"}}}}
	for _, test := range []struct {
		name   string
		result inprocess.Result
		err    error
	}{
		{name: "multiline", result: inprocess.Result{Output: "Title\nCommentary"}},
		{name: "provider", err: errors.New("provider unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := New(store.NewMemory(), cfg, &titleAgent{result: test.result, err: test.err})
			if _, err := d.GenerateTaskTitle(t.Context(), core.Task{Body: "context"}); err == nil {
				t.Fatal("generation succeeded")
			}
		})
	}
}
