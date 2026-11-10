package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
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

func writeTaskCreateError(w http.ResponseWriter, r *http.Request, err error) {
	typed, ok := err.(*taskCreateError)
	if !ok {
		writeStoreFailure(w, r, err)
		return
	}
	if typed.Code == "" {
		http.Error(w, typed.Message, typed.Status)
		return
	}
	writeJSON(w, typed.Status, map[string]string{"error": typed.Code, "message": typed.Message})
}

// createTaskRecord is the single durable intake path for HTTP and MCP. MCP
// callers provide an idempotency key so transport retries return the original
// task without enqueueing Luna triage twice (component-http-api; DEC-8).
func (s *Server) createTaskRecord(ctx context.Context, req createTaskReq, intakeKey, defaultSource string) (taskCreateResult, error) {
	return s.createTaskRecordWithState(ctx, req, intakeKey, defaultSource, core.TaskQueued)
}

func (s *Server) createTaskRecordWithState(ctx context.Context, req createTaskReq, intakeKey, defaultSource string, initialState core.TaskState) (taskCreateResult, error) {
	req.Repo = strings.TrimSpace(req.Repo)
	req.BaseBranch = strings.TrimSpace(req.BaseBranch)
	req.Source = strings.TrimSpace(req.Source)
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
	if len(intakeKey) > 200 {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Message: "idempotency_key must be at most 200 characters"}
	}
	// The plan refusals and the filer-approval capability check precede the
	// idempotency lookup, so a refused request has no side effect even when
	// its key names a task (component-plan-state invariant 2;
	// component-task-lifecycle invariant 16).
	planRequest, intakePlan, planErr := validateIntakePlan(req)
	if planErr != nil {
		return taskCreateResult{}, planErr
	}
	if intakePlan != nil && intakePlan.Approved {
		if err := s.authorizeIntakePlanApproval(ctx); err != nil {
			return taskCreateResult{}, err
		}
	}
	originalReq := req
	if intakeKey != "" {
		if existing, found, err := s.Store.GetTaskByIntakeKey(ctx, intakeKey); err != nil {
			return taskCreateResult{}, err
		} else if found {
			active, intake, record, contextErr := s.taskContextsForRetry(ctx, existing.ID)
			if contextErr != nil {
				return taskCreateResult{}, contextErr
			}
			existing.Context = active
			if !sameIntakeRequest(existing, originalReq, intake, record) {
				return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: "idempotency_key is already used by a different task"}
			}
			return taskCreateResult{Task: existing}, nil
		}
	}
	// Hold is retired: a task carries no worker reservation (DEC-125(3)). The
	// refusal follows the idempotency lookup so a retry of a task stored with
	// hold before the retirement still returns that original task
	// (component-http-api, Task intake).
	if req.Hold {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusBadRequest, Code: taskHoldRetired, Message: taskHoldRetiredIntakeMessage}
	}
	repos := s.Repos
	workspace, _ := store.WorkspaceFromContext(ctx)
	var current *config.Config
	if s.ConfigProvider != nil {
		var err error
		current, err = s.ConfigProvider(ctx)
		if err != nil {
			log.Printf("httpapi: task intake configuration unavailable: request_id=%q error=%q", middleware.GetReqID(ctx), redactedErrorLogText(err))
			return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: "workspace configuration is unavailable"}
		}
		repos = current.RepoNames()
		workspace = current.Workspace
	}
	var selectedSetup config.PolicyContract
	if current != nil {
		selectedSetup = current.FreezePolicy()
	}
	// No mode axis, no reservation, and no intake-time health gating:
	// serviceability is advisory and orders queue openly (DEC-125(2), DEC-125(3);
	// component-task-lifecycle).
	planState, mergeApproval := resolvedIntakePolicy(req, planRequest, current)
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
	generated, err := s.GenerateTaskTitle(ctx, core.Task{Source: req.Source, Body: req.Body, Repo: req.Repo, SetupContract: selectedSetup})
	if err != nil {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: fmt.Sprintf("generate task title: %v", err)}
	}
	title := strings.TrimSpace(generated)
	if title == "" || len(title) > 200 {
		return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: "generate task title: AI returned an invalid title"}
	}
	// A model can echo a secret from the body. Only the body is exempt from
	// redaction, and a failed secret read refuses creation as a failed
	// generation does. Placeholders can lengthen a title, so the bound applies
	// again, after the whole title is redacted (req-security-boundaries REQ-3;
	// DEC-8; component-task-lifecycle invariant 13).
	title, titleStats, err := redact.Text(ctx, s.redactionSecrets(), title)
	if err != nil {
		logged, _ := redact.New(nil).Redact(err.Error())
		log.Printf("task title redaction unavailable: %s", logged)
		return taskCreateResult{}, &taskCreateError{Status: http.StatusServiceUnavailable, Message: "task title redaction is unavailable"}
	}
	title = core.TruncateUTF8Bytes(title, 200)

	id := core.NewTaskID()
	task := core.Task{
		RepositoryInstallAttempt: req.repositoryInstallAttempt,

		ID:            id,
		Workspace:     workspace,
		Source:        req.Source,
		IntakeKey:     intakeKey,
		Title:         title,
		Body:          req.Body,
		Level:         req.Level,
		PlanState:     planState,
		SpecApproval:  store.SpecApprovalBridge(planState),
		IntakePlan:    intakePlan,
		MergeApproval: mergeApproval,
		PolicyVersion: 1,
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
	var createErr error
	createCtx := store.WithTaskTitleRedactions(ctx, titleStats)
	if req.multipartIntake {
		createErr = s.Store.CreateTaskWithAttachments(createCtx, task, req.DependsOn, attached, req.attachments)
	} else {
		createErr = s.Store.CreateTaskWithDependenciesAndContext(createCtx, task, req.DependsOn, attached)
	}
	if err := createErr; err != nil {
		// A concurrent retry may win the unique intake-key race between the
		// lookup and insert. Resolve that race as the same idempotent result.
		if intakeKey != "" {
			if existing, found, getErr := s.Store.GetTaskByIntakeKey(ctx, intakeKey); getErr != nil {
				return taskCreateResult{}, getErr
			} else if found {
				active, intake, record, contextErr := s.taskContextsForRetry(ctx, existing.ID)
				if contextErr != nil {
					return taskCreateResult{}, contextErr
				}
				existing.Context = active
				if sameIntakeRequest(existing, originalReq, intake, record) {
					return taskCreateResult{Task: existing}, nil
				}
				return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: "idempotency_key is already used by a different task"}
			}
		}
		if code := contextArchiveCode(err); code != "" {
			return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Code: code, Message: err.Error()}
		}
		var referenceErr *store.TaskContextReferenceError
		if errors.As(err, &referenceErr) {
			return taskCreateResult{}, taskContextCreateError(referenceErr)
		}
		if !isOperationRefusal(err) {
			return taskCreateResult{}, err
		}
		return taskCreateResult{}, &taskCreateError{Status: http.StatusConflict, Message: fmt.Sprintf("create task: %v", err)}
	}
	if initialState == core.TaskQueued && s.OnCreate != nil {
		s.OnCreate(ctx, id)
	}
	task.Context, _ = store.TaskContextForTask(ctx, s.Store, task.ID)
	return taskCreateResult{Task: task, Created: true}, nil
}

