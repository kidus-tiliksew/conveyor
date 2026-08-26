package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) getWorkspaceForgeToken(w http.ResponseWriter, r *http.Request) {
	if s.WorkspaceForgeTokens == nil {
		http.Error(w, "workspace forge token unavailable", http.StatusNotFound)
		return
	}
	workspaceID, ok := store.WorkspaceFromContext(r.Context())
	if !ok {
		writeWorkspaceNotFound(w)
		return
	}
	status, err := s.WorkspaceForgeTokens.GetWorkspaceForgeTokenStatus(r.Context(), workspaceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeWorkspaceNotFound(w)
			return
		}
		log.Printf("get workspace forge token status: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) putWorkspaceForgeToken(w http.ResponseWriter, r *http.Request) {
	if s.WorkspaceForgeTokens == nil || s.ValidateForgeToken == nil {
		http.Error(w, "workspace forge token unavailable", http.StatusNotFound)
		return
	}
	workspaceID, ok := store.WorkspaceFromContext(r.Context())
	if !ok {
		writeWorkspaceNotFound(w)
		return
	}
	request, err := decodeForgeTokenRequest(r)
	if err != nil {
		writeValidationError(w, "token", err)
		return
	}
	login, err := s.ValidateForgeToken(r.Context(), request.Token)
	if err != nil || strings.TrimSpace(login) == "" {
		http.Error(w, forgeTokenValidationFailure, http.StatusUnprocessableEntity)
		return
	}
	status, err := s.WorkspaceForgeTokens.StoreWorkspaceForgeToken(r.Context(), workspaceID, request.Token, login)
	if err != nil {
		log.Printf("store workspace forge token failed")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) deleteWorkspaceForgeToken(w http.ResponseWriter, r *http.Request) {
	if s.WorkspaceForgeTokens == nil {
		http.Error(w, "workspace forge token unavailable", http.StatusNotFound)
		return
	}
	workspaceID, ok := store.WorkspaceFromContext(r.Context())
	if !ok {
		writeWorkspaceNotFound(w)
		return
	}
	if err := s.WorkspaceForgeTokens.DeleteWorkspaceForgeToken(r.Context(), workspaceID); err != nil {
		log.Printf("delete workspace forge token: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
