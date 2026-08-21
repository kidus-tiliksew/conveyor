package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	passwordAttemptBudget  = 5
	passwordAttemptWindow  = 15 * time.Minute
	passwordLimiterMaxKeys = 4096
)

const passwordSignInRefusal = "invalid email or password"

type passwordAttemptWindowState struct {
	started time.Time
	last    time.Time
	count   int
}

// passwordAttemptLimiter is deliberately process-local: it adds the bounded
// application protection required in front of Argon2id without creating a new
// persistence or queue dependency. Account keys are hashed so email addresses
// do not remain in limiter memory.
type passwordAttemptLimiter struct {
	mu      sync.Mutex
	windows map[string]passwordAttemptWindowState
	now     func() time.Time
}

func newPasswordAttemptLimiter() *passwordAttemptLimiter {
	return &passwordAttemptLimiter{windows: make(map[string]passwordAttemptWindowState), now: time.Now}
}

func (l *passwordAttemptLimiter) allow(keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().UTC()
	if len(l.windows) >= passwordLimiterMaxKeys {
		for key, state := range l.windows {
			if now.Sub(state.last) >= passwordAttemptWindow {
				delete(l.windows, key)
			}
		}
	}
	if len(l.windows) >= passwordLimiterMaxKeys {
		var oldestKey string
		var oldest time.Time
		for key, state := range l.windows {
			if oldestKey == "" || state.last.Before(oldest) {
				oldestKey, oldest = key, state.last
			}
		}
		delete(l.windows, oldestKey)
	}
	allowed := true
	for _, key := range keys {
		state := l.windows[key]
		if state.started.IsZero() || now.Sub(state.started) >= passwordAttemptWindow {
			state = passwordAttemptWindowState{started: now}
		}
		state.last = now
		state.count++
		l.windows[key] = state
		if state.count > passwordAttemptBudget {
			allowed = false
		}
	}
	return allowed
}

func passwordAccountLimitKey(email string) string {
	hash := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "account:" + hex.EncodeToString(hash[:])
}

func passwordSourceLimitKey(r *http.Request) string {
	// RemoteAddr is supplied by the HTTP server. Client-controlled forwarding
	// headers are intentionally ignored until the deployment has an explicit
	// trusted-proxy contract.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "source:" + host
}

func (s *Server) passwordAttempts() *passwordAttemptLimiter {
	s.passwordLimiterOnce.Do(func() { s.passwordLimiter = newPasswordAttemptLimiter() })
	return s.passwordLimiter
}

func (s *Server) signInWithPassword(w http.ResponseWriter, r *http.Request) {
	if s.InvitationSessions == nil {
		http.Error(w, "sign-in unavailable", http.StatusNotFound)
		return
	}
	if !s.requirePasswordSignInProof(w, r) {
		return
	}
	var request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Email == "" || request.Password == "" {
		writeValidationError(w, "credentials", errors.New("email and password are required"))
		return
	}
	if !s.passwordAttempts().allow(passwordAccountLimitKey(request.Email), passwordSourceLimitKey(r)) {
		http.Error(w, passwordSignInRefusal, http.StatusTooManyRequests)
		return
	}
	session, user, err := s.InvitationSessions.SignInWithPassword(r.Context(), request.Email, request.Password)
	if err != nil {
		if errors.Is(err, core.ErrInvalidCredential) {
			http.Error(w, passwordSignInRefusal, http.StatusUnauthorized)
			return
		}
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	setDashboardSessionCookie(w, r, s.InvitationDelivery.PublicURL, session.Value, session.ExpiresAt)
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "expires_at": session.ExpiresAt})
}

func (s *Server) setOwnPassword(w http.ResponseWriter, r *http.Request) {
	credential, _ := store.CredentialFromContext(r.Context())
	if credential.Method != core.CredentialMethodSession {
		http.Error(w, "session required", http.StatusBadRequest)
		return
	}
	var request struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeValidationError(w, "password", errors.New("valid password input is required"))
		return
	}
	if err := s.InvitationSessions.SetOwnPassword(r.Context(), credential.OwnerUserID, credential.ID, request.CurrentPassword, request.NewPassword); err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidPassword):
			writeValidationError(w, "new_password", err)
		case errors.Is(err, store.ErrInvalidCurrentPassword):
			http.Error(w, "current password is incorrect", http.StatusUnauthorized)
		case errors.Is(err, core.ErrInvalidCredential):
			http.Error(w, "session unavailable", http.StatusUnauthorized)
		default:
			http.Error(w, "could not update password", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requirePasswordSignInProof(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Conveyor-CSRF") != "1" {
		http.Error(w, "CSRF proof required", http.StatusForbidden)
		return false
	}
	return s.requireRequestOrigin(w, r)
}
