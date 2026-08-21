package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const forgeTokenValidationFailure = "forge token validation failed: authenticated forge identity read failed"

func (s *Server) getOwnForgeToken(w http.ResponseWriter, r *http.Request) {
	if s.ForgeTokens == nil {
		http.Error(w, "forge token unavailable", http.StatusNotFound)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	status, err := s.ForgeTokens.GetForgeTokenStatus(r.Context(), credential.OwnerUserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("get forge token status: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) putOwnForgeToken(w http.ResponseWriter, r *http.Request) {
	if s.ForgeTokens == nil || s.ValidateForgeToken == nil {
		http.Error(w, "forge token unavailable", http.StatusNotFound)
		return
	}
	var request struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeValidationError(w, "token", err)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeValidationError(w, "token", errors.New("request body must contain one JSON object"))
		return
	}
	if strings.TrimSpace(request.Token) == "" {
		writeValidationError(w, "token", errors.New("token is required"))
		return
	}
	login, err := s.ValidateForgeToken(r.Context(), request.Token)
	if err != nil || strings.TrimSpace(login) == "" {
		http.Error(w, forgeTokenValidationFailure, http.StatusUnprocessableEntity)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	status, err := s.ForgeTokens.StoreForgeToken(r.Context(), credential.OwnerUserID, request.Token, login)
	if err != nil {
		log.Printf("store forge token: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) deleteOwnForgeToken(w http.ResponseWriter, r *http.Request) {
	if s.ForgeTokens == nil {
		http.Error(w, "forge token unavailable", http.StatusNotFound)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	if err := s.ForgeTokens.DeleteForgeToken(r.Context(), credential.OwnerUserID); err != nil {
		log.Printf("delete forge token: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
