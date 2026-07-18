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
	return s.createTaskRecordWithState(ctx, req, intakeKey, defaultSource, core.TaskQueued)
}

func (s *Server) createTaskRecordWithState(ctx context.Context, req createTaskReq, intakeKey, defaultSource string, initialState core.TaskState) (taskCreateResult, error) {
	req.Title = strings.TrimSpace(req.Title)
	req.Repo = strings.TrimSpace(req.Repo)
	req.BaseBranch = strings.TrimSpace(req.BaseBranch)
	req.Source = strings.TrimSpace(req.Source)
	intakeKey = strings.TrimSpace(intakeKey)
	if len(req.Title) > 200 {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "title must be at most 200 characters"}
	}
	if req.Title == "" && strings.TrimSpace(req.Body) == "" {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "body is required when title is omitted"}
	}
	if req.Repo == "" {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "repo is required"}
	}
	if req.Source == "" {
		req.Source = defaultSource
	}
	if req.Level != "" && req.Level != core.L0 && req.Level != core.L1 && req.Level != core.L2 && req.Level != core.L3 {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "level must be L0, L1, L2, or L3"}
	}
	if req.Mode != "" && !req.Mode.Valid() {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "mode must be auto or manual"}
	}
	if len(intakeKey) > 200 {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "idempotency_key must be at most 200 characters"}
	}
	originalReq := req
	if intakeKey != "" {
		if existing, found, err := s.Store.GetTaskByIntakeKey(ctx, intakeKey); err != nil {
			return taskCreateResult{}, err
		} else if found {
			if !sameIntakeRequest(existing, originalReq) {
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
	explicitMode := req.Mode != ""
	fellBack := false
	var specApproval, mergeApproval bool
	if req.Mode == "" && req.Level != "" {
		req.Mode, specApproval, mergeApproval = core.LegacyPolicy(req.Level)
	} else if current != nil {
		if req.Mode == "" {
			req.Mode = core.TaskMode(current.Execution.DefaultMode)
		}
		specApproval, mergeApproval = current.Execution.SpecApproval, current.Execution.MergeApproval
	} else {
		if req.Mode == "" {
			req.Mode = core.TaskModeAuto
		}
		specApproval, mergeApproval = true, true
	}
	if req.SpecApproval != nil {
		specApproval = *req.SpecApproval
	}
	if req.MergeApproval != nil {
		mergeApproval = *req.MergeApproval
	}
	if req.Mode == core.TaskModeAuto && s.Workers != nil && current != nil {
		available, reason := s.Workers.AutoAvailable(ctx, current)
		if !available && explicitMode {
			return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: "auto mode unavailable: " + reason}
		}
		if !available {
			req.Mode = core.TaskModeManual
			fellBack = true
		}
	}
	req.Level = core.LegacyLevel(req.Mode, specApproval, mergeApproval)
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
	if req.Title == "" {
		if s.GenerateTaskTitle == nil {
			return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: "task title generation is unavailable"}
		}
		generated, err := s.GenerateTaskTitle(ctx, core.Task{Source: req.Source, Body: req.Body, Repo: req.Repo})
		if err != nil {
			return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: fmt.Sprintf("generate task title: %v", err)}
		}
		req.Title = strings.TrimSpace(generated)
		if req.Title == "" || len(req.Title) > 200 {
			return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: "generate task title: AI returned an invalid title"}
		}
	}

	id := core.NewTaskID()
	task := core.Task{
		ID:            id,
		Workspace:     workspace,
		Source:        req.Source,
		IntakeKey:     intakeKey,
		Title:         req.Title,
		Body:          req.Body,
		Level:         req.Level,
		Mode:          req.Mode,
		SpecApproval:  specApproval,
		MergeApproval: mergeApproval,
		PolicyVersion: 1,
		Repo:          req.Repo,
		BaseBranch:    req.BaseBranch,
		Branch:        gitx.BranchName(id),
		State:         initialState,
		NextStage:     core.InitialStage(req.Level),
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.Store.CreateTask(ctx, task); err != nil {
		// A concurrent retry may win the unique intake-key race between the
		// lookup and insert. Resolve that race as the same idempotent result.
		if intakeKey != "" {
			if existing, found, getErr := s.Store.GetTaskByIntakeKey(ctx, intakeKey); getErr == nil && found {
				if sameIntakeRequest(existing, originalReq) {
					return taskCreateResult{Task: existing}, nil
				}
				return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: "idempotency_key is already used by a different task"}
			}
		}
		return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: fmt.Sprintf("create task: %v", err)}
	}
	if fellBack {
		_ = s.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "task.auto_fallback", Payload: core.JSONPayload(map[string]string{"requested": "workspace-default-auto", "resolved": "manual", "reason": "worker-or-routed-harness-unhealthy"})})
	}
	if initialState == core.TaskQueued && s.OnCreate != nil {
		s.OnCreate(ctx, id)
	}
	return taskCreateResult{Task: task, Created: true}, nil
}

func sameIntakeRequest(task core.Task, req createTaskReq) bool {
	if (req.Title != "" && task.Title != req.Title) || task.Body != req.Body || task.Repo != req.Repo || task.Source != req.Source || (req.BaseBranch != "" && task.BaseBranch != req.BaseBranch) {
		return false
	}
	if req.Mode == "" && req.Level != "" {
		mode, specApproval, mergeApproval := core.LegacyPolicy(req.Level)
		if req.SpecApproval != nil {
			specApproval = *req.SpecApproval
		}
		if req.MergeApproval != nil {
			mergeApproval = *req.MergeApproval
		}
		return task.Mode == mode && task.SpecApproval == specApproval && task.MergeApproval == mergeApproval
	}
	if req.Mode != "" && task.Mode != req.Mode {
		return false
	}
	if req.SpecApproval != nil && task.SpecApproval != *req.SpecApproval {
		return false
	}
	if req.MergeApproval != nil && task.MergeApproval != *req.MergeApproval {
		return false
	}
	return true
}
