package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

type workerContextKey struct{}

func workerFromContext(ctx context.Context) (core.Worker, bool) {
	worker, ok := ctx.Value(workerContextKey{}).(core.Worker)
	return worker, ok
}

func (s *Server) requireWorkerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Workers == nil {
			http.Error(w, "worker service unavailable", http.StatusServiceUnavailable)
			return
		}
		credential, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		workspace := strings.TrimSpace(r.Header.Get("X-Workspace-ID"))
		ctx, worker, err := s.Workers.Authenticate(r.Context(), credential, workspace)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx = context.WithValue(ctx, workerContextKey{}, worker)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) issueWorkerPairing(w http.ResponseWriter, r *http.Request) {
	if s.Workers == nil {
		http.Error(w, "worker service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		TTLSeconds int64 `json:"ttl_seconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	token, pairing, err := s.Workers.IssuePairing(r.Context(), time.Duration(request.TTLSeconds)*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pairing_token": token, "expires_at": pairing.ExpiresAt})
}

func (s *Server) enrollWorker(w http.ResponseWriter, r *http.Request) {
	if s.Workers == nil {
		http.Error(w, "worker service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		PairingToken string `json:"pairing_token"`
		Name         string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	enrollment, err := s.Workers.Enroll(r.Context(), request.PairingToken, request.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusCreated, enrollment)
}

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	if s.Workers == nil {
		http.Error(w, "worker service unavailable", http.StatusServiceUnavailable)
		return
	}
	workers, err := s.Store.ListWorkers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if workers == nil {
		workers = []core.Worker{}
	}
	for i := range workers {
		if workers[i].Probes == nil {
			workers[i].Probes = []core.HarnessProbe{}
		}
	}
	cfg, err := s.ConfigProvider(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	available, reason := s.Workers.AutoAvailable(r.Context(), cfg)
	rateLimits, err := s.Workers.RateLimitHealth(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	serviceability := make(map[string]any, len(cfg.Setups))
	for _, setup := range cfg.Setups {
		setupAvailable, setupReason := s.Workers.AutoAvailableForSetup(r.Context(), cfg, setup)
		modelFailures, failureErr := s.Workers.ModelFailuresForSetup(r.Context(), setup)
		if failureErr != nil {
			http.Error(w, failureErr.Error(), http.StatusInternalServerError)
			return
		}
		serviceability[setup.Name] = map[string]any{"auto_available": setupAvailable, "auto_unavailable_reason": setupReason, "model_failures": modelFailures}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers, "auto_available": available, "auto_unavailable_reason": reason, "setup_serviceability": serviceability, "rate_limits": rateLimits})
}

func (s *Server) revokeWorker(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.RevokeWorker(r.Context(), chi.URLParam(r, "id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) heartbeatWorker(w http.ResponseWriter, r *http.Request) {
	worker, ok := workerFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var request struct {
		Probes []core.HarnessProbe `json:"probes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updated, err := s.Workers.Heartbeat(r.Context(), worker, request.Probes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) getWorkerConfig(w http.ResponseWriter, r *http.Request) {
	if s.ConfigProvider == nil {
		http.Error(w, "worker configuration requires the Postgres database backend", http.StatusNotImplemented)
		return
	}
	cfg, err := s.ConfigProvider(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	active, err := s.Workers.ActiveHarnesses(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if active == nil {
		active = []workerservice.HarnessProbeTarget{}
	}
	writeJSON(w, http.StatusOK, workerservice.WorkerConfig{WorkspaceDocument: cfg.WorkspaceDocument(), ActiveHarnesses: active})
}

func (s *Server) listWorkerOrders(w http.ResponseWriter, r *http.Request) {
	worker, ok := workerFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	orders, err := s.Workers.ListClaimable(r.Context(), worker)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, orders)
}

func (s *Server) claimWorkerOrder(w http.ResponseWriter, r *http.Request) {
	worker, ok := workerFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var request struct {
		SessionID    string `json:"session_id"`
		ClientToken  string `json:"client_token"`
		LeaseSeconds int64  `json:"lease_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	order, err := s.Workers.ClaimForWorker(r.Context(), worker, chi.URLParam(r, "id"), core.WorkOrderClaim{SessionID: request.SessionID, ClientToken: request.ClientToken, Lease: time.Duration(request.LeaseSeconds) * time.Second})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (s *Server) renewWorkerOrder(w http.ResponseWriter, r *http.Request) {
	worker, _ := workerFromContext(r.Context())
	var request struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	order, err := s.Workers.Renew(r.Context(), worker, chi.URLParam(r, "id"), request.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrWorkOrderPreempted) {
			w.Header().Set("X-Conveyor-Error-Code", "work_order_preempted")
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (s *Server) reconcileWorkerOrder(w http.ResponseWriter, r *http.Request) {
	worker, ok := workerFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	result, err := s.Workers.Reconcile(r.Context(), worker, chi.URLParam(r, "id"), r.URL.Query().Get("session_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) releaseWorkerOrder(w http.ResponseWriter, r *http.Request) {
	worker, _ := workerFromContext(r.Context())
	var request struct {
		SessionID     string `json:"session_id"`
		Reason        string `json:"reason"`
		Outcome       string `json:"outcome"`
		ExitStatus    *int   `json:"exit_status"`
		FailureDetail string `json:"failure_detail"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	order, err := s.Workers.Release(r.Context(), worker, chi.URLParam(r, "id"), core.WorkOrderRelease{SessionID: request.SessionID, Reason: request.Reason, Outcome: request.Outcome, ExitStatus: request.ExitStatus, FailureDetail: request.FailureDetail})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (s *Server) checkpointWorkerOrderAttempt(w http.ResponseWriter, r *http.Request) {
	worker, _ := workerFromContext(r.Context())
	var request core.WorkOrderAttemptCheckpoint
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	created, err := s.Workers.CheckpointAttempt(r.Context(), worker, chi.URLParam(r, "id"), request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"created": created})
}
