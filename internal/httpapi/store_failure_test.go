package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func storeFailureRequest(requestID string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/v1/example", nil)
	return request.WithContext(context.WithValue(request.Context(), middleware.RequestIDKey, requestID))
}

// TestWriteStoreFailureLogsRedactedCauseWithRequestID proves the DEC-105(1)
// writer: a generic 500, no error text in the response, and one log line with
// the request ID and the redacted cause.
func TestWriteStoreFailureLogsRedactedCauseWithRequestID(t *testing.T) {
	captured := captureStandardLog(t)
	response := httptest.NewRecorder()
	writeStoreFailure(response, storeFailureRequest("host/abc-000042"), errors.New(failingStoreDriverText))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", response.Code)
	}
	if body := strings.TrimSpace(response.Body.String()); body != "internal server error" {
		t.Fatalf("body = %q", body)
	}
	logged := captured.String()
	if !strings.Contains(logged, `request_id="host/abc-000042"`) || !strings.Contains(logged, `relation \"tasks\" does not exist`) {
		t.Fatalf("log lacks the request ID or cause: %q", logged)
	}
	if strings.Contains(logged, failingStoreCredential) {
		t.Fatalf("log carries the credential: %q", logged)
	}
}

// TestWriteStoreFailureCutsLongCause keeps one failure from writing an
// unbounded log line.
func TestWriteStoreFailureCutsLongCause(t *testing.T) {
	captured := captureStandardLog(t)
	writeStoreFailure(httptest.NewRecorder(), storeFailureRequest("id"), errors.New(strings.Repeat("é", errorLogLimit)))
	if logged := captured.String(); len(logged) > 2*errorLogLimit || strings.ContainsRune(logged, '�') {
		t.Fatalf("log length %d is not cut on a rune boundary", len(logged))
	}
}

type typedStoreRefusal struct{ name string }

func (e *typedStoreRefusal) Error() string { return "typed refusal " + e.name }

// TestWriteClassifiedStoreError pins the classifying rule: a sentinel or typed
// error answers its 4xx with the fixed message, and an unnamed error, including
// a wrapped backend failure or an untranslated driver error, falls to the
// logged generic 500 (DEC-105(1), (2)).
func TestWriteClassifiedStoreError(t *testing.T) {
	typed := func(err error) (int, string, bool) {
		var refused *typedStoreRefusal
		if errors.As(err, &refused) {
			return http.StatusConflict, "conflict named by " + refused.name, true
		}
		return 0, "", false
	}
	refusals := []storeRefusal{refuseSentinel(store.ErrNotFound, http.StatusNotFound, "task not found"), typed}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		body   string
		logged bool
	}{
		{"sentinel", fmt.Errorf("%w: task secret-id", store.ErrNotFound), http.StatusNotFound, "task not found", false},
		{"typed", fmt.Errorf("wrapped: %w", &typedStoreRefusal{name: "branch"}), http.StatusConflict, "conflict named by branch", false},
		{"backend operation", fmt.Errorf("SingleStore %w", store.ErrBackendOperation), http.StatusInternalServerError, "internal server error", true},
		{"untranslated driver", errors.New(failingStoreDriverText), http.StatusInternalServerError, "internal server error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captured := captureStandardLog(t)
			response := httptest.NewRecorder()
			writeClassifiedStoreError(response, storeFailureRequest("req-1"), tc.err, refusals...)
			if response.Code != tc.status || strings.TrimSpace(response.Body.String()) != tc.body {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if got := strings.Contains(captured.String(), `request_id="req-1"`); got != tc.logged {
				t.Fatalf("logged=%v, want %v: %q", got, tc.logged, captured.String())
			}
		})
	}
}

// TestRouterRequestIDReachesStoreFailureLog proves the ID comes from the
// router's request-ID middleware on a real route.
func TestRouterRequestIDReachesStoreFailureLog(t *testing.T) {
	captured := captureStandardLog(t)
	handler := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeStoreFailure(w, r, errors.New("boom"))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/example", nil))
	if logged := captured.String(); strings.Contains(logged, `request_id=""`) || !strings.Contains(logged, "request_id=") {
		t.Fatalf("log has no request ID: %q", logged)
	}
}
