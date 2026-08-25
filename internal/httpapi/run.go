package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

var (
	errTaskRunConfigUnavailable   = errors.New("task run configuration unavailable")
	errTaskRepositoryUnconfigured = errors.New("task repository is not configured")
)

func (s *Server) requireTaskRunAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if credential, ok := store.CredentialFromContext(r.Context()); ok && credential.Kind == core.CredentialUser && credential.Method == core.CredentialMethodBearer {
			next.ServeHTTP(w, r)
			return
		}
		credential, err := s.authenticateUserCredential(r)
		if err != nil {
			writeCredentialVerificationError(w, err)
			return
		}
		if credential.Kind != core.CredentialUser {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if credential.Method == "" {
			credential.Method = core.CredentialMethodBearer
		}
		ctx := store.WithCredential(r.Context(), credential)
		ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireTaskRequestChangesAuth preserves the dashboard session boundary for
// the human merge-gate action. In production the workspace group has already
// populated the credential; the fallback keeps memory-mode task routes on the
// same authenticated contract.
func (s *Server) requireTaskRequestChangesAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if credential, ok := store.CredentialFromContext(r.Context()); ok && credential.Kind == core.CredentialUser {
			next.ServeHTTP(w, r)
			return
		}
		credential, err := s.authenticateHumanCredential(w, r)
		if err != nil {
			writeCredentialVerificationError(w, err)
			return
		}
		if credential.Kind != core.CredentialUser {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.requireSessionMutationProof(w, r, credential) {
			return
		}
		ctx := store.WithCredential(r.Context(), credential)
		ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// getTaskRunOrder is deliberately task-scoped: unlike the worker dispatcher it
// cannot enumerate or auto-claim work belonging to another task (REQ-5,
// AC-5.1). Assignment is enforced atomically by the claim boundary so the CLI
// can surface the server's exact refusal instead of disguising it as no work.
func (s *Server) getTaskRunOrder(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil || s.ConfigProvider == nil {
		http.Error(w, "task run service unavailable", http.StatusServiceUnavailable)
		return
	}
	taskID := chi.URLParam(r, "id")
	task, err := s.Store.GetTask(r.Context(), taskID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	proposals, err := s.taskRunPendingProposals(r.Context(), task)
	if err != nil {
		log.Printf("get task run proposals: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	dispatch, found, err := s.nextTaskRunOrder(r.Context(), task)
	if errors.Is(err, errTaskRunConfigUnavailable) {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if errors.Is(err, errTaskRepositoryUnconfigured) {
		http.Error(w, errTaskRepositoryUnconfigured.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("get task run order: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !found {
		gate, gateErr := s.taskRunGate(r.Context(), task)
		if gateErr != nil {
			log.Printf("get task run gate: %v", gateErr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, workerservice.DispatchOrder{Task: task, Gate: gate, PendingProposals: proposals, Dispatch: "run", Auth: "user"})
		return
	}
	dispatch.PendingProposals = proposals
	writeJSON(w, http.StatusOK, dispatch)
}

// taskRunPendingProposals projects only unresolved authority authored by this
// task. The store read is already workspace-scoped; the origin filter prevents
// proposals from another task in that workspace reaching the run response.
// Capability flags are server-derived and grant no new mutation surface
// (req-260811-0ee057 AC-1.5, AC-2.2, AC-5.8; design-260805-973cd4).
func (s *Server) taskRunPendingProposals(ctx context.Context, task core.Task) ([]workerservice.TaskRunProposal, error) {
	items, err := s.Store.ListPendingProposals(ctx)
	if err != nil {
		return nil, err
	}
	canConfirmDocuments, err := s.taskRunCapability(ctx, core.CapabilityConfirmDocuments)
	if err != nil {
		return nil, err
	}
	canOperateGates, err := s.taskRunCapability(ctx, core.CapabilityOperateGates)
	if err != nil {
		return nil, err
	}
	proposals := make([]workerservice.TaskRunProposal, 0)
	for _, item := range items {
		if item.OriginType != "task" || item.OriginID != task.ID {
			continue
		}
		kind := ""
		switch item.Tier {
		case "system_design":
			kind = "design"
		case "decision":
			kind = "decision"
		default:
			continue
		}
		version := item.Version
		if kind == "decision" {
			// Decisions are immutable single-version records. Project that
			// proposed record as v1 so every proposal kind has an explicit
			// version in the run context.
			version = 1
		}
		proposals = append(proposals, workerservice.TaskRunProposal{
			Kind: kind, DocumentID: item.ID, Title: item.Title, Version: version,
			CanConfirm: canConfirmDocuments, ActorHint: "an operator can confirm",
		})
	}
	if revision, pending, revisionErr := dispatch.PendingPlanRevisionGate(ctx, s.Store, task.ID); revisionErr != nil {
		return nil, revisionErr
	} else if pending {
		proposals = append(proposals, workerservice.TaskRunProposal{
			Kind: "plan_revision", DocumentID: task.ID, Title: "Execution plan",
			Version: revision.PlanVersion, CanConfirm: canOperateGates,
			ActorHint: "a maintainer or operator can confirm",
		})
	}
	sort.Slice(proposals, func(i, j int) bool {
		if proposals[i].Kind != proposals[j].Kind {
			return proposals[i].Kind < proposals[j].Kind
		}
		if proposals[i].DocumentID != proposals[j].DocumentID {
			return proposals[i].DocumentID < proposals[j].DocumentID
		}
		return proposals[i].Version < proposals[j].Version
	})
	return proposals, nil
}

// taskRunGate derives presentation state from the same audited transitions the
// dispatcher and dashboard use. It never creates or touches a claim.
func (s *Server) taskRunGate(ctx context.Context, task core.Task) (*workerservice.TaskRunGate, error) {
	if task.State != core.TaskAwaiting {
		return nil, nil
	}
	canOperate, err := s.taskRunCapability(ctx, core.CapabilityOperateGates)
	if err != nil {
		return nil, err
	}
	canRequestChanges, err := s.taskRunCapability(ctx, core.CapabilityRequestChanges)
	if err != nil {
		return nil, err
	}
	if task.Assignee != nil {
		credential, _ := store.CredentialFromContext(ctx)
		if task.Assignee.UserID != credential.OwnerUserID {
			canSetAssignee, capabilityErr := s.taskRunCapability(ctx, core.CapabilitySetAssignee)
			if capabilityErr != nil {
				return nil, capabilityErr
			}
			canRequestChanges = canRequestChanges && canSetAssignee
		}
	}

	if revision, pending, revisionErr := dispatch.PendingPlanRevisionGate(ctx, s.Store, task.ID); revisionErr != nil {
		return nil, revisionErr
	} else if pending {
		return &workerservice.TaskRunGate{
			Kind: "plan_revision", Label: "plan revision gate",
			Summary:     fmt.Sprintf("implementation requested revision of execution plan v%d", revision.PlanVersion),
			PlanVersion: revision.PlanVersion, Rationale: revision.Rationale,
			CanOperate: canOperate, CanRequestChanges: canOperate,
		}, nil
	}

	command, err := latestTaskGateCommand(ctx, s.Store, task.ID)
	if err != nil {
		return nil, err
	}
	switch command {
	case core.TaskGateSpec:
		spec, found, specErr := s.Store.GetLatestSpecVersion(ctx, task.ID)
		if specErr != nil {
			return nil, specErr
		}
		gate := &workerservice.TaskRunGate{Kind: "spec", Label: "spec approval gate", Summary: "submitted execution plan", CanOperate: canOperate, CanRequestChanges: canOperate}
		if found {
			gate.SpecVersion = spec.Version
			gate.Summary = fmt.Sprintf("submitted execution plan v%d", spec.Version)
		}
		return gate, nil
	case core.TaskGateMerge:
		summary := "reviewed task branch"
		if task.Branch != "" {
			summary = task.Branch
			if task.BaseBranch != "" {
				summary += " into " + task.BaseBranch
			}
		}
		return &workerservice.TaskRunGate{Kind: "merge", Label: "merge approval gate", Summary: summary, CanOperate: canOperate, CanRequestChanges: canRequestChanges}, nil
	default:
		return &workerservice.TaskRunGate{Kind: "human", Label: "human recovery gate", Summary: fmt.Sprintf("task is %s after %s", task.State, task.NextStage), CanOperate: canOperate, CanRequestChanges: canOperate}, nil
	}
}

func (s *Server) taskRunCapability(ctx context.Context, capability core.Capability) (bool, error) {
	credential, ok := store.CredentialFromContext(ctx)
	if !ok || credential.Kind != core.CredentialUser {
		return false, nil
	}
	workspaceID, scoped := store.WorkspaceFromContext(ctx)
	if scoped && s.Memberships != nil {
		return s.Memberships.AuthorizeWorkspace(ctx, credential.OwnerUserID, workspaceID, capability)
	}
	return credential.Scope == core.CredentialScopeOperator, nil
}

func latestTaskGateCommand(ctx context.Context, st store.Store, taskID string) (core.TaskCommand, error) {
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		return "", err
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Kind != "task.state_changed" {
			continue
		}
		var transition struct {
			Command core.TaskCommand `json:"command"`
		}
		if err := json.Unmarshal(events[index].Payload, &transition); err != nil {
			return "", fmt.Errorf("decode latest task transition for %s: %w", taskID, err)
		}
		return transition.Command, nil
	}
	return "", nil
}

func (s *Server) nextTaskRunOrder(ctx context.Context, task core.Task) (workerservice.DispatchOrder, bool, error) {
	orders, err := s.WorkOrders.List(ctx)
	if err != nil {
		return workerservice.DispatchOrder{}, false, err
	}
	eligible := orders[:0]
	for _, order := range orders {
		if order.TaskID == task.ID && order.State == core.WorkOrderQueued && order.Claimable &&
			(order.Stage == core.StageSpec || order.Stage == core.StageImplement || order.Stage == core.StageReview) {
			eligible = append(eligible, order)
		}
	}
	if len(eligible) == 0 {
		return workerservice.DispatchOrder{}, false, nil
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Stage != eligible[j].Stage {
			return taskRunStageOrder(eligible[i].Stage) < taskRunStageOrder(eligible[j].Stage)
		}
		if !eligible[i].QueueEnteredAt.Equal(eligible[j].QueueEnteredAt) {
			return eligible[i].QueueEnteredAt.Before(eligible[j].QueueEnteredAt)
		}
		return eligible[i].ID < eligible[j].ID
	})
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return workerservice.DispatchOrder{}, false, fmt.Errorf("%w: %v", errTaskRunConfigUnavailable, err)
	}
	repository, ok := cfg.Repo(task.Repo)
	if !ok {
		return workerservice.DispatchOrder{}, false, errTaskRepositoryUnconfigured
	}
	item := workerservice.DispatchOrder{
		Order: eligible[0], Task: task, Repository: repository,
		HarnessSelection: "local", Dispatch: "run", Confinement: "none", Auth: "user",
	}
	if credential, authenticated := store.CredentialFromContext(ctx); authenticated && s.CallerIdentities != nil {
		item.GitAuthor, err = store.GitAuthorForUser(ctx, s.CallerIdentities, credential.OwnerUserID)
		if err != nil {
			return workerservice.DispatchOrder{}, false, err
		}
	}
	return item, true, nil
}

func taskRunStageOrder(stage core.Stage) int {
	switch stage {
	case core.StageSpec:
		return 0
	case core.StageImplement:
		return 1
	case core.StageReview:
		return 2
	default:
		return 3
	}
}

type taskRunClaimRequest struct {
	SessionID    string `json:"session_id"`
	ClientToken  string `json:"client_token"`
	Agent        string `json:"agent"`
	Model        string `json:"model"`
	LeaseSeconds int64  `json:"lease_seconds"`
}

func (s *Server) claimTaskRunOrder(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request taskRunClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.Kind != core.CredentialUser {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	orderID := chi.URLParam(r, "order_id")
	order, err := s.Store.GetWorkOrder(r.Context(), orderID)
	if err != nil || order.TaskID != chi.URLParam(r, "id") {
		http.NotFound(w, r)
		return
	}
	task, err := s.Store.GetTask(r.Context(), order.TaskID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	next, found, err := s.nextTaskRunOrder(r.Context(), task)
	if errors.Is(err, errTaskRunConfigUnavailable) {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if errors.Is(err, errTaskRepositoryUnconfigured) {
		http.Error(w, errTaskRepositoryUnconfigured.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("resolve task run claim order: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !found || next.Order.ID != orderID {
		http.Error(w, "work order is not the task's next claimable order", http.StatusConflict)
		return
	}
	lease := time.Duration(request.LeaseSeconds) * time.Second
	if lease <= 0 || lease > time.Hour {
		lease = core.DefaultWorkOrderClaimLease
	}
	claimed, err := s.WorkOrders.Claim(r.Context(), orderID, core.WorkOrderClaim{
		SessionID: request.SessionID, ClientToken: request.ClientToken,
		ClaimantID: core.TaskRunClaimantID(credential.OwnerUserID), OwnerUserID: credential.OwnerUserID,
		Agent: strings.TrimSpace(request.Agent), Model: strings.TrimSpace(request.Model), Lease: lease,
	})
	if err != nil {
		// This is intentionally the original claim error, including the assignee
		// identity owned by the server-side eligibility contract.
		if errors.Is(err, store.ErrForgeTokenRequired) {
			w.Header().Set("X-Conveyor-Error-Code", store.ForgeTokenRequiredCode)
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, claimed)
}

func (s *Server) authorizeTaskRunOrder(r *http.Request, sessionID string) (core.WorkOrder, bool) {
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.Kind != core.CredentialUser {
		return core.WorkOrder{}, false
	}
	order, err := s.Store.GetWorkOrder(r.Context(), chi.URLParam(r, "order_id"))
	return order, err == nil && order.TaskID == chi.URLParam(r, "id") && order.WorkerID == "" &&
		order.ClaimantID == core.TaskRunClaimantID(credential.OwnerUserID) && order.SessionID == sessionID
}

type taskRunAgentCredentialRequest struct {
	SessionID    string `json:"session_id"`
	CredentialID string `json:"credential_id,omitempty"`
}

// issueTaskRunAgentCredential splits the human control plane from the exact
// child execution principal after claim (req-security-boundaries REQ-1/AC-1.1,
// REQ-2/AC-2.2; design-harness-execution; design-http-api).
func (s *Server) issueTaskRunAgentCredential(w http.ResponseWriter, r *http.Request) {
	if s.AgentCredentials == nil {
		http.Error(w, "agent credential service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request taskRunAgentCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	order, ok := s.authorizeTaskRunOrder(r, request.SessionID)
	if !ok || order.State != core.WorkOrderClaimed {
		http.Error(w, store.ErrWorkOrderClaimLost.Error(), http.StatusConflict)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	workspaceID, _ := store.WorkspaceFromContext(r.Context())
	label, err := store.RunAgentCredentialLabel(store.RunAgentCredentialBinding{WorkspaceID: workspaceID, WorkOrderID: order.ID, SessionID: request.SessionID})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	issued, err := s.AgentCredentials.IssueAgentCredential(r.Context(), credential.OwnerUserID, label)
	if err != nil {
		log.Printf("issue task run agent credential: %v", err)
		http.Error(w, "agent credential issuance failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, issued)
}

func (s *Server) revokeTaskRunAgentCredential(w http.ResponseWriter, r *http.Request) {
	if s.AgentCredentials == nil {
		http.Error(w, "agent credential service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request taskRunAgentCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Revocation must remain available after the child has submitted or released
	// the order and the live claim fields have been cleared. Ownership and the
	// agent-kind predicate are enforced atomically by AgentCredentialStore.
	if strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.CredentialID) == "" {
		http.Error(w, "session_id and credential_id are required", http.StatusBadRequest)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	workspaceID, _ := store.WorkspaceFromContext(r.Context())
	binding := store.RunAgentCredentialBinding{
		WorkspaceID: workspaceID,
		WorkOrderID: chi.URLParam(r, "order_id"),
		SessionID:   request.SessionID,
	}
	if err := s.AgentCredentials.RevokeRunAgentCredential(r.Context(), credential.OwnerUserID, request.CredentialID, binding); errors.Is(err, store.ErrRunAgentCredentialBindingMismatch) {
		http.Error(w, store.ErrRunAgentCredentialBindingMismatch.Error(), http.StatusConflict)
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("revoke task run agent credential: %v", err)
		http.Error(w, "agent credential revocation failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) renewTaskRunOrder(w http.ResponseWriter, r *http.Request) {
	if s.Workers == nil {
		http.Error(w, "task run service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request workOrderRenewRequest
	_ = json.NewDecoder(r.Body).Decode(&request)
	credential, ok := store.CredentialFromContext(r.Context())
	order, err := s.Store.GetWorkOrder(r.Context(), chi.URLParam(r, "order_id"))
	if !ok || credential.Kind != core.CredentialUser || err != nil || order.TaskID != chi.URLParam(r, "id") {
		http.Error(w, store.ErrWorkOrderClaimLost.Error(), http.StatusConflict)
		return
	}
	claim := core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID(credential.OwnerUserID), SessionID: request.SessionID}
	renewed, err := s.Workers.RenewClaim(r.Context(), claim, order.ID, request.snapshot())
	if err != nil {
		if errors.Is(err, store.ErrWorkOrderPreempted) {
			w.Header().Set("X-Conveyor-Error-Code", "work_order_preempted")
		} else if errors.Is(err, store.ErrWorkOrderReleasedAtCheckpoint) {
			w.Header().Set("X-Conveyor-Error-Code", "work_order_released_checkpoint")
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, renewed)
}

func (s *Server) reconcileTaskRunOrder(w http.ResponseWriter, r *http.Request) {
	if s.Workers == nil {
		http.Error(w, "task run service unavailable", http.StatusServiceUnavailable)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	credential, ok := store.CredentialFromContext(r.Context())
	order, err := s.Store.GetWorkOrder(r.Context(), chi.URLParam(r, "order_id"))
	if !ok || credential.Kind != core.CredentialUser || err != nil || order.TaskID != chi.URLParam(r, "id") {
		http.Error(w, store.ErrWorkOrderClaimLost.Error(), http.StatusConflict)
		return
	}
	claim := core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID(credential.OwnerUserID), SessionID: sessionID}
	result, err := s.Workers.ReconcileClaim(r.Context(), claim, order.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	terminalHandoff := (result.WorkOrder.State == core.WorkOrderSubmitted || result.WorkOrder.State == core.WorkOrderCompleted) &&
		result.WorkOrder.WorkerID == claim.WorkerID && result.WorkOrder.ClaimantID == claim.ClaimantID &&
		result.WorkOrder.SessionID == claim.SessionID && result.WorkOrder.AttemptID != ""
	if !result.Authorized && !result.ReleasedAtCheckpoint && !terminalHandoff {
		http.Error(w, store.ErrWorkOrderClaimLost.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) releaseTaskRunOrder(w http.ResponseWriter, r *http.Request) {
	if s.Workers == nil {
		http.Error(w, "task run service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		SessionID     string                    `json:"session_id"`
		Reason        string                    `json:"reason"`
		Checkpoint    *core.WorkOrderCheckpoint `json:"checkpoint"`
		Cause         string                    `json:"release_cause"`
		Outcome       string                    `json:"outcome"`
		ExitStatus    *int                      `json:"exit_status"`
		FailureDetail string                    `json:"failure_detail"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	order, ok := s.authorizeTaskRunOrder(r, request.SessionID)
	if !ok {
		http.Error(w, store.ErrWorkOrderClaimLost.Error(), http.StatusConflict)
		return
	}
	claim := core.WorkOrderClaimIdentity{WorkerID: order.WorkerID, ClaimantID: order.ClaimantID, SessionID: order.SessionID}
	released, err := s.Workers.ReleaseClaim(r.Context(), claim, order.ID, core.WorkOrderRelease{
		SessionID: request.SessionID, Reason: request.Reason, Checkpoint: request.Checkpoint, Cause: request.Cause,
		Outcome: request.Outcome, ExitStatus: request.ExitStatus, FailureDetail: request.FailureDetail,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, released)
}

func (s *Server) checkpointTaskRunOrderAttempt(w http.ResponseWriter, r *http.Request) {
	if s.Workers == nil {
		http.Error(w, "task run service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request workOrderAttemptCheckpointRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	checkpoint := request.checkpoint()
	order, ok := s.authorizeTaskRunOrder(r, checkpoint.SessionID)
	if !ok {
		http.Error(w, store.ErrWorkOrderClaimLost.Error(), http.StatusConflict)
		return
	}
	created, err := s.Workers.CheckpointAttempt(r.Context(), core.Worker{}, order.ID, checkpoint)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"created": created})
}