// redactionSecrets is the stored-secret source for an intake write: the daemon's
// wired source, else the store itself, so a server without a work-order service
// still covers stored keys.
func (s *Server) redactionSecrets() redact.SecretSource {
	if s.WorkOrders != nil && s.WorkOrders.RedactionSecrets != nil {
		return s.WorkOrders.RedactionSecrets
	}
	source, _ := s.Store.(redact.SecretSource)
	return source
}

const (
	// taskHoldRetired refuses a new hold: true intake and answers the retired
	// PUT /v1/tasks/{id}/hold route (DEC-125(3); component-http-api).
	taskHoldRetired              = "task_hold_retired"
	taskHoldRetiredIntakeMessage = "hold is retired (DEC-125); a task carries no worker reservation, so omit hold or send false"
	taskHoldRetiredRouteMessage  = "task hold is retired (DEC-125); a task carries no worker reservation"
	// invalidGateOverride refuses a gate or level input that is not a value
	// the caller's request can apply. The wire name is kept as
	// component-task-lifecycle invariant 16 states it.
	invalidGateOverride = "invalid_agent_gate_override"
)

// resolvedIntakePolicy maps the request onto the two intake policy decisions:
// the plan state and the merge gate (DEC-125(1); component-task-lifecycle).
// The plan state follows component-plan-state invariant 1: a filed plan, then
// the request's plan_state, then the legacy spec_approval, then the legacy
// level, then execution.default_plan_state, which is needed when unset. The
// merge gate follows component-task-lifecycle invariant 18: a legacy level
// first, else the workspace default, and an explicit merge_approval last.
func resolvedIntakePolicy(req createTaskReq, plan store.PlanStateRequest, current *config.Config) (core.PlanState, bool) {
	var mergeApproval bool
	var workspaceDefault core.PlanState
	if req.Level != "" {
		_, mergeApproval = core.LegacyPolicy(req.Level)
	} else if current != nil {
		mergeApproval = current.Execution.MergeApproval
	} else {
		mergeApproval = true
	}
	if current != nil {
		workspaceDefault = core.PlanState(current.Execution.DefaultPlanState)
	}
	if req.MergeApproval != nil {
		mergeApproval = *req.MergeApproval
	}
	return store.ResolveIntakePlanState(plan, workspaceDefault), mergeApproval
}

