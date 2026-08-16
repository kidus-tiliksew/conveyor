package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"gopkg.in/yaml.v3"
)

type WorkspaceConfigStore interface {
	WorkspaceConfig(context.Context) (config.VersionedDocument, error)
	UpdateWorkspaceConfig(context.Context, int64, *config.Config) (config.UpdateReceipt, error)
}

func (s *Server) getHarnessTemplates(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusGone, map[string]string{"error": "execution_configuration_retired", "message": "server harness templates are retired; execution setups are client-local"})
}

func (s *Server) getWorkspaceConfig(w http.ResponseWriter, r *http.Request) {
	if s.ConfigStore == nil {
		http.Error(w, "workspace config unavailable", http.StatusNotFound)
		return
	}
	record, err := s.ConfigStore.WorkspaceConfig(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "workspace config unavailable", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("get workspace config: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if s.Deployment != nil {
		data, marshalErr := yaml.Marshal(record.Document)
		if marshalErr != nil {
			http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
			return
		}
		base := *s.Deployment
		if workspace, ok := store.WorkspaceFromContext(r.Context()); ok {
			base.Workspace = workspace
		}
		normalized, parseErr := config.ParseWorkspaceDocument(data, &base, "workspace config API")
		if parseErr != nil {
			http.Error(w, parseErr.Error(), http.StatusInternalServerError)
			return
		}
		record.Document = normalized.PolicyDocument()
	}
	if record.Document.Repos == nil {
		record.Document.Repos = []config.Repo{}
	}
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", record.Version))
	writeJSON(w, http.StatusOK, record)
}

type workspaceConfigRequest struct {
	Document config.WorkspaceDocument `json:"document"`
}

type configFieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (s *Server) putWorkspaceConfig(w http.ResponseWriter, r *http.Request) {
	if s.ConfigStore == nil || s.Deployment == nil {
		http.Error(w, "workspace config unavailable", http.StatusNotFound)
		return
	}
	expected, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{"error": "if_match_required", "message": err.Error()})
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if readErr != nil {
		writeValidationError(w, "document", readErr)
		return
	}
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeValidationError(w, "document", err)
		return
	}
	if field := forbiddenExecutionField(raw); field != "" {
		writeValidationError(w, field, fmt.Errorf("%s is retired execution detail and must not be supplied", field))
		return
	}
	var request workspaceConfigRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeValidationError(w, "document", err)
		return
	}
	if request.Document.Review.Seats != nil && len(request.Document.Review.Seats) == 0 {
		writeValidationError(w, "review", errors.New("review.seats must contain at least one seat"))
		return
	}
	if request.Document.Setups != nil && len(request.Document.Setups) == 0 {
		writeValidationError(w, "setups", errors.New("setups must contain at least one setup"))
		return
	}
	data, err := yaml.Marshal(request.Document)
	if err != nil {
		writeValidationError(w, "document", err)
		return
	}
	base := *s.Deployment
	base.Workspace, _ = store.WorkspaceFromContext(r.Context())
	next, err := config.ParseWorkspaceDocument(data, &base, "workspace config API")
	if err != nil {
		writeValidationError(w, validationField(err), err)
		return
	}
	receipt, err := s.ConfigStore.UpdateWorkspaceConfig(r.Context(), expected, next)
	if errors.Is(err, config.ErrVersionConflict) {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error": "version_conflict", "message": "workspace config changed; reload and retry",
		})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "workspace config unavailable", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("update workspace config: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", receipt.Version))
	writeJSON(w, http.StatusOK, receipt)
}

func forbiddenExecutionField(value any) string {
	forbidden := map[string]struct{}{
		"execution_settings": {}, "routing": {}, "harnesses": {}, "setups": {}, "default_setup": {},
		"planning_models": {}, "model": {}, "model_policy": {}, "harness": {}, "effort": {}, "argv": {},
		"command": {}, "model_args": {}, "effort_args": {}, "probe_command": {}, "mcp_transport": {},
	}
	var walk func(any) string
	walk = func(node any) string {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				if _, blocked := forbidden[strings.ToLower(key)]; blocked {
					return key
				}
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(value)
}

func parseIfMatch(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("If-Match with the current config version is required")
	}
	value = strings.TrimPrefix(value, "W/")
	value = strings.Trim(value, "\"")
	version, err := strconv.ParseInt(value, 10, 64)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("If-Match must contain a positive config version")
	}
	return version, nil
}

func writeValidationError(w http.ResponseWriter, field string, err error) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":  "validation_failed",
		"fields": []configFieldError{{Field: field, Message: err.Error()}},
	})
}

func validationField(err error) string {
	message := err.Error()
	for _, field := range []string{"execution_settings.control_plane.triage.effort", "execution_settings.spec.effort", "execution_settings.implementation.effort", "default_setup", "setups", "max_bounces", "work_order_queue_timeout", "execution_settings", "routing", "harnesses", "review", "repo", "workspace"} {
		if strings.Contains(message, field) {
			if field == "repo" {
				return "repos"
			}
			return field
		}
	}
	return "document"
}
