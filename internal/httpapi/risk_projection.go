package httpapi

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type taskRiskProjectionKey struct{}

func withTaskRiskProjection(ctx context.Context, risks map[string]*core.TaskRisk) context.Context {
	return context.WithValue(ctx, taskRiskProjectionKey{}, risks)
}

func taskRiskProjectionFrom(ctx context.Context) (map[string]*core.TaskRisk, bool) {
	risks, ok := ctx.Value(taskRiskProjectionKey{}).(map[string]*core.TaskRisk)
	return risks, ok
}

// loadTaskRisks projects risk fields for tasks once and stores the map on ctx
// so taskRisk reuses it. Parent hooks this before a task page, inbox, map, or
// MCP list walks taskRisk.
func (s *Server) loadTaskRisks(ctx context.Context, tasks []core.Task) (context.Context, error) {
	return s.loadTaskRisksWith(ctx, tasks, store.RiskProjectionOptions{})
}

// loadTaskRisksWith is loadTaskRisks with preloaded work orders from
// ReadActivityInputs.
func (s *Server) loadTaskRisksWith(ctx context.Context, tasks []core.Task, opts store.RiskProjectionOptions) (context.Context, error) {
	if _, ok := taskRiskProjectionFrom(ctx); ok {
		return ctx, nil
	}
	risks, ctx, err := s.projectTaskRisks(ctx, tasks, opts)
	if err != nil {
		return ctx, err
	}
	return withTaskRiskProjection(ctx, risks), nil
}

func (s *Server) projectTaskRisks(ctx context.Context, tasks []core.Task, opts store.RiskProjectionOptions) (map[string]*core.TaskRisk, context.Context, error) {
	if len(tasks) == 0 {
		return map[string]*core.TaskRisk{}, ctx, nil
	}
	workspace := tasks[0].Workspace
	for _, task := range tasks[1:] {
		if task.Workspace != workspace {
			return nil, ctx, fmt.Errorf("risk projection spans workspaces")
		}
	}
	if workspace != "" {
		ctx = store.WithWorkspace(ctx, workspace)
	}
	needConfig := false
	for _, task := range tasks {
		if !task.SetupContract.Risk.Enabled() {
			continue
		}
		if task.SetupContract.Review.Seats == nil || task.SetupContract.Risk.Enforcing() {
			needConfig = true
			break
		}
	}
	if needConfig {
		var loaded *config.Config
		var cfgErr error
		ctx, loaded, cfgErr = s.requestConfiguration(ctx)
		if cfgErr != nil {
			return nil, ctx, fmt.Errorf("resolve risk workspace inputs: %w", cfgErr)
		}
		if loaded != nil {
			opts.WorkspaceSeats = len(loaded.Review.Seats)
			if opts.TrustedAuthors == nil {
				opts.TrustedAuthors = map[string][]string{}
			}
			for _, task := range tasks {
				if !task.SetupContract.Risk.Enforcing() {
					continue
				}
				repo, _ := loaded.Repo(task.Repo)
				opts.TrustedAuthors[task.ID] = repo.TrustedAuthors
			}
		}
	}
	var st store.Store = s.Store
	if s.Memberships != nil {
		st = store.WithCapabilityAuthorizer(st, s.Memberships)
	}
	risks, err := store.ProjectTaskRisks(ctx, st, tasks, opts)
	if err != nil {
		return nil, ctx, err
	}
	return risks, ctx, nil
}
