package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

var verificationTools = []string{"prepare_verification_operation", "reconcile_verification_operation", "submit_verification", "get_verification_context", "prepare_verification", "register_verification_obligation", "start_verification_attempt", "report_verification_outcome", "get_evidence_schemas", "submit_verification_evidence", "upload_verification_artifact", "read_verification_evidence", "get_verification_publication"}

func verificationRead(operation string) bool {
	return operation == "get_verification_context" || operation == "get_evidence_schemas" || operation == "read_verification_evidence" || operation == "get_verification_publication"
}
func verificationMCPTools() []map[string]any {
	var result []map[string]any
	for _, name := range verificationTools {
		schema := core.VerificationJSONSchema(reflect.TypeOf(workorder.VerificationRequestType(name)))
		props := schema["properties"].(map[string]any)
		for _, field := range []string{"workspace_id", "work_order_id", "session_id", "client_token"} {
			props[field] = map[string]any{"type": "string"}
		}
		required := schema["required"].([]string)
		required = append(required, "workspace_id")
		if name != "get_evidence_schemas" {
			required = append(required, "work_order_id")
		}
		if !verificationRead(name) {
			required = append(required, "session_id", "client_token")
		}
		schema["required"] = required
		result = append(result, map[string]any{"name": name, "description": "Claim-bound verification evidence. Results support independent review and never confer acceptance or operator approval.", "inputSchema": schema})
	}
	return result
}

func (s *Server) callVerificationMCP(r *http.Request, name string, args map[string]any) (any, error) {
	get := func(key string) string { v, _ := args[key].(string); return v }
	if get("workspace_id") == "" {
		return nil, store.ErrVerificationAccess
	}
	ws, err := s.resolveMCPWorkspace(r.Context(), get("workspace_id"))
	if err != nil {
		return nil, store.ErrVerificationAccess
	}
	ctx := store.WithWorkspace(r.Context(), ws)
	if credential, ok := store.CredentialFromContext(ctx); ok && credential.Kind == core.CredentialAgent && credential.RunWorkOrderID != "" {
		if credential.RunWorkspaceID != ws || (name != "get_evidence_schemas" && (credential.RunWorkOrderID != get("work_order_id") || credential.RunSessionID != get("session_id"))) {
			return nil, store.ErrVerificationAccess
		}
	}
	if _, worker := workerFromContext(ctx); !worker && s.Workspaces != nil {
		credential, ok := store.CredentialFromContext(ctx)
		if !ok || s.Memberships == nil {
			return nil, store.ErrVerificationAccess
		}
		allowed, err := s.Memberships.AuthorizeWorkspace(ctx, credential.OwnerUserID, ws, mcpCapabilities[name])
		if err != nil || !allowed {
			return nil, store.ErrVerificationAccess
		}
	}

	if worker, ok := workerFromContext(ctx); ok {
		ctx = store.WithActor(ctx, store.Actor{ID: store.WorkerActorID(worker.ID), Role: core.ActorWorker})
	}
	payload := map[string]any{}
	for k, v := range args {
		switch k {
		case "workspace_id", "work_order_id", "session_id", "client_token":
			if _, ok := v.(string); !ok {
				return nil, store.ErrVerificationInvalid
			}
		default:
			payload[k] = v
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, store.ErrVerificationInvalid
	}
	if s.WorkOrders == nil {
		return nil, store.ErrVerificationAccess
	}
	return s.WorkOrders.Verification(ctx, get("work_order_id"), get("session_id"), get("client_token"), name, raw)
}

func (s *Server) verificationOrder(w http.ResponseWriter, r *http.Request) {
	name := strings.ReplaceAll(chi.URLParam(r, "operation"), "-", "_")
	if workorder.VerificationRequestType(name) == nil {
		http.NotFound(w, r)
		return
	}
	var args map[string]any
	if r.Method == http.MethodGet {
		if !verificationRead(name) {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		args = map[string]any{}
		for k, v := range r.URL.Query() {
			if len(v) != 1 {
				http.Error(w, "invalid verification request", 400)
				return
			}
			if k == "offset" {
				n, err := strconv.Atoi(v[0])
				if err != nil {
					http.Error(w, "invalid verification request", 400)
					return
				}
				args[k] = n
			} else {
				args[k] = v[0]
			}
		}
	} else {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "invalid verification request", 400)
			return
		}
		if err = core.DecodeVerificationRequest(raw, &args); err != nil {
			http.Error(w, "invalid verification request", 400)
			return
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	if supplied, ok := args["work_order_id"]; ok && supplied != chi.URLParam(r, "id") {
		http.Error(w, "verification access refused", 404)
		return
	}
	args["work_order_id"] = chi.URLParam(r, "id")
	ws := r.Header.Get("X-Workspace-ID")
	if ws == "" {
		ws, _ = args["workspace_id"].(string)
	}
	if _, ok := args["client_token"]; !ok && r.Header.Get("X-Conveyor-Client-Token") != "" {
		args["client_token"] = r.Header.Get("X-Conveyor-Client-Token")
	}
	if supplied, ok := args["workspace_id"]; ok && supplied != ws {
		http.Error(w, "verification access refused", 404)
		return
	}
	args["workspace_id"] = ws
	result, err := s.callVerificationMCP(r, name, args)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrVerificationAccess) {
			code = http.StatusNotFound
		}
		if errors.Is(err, store.ErrVerificationConflict) || errors.Is(err, store.ErrVerificationState) {
			code = http.StatusConflict
		}
		// Foreign scope and underlying database/forge errors never carry payloads.
		message := "invalid verification request"
		if code == 404 {
			message = "verification access refused"
		}
		if code == 409 {
			message = "verification transition or retry conflict"
		}
		http.Error(w, message, code)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
