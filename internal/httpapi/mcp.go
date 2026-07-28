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
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
		response.Result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]string{"name": "conveyor", "version": "phase-4.7-v1.5"}}
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
	worker, workerAuth := workerFromContext(r.Context())
	explicitWorkspace := stringArg("workspace_id")
	if workerAuth && explicitWorkspace == "" {
		explicitWorkspace = worker.Workspace
	}
	workspace, err := s.resolveMCPWorkspace(r.Context(), explicitWorkspace)
	if err != nil {
		return nil, err
	}
	ctx := store.WithWorkspace(r.Context(), workspace)
	session := stringArg("session_id")
	switch name {
	case "create_task":
		if workerAuth {
			return nil, fmt.Errorf("worker credentials cannot create tasks")
		}
		if _, supplied := args["title"]; supplied {
			return nil, fmt.Errorf("title is generated and must not be supplied")
		}
		if strings.TrimSpace(stringArg("idempotency_key")) == "" {
			return nil, fmt.Errorf("idempotency_key is required")
		}
		result, err := s.createTaskRecord(ctx, createTaskReq{
			Body:          stringArg("body"),
			Repo:          stringArg("repo"),
			BaseBranch:    stringArg("base_branch"),
			Source:        stringArg("source"),
			Level:         core.EscalationLevel(stringArg("level")),
			Mode:          core.TaskMode(stringArg("mode")), // deprecated §21.31: manual→hold, auto→no-op
			Hold:          boolArg(args, "hold") != nil && *boolArg(args, "hold"),
			SpecApproval:  boolArg(args, "spec_approval"),
			MergeApproval: boolArg(args, "merge_approval"),
			Setup:         stringArg("setup"),
		}, stringArg("idempotency_key"), "mcp")
		if err != nil {
			return nil, err
		}
		return map[string]any{"task": result.Task, "created": result.Created}, nil
	}
	if s.WorkOrders == nil {
		return nil, fmt.Errorf("work-order service unavailable")
	}
	switch name {
	case "list_work_orders":
		if workerAuth {
			return s.Workers.ListVisibleOrders(ctx, worker)
		}
		return s.WorkOrders.List(ctx)
	case "claim_work_order":
		lease := core.DefaultWorkOrderClaimLease
		if value, ok := numberArg(args["lease_seconds"]); ok && value > 0 && value <= 3600 {
			lease = time.Duration(value) * time.Second
		}
		claim := core.WorkOrderClaim{SessionID: session, ClientToken: stringArg("client_token"), ClaimantID: stringArg("claimant_id"), Agent: stringArg("agent"), Model: stringArg("model"), Lease: lease}
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
			return nil, fmt.Errorf("renew_work_order requires a worker credential")
		}
		return s.Workers.Renew(ctx, worker, stringArg("work_order_id"), session)
	case "release_work_order":
		if !workerAuth {
			return nil, fmt.Errorf("release_work_order requires a worker credential")
		}
		return s.Workers.Release(ctx, worker, stringArg("work_order_id"), core.WorkOrderRelease{SessionID: session, Reason: stringArg("reason"), Outcome: core.WorkOrderOutcomeReleased})
	case "get_work_order":
		if err := s.authorizeWorkerOrder(ctx, workerAuth, worker, stringArg("work_order_id")); err != nil {
			return nil, err
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
		return s.WorkOrders.UsageWithRateLimit(ctx, stringArg("work_order_id"), session, in, out, cost, rateLimit)
	case "upload_transcript":
		return s.WorkOrders.UploadTranscript(ctx, stringArg("work_order_id"), session, stringArg("transcript"))
	case "submit_spec":
		payload, marshalErr := json.Marshal(map[string]any{"markdown": args["markdown"], "acceptance": args["acceptance"], "decomposition": args["decomposition"]})
		if marshalErr != nil {
			return nil, marshalErr
		}
		var value pipeline.StructuredSpec
		decoder := json.NewDecoder(strings.NewReader(string(payload)))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&value); decodeErr != nil {
			return nil, fmt.Errorf("invalid structured spec: %w", decodeErr)
		}
		return s.WorkOrders.SubmitSpec(ctx, stringArg("work_order_id"), session, value)
	case "submit_for_review":
		return s.WorkOrders.SubmitForReview(ctx, stringArg("work_order_id"), session)
	case "await_review":
		seconds := 300.0
		if value, ok := floatArg(args["timeout_seconds"]); ok {
			seconds = value
		}
		return s.WorkOrders.AwaitReview(ctx, stringArg("work_order_id"), session, time.Duration(seconds*float64(time.Second)))
	case "submit_review_verdict":
		return s.WorkOrders.SubmitVerdict(ctx, stringArg("work_order_id"), session, pipeline.Review{Verdict: stringArg("verdict"), ReasonCode: stringArg("reason_code"), Summary: stringArg("summary"), Feedback: stringArg("feedback")})
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

