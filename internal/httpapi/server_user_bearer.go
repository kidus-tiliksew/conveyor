package httpapi

import (
	"net/http"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// requireUserBearerAuth admits only a bearer user credential and stamps the
// user actor (component-http-api, One router and two credential classes).
func (s *Server) requireUserBearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if credential, ok := store.CredentialFromContext(r.Context()); ok && credential.Kind == core.CredentialUser && credential.Method == core.CredentialMethodBearer {
			next.ServeHTTP(w, r)
			return
		}
		credential, err := s.authenticateUserCredential(r)
		if err != nil {
			writeCredentialVerificationError(w, err)
			return
		}
		if credential.Kind != core.CredentialUser {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if credential.Method == "" {
			credential.Method = core.CredentialMethodBearer
		}
		ctx := store.WithCredential(r.Context(), credential)
		ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireTaskRequestChangesAuth preserves the dashboard session boundary for
// the human merge-gate action. In production the workspace group has already
// populated the credential; the fallback keeps memory-mode task routes on the
// same authenticated contract.
func (s *Server) requireTaskRequestChangesAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if credential, ok := store.CredentialFromContext(r.Context()); ok && credential.Kind == core.CredentialUser {
			next.ServeHTTP(w, r)
			return
		}
		credential, err := s.authenticateHumanCredential(w, r)
		if err != nil {
			writeCredentialVerificationError(w, err)
			return
		}
		if credential.Kind != core.CredentialUser {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.requireSessionMutationProof(w, r, credential) {
			return
		}
		ctx := store.WithCredential(r.Context(), credential)
		ctx = store.WithActor(ctx, store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
