package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
)

// GenerateTaskTitle resolves an intake title through the same trusted
// in-process AI client and triage route used by the control-plane pipeline.
// Intake fails if the model cannot produce one valid title; untitled tasks are
// never persisted.
func (d *Dispatcher) GenerateTaskTitle(ctx context.Context, task core.Task) (string, error) {
	if d.Agent == nil {
		return "", fmt.Errorf("in-process agent is not configured")
	}
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return "", err
	}
	if task.SetupContract.HasFrozenPolicy() {
		cfg = cfg.WithSetup(task.SetupContract)
	}
	route, ok := cfg.Routing.Stages[string(core.StageTriage)]
	if !ok || strings.TrimSpace(route.Model) == "" {
		return "", fmt.Errorf("triage route with a model is not configured")
	}
	runCtx := ctx
	cancel := func() {}
	if route.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, route.Timeout)
	}
	defer cancel()
	prompt := fmt.Sprintf(`Generate one concise task title from the submitted task context.
Return only the title as a single plain-text line: no quotes, Markdown, prefix, or commentary.
Keep it under 200 characters and do not invent requirements absent from the context.

Repository: %s
Source: %s

Task description:
%s`, task.Repo, task.Source, task.Body)
	result, err := d.Agent.Run(runCtx, route.Model, inprocess.Input{Prompt: prompt})
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(result.Output)
	if title == "" || len(title) > 200 || strings.ContainsAny(title, "\r\n") {
		return "", fmt.Errorf("AI returned an invalid title")
	}
	return title, nil
}
