package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// maxTokenLabelLength keeps a label readable in the list without a migration;
// user_tokens.label is unconstrained text.
const maxTokenLabelLength = 120

// Self-service personal access tokens (REQ-2, req-security-boundaries AC-2.1).
// Every handler here derives its subject from the presented credential, which
// requireSelfServiceCredential has already resolved. None of them reads a user
// identifier from the path or body, so no request shape exists that would let
// one person list or revoke another person's credentials.

func (s *Server) listOwnPersonalAccessTokens(w http.ResponseWriter, r *http.Request) {
	if s.PersonalTokens == nil {
		http.Error(w, "personal access tokens unavailable", http.StatusNotFound)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	items, err := s.PersonalTokens.ListOwnPersonalAccessTokens(r.Context(), credential.OwnerUserID)
	if err != nil {
		log.Printf("list personal access tokens: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []core.PersonalAccessToken{}
	}
	writeJSON(w, http.StatusOK, items)
}

// issueOwnPersonalAccessToken returns the bearer value exactly once. The value
// is never persisted in cleartext and no read path reproduces it, so a caller
// that loses this response must issue a replacement.
func (s *Server) issueOwnPersonalAccessToken(w http.ResponseWriter, r *http.Request) {
	if s.PersonalTokens == nil {
		http.Error(w, "personal access tokens unavailable", http.StatusNotFound)
		return
	}
	var request struct {
		Label string `json:"label"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeValidationError(w, "label", err)
		return
	}
	label := strings.TrimSpace(request.Label)
	if label == "" {
		writeValidationError(w, "label", errors.New("label is required"))
		return
	}
	if len(label) > maxTokenLabelLength {
		writeValidationError(w, "label", errors.New("label is too long"))
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	issued, err := s.PersonalTokens.IssueOwnPersonalAccessToken(r.Context(), credential.OwnerUserID, label)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("issue personal access token: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, issued)
}

func (s *Server) revokeOwnPersonalAccessToken(w http.ResponseWriter, r *http.Request) {
	if s.PersonalTokens == nil {
		http.Error(w, "personal access tokens unavailable", http.StatusNotFound)
		return
	}
	credential, _ := store.CredentialFromContext(r.Context())
	// A token owned by somebody else reaches the same branch as a token that
	// does not exist: ownership is part of the statement, not a prior check.
	if _, err := s.PersonalTokens.RevokeOwnPersonalAccessToken(r.Context(), credential.OwnerUserID, chi.URLParam(r, "token_id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "personal access token not found", http.StatusNotFound)
			return
		}
		log.Printf("revoke personal access token: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
