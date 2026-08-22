package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// resolveOptionalWorkspaceCapability preserves /v1/me as an unscoped
// self-service read unless the caller explicitly supplies workspace context.
// Explicit context is authorized before identity is read, so a missing binding
// and a missing workspace are indistinguishable (REQ-3/AC-3.1, DEC-19).
func (s *Server) resolveOptionalWorkspaceCapability(capability core.Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			queryID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
			headerID := strings.TrimSpace(r.Header.Get("X-Workspace-ID"))
			if queryID != "" && headerID != "" && queryID != headerID {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace_conflict", "message": "workspace query and header disagree"})
				return
			}
			workspaceID := queryID
			if workspaceID == "" {
				workspaceID = headerID
			}
			if workspaceID == "" {
				next.ServeHTTP(w, r)
				return
			}
			credential, ok := store.CredentialFromContext(r.Context())
			if !ok || s.Memberships == nil {
				writeWorkspaceNotFound(w)
				return
			}
			allowed, err := s.Memberships.AuthorizeWorkspace(r.Context(), credential.OwnerUserID, workspaceID, capability)
			if err != nil {
				log.Printf("authorize caller workspace: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if !allowed {
				writeWorkspaceNotFound(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(store.WithWorkspace(r.Context(), workspaceID)))
		})
	}
}

func (s *Server) getCallerIdentity(w http.ResponseWriter, r *http.Request) {
	if s.CallerIdentities == nil {
		http.Error(w, "caller identity unavailable", http.StatusNotFound)
		return
	}
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	workspaceID, _ := store.WorkspaceFromContext(r.Context())
	identity, err := s.CallerIdentities.GetCallerIdentity(r.Context(), credential.OwnerUserID, workspaceID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "caller identity unavailable", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("get caller identity: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, identity)
}

const maxDisplayNameBytes = 128

// putOwnDisplayName is deliberately session-only even though GET /v1/me also
// accepts human bearer credentials. The target user and session are both
// credential-derived; the JSON body cannot name another user (AC-10.8).
func (s *Server) putOwnDisplayName(w http.ResponseWriter, r *http.Request) {
	if s.OwnProfiles == nil {
		http.Error(w, "profile unavailable", http.StatusNotFound)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	if credential.Method != core.CredentialMethodSession {
		http.Error(w, "session required", http.StatusBadRequest)
		return
	}
	var request struct {
		DisplayName string `json:"display_name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeValidationError(w, "display_name", errors.New("valid display name input is required"))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeValidationError(w, "display_name", errors.New("request body must contain one JSON object"))
		return
	}
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	if request.DisplayName == "" {
		writeValidationError(w, "display_name", errors.New("display name is required"))
		return
	}
	if len(request.DisplayName) > maxDisplayNameBytes {
		writeValidationError(w, "display_name", errors.New("display name must contain at most 128 bytes"))
		return
	}
	identity, err := s.OwnProfiles.SetOwnDisplayName(r.Context(), credential.OwnerUserID, credential.ID, request.DisplayName)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, core.ErrInvalidCredential) {
		http.Error(w, "session unavailable", http.StatusUnauthorized)
		return
	}
	if err != nil {
		log.Printf("set own display name: %v", err)
		http.Error(w, "could not update profile", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, identity)
}
