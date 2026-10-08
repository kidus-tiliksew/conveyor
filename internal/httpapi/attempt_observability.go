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

// attemptObservabilityBodyLimit bounds the whole encoded capture request. The
// service retains only the redacted newest 4 MiB of transcript content; the
// remaining allowance covers JSON escaping of that content
// (req-260820-221be8 AC-2.1; DEC-26; component-http-api).
const attemptObservabilityBodyLimit = 6 << 20

type attemptObservabilityRequest struct {
	SessionID         string          `json:"session_id"`
	AttemptID         string          `json:"attempt_id"`
	TerminationReason string          `json:"termination_reason"`
	Transcript        json.RawMessage `json:"transcript"`
}

// decodeAttemptObservability reads one bounded capture body. A malformed
// optional transcript is refused instead of silently dropped so a launcher
// bug cannot record an ending without the capture it meant to deliver.
func decodeAttemptObservability(w http.ResponseWriter, r *http.Request) (core.WorkOrderAttemptCapture, bool) {
	body := http.MaxBytesReader(w, r.Body, attemptObservabilityBodyLimit)
	var request attemptObservabilityRequest
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "attempt capture exceeds 6 MiB", http.StatusRequestEntityTooLarge)
			return core.WorkOrderAttemptCapture{}, false
		}
		http.Error(w, "malformed attempt capture", http.StatusBadRequest)
		return core.WorkOrderAttemptCapture{}, false
	}
	capture := core.WorkOrderAttemptCapture{
		SessionID:         strings.TrimSpace(request.SessionID),
		AttemptID:         strings.TrimSpace(request.AttemptID),
		TerminationReason: strings.TrimSpace(request.TerminationReason),
	}
	if capture.SessionID == "" || capture.AttemptID == "" {
		http.Error(w, "session_id and attempt_id are required", http.StatusBadRequest)
		return core.WorkOrderAttemptCapture{}, false
	}
	if len(request.Transcript) > 0 && string(request.Transcript) != "null" {
		var transcript struct {
			Content   *string `json:"content"`
			Truncated bool    `json:"truncated"`
		}
		if err := json.Unmarshal(request.Transcript, &transcript); err != nil || transcript.Content == nil {
			http.Error(w, "malformed attempt transcript", http.StatusBadRequest)
			return core.WorkOrderAttemptCapture{}, false
		}
		capture.Transcript = &core.WorkOrderAttemptTranscript{Content: *transcript.Content, Truncated: transcript.Truncated}
	}
	return capture, true
}

// writeAttemptCaptureResult maps observational capture refusals. None of them
// writes lifecycle state; the launcher treats every refusal as a warning.
func writeAttemptCaptureResult(w http.ResponseWriter, result core.WorkOrderAttemptCaptureResult, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, result)
	case errors.Is(err, store.ErrAttemptCaptureReasonConflict):
		w.Header().Set("X-Conveyor-Error-Code", "attempt_capture_reason_conflict")
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrAttemptCaptureUnverified):
		w.Header().Set("X-Conveyor-Error-Code", "attempt_capture_unverified")
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrWorkOrderClaimUnauthorized), errors.Is(err, store.ErrWorkOrderClaimLost), errors.Is(err, store.ErrNotFound):
		w.Header().Set("X-Conveyor-Error-Code", "attempt_capture_unauthorized")
		http.Error(w, "attempt capture unauthorized", http.StatusConflict)
	case strings.Contains(err.Error(), "are required"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		log.Printf("record attempt capture: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// captureWorkerOrderAttempt records an ended attempt's capture for the
// enrolled worker that claimed it, for every stage (req-260820-221be8 AC-2.1;
// component-mcp-protocol worker plane).
func (s *Server) captureWorkerOrderAttempt(w http.ResponseWriter, r *http.Request) {
	if s.Workers == nil {
		http.Error(w, "worker service unavailable", http.StatusServiceUnavailable)
		return
	}
	worker, ok := workerFromContext(r.Context())
	if !ok || worker.ID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	capture, ok := decodeAttemptObservability(w, r)
	if !ok {
		return
	}
	result, err := s.Workers.CaptureWorkerAttempt(r.Context(), worker, chi.URLParam(r, "id"), capture)
	writeAttemptCaptureResult(w, result, err)
}

// captureTaskRunOrderAttempt records an ended attempt's capture for the user
// whose explicit run claimed it. Only the invoking user's credential reaches
// this route; a session-bound run child credential is refused by
// requireTaskRunAuth, and identity is checked against the immutable claim
// record rather than the order's current, possibly successor, claim.
func (s *Server) captureTaskRunOrderAttempt(w http.ResponseWriter, r *http.Request) {
	if s.Workers == nil {
		http.Error(w, "task run service unavailable", http.StatusServiceUnavailable)
		return
	}
	credential, ok := store.CredentialFromContext(r.Context())
	if !ok || credential.Kind != core.CredentialUser || credential.OwnerUserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	capture, ok := decodeAttemptObservability(w, r)
	if !ok {
		return
	}
	orderID := chi.URLParam(r, "order_id")
	order, err := s.Store.GetWorkOrder(r.Context(), orderID)
	if err != nil || order.TaskID != chi.URLParam(r, "id") {
		writeAttemptCaptureResult(w, core.WorkOrderAttemptCaptureResult{}, store.ErrWorkOrderClaimUnauthorized)
		return
	}
	claim := core.WorkOrderClaimIdentity{ClaimantID: core.TaskRunClaimantID(credential.OwnerUserID), SessionID: capture.SessionID}
	result, err := s.Workers.CaptureAttempt(r.Context(), claim, order.ID, capture)
	writeAttemptCaptureResult(w, result, err)
}
