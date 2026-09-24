package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func validateContextRefreshArgs(args map[string]any) error {
	b, e := json.Marshal(args)
	if e != nil || len(b) > 8192 {
		return fmt.Errorf("invalid context refresh arguments")
	}
	for k, v := range args {
		if k != "workspace_id" && k != "work_order_id" && k != "session_id" && k != "prior_revision" {
			return fmt.Errorf("unknown context refresh field")
		}
		s, ok := v.(string)
		if !ok || len(s) > 256 {
			return fmt.Errorf("invalid context refresh argument")
		}
		if k == "prior_revision" && s != "" && !core.ValidContextRevision(s) {
			return fmt.Errorf("invalid prior_revision")
		}
	}
	for _, k := range []string{"workspace_id", "work_order_id", "session_id"} {
		s, _ := args[k].(string)
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", k)
		}
	}
	return nil
}
func (s *Server) refreshWorkOrderContext(w http.ResponseWriter, r *http.Request) {
	b, e := io.ReadAll(io.LimitReader(r.Body, 8193))
	if e != nil || len(b) > 8192 {
		http.Error(w, "invalid context refresh arguments", http.StatusBadRequest)
		return
	}
	var body struct {
		WorkspaceID   string `json:"workspace_id"`
		SessionID     string `json:"session_id"`
		PriorRevision string `json:"prior_revision,omitempty"`
	}
	if e = core.DecodeVerificationRequest(b, &body); e != nil {
		http.Error(w, "invalid context refresh arguments", http.StatusBadRequest)
		return
	}
	result, e := s.callMCPTool(r, "refresh_work_order_context", map[string]any{"workspace_id": body.WorkspaceID, "work_order_id": chi.URLParam(r, "id"), "session_id": body.SessionID, "prior_revision": body.PriorRevision})
	if e != nil {
		http.Error(w, e.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