// authorizeWorkerOrder scopes worker-credentialed MCP calls to orders the
// worker currently holds. A mismatch is a lost or reassigned claim — e.g.
// the lease expired and ownership returned to the queue (spec §21.9) — not a
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

func (s *Server) resolveMCPWorkspace(ctx context.Context, explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	var items []core.Workspace
	if s.Workspaces != nil {
		var err error
		items, err = s.Workspaces.ListWorkspaces(ctx)
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
	return "", fmt.Errorf("workspace_not_found: %s", explicit)
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
	identity := map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str}
	return []map[string]any{
		{"name": "create_task", "description": "Create one durable task in an explicit workspace under an optional named execution setup, generate its title from body with the trusted control-plane AI integration, and enqueue the existing triage pipeline. Reusing the same idempotency key returns the original task.", "inputSchema": object(map[string]any{"workspace_id": str, "body": map[string]any{"type": "string", "description": "Task description in GitHub-flavored Markdown. Structured descriptions using headings and lists are encouraged."}, "repo": str, "base_branch": str, "source": str, "setup": str, "hold": map[string]any{"type": "boolean", "description": "Reserve the task from the worker daemon; an operator-attached agent claims it explicitly (spec §21.31)."}, "mode": map[string]any{"type": "string", "enum": []string{"auto", "manual"}, "description": "Deprecated (spec §21.31): manual maps to hold=true, auto is a no-op."}, "spec_approval": map[string]string{"type": "boolean"}, "merge_approval": map[string]string{"type": "boolean"}, "idempotency_key": str}, "body", "repo", "idempotency_key")},
		{"name": "list_work_orders", "description": "List active, stale, or execution-timed-out spec, implement, and review work orders in one workspace with distinct queue, execution, and lease clocks.", "inputSchema": object(map[string]any{"workspace_id": str})},
		{"name": "claim_work_order", "description": "Claim a work order with a bounded lease. Review self-claim is forbidden.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "client_token": str, "claimant_id": str, "agent": str, "model": str, "lease_seconds": num}, "work_order_id", "session_id", "client_token", "agent", "model")},
		{"name": "redispatch_work_order", "description": "Return a stale queued work order in one workspace to the queue with a fresh queue deadline. Active and execution-timed-out work orders are rejected.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str}, "work_order_id")},
		{"name": "renew_work_order", "description": "Renew the exact worker child session lease without extending its fixed attempt deadline.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str}, "work_order_id", "session_id")},
		{"name": "release_work_order", "description": "Release the exact worker child session without allowing a stale child to alter a newer claim.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "reason": str}, "work_order_id", "session_id")},
		{"name": "get_work_order", "description": "Get the claimed order contract, spec, branch, feedback, artifacts, and review diff.", "inputSchema": object(identity, "work_order_id", "session_id")},
		{"name": "read_artifact", "description": "Read one artifact authorized for the claimed work order. The workspace, work order, session, and artifact ownership must all match; content is returned as base64.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "artifact_id": str}, "workspace_id", "work_order_id", "session_id", "artifact_id")},
		{"name": "report_progress", "description": "Record self-reported progress for a claimed order.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "message": str}, "work_order_id", "session_id", "message")},
		{"name": "report_usage", "description": "Record cumulative self-reported token, cost, and optional provider rate-limit status as observational audit telemetry.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "tokens_in": num, "tokens_out": num, "cost_usd": num, "rate_limit": rateLimit}, "work_order_id", "session_id", "tokens_in", "tokens_out", "cost_usd")},
		{"name": "upload_transcript", "description": "Upload an optional self-reported transcript through Conveyor redaction.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "transcript": str}, "work_order_id", "session_id", "transcript")},
		{"name": "submit_spec", "description": "Validate and submit a structured specification for a claimed spec order. Validation errors leave the order claimed for correction.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "markdown": str, "acceptance": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": str, "criterion": str, "verify": str, "ref": map[string]any{"type": []string{"string", "null"}}}, "required": []string{"id", "criterion", "verify"}, "additionalProperties": false}}, "decomposition": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": str, "repo": str, "summary": str, "depends_on": map[string]any{"type": "array", "items": str}}, "required": []string{"id", "repo", "summary", "depends_on"}, "additionalProperties": false}}}, "work_order_id", "session_id", "markdown", "acceptance", "decomposition")},
		{"name": "submit_for_review", "description": "Open or reuse the pushed branch PR and dispatch independent review.", "inputSchema": object(identity, "work_order_id", "session_id")},
		{"name": "await_review", "description": "Long-poll for the review verdict so changes requested returns to the warm implementer session.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "timeout_seconds": num}, "work_order_id", "session_id")},
		{"name": "submit_review_verdict", "description": "Submit a validated independent review verdict and structured feedback.", "inputSchema": object(map[string]any{"workspace_id": str, "work_order_id": str, "session_id": str, "verdict": map[string]any{"type": "string", "enum": []string{"approve", "changes_requested"}}, "reason_code": str, "summary": str, "feedback": str}, "work_order_id", "session_id", "verdict", "reason_code", "summary")},
	}
}
