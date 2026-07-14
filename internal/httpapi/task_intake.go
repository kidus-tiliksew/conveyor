package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type taskCreateResult struct {
	Task    core.Task
	Created bool
}

type taskCreateError struct {
	Status  int
	Message string
}

func (e *taskCreateError) Error() string { return e.Message }

func taskCreateStatus(err error) int {
	if typed, ok := err.(*taskCreateError); ok {
		return typed.Status
	}
	return http.StatusInternalServerError
}

// createTaskRecord is the single durable intake path for HTTP and MCP. MCP
// callers provide an idempotency key so transport retries return the original
// task without enqueueing Luna triage twice (spec §17.4, §21.5).
func (s *Server) createTaskRecord(ctx context.Context, req createTaskReq, intakeKey, defaultSource string) (taskCreateResult, error) {
	req.Title = strings.TrimSpace(req.Title)
	req.Repo = strings.TrimSpace(req.Repo)
	req.BaseBranch = strings.TrimSpace(req.BaseBranch)
	req.Source = strings.TrimSpace(req.Source)
	intakeKey = strings.TrimSpace(intakeKey)
	if req.Title == "" {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "title is required"}
	}
	if len(req.Title) > 200 {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "title must be at most 200 characters"}
	}
	if req.Repo == "" {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "repo is required"}
	}
	if req.Source == "" {
		req.Source = defaultSource
	}
	if req.Level == "" {
		req.Level = core.L2
	}
	if req.Level != core.L0 && req.Level != core.L1 && req.Level != core.L2 && req.Level != core.L3 {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "level must be L0, L1, L2, or L3"}
	}
	if len(intakeKey) > 200 {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "idempotency_key must be at most 200 characters"}
	}

	if intakeKey != "" {
		if existing, found, err := s.Store.GetTaskByIntakeKey(ctx, intakeKey); err != nil {
			return taskCreateResult{}, err
		} else if found {
			if !sameIntake(existing, req) {
				return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: "idempotency_key is already used by a different task"}
			}
			return taskCreateResult{Task: existing}, nil
		}
	}

	repos := s.Repos
	workspace, _ := store.WorkspaceFromContext(ctx)
	var current *config.Config
	if s.ConfigProvider != nil {
		var err error
		current, err = s.ConfigProvider(ctx)
		if err != nil {
			return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: err.Error()}
		}
		repos = current.RepoNames()
		workspace = current.Workspace
	}
	if repos != nil && !contains(repos, req.Repo) {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "unknown repo " + req.Repo}
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
		if current != nil {
			if repo, ok := current.Repo(req.Repo); ok {
				req.BaseBranch = repo.Base
			}
		}
	}

	id := core.NewTaskID()
	task := core.Task{
		ID:         id,
		Workspace:  workspace,
		Source:     req.Source,
		IntakeKey:  intakeKey,
		Title:      req.Title,
		Body:       req.Body,
		Level:      req.Level,
		Repo:       req.Repo,
		BaseBranch: req.BaseBranch,
		Branch:     gitx.BranchName(id),
		State:      core.TaskQueued,
		NextStage:  core.InitialStage(req.Level),
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.Store.CreateTask(ctx, task); err != nil {
		// A concurrent retry may win the unique intake-key race between the
		// lookup and insert. Resolve that race as the same idempotent result.
		if intakeKey != "" {
			if existing, found, getErr := s.Store.GetTaskByIntakeKey(ctx, intakeKey); getErr == nil && found {
				if sameIntake(existing, req) {
					return taskCreateResult{Task: existing}, nil
				}
				return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: "idempotency_key is already used by a different task"}
			}
		}
		return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: fmt.Sprintf("create task: %v", err)}
	}
	if s.OnCreate != nil {
		s.OnCreate(ctx, id)
	}
	return taskCreateResult{Task: task, Created: true}, nil
}

func sameIntake(task core.Task, req createTaskReq) bool {
	return task.Title == req.Title &&
		task.Body == req.Body &&
		task.Repo == req.Repo &&
		task.Source == req.Source &&
		task.Level == req.Level &&
		(req.BaseBranch == "" || task.BaseBranch == req.BaseBranch)
}
