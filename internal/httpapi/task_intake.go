package httpapi

import (
	"context"
	"encoding/json"
	"errors"
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
	Code    string
	Message string
}

func (e *taskCreateError) Error() string { return e.Message }

func taskCreateStatus(err error) int {
	if typed, ok := err.(*taskCreateError); ok {
		return typed.Status
	}
	return http.StatusInternalServerError
}

func writeTaskCreateError(w http.ResponseWriter, err error) {
	typed, ok := err.(*taskCreateError)
	if !ok || typed.Code == "" {
		http.Error(w, err.Error(), taskCreateStatus(err))
		return
	}
	writeJSON(w, typed.Status, map[string]string{"error": typed.Code, "message": typed.Message})
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
	attached, err := store.NormalizeTaskContextInput(store.TaskContextInput{RequirementIDs: req.RequirementIDs, DesignIDs: req.SystemDesignIDs})
	if err != nil {
		return taskCreateResult{}, taskContextCreateError(err)
	}
	req.RequirementIDs, req.SystemDesignIDs = attached.RequirementIDs, attached.DesignIDs
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
			active, intake, contextErr := s.taskContextsForRetry(ctx, existing.ID)
			if contextErr != nil {
				return taskCreateResult{}, contextErr
			}
			existing.Context = active
			if !sameIntakeRequest(existing, originalReq, intake) {
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
	if err := s.Store.ValidateTaskDependencies(ctx, req.DependsOn); err != nil {
		return taskCreateResult{}, &taskCreateError{
			Status: http.StatusBadRequest, Code: "invalid_dependencies",
			Message: fmt.Sprintf("invalid depends_on: %v", err),
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
	// The create event is immutable retry authority. Keep the intake-time IDs
	// on its task payload so later context edits do not change the meaning of
	// an otherwise byte-identical idempotent retry.
	task.Context = intakeTaskContext(attached)
	if err := s.Store.CreateTaskWithDependenciesAndContext(ctx, task, req.DependsOn, attached); err != nil {
		// A concurrent retry may win the unique intake-key race between the
		// lookup and insert. Resolve that race as the same idempotent result.
		if intakeKey != "" {
			if existing, found, getErr := s.Store.GetTaskByIntakeKey(ctx, intakeKey); getErr != nil {
				return taskCreateResult{}, getErr
			} else if found {
				active, intake, contextErr := s.taskContextsForRetry(ctx, existing.ID)
				if contextErr != nil {
					return taskCreateResult{}, contextErr
				}
				existing.Context = active
				if sameIntakeRequest(existing, originalReq, intake) {
					return taskCreateResult{Task: existing}, nil
				}
				return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: "idempotency_key is already used by a different task"}
			}
		}
		var referenceErr *store.TaskContextReferenceError
		if errors.As(err, &referenceErr) {
			return taskCreateResult{}, taskContextCreateError(referenceErr)
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
	task.Context, _ = store.TaskContextForTask(ctx, s.Store, task.ID)
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

func sameIntakeRequest(task core.Task, req createTaskReq, intake store.TaskContextInput) bool {
	if task.Body != req.Body || task.Repo != req.Repo || task.Source != req.Source || (req.BaseBranch != "" && task.BaseBranch != req.BaseBranch) {
		return false
	}
	if req.Setup != "" && task.SetupName != req.Setup {
		return false
	}
	actual := make([]string, 0, len(task.Dependencies))
	for _, dependency := range task.Dependencies {
		actual = append(actual, dependency.ID)
	}
	expected := make([]string, len(req.DependsOn))
	copy(expected, req.DependsOn)
	sort.Strings(actual)
	sort.Strings(expected)
	if !reflect.DeepEqual(actual, expected) {
		return false
	}
	if !reflect.DeepEqual(intake.RequirementIDs, req.RequirementIDs) || !reflect.DeepEqual(intake.DesignIDs, req.SystemDesignIDs) {
		return false
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

func intakeTaskContext(input store.TaskContextInput) core.TaskContext {
	result := core.TaskContext{
		Requirements: make([]core.TaskRequirementContext, 0, len(input.RequirementIDs)),
		Designs:      make([]core.TaskDesignContext, 0, len(input.DesignIDs)),
	}
	for _, id := range input.RequirementIDs {
		result.Requirements = append(result.Requirements, core.TaskRequirementContext{ID: id})
	}
	for _, id := range input.DesignIDs {
		result.Designs = append(result.Designs, core.TaskDesignContext{ID: id})
	}
	return result
}

func (s *Server) taskContextsForRetry(ctx context.Context, taskID string) (core.TaskContext, store.TaskContextInput, error) {
	events, err := s.Store.ListEvents(ctx, taskID)
	if err != nil {
		return core.TaskContext{}, store.TaskContextInput{}, err
	}
	active, err := store.TaskContextFromEvents(ctx, s.Store, events)
	if err != nil {
		return core.TaskContext{}, store.TaskContextInput{}, err
	}
	for _, event := range events {
		if event.Kind != "task.created" {
			continue
		}
		var created core.Task
		if err := json.Unmarshal(event.Payload, &created); err != nil {
			return core.TaskContext{}, store.TaskContextInput{}, fmt.Errorf("decode task.created context: %w", err)
		}
		intake := store.TaskContextInput{}
		for _, requirement := range created.Context.Requirements {
			intake.RequirementIDs = append(intake.RequirementIDs, requirement.ID)
		}
		for _, design := range created.Context.Designs {
			intake.DesignIDs = append(intake.DesignIDs, design.ID)
		}
		intake, err = store.NormalizeTaskContextInput(intake)
		return active, intake, err
	}
	return core.TaskContext{}, store.TaskContextInput{}, fmt.Errorf("task %s has no task.created event", taskID)
}

func taskContextCreateError(err error) *taskCreateError {
	return &taskCreateError{Status: http.StatusBadRequest, Code: "invalid_context_reference", Message: err.Error()}
}
