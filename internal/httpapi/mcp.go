package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/releaseinfo"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	systemDesignProposalGuidance = "This System Design proposal is fire-and-forget and confers no authority. Do not checkpoint, pause, or wait for operator confirmation. Proceed now to commit, push, and `submit_for_review`; review dispatch waits on the operator's decision automatically."
	decisionProposalGuidance     = "This decision proposal is fire-and-forget and confers no authority. Do not checkpoint, pause, or wait for operator confirmation. Proceed now to commit, push, and `submit_for_review`; review dispatch waits on the operator's decision automatically."
)

type systemDesignProposalResult struct {
	core.SystemDesignVersion
	Guidance string `json:"guidance"`
}

type decisionProposalResult struct {
	core.Decision
	Guidance string `json:"guidance"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request rpcRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := decoder.Decode(&request); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
	switch request.Method {
	case "initialize":
		response.Result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]string{"name": "conveyor", "version": releaseinfo.Version}}
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": mcpTools()}
	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &call); err != nil {
			response.Error = &rpcError{Code: -32602, Message: "invalid tool arguments"}
			break
		}
		result, err := s.callMCPTool(r, call.Name, call.Arguments)
		if err != nil {
			response.Result = map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		} else {
			data, _ := json.Marshal(result)
			response.Result = map[string]any{"content": []map[string]string{{"type": "text", "text": string(data)}}}
		}
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	writeRPC(w, response)
}

func writeRPC(w http.ResponseWriter, response rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) callMCPTool(r *http.Request, name string, args map[string]any) (any, error) {
	stringArg := func(key string) string { value, _ := args[key].(string); return value }
	stringSliceArg := func(key string) []string {
		values, _ := args[key].([]any)
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	}
	worker, workerAuth := workerFromContext(r.Context())
	if credential, ok := store.CredentialFromContext(r.Context()); ok && credential.Kind == core.CredentialAgent && humanReservedMCPTool(name) {
		return nil, fmt.Errorf("%s requires an operator-scoped user credential", name)
	}
	explicitWorkspace := stringArg("workspace_id")
	workspace, err := s.resolveMCPWorkspace(r.Context(), explicitWorkspace)
	if err != nil {
		return nil, err
	}
	ctx := store.WithWorkspace(r.Context(), workspace)
	if !workerAuth && s.Workspaces != nil {
		if s.Memberships == nil {
			return nil, fmt.Errorf("workspace_not_found: workspace not found")
		}
		credential, ok := store.CredentialFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("workspace_not_found: workspace not found")
		}
		allowed, authErr := s.Memberships.AuthorizeWorkspace(ctx, credential.OwnerUserID, workspace, mcpCapability(name))
		if authErr != nil {
			return nil, authErr
		}
		if !allowed {
			return nil, fmt.Errorf("workspace_not_found: workspace not found")
		}
	}
	session := stringArg("session_id")
	switch name {
	case "create_task":
		if workerAuth {
			return nil, fmt.Errorf("worker credentials cannot create tasks")
		}
		if _, supplied := args["title"]; supplied {
			return nil, fmt.Errorf("title is generated and must not be supplied")
		}
		for _, field := range []string{"setup", "setup_contract", "policy_contract", "execution_settings", "routing", "harness", "model", "effort", "argv"} {
			if _, supplied := args[field]; supplied {
				return nil, fmt.Errorf("%s is retired execution detail and must not be supplied", field)
			}
		}
		if strings.TrimSpace(stringArg("idempotency_key")) == "" {
			return nil, fmt.Errorf("idempotency_key is required")
		}
		result, err := s.createTaskRecord(ctx, createTaskReq{
			Body:            stringArg("body"),
			Repo:            stringArg("repo"),
			BaseBranch:      stringArg("base_branch"),
			Source:          stringArg("source"),
			Level:           core.EscalationLevel(stringArg("level")),
			Hold:            boolArg(args, "hold") != nil && *boolArg(args, "hold"),
			SpecApproval:    boolArg(args, "spec_approval"),
			MergeApproval:   boolArg(args, "merge_approval"),
			DependsOn:       stringSliceArg("depends_on"),
			RequirementIDs:  stringSliceArg("requirement_ids"),
			SystemDesignIDs: stringSliceArg("system_design_ids"),
		}, stringArg("idempotency_key"), "mcp")
		if err != nil {
			return nil, err
		}
		return map[string]any{"task": result.Task, "created": result.Created}, nil
	case "set_assignee":
		if workerAuth {
			return nil, fmt.Errorf("worker credentials cannot assign tasks")
		}
		return taskops.New(s.Store).SetAssignee(ctx, stringArg("task_id"), stringArg("assignee_user_id"))
	}
	if s.WorkOrders == nil {
		return nil, fmt.Errorf("work-order service unavailable")
	}
	switch name {
	case "list_work_orders":
		if workerAuth {
			return s.Workers.ListVisibleOrders(ctx, worker)
		}
		orders, listErr := s.WorkOrders.List(ctx)
		if listErr != nil {
			return nil, listErr
		}
		return projectAssigneeClaimability(ctx, orders), nil
	case "claim_work_order":
		lease := core.DefaultWorkOrderClaimLease
		if value, ok := numberArg(args["lease_seconds"]); ok && value > 0 && value <= 3600 {
			lease = time.Duration(value) * time.Second
		}
		claim := core.WorkOrderClaim{SessionID: session, ClientToken: stringArg("client_token"), Agent: stringArg("agent"), Model: stringArg("model"), Lease: lease}
		if credential, ok := store.CredentialFromContext(ctx); ok {
			claim.OwnerUserID = credential.OwnerUserID
			if !workerAuth {
				// REQ-2/AC-2.1: claimant identity is credential-derived. Keep
				// accepting claimant_id in the wire schema for compatibility,
				// but never use or persist the client assertion.
				claim.ClaimantID = core.TaskRunClaimantID(credential.OwnerUserID)
			}
		}
		if workerAuth {
			return s.Workers.ClaimForWorker(ctx, worker, stringArg("work_order_id"), claim)
		}
		return s.WorkOrders.Claim(ctx, stringArg("work_order_id"), claim)
	case "redispatch_work_order":
		if workerAuth {
			return nil, fmt.Errorf("worker credentials cannot redispatch work orders")
		}
		return s.WorkOrders.Redispatch(ctx, stringArg("work_order_id"))
	case "renew_work_order":
		if !workerAuth {
			if err := s.authorizeUserRunMCPOrder(ctx, stringArg("work_order_id"), session); err != nil {
				return nil, err
			}
		}
		return s.Workers.Renew(ctx, worker, stringArg("work_order_id"), session)
	case "release_work_order":
		claim, err := s.authorizeClaimMutation(ctx, workerAuth, worker, stringArg("work_order_id"), session)
		if err != nil {
			return nil, err
		}
		return s.Workers.ReleaseClaim(ctx, claim, stringArg("work_order_id"), core.WorkOrderRelease{SessionID: session, Reason: stringArg("reason"), Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased})
	case "request_plan_revision":
		claim, err := s.authorizeClaimMutation(ctx, workerAuth, worker, stringArg("work_order_id"), session)
		if err != nil {
			return nil, err
		}
		return s.Workers.RequestPlanRevisionClaim(ctx, claim, stringArg("work_order_id"), stringArg("rationale"))
	case "get_work_order":
		if workerAuth {
			workOrderID := stringArg("work_order_id")
			visible, listErr := s.Workers.ListVisibleOrders(ctx, worker)
			if listErr != nil {
				return nil, listErr
			}
			for _, order := range visible {
				if order.ID != workOrderID {
					continue
				}
				if order.WorkerID == worker.ID {
					return s.WorkOrders.Get(ctx, workOrderID, session)
				}
				return s.WorkOrders.GetVisible(ctx, workOrderID)
			}
			return nil, store.ErrWorkOrderClaimLost
		}
		return s.WorkOrders.Get(ctx, stringArg("work_order_id"), session)
	case "read_artifact":
		if err := s.authorizeWorkerOrder(ctx, workerAuth, worker, stringArg("work_order_id")); err != nil {
			return nil, err
		}
		return s.WorkOrders.ReadArtifact(ctx, stringArg("work_order_id"), session, stringArg("artifact_id"))
	case "report_progress":
		return s.WorkOrders.Progress(ctx, stringArg("work_order_id"), session, stringArg("message"))
	case "report_usage":
		in, _ := numberArg(args["tokens_in"])
		out, _ := numberArg(args["tokens_out"])
		cost, _ := floatArg(args["cost_usd"])
		var rateLimit *core.RateLimitStatus
		if raw, ok := args["rate_limit"]; ok && raw != nil {
			data, marshalErr := json.Marshal(raw)
			if marshalErr != nil {
				return nil, fmt.Errorf("invalid rate_limit: %w", marshalErr)
			}
			var status core.RateLimitStatus
			decoder := json.NewDecoder(strings.NewReader(string(data)))
			decoder.DisallowUnknownFields()
			if decodeErr := decoder.Decode(&status); decodeErr != nil {
				return nil, fmt.Errorf("invalid rate_limit: %w", decodeErr)
			}
			rateLimit = &status
		}
		if stringArg("source") == "worker_fallback" {
			if !workerAuth {
				return nil, fmt.Errorf("worker_fallback usage requires a worker credential")
			}
			if rateLimit != nil {
				return nil, fmt.Errorf("worker_fallback usage cannot include rate_limit")
			}
			if err := s.authorizeWorkerOrder(ctx, true, worker, stringArg("work_order_id")); err != nil {
				return nil, err
			}
			return s.WorkOrders.UsageFromWorkerFallback(ctx, stringArg("work_order_id"), session, in, out, cost)
		}
		return s.WorkOrders.UsageWithRateLimit(ctx, stringArg("work_order_id"), session, in, out, cost, rateLimit)
	case "report_continuation":
		claim, err := s.authorizeClaimMutation(ctx, workerAuth, worker, stringArg("work_order_id"), session)
		if err != nil {
			return nil, err
		}
		return s.WorkOrders.ReportContinuation(ctx, stringArg("work_order_id"), claim, core.WorkOrderContinuation{
			SessionID: stringArg("continuation_session_id"), AttemptID: stringArg("attempt_id"),
			Harness: stringArg("harness"), LaunchEnvironment: stringArg("launch_environment"),
		})
	case "propose_system_design_revision":
		order, err := s.implementationGovernanceOrder(ctx, workerAuth, worker, stringArg("work_order_id"), session)
		if err != nil {
			return nil, err
		}
		version, err := s.Store.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{
			DocumentID: stringArg("document_id"), Content: stringArg("content"),
			Origin: core.SystemDesignOriginImplementation, OriginTaskID: order.TaskID,
		})
		if err != nil {
			return nil, err
		}
		return systemDesignProposalResult{SystemDesignVersion: version, Guidance: systemDesignProposalGuidance}, nil
	case "propose_decision":
		order, err := s.implementationGovernanceOrder(ctx, workerAuth, worker, stringArg("work_order_id"), session)
		if err != nil {
			return nil, err
		}
		decision, err := s.Store.ProposeDecision(ctx, core.Decision{
			Statement: stringArg("statement"), Context: stringArg("context"), AlternativesRejected: stringArg("alternatives_rejected"),
			Supersedes: stringArg("supersedes"), Origin: core.DecisionOriginImplementation, OriginTaskID: order.TaskID,
		})
		if err != nil {
			return nil, err
		}
		return decisionProposalResult{Decision: decision, Guidance: decisionProposalGuidance}, nil
	case "upload_transcript":
		return s.WorkOrders.UploadTranscript(ctx, stringArg("work_order_id"), session, stringArg("transcript"))
	case "submit_plan":
		payload, marshalErr := json.Marshal(map[string]any{"markdown": args["markdown"], "decomposition": args["decomposition"]})
		if marshalErr != nil {
			return nil, marshalErr
		}
		var value pipeline.StructuredPlan
		decoder := json.NewDecoder(strings.NewReader(string(payload)))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&value); decodeErr != nil {
			return nil, fmt.Errorf("invalid structured plan: %w", decodeErr)
		}
		return s.WorkOrders.SubmitPlan(ctx, stringArg("work_order_id"), session, value)
	case "submit_spec":
		return nil, fmt.Errorf("MCP tool submit_spec not found; use submit_plan")
	case "submit_for_review":
		return s.WorkOrders.SubmitForReview(ctx, stringArg("work_order_id"), session)
	case "await_review":
		seconds := 300.0
		if value, ok := floatArg(args["timeout_seconds"]); ok {
			seconds = value
		}
		return s.WorkOrders.AwaitReview(ctx, stringArg("work_order_id"), session, time.Duration(seconds*float64(time.Second)))
	case "submit_review_verdict":
		payload, marshalErr := json.Marshal(map[string]any{
			"verdict": args["verdict"], "reason_code": args["reason_code"], "summary": args["summary"],
			"feedback": args["feedback"], "requirement_citations": args["requirement_citations"],
			"done_criteria_coverage": args["done_criteria_coverage"],
			"governance_assessment":  args["governance_assessment"],
		})
		if marshalErr != nil {
			return nil, marshalErr
		}
		var review pipeline.Review
		decoder := json.NewDecoder(strings.NewReader(string(payload)))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&review); decodeErr != nil {
			return nil, fmt.Errorf("invalid review verdict: %w", decodeErr)
		}
		return s.WorkOrders.SubmitVerdict(ctx, stringArg("work_order_id"), session, review)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func humanReservedMCPTool(name string) bool {
	switch name {
	case "create_task", "redispatch_work_order", "set_assignee", "report_continuation":
		return true
	default:
		return false
	}
}

func mcpCapability(name string) core.Capability {
	switch name {
	case "list_work_orders":
		return core.CapabilityViewWorkspace
	case "set_assignee":
		return core.CapabilitySetAssignee
	case "propose_system_design_revision", "propose_decision", "request_plan_revision":
		// These tools are claim-bound below. Workspace visibility is the only
		// membership prerequisite; the exact live claim supplies mutation
		// authority for executor sessions (REQ-1/AC-1.5).
		return core.CapabilityViewWorkspace
	case "create_task":
		return core.CapabilityOperateGates
	case "redispatch_work_order":
		return core.CapabilityRecoverWork
	default:
		return core.CapabilityClaimWork
	}
}

// authorizeWorkerOrder scopes worker-credentialed MCP calls to orders the
// worker currently holds. A mismatch is a lost or reassigned claim — e.g.
// the lease expired and ownership returned to the queue (design-260805-973cd4) — not a
// credential failure, which requireMCPAuth already rejected.
func (s *Server) authorizeWorkerOrder(ctx context.Context, workerAuth bool, worker core.Worker, workOrderID string) error {
	if !workerAuth {
		return nil
	}
	order, err := s.Store.GetWorkOrder(ctx, workOrderID)
	if err != nil || order.WorkerID != worker.ID {
		return store.ErrWorkOrderClaimLost
	}
	return nil
}

func (s *Server) authorizeUserRunMCPOrder(ctx context.Context, workOrderID, sessionID string) error {
	credential, ok := store.CredentialFromContext(ctx)
	if !ok || credential.Kind != core.CredentialUser {
		return store.ErrWorkOrderClaimLost
	}
	order, err := s.Store.GetWorkOrder(ctx, workOrderID)
	if err != nil || order.WorkerID != "" || order.ClaimantID != core.TaskRunClaimantID(credential.OwnerUserID) || order.SessionID != sessionID {
		return store.ErrWorkOrderClaimLost
	}
	return nil
}

// authorizeClaimMutation resolves the authenticated owner of one live claim.
// Worker credentials retain worker ownership checks, user credentials retain
// explicit conveyor-run ownership, and worker-dispatched agent children are
// admitted only through the exact live implement session they already hold.
func (s *Server) authorizeClaimMutation(ctx context.Context, workerAuth bool, worker core.Worker, workOrderID, sessionID string) (core.WorkOrderClaimIdentity, error) {
	if workerAuth {
		if err := s.authorizeWorkerOrder(ctx, true, worker, workOrderID); err != nil {
			return core.WorkOrderClaimIdentity{}, err
		}
		order, err := s.WorkOrders.AuthorizeClaimed(ctx, workOrderID, sessionID)
		if err != nil {
			return core.WorkOrderClaimIdentity{}, store.ErrWorkOrderClaimLost
		}
		return claimIdentity(order), nil
	}
	credential, ok := store.CredentialFromContext(ctx)
	if !ok {
		return core.WorkOrderClaimIdentity{}, store.ErrWorkOrderClaimUnauthorized
	}
	if credential.Kind == core.CredentialUser {
		order, err := s.Store.GetWorkOrder(ctx, workOrderID)
		if err != nil || !liveWorkOrderClaim(order, time.Now()) {
			return core.WorkOrderClaimIdentity{}, store.ErrWorkOrderClaimLost
		}
		if order.WorkerID != "" || order.ClaimantID != core.TaskRunClaimantID(credential.OwnerUserID) || order.SessionID != sessionID {
			return core.WorkOrderClaimIdentity{}, store.ErrWorkOrderClaimUnauthorized
		}
		return claimIdentity(order), nil
	}
	if credential.Kind != core.CredentialAgent {
		return core.WorkOrderClaimIdentity{}, store.ErrWorkOrderClaimUnauthorized
	}
	orders, err := s.Store.ListWorkOrders(ctx)
	if err != nil {
		return core.WorkOrderClaimIdentity{}, err
	}
	now := time.Now()
	for _, order := range orders {
		if order.SessionID != sessionID || !liveWorkOrderClaim(order, now) {
			continue
		}
		if order.ID != workOrderID || order.Stage != core.StageImplement || order.WorkerID == "" {
			return core.WorkOrderClaimIdentity{}, store.ErrWorkOrderClaimUnauthorized
		}
		workers, listErr := s.Store.ListWorkers(ctx)
		if listErr != nil {
			return core.WorkOrderClaimIdentity{}, listErr
		}
		for _, claimedWorker := range workers {
			if claimedWorker.ID == order.WorkerID && claimedWorker.OwnerUserID != "" && claimedWorker.OwnerUserID == credential.OwnerUserID {
				return claimIdentity(order), nil
			}
		}
		return core.WorkOrderClaimIdentity{}, store.ErrWorkOrderClaimUnauthorized
	}
	return core.WorkOrderClaimIdentity{}, store.ErrWorkOrderClaimLost
}

func claimIdentity(order core.WorkOrder) core.WorkOrderClaimIdentity {
	return core.WorkOrderClaimIdentity{WorkerID: order.WorkerID, ClaimantID: order.ClaimantID, SessionID: order.SessionID}
}

func liveWorkOrderClaim(order core.WorkOrder, now time.Time) bool {
	return order.State == core.WorkOrderClaimed && order.SessionID != "" && order.LeaseExpiresAt.After(now) &&
		(order.ExecutionDeadline.IsZero() || order.ExecutionDeadline.After(now))
}

func (s *Server) implementationGovernanceOrder(ctx context.Context, workerAuth bool, worker core.Worker, workOrderID, sessionID string) (core.WorkOrder, error) {
	if _, err := s.authorizeClaimMutation(ctx, workerAuth, worker, workOrderID, sessionID); err != nil {
		return core.WorkOrder{}, err
	}
	order, err := s.WorkOrders.AuthorizeClaimed(ctx, workOrderID, sessionID)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if order.Stage != core.StageImplement {
		return core.WorkOrder{}, fmt.Errorf("governance proposals require a claimed implement work order")
	}
	return order, nil
}

func (s *Server) resolveMCPWorkspace(ctx context.Context, explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if worker, ok := workerFromContext(ctx); ok {
		if explicit == "" || explicit == worker.Workspace {
			return worker.Workspace, nil
		}
		return "", fmt.Errorf("workspace_not_found: workspace not found")
	}
	var items []core.Workspace
	if s.Workspaces != nil {
		if s.Memberships == nil {
			return "", fmt.Errorf("workspace_not_found: workspace not found")
		}
		var err error
		if credential, ok := store.CredentialFromContext(ctx); ok {
			items, err = s.Memberships.ListWorkspacesForUser(ctx, credential.OwnerUserID)
		} else {
			return "", fmt.Errorf("workspace_not_found: workspace not found")
		}
		if err != nil {
			return "", err
		}
	} else {
		seen := map[string]bool{}
		fallbacks := []string{s.Workspace}
		if s.Deployment != nil {
			fallbacks = append(fallbacks, s.Deployment.Workspace)
		}
		for _, id := range fallbacks {
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] {
				items = append(items, core.Workspace{ID: id, Name: id})
				seen[id] = true
			}
		}
	}
	if explicit == "" {
		if len(items) == 0 {
			return "", fmt.Errorf("workspace_unavailable: create a workspace first")
		}
		if len(items) != 1 {
			return "", fmt.Errorf("workspace_required: workspace_id is required")
		}
		return items[0].ID, nil
	}
	for _, item := range items {
		if item.ID == explicit {
			return explicit, nil
		}
	}
	return "", fmt.Errorf("workspace_not_found: workspace not found")
}

func numberArg(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case json.Number:
		n, err := typed.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(typed, 10, 64)
		return n, err == nil
	}
	return 0, false
}
func floatArg(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		n, err := typed.Float64()
		return n, err == nil
	case string:
		n, err := strconv.ParseFloat(typed, 64)
		return n, err == nil
	}
	return 0, false
}

func boolArg(args map[string]any, key string) *bool {
	value, ok := args[key].(bool)
	if !ok {
		return nil
	}
	return &value
}

func mcpTools() []map[string]any {
	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		// A nil variadic slice marshals as `"required": null`, which strict
		// MCP clients reject wholesale at tools/list — omit the key instead.
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	str := map[string]string{"type": "string"}
	num := map[string]string{"type": "number"}
	rateLimit := object(map[string]any{
		"status":    str,
		"limit":     num,
		"remaining": num,
		"reset_at":  map[string]any{"type": "string", "format": "date-time"},
	}, "status")
	stringList := map[string]any{"type": "array", "items": str}
	requirementCitations := object(map[string]any{
		"applicable": map[string]string{"type": "boolean"},
		"cited_ids":  stringList, "unknown_ids": stringList,
		"unserved_ids": stringList, "conflicts": stringList,
	}, "applicable", "cited_ids", "unknown_ids", "unserved_ids", "conflicts")
	criteriaList := map[string]any{"type": "array", "items": str, "description": "Verbatim-trimmed criterion text from the approved execution plan. Do not paraphrase, split, combine, duplicate, or include non-criteria; each criterion belongs in at most one coverage list."}
	doneCriteriaCoverage := object(map[string]any{
		"applicable":  map[string]any{"type": "boolean", "description": "True exactly when the approved task document is a valid execution plan."},
		"summary":     str,
		"satisfied":   map[string]any{"type": "array", "items": str, "description": "Evidence-backed completed criteria, as verbatim-trimmed criterion text."},
		"unsatisfied": criteriaList,
		"unverified":  map[string]any{"type": "array", "items": str, "description": "Criteria whose completion cannot be established from available evidence, as verbatim-trimmed criterion text."},
		"conflicts":   map[string]any{"type": "array", "items": str, "description": "Criteria contradicted by governing authority, as verbatim-trimmed criterion text."},
	}, "applicable", "summary", "satisfied", "unsatisfied", "unverified", "conflicts")
	governanceAssessment := object(map[string]any{
		"applicable":        map[string]any{"type": "boolean", "description": "Legacy compatibility alias for design_applicable; new callers should send the split fields."},
		"design_applicable": map[string]any{"type": "boolean", "description": "Whether the pinned System Design versions govern the task repository."},
		"decision_citable":  map[string]any{"type": "boolean", "description": "Whether the pinned workspace decision authority contains confirmed decisions."},
		"cited_ids":         stringList,
		"unknown_ids":       stringList, "ungoverned_ids": stringList, "superseded_ids": stringList, "conflicts": stringList,
	}, "cited_ids", "unknown_ids", "ungoverned_ids", "superseded_ids", "conflicts")
	governanceAssessment["anyOf"] = []map[string]any{
		{"required": []string{"design_applicable", "decision_citable"}},
		{"required": []string{"applicable"}},
	}
	identity := map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str}
	return []map[string]any{
		{"name": "create_task", "description": "Create one durable task in an explicit workspace with optional desired-state context, generate its title from body, and enqueue triage. Reusing the same idempotency key returns the original task.", "inputSchema": object(map[string]any{"workspace_id": str, "body": map[string]any{"type": "string", "description": "Task description in GitHub-flavored Markdown. Structured descriptions using headings and lists are encouraged."}, "repo": str, "base_branch": str, "source": str, "depends_on": map[string]any{"type": "array", "items": str, "description": "Optional open task IDs in this workspace that must merge first."}, "requirement_ids": map[string]any{"type": "array", "items": str, "description": "Optional confirmed requirements this task serves."}, "system_design_ids": map[string]any{"type": "array", "items": str, "description": "Optional confirmed System Design documents governing this task."}, "hold": map[string]any{"type": "boolean", "description": "Reserve the task from the worker daemon; claim it yourself (DEC-5)."}, "spec_approval": map[string]string{"type": "boolean"}, "merge_approval": map[string]string{"type": "boolean"}, "idempotency_key": str}, "body", "repo", "idempotency_key")},
		{"name": "set_assignee", "description": "Set or clear a task assignee as an audited operator act. Assignment constrains claim eligibility and never queue order.", "inputSchema": object(map[string]any{"workspace_id": str, "task_id": str, "assignee_user_id": str}, "task_id", "assignee_user_id")},
		{"name": "list_work_orders", "description": "List active, stale, or execution-timed-out spec, implement, and review work orders in one workspace with distinct queue, execution, and lease clocks.", "inputSchema": object(map[string]any{"workspace_id": str})},
		{"name": "claim_work_order", "description": "Claim a work order with a bounded lease. Review self-claim is forbidden. claimant_id is accepted for wire compatibility but ignored; claimant identity is derived from the authenticated credential.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "client_token": str, "claimant_id": str, "agent": str, "model": str, "lease_seconds": num}, "work_order_id", "session_id", "client_token", "agent", "model")},
		{"name": "redispatch_work_order", "description": "Return a stale queued work order in one workspace to the queue with a fresh queue deadline. Active and execution-timed-out work orders are rejected.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str}, "work_order_id")},
		{"name": "renew_work_order", "description": "Renew the exact execution child session lease without extending its fixed attempt deadline.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str}, "work_order_id", "session_id")},
		{"name": "release_work_order", "description": "Release the exact execution child session without allowing a stale child to alter a newer claim.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "reason": str}, "work_order_id", "session_id")},
		{"name": "request_plan_revision", "description": "Request operator-gated revision of the approved execution plan for the exact claimed implement session.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "rationale": str}, "work_order_id", "session_id", "rationale")},
		{"name": "get_work_order", "description": "Get the claimed order contract, spec, branch, feedback, artifacts, and review diff. The authority_source response field is live for a provisional queued-review peek and pinned for claim-time snapshot authority.", "inputSchema": object(identity, "work_order_id", "session_id")},
		{"name": "read_artifact", "description": "Read one artifact authorized for the claimed work order. The workspace, work order, session, and artifact ownership must all match; content is returned as base64.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "artifact_id": str}, "workspace_id", "work_order_id", "session_id", "artifact_id")},
		{"name": "report_progress", "description": "Record self-reported progress for a claimed order.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "message": str}, "work_order_id", "session_id", "message")},
		{"name": "report_usage", "description": "Record best-effort cumulative self-reported token, cost, and optional provider rate-limit status as observational audit telemetry. Report at natural checkpoints and immediately before the stage's terminal lifecycle tool when figures are available; missing usage never blocks lifecycle progress (DEC-1).", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "tokens_in": num, "tokens_out": num, "cost_usd": num, "rate_limit": rateLimit, "source": map[string]any{"type": "string", "enum": []string{"self_reported", "worker_fallback"}, "description": "Reserved worker provenance; agents omit this or use self_reported."}}, "work_order_id", "session_id", "tokens_in", "tokens_out", "cost_usd")},
		{"name": "report_continuation", "description": "Record best-effort advisory harness-native continuation metadata for the exact active attempt. Only the launching worker or conveyor run client may report it; failure or absence never changes lifecycle outcome.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "continuation_session_id": str, "attempt_id": str, "harness": str, "launch_environment": str}, "work_order_id", "session_id", "continuation_session_id", "attempt_id", "harness", "launch_environment")},
		{"name": "propose_system_design_revision", "description": "Propose a complete immutable System Design revision from the current claimed implementation. The operator alone confirms after submission; confirmation never blocks implementation.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "document_id": str, "content": str}, "work_order_id", "session_id", "document_id", "content")},
		{"name": "propose_decision", "description": "Propose the next stable DEC-n record from implementation deliberation. The operator alone confirms after submission; confirmation never blocks implementation.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "statement": str, "context": str, "alternatives_rejected": str, "supersedes": str}, "work_order_id", "session_id", "statement", "context", "alternatives_rejected")},
		{"name": "upload_transcript", "description": "Upload an optional self-reported transcript through Conveyor redaction.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "transcript": str}, "work_order_id", "session_id", "transcript")},
		{"name": "submit_plan", "description": "Validate and submit a Markdown execution plan for a claimed plan-stage order. Include Approach, Files touched, Ordering, Risks, and Done criteria headings. Plans never create child tasks; decomposition must be empty. Validation errors leave the order claimed for correction.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "markdown": map[string]any{"type": "string", "description": "Example: ## Approach\\nImplement the shared handler.\\n\\n## Files touched\\n- internal/httpapi/mcp.go\\n\\n## Ordering\\n1. Validate, then persist.\\n\\n## Risks\\n- Preserve gate events.\\n\\n## Done criteria\\n- submit_plan persists the task execution plan."}, "decomposition": map[string]any{"type": "array", "description": "Must be empty; plans cannot fan out tasks.", "items": map[string]any{"type": "object", "properties": map[string]any{"id": str, "repo": str, "summary": str, "depends_on": map[string]any{"type": "array", "items": str}}, "required": []string{"id", "repo", "summary", "depends_on"}, "additionalProperties": false}}}, "work_order_id", "session_id", "markdown", "decomposition")},
		{"name": "submit_for_review", "description": "Open or reuse the pushed branch PR and dispatch independent review.", "inputSchema": object(identity, "work_order_id", "session_id")},
		{"name": "await_review", "description": "Long-poll for the review verdict so changes requested returns to the warm implementer session.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "timeout_seconds": num}, "work_order_id", "session_id")},
		{"name": "submit_review_verdict", "description": "Submit a validated independent review verdict, feedback, pinned REQ-n/AC-n.m citations, plan done-criteria coverage, and System Design/DEC governance assessment.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "verdict": map[string]any{"type": "string", "enum": []string{"approve", "changes_requested"}}, "reason_code": str, "summary": str, "feedback": str, "requirement_citations": requirementCitations, "done_criteria_coverage": doneCriteriaCoverage, "governance_assessment": governanceAssessment}, "work_order_id", "session_id", "verdict", "reason_code", "summary", "requirement_citations", "done_criteria_coverage", "governance_assessment")},
	}
}
