package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// ART-HTTP-3 requires an explicit selector even for singleton deployments.
func requireExplicitArtifactWorkspace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.URL.Query().Get("workspace_id")) == "" && strings.TrimSpace(r.Header.Get("X-Workspace-ID")) == "" {
			http.Error(w, "workspace_id is required for artifact metadata repair", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) repairArtifactMetadata(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpectedOldContentType string          `json:"expected_old_content_type"`
		NewContentType         string          `json:"new_content_type"`
		RequestID              string          `json:"request_id"`
		DryRun                 json.RawMessage `json:"dry_run"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid repair request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		http.Error(w, "repair request must contain one JSON object", http.StatusBadRequest)
		return
	}
	dryRun := false
	if len(req.DryRun) > 0 {
		if string(req.DryRun) == "null" {
			http.Error(w, "dry_run must be a boolean", 400)
			return
		}
		if err := json.Unmarshal(req.DryRun, &dryRun); err != nil {
			http.Error(w, "dry_run must be a boolean", 400)
			return
		}
	}
	result, err := s.Store.RepairArtifactMetadata(r.Context(), store.ArtifactRepairRequest{ArtifactID: chi.URLParam(r, "id"), ExpectedOldContentType: req.ExpectedOldContentType, NewContentType: req.NewContentType, RequestID: req.RequestID, DryRun: dryRun})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrArtifactRepairInvalid):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, store.ErrArtifactRepairConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "artifact not found", http.StatusNotFound)
		default:
			log.Printf("artifact metadata repair: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}