// taskIntakeRecord holds the plan values a task was created with: the plan
// state task.created recorded and plan version 1 when intake filed one.
type taskIntakeRecord struct {
	PlanState   core.PlanState
	PlanVersion *core.SpecVersion
}

// sameIntakeRequest decides whether a keyed retry repeats the original
// request. It never compares hold: a retry of a task stored with hold before
// the retirement returns that original task (DEC-125(3); component-http-api).
func sameIntakeRequest(task core.Task, req createTaskReq, intake store.TaskContextInput, record taskIntakeRecord) bool {
	if task.Body != req.Body || task.Repo != req.Repo || task.Source != req.Source || (req.BaseBranch != "" && task.BaseBranch != req.BaseBranch) {
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
	if !sameIntakePlan(req, record) {
		return false
	}
	if req.MergeApproval != nil && task.MergeApproval != *req.MergeApproval {
		return false
	}
	if req.Level != "" {
		_, mergeApproval := core.LegacyPolicy(req.Level)
		if req.MergeApproval == nil && task.MergeApproval != mergeApproval {
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

func (s *Server) taskContextsForRetry(ctx context.Context, taskID string) (core.TaskContext, store.TaskContextInput, taskIntakeRecord, error) {
	events, err := s.Store.ListCompactEvents(ctx, taskID)
	if err != nil {
		return core.TaskContext{}, store.TaskContextInput{}, taskIntakeRecord{}, err
	}
	active, err := store.TaskContextFromEvents(ctx, s.Store, events)
	if err != nil {
		return core.TaskContext{}, store.TaskContextInput{}, taskIntakeRecord{}, err
	}
	for _, event := range events {
		if event.Kind != "task.created" {
			continue
		}
		var created store.TaskCreatedEvent
		if err := json.Unmarshal(event.Payload, &created); err != nil {
			return core.TaskContext{}, store.TaskContextInput{}, taskIntakeRecord{}, fmt.Errorf("decode task.created context: %w", err)
		}
		// An older payload carries no plan state; its spec_approval is the
		// intake decision the plan state replaces (component-task-lifecycle
		// invariant 22).
		record := taskIntakeRecord{PlanState: created.PlanState}
		if record.PlanState == "" {
			record.PlanState = store.PlanStateFromSpecApproval(created.SpecApproval)
		}
		if created.PlanVersion > 0 {
			version, found, versionErr := s.Store.GetSpecVersion(ctx, taskID, created.PlanVersion)
			if versionErr != nil {
				return core.TaskContext{}, store.TaskContextInput{}, taskIntakeRecord{}, versionErr
			}
			if found {
				record.PlanVersion = &version
			}
		}
		intake := store.TaskContextInput{}
		for _, requirement := range created.Context.Requirements {
			intake.RequirementIDs = append(intake.RequirementIDs, requirement.ID)
		}
		for _, design := range created.Context.Designs {
			intake.DesignIDs = append(intake.DesignIDs, design.ID)
		}
		intake, err = store.NormalizeTaskContextInput(intake)
		return active, intake, record, err
	}
	return core.TaskContext{}, store.TaskContextInput{}, taskIntakeRecord{}, fmt.Errorf("task %s has no task.created event", taskID)
}

func taskContextCreateError(err error) *taskCreateError {
	return &taskCreateError{Status: http.StatusBadRequest, Code: "invalid_context_reference", Message: err.Error()}
}
