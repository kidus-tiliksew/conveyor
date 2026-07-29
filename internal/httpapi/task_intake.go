package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sort"
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
	req.Repo = strings.TrimSpace(req.Repo)
	req.BaseBranch = strings.TrimSpace(req.BaseBranch)
	req.Source = strings.TrimSpace(req.Source)
	req.Setup = strings.TrimSpace(req.Setup)
	for index := range req.DependsOn {
		req.DependsOn[index] = strings.TrimSpace(req.DependsOn[index])
	}
	intakeKey = strings.TrimSpace(intakeKey)
	if strings.TrimSpace(req.Body) == "" {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "body is required"}
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
	// Deprecated §21.31 change 6 input: mode is accepted through the v1.33
	// window, mapping manual→hold and auto→no-op, but never persisted.
	if req.Mode != "" && !req.Mode.Valid() {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "mode is deprecated and must be auto or manual when supplied"}
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
	var selectedSetup config.ExecutionSetup
	if current != nil {
		var ok bool
		selectedSetup, ok = current.Setup(req.Setup)
		if !ok {
			return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "unknown setup " + req.Setup}
		}
		req.Setup = selectedSetup.Name
	}
	// §21.31: no mode axis, no intake-time health gating — serviceability is
	// advisory and orders queue openly. Hold is the only reservation input.
	hold, specApproval, mergeApproval := resolvedIntakePolicy(req, current)
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
	if s.GenerateTaskTitle == nil {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: "task title generation is unavailable"}
	}
	generated, err := s.GenerateTaskTitle(ctx, core.Task{Source: req.Source, Body: req.Body, Repo: req.Repo, SetupName: req.Setup, SetupContract: selectedSetup})
	if err != nil {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: fmt.Sprintf("generate task title: %v", err)}
	}
	title := strings.TrimSpace(generated)
	if title == "" || len(title) > 200 {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: "generate task title: AI returned an invalid title"}
	}

	id := core.NewTaskID()
	task := core.Task{
		ID:            id,
		Workspace:     workspace,
		Source:        req.Source,
		IntakeKey:     intakeKey,
		Title:         title,
		Body:          req.Body,
		Level:         req.Level,
		Hold:          hold,
		SpecApproval:  specApproval,
		MergeApproval: mergeApproval,
		PolicyVersion: 1,
		SetupName:     req.Setup,
		SetupContract: selectedSetup,
		Repo:          req.Repo,
		BaseBranch:    req.BaseBranch,
		Branch:        gitx.BranchName(id),
		State:         initialState,
		NextStage:     core.StageTriage,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.Store.CreateTaskWithDependencies(ctx, task, req.DependsOn); err != nil {
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
	if req.Mode != "" {
		// Deprecated usage is recorded, never persisted on the task (§21.31 change 6).
		_ = s.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "task.mode.deprecated", Payload: core.JSONPayload(map[string]any{"mode": req.Mode, "mapped_hold": req.Mode == core.TaskModeManual})})
	}
	if initialState == core.TaskQueued && s.OnCreate != nil {
		s.OnCreate(ctx, id)
	}
	return taskCreateResult{Task: task, Created: true}, nil
}

// resolvedIntakePolicy maps the request onto the three §21.31 policy
// decisions: hold, spec approval, merge approval. A legacy escalation level
// or deprecated mode contributes through its accepted mapping; explicit gate
// overrides win last.
func resolvedIntakePolicy(req createTaskReq, current *config.Config) (bool, bool, bool) {
	hold := req.Hold || req.Mode == core.TaskModeManual
	var specApproval, mergeApproval bool
	if req.Mode == "" && req.Level != "" {
		var legacyHold bool
		legacyHold, specApproval, mergeApproval = core.LegacyPolicy(req.Level)
		hold = hold || legacyHold
	} else if current != nil {
		specApproval, mergeApproval = current.Execution.SpecApproval, current.Execution.MergeApproval
	} else {
		specApproval, mergeApproval = true, true
	}
	if req.SpecApproval != nil {
		specApproval = *req.SpecApproval
	}
	if req.MergeApproval != nil {
		mergeApproval = *req.MergeApproval
	}
	return hold, specApproval, mergeApproval
}

func sameIntakeRequest(task core.Task, req createTaskReq) bool {
	if task.Body != req.Body || task.Repo != req.Repo || task.Source != req.Source || (req.BaseBranch != "" && task.BaseBranch != req.BaseBranch) {
		return false
	}
	if req.Setup != "" && task.SetupName != req.Setup {
		return false
	}
	if len(req.DependsOn) > 0 {
		actual := make([]string, 0, len(task.Dependencies))
		for _, dependency := range task.Dependencies {
			actual = append(actual, dependency.ID)
		}
		expected := append([]string(nil), req.DependsOn...)
		sort.Strings(actual)
		sort.Strings(expected)
		if !reflect.DeepEqual(actual, expected) {
			return false
		}
	}
	if (req.Hold || req.Mode == core.TaskModeManual) && !task.Hold {
		return false
	}
	if req.SpecApproval != nil && task.SpecApproval != *req.SpecApproval {
		return false
	}
	if req.MergeApproval != nil && task.MergeApproval != *req.MergeApproval {
		return false
	}
	if req.Mode == "" && req.Level != "" {
		legacyHold, specApproval, mergeApproval := core.LegacyPolicy(req.Level)
		if req.SpecApproval == nil && task.SpecApproval != specApproval {
			return false
		}
		if req.MergeApproval == nil && task.MergeApproval != mergeApproval {
			return false
		}
		if legacyHold && !task.Hold {
			return false
		}
	}
	return true
}
