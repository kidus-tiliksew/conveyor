package httpapi

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) writeTask(w http.ResponseWriter, r *http.Request, task core.Task) {
	if response, ok := s.taskResponse(w, r, task); ok {
		writeJSON(w, http.StatusOK, response)
	}
}

// taskResponse is a task with its next action. On a failed read it answers the
// request itself and reports false.
func (s *Server) taskResponse(w http.ResponseWriter, r *http.Request, task core.Task) (any, bool) {
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return nil, false
	}
	named := []core.Task{task}
	if err := s.hydrateTaskCrates(r.Context(), named); err != nil {
		writeStoreFailure(w, r, err)
		return nil, false
	}
	task = named[0]
	// One order read serves the next action and plan_acts, so plan_acts
	// makes no store call of its own (component-plan-state invariant 6).
	orders, err := s.Store.ListWorkOrdersForTasks(r.Context(), []string{task.ID})
	if err != nil {
		writeStoreFailure(w, r, err)
		return nil, false
	}
	action, err := store.ReadNextActionForTask(r.Context(), s.Store, task, orders, opts)
	if err != nil {
		writeStoreFailure(w, r, err)
		return nil, false
	}
	risk, err := s.taskRisk(r.Context(), task)
	if err != nil {
		writeStoreFailure(w, r, err)
		return nil, false
	}
	return struct {
		core.Task
		NextAction nextaction.NextAction `json:"next_action"`
		PlanActs   store.PlanActs        `json:"plan_acts"`
		Risk       *core.TaskRisk        `json:"risk,omitempty"`
	}{Task: task, NextAction: action, PlanActs: store.PlanActsFor(task, orders), Risk: risk}, true
}

func planActsPointer(acts store.PlanActs) *store.PlanActs { return &acts }

