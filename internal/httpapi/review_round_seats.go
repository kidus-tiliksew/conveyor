package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// addReviewSeats serves POST /v1/tasks/{id}/review-round/seats, the operator
// review-seat addition (feature-review-seat-minimum RS-5;
// req-review-gates-evidence AC-2.5, AC-2.6). The route's
// requireMutationCapability(operate_gates) admits only user credentials.
func (s *Server) addReviewSeats(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	request, err := decodeReviewSeatAddition(r.Body)
	if err == nil {
		request.TaskID = chi.URLParam(r, "id")
		err = request.Validate()
	}
	if err != nil {
		writeReviewSeatAdditionError(w, err)
		return
	}
	result, err := s.WorkOrders.AddReviewSeats(r.Context(), request)
	if err != nil {
		writeReviewSeatAdditionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// decodeReviewSeatAddition strictly decodes exactly one {request_id, reason,
// add} object: unknown fields and trailing JSON are refused, and add must be
// a JSON integer literal (no string, fraction, or exponent).
func decodeReviewSeatAddition(body io.Reader) (store.ReviewSeatAdditionRequest, error) {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", store.ErrReviewSeatAdditionInvalid, message) }
	var raw struct {
		RequestID string          `json:"request_id"`
		Reason    string          `json:"reason"`
		Add       json.RawMessage `json:"add"`
	}
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return store.ReviewSeatAdditionRequest{}, invalid("body must be one {request_id, reason, add} JSON object: " + err.Error())
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return store.ReviewSeatAdditionRequest{}, invalid("body must be one {request_id, reason, add} JSON object")
	}
	literal := bytes.TrimSpace(raw.Add)
	if len(literal) == 0 || bytes.ContainsAny(literal, ".eE\"") {
		return store.ReviewSeatAdditionRequest{}, invalid("add must be an integer")
	}
	add, err := strconv.ParseInt(string(literal), 10, 64)
	if err != nil || add < 1 || add > store.MaxReviewSeatCount {
		return store.ReviewSeatAdditionRequest{}, invalid("add must be an integer from 1 to " + strconv.Itoa(store.MaxReviewSeatCount))
	}
	return store.ReviewSeatAdditionRequest{RequestID: raw.RequestID, Reason: raw.Reason, Add: int(add)}, nil
}

func writeReviewSeatAdditionError(w http.ResponseWriter, err error) {
	var code string
	var status int
	switch {
	case errors.Is(err, store.ErrReviewSeatAdditionInvalid):
		code, status = "invalid_review_seats", http.StatusBadRequest
	case errors.Is(err, store.ErrReviewRoundNotOpen):
		code, status = "review_round_not_open", http.StatusConflict
	case errors.Is(err, store.ErrReviewRoundSettled):
		code, status = "review_round_settled", http.StatusConflict
	case errors.Is(err, store.ErrReviewSeatAdditionConflict):
		code, status = "review_seat_addition_conflict", http.StatusConflict
	case errors.Is(err, store.ErrReviewSeatAdditionForbidden):
		code, status = "review_seat_addition_forbidden", http.StatusForbidden
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	default:
		log.Printf("add review seats: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, status, map[string]string{"error": code, "message": err.Error()})
}