// writeTasks answers one task page with each row's compact next action. The
// paging headers go out only with the rows, so a failed read states no page.
func (s *Server) writeTasks(w http.ResponseWriter, r *http.Request, page store.TaskPage, limit, offset int) {
	tasks := page.Tasks
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	if err := s.hydrateTaskCrates(r.Context(), tasks); err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	inputs, err := s.Store.ReadActivityInputs(r.Context(), ids, opts.Now)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	memoHorizon(r.Context(), inputs.ValidUntil)
	opts.MarkersLoaded, opts.Markers = true, inputs.Markers
	opts.OrdersLoaded, opts.Orders = true, inputs.Orders
	ctx, err := s.loadTaskRisksWith(r.Context(), tasks, store.RiskProjectionOptions{Orders: inputs.Orders, OrdersLoaded: true})
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	r = r.WithContext(ctx)
	// Rows carry only the compact action, so the projection skips the
	// disposition read and covers the given page alone (component-http-api,
	// Task list read).
	opts.CompactOnly = true
	projected, err := store.ReadNextActions(r.Context(), s.Store, tasks, opts)
	if err != nil {
		// A row without its next action would read as a task waiting on
		// nothing, so the read fails (req-operator-next-action AC-1.7;
		// component-next-action invariant 1; component-http-api invariant 16).
		logged, _ := redact.New(nil).Redact(err.Error())
		log.Printf("task list next action: request_id=%q error=%q", middleware.GetReqID(r.Context()), logged)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	rows := make([]struct {
		core.Task
		NextAction nextaction.Compact `json:"next_action"`
		Risk       *core.TaskRisk     `json:"risk,omitempty"`
	}, len(tasks))
	for i, task := range tasks {
		rows[i].Task = task
		rows[i].NextAction = nextaction.CompactFrom(projected[task.ID])
		if rows[i].NextAction.Deadline != nil {
			memoHorizon(r.Context(), *rows[i].NextAction.Deadline)
		}
		// Only a task whose frozen risk mode is record or enforce reads its
		// ratings (req-risk-based-review REQ-11).
		risk, riskErr := s.taskRisk(r.Context(), task)
		if riskErr != nil {
			writeStoreFailure(w, r, riskErr)
			return
		}
		rows[i].Risk = risk
	}
	w.Header().Set("X-Conveyor-Total", strconv.Itoa(page.Total))
	w.Header().Set("X-Conveyor-Limit", strconv.Itoa(limit))
	w.Header().Set("X-Conveyor-Offset", strconv.Itoa(offset))
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) nextActionOptions(r *http.Request) (store.NextActionOptions, error) {
	return s.nextActionOptionsFor(r.Context())
}

// nextActionCapabilities are the capabilities that gate next-action choices.
var nextActionCapabilities = []core.Capability{core.CapabilityOperateGates, core.CapabilityRecoverWork, core.CapabilityRequestChanges, core.CapabilityConfirmDocuments}

// nextActionOptionsFor derives projection options from the request
// credential, so REST routes and MCP reads state one next action (DEC-61).
// Each capability is granted exactly when route admission would grant it:
// the caller's workspace role when memberships are configured, otherwise
// the operator credential scope.
func (s *Server) nextActionOptionsFor(ctx context.Context) (store.NextActionOptions, error) {
	now := time.Now().UTC()
	if s.nextActionClock != nil {
		now = s.nextActionClock()
	}
	if captured, ok := ctx.Value(memoReadNowKey{}).(time.Time); ok {
		now = captured
	}
	opts := store.NextActionOptions{Now: now, Capabilities: map[core.Capability]bool{}}
	for _, capability := range nextActionCapabilities {
		allowed, err := s.callerCapability(ctx, capability)
		if err != nil {
			return store.NextActionOptions{}, err
		}
		if allowed {
			opts.Capabilities[capability] = true
		}
	}
	opts.MergeReadiness = s.mergeReadiness.lastKnown
	opts.TrustedAuthors = s.requestTrustedAuthors()
	if loaded, ok := ctx.Value(requestConfigurationKey{}).(loadedRequestConfiguration); ok {
		opts.TrustedAuthors = func(_ context.Context, task core.Task) ([]string, error) {
			if loaded.config == nil {
				return nil, nil
			}
			repo, _ := loaded.config.Repo(task.Repo)
			return repo.TrustedAuthors, nil
		}
	}
	if credential, ok := store.CredentialFromContext(ctx); ok && credential.Kind == core.CredentialUser {
		opts.CallerUserID = credential.OwnerUserID
	}
	return opts, nil
}

func (s *Server) getNextAction(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	// An unchanged poll is answered from the validation read alone, before
	// the task read and the projection, exactly as the Board activity read
	// does (component-http-api, Activity conditional admission).
	now := activityClock().UTC()
	opts.Now = now
	validation, err := s.Store.ReadActivityValidation(r.Context(), now)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	conditional := &activityConditional{now: now, opts: opts, scope: taskObservationValidatorScope(r.Context(), "next-action", taskID, opts.Capabilities), validation: validation}
	if tag, admitted := conditional.admits(r.Header.Get("If-None-Match")); admitted {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", tag.raw)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	action, err := store.ReadNextAction(r.Context(), s.Store, taskID, opts)
	if err != nil {
		writeClassifiedStoreError(w, r, err, refuseSentinel(store.ErrNotFound, http.StatusNotFound, "task not found"))
		return
	}
	w.Header().Set("ETag", conditional.etag(taskObservationValidUntil(now, action), ""))
	writeJSON(w, http.StatusOK, action)
}

func (s *Server) listInbox(w http.ResponseWriter, r *http.Request) {
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.OwnerUserID == "" {
		http.Error(w, "authenticated user is required", http.StatusUnauthorized)
		return
	}
	listed, ok := readListPage(w, r, defaultInboxPageLimit)
	if !ok {
		return
	}
	page, err := s.Store.ListCallerAttentionTaskPage(r.Context(), store.CallerAttentionQuery{UserID: credential.OwnerUserID, Limit: listed.Limit, Offset: listed.Offset})
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	projected, err := store.ReadNextActions(r.Context(), s.Store, page.Tasks, opts)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	items := make([]nextaction.Compact, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		items = append(items, nextaction.CompactFrom(projected[task.ID]))
	}
	listed.writeHeaders(w, page.Total)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": page.Total})
}

// listFactoryActs serves the workspace's system recovery acts, newest first,
// from one bounded store page (DEC-105(4); component-next-action invariant 21).
func (s *Server) listFactoryActs(w http.ResponseWriter, r *http.Request) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			http.Error(w, "since must be RFC3339", http.StatusBadRequest)
			return
		}
		since = parsed
	}
	listed, ok := readListPage(w, r, defaultInboxPageLimit)
	if !ok {
		return
	}
	page, err := s.Store.ListRecoveryActPage(r.Context(), store.RecoveryActQuery{Since: since, Page: listed.query()})
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	listed.writeHeaders(w, page.Total)
	writeJSON(w, http.StatusOK, map[string]any{"acts": page.Acts})
}
