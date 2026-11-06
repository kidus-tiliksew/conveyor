package httpapi

import (
	"errors"
	"log"
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// errorLogLimit bounds the error text one log line carries.
const errorLogLimit = 2048

// redactedErrorLogText is an error's text as a log line may carry it:
// credentials redacted and cut at errorLogLimit on a rune boundary
// (req-security-boundaries REQ-2).
func redactedErrorLogText(err error) string {
	logged, _ := redact.New(nil).Redact(err.Error())
	if len(logged) > errorLogLimit {
		cut := errorLogLimit
		for cut > 0 && !utf8.RuneStart(logged[cut]) {
			cut--
		}
		logged = logged[:cut]
	}
	return logged
}

// writeStoreFailure answers a store failure: it logs the cause, redacted, with
// the request ID, and answers 500 with a generic message that carries no error
// text. A failure of GitHub or a model provider is not a store failure and
// keeps its own answer, so a forge failure still names its remedy (DEC-105(1);
// component-http-api invariant 16; req-delivery-and-forge AC-1.4, AC-1.8).
func writeStoreFailure(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("httpapi: store failure: request_id=%q method=%s path=%q error=%q", middleware.GetReqID(r.Context()), r.Method, r.URL.Path, redactedErrorLogText(err))
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// storeRefusal classifies one sentinel or typed store error as the 4xx it
// answers. refused is false for every other error.
type storeRefusal func(err error) (status int, message string, refused bool)

// refuseSentinel answers status with message when err wraps target. The
// message is fixed, so no store text reaches the response.
func refuseSentinel(target error, status int, message string) storeRefusal {
	return func(err error) (int, string, bool) {
		return status, message, errors.Is(err, target)
	}
}

// writeClassifiedStoreError answers err through the first refusal that matches
// it. An error no refusal names is a store failure, an untranslated driver
// error included, so it never reads as a 4xx (DEC-105(1), (2)).
func writeClassifiedStoreError(w http.ResponseWriter, r *http.Request, err error, refusals ...storeRefusal) {
	for _, refusal := range refusals {
		if status, message, refused := refusal(err); refused {
			http.Error(w, message, status)
			return
		}
	}
	writeStoreFailure(w, r, err)
}

// writeTaskReadFailure answers a failed read of the task a route addresses: a
// missing task is 404, and anything else is a store failure, so an outage never
// reads as not found (DEC-105(1)).
func writeTaskReadFailure(w http.ResponseWriter, r *http.Request, err error) {
	writeClassifiedStoreError(w, r, err, refuseSentinel(store.ErrNotFound, http.StatusNotFound, "task not found"))
}

// isOperationRefusal reports whether err from a task, gate, or work-order
// operation is a refusal or a forge failure rather than a store failure. It
// names sentinels and typed errors only, never error text (DEC-105(1), (2);
// component-http-api invariants 15, 16).
func isOperationRefusal(err error) bool {
	if err == nil || errors.Is(err, store.ErrBackendOperation) {
		return false
	}
	if dispatch.MergeRouteRefusal(err) {
		return true
	}
	var (
		remedy     *store.VerificationRemedyError
		dependency *store.TaskDependencyError
		branch     *store.BranchInUseError
		invalid    *core.ErrInvalidTransition
	)
	if errors.As(err, &remedy) || errors.As(err, &dependency) || errors.As(err, &branch) || errors.As(err, &invalid) {
		return true
	}
	for _, refusal := range []error{
		store.ErrWorkspaceRequired, store.ErrWorkOrderStale, store.ErrWorkOrderTimedOut, store.ErrWorkOrderCancelled,
		store.ErrWorkOrderPreempted, store.ErrWorkOrderPreemptConflict, store.ErrWorkOrderClaimLost,
		store.ErrWorkOrderReleasedAtCheckpoint, store.ErrWorkOrderClaimUnauthorized, store.ErrReviewRetryConflict,
		store.ErrSetupChangeConflict, store.ErrTaskHeldByWorkOrder, store.ErrTaskBranchConflict,
		store.ErrPlanRevisionGateUnreadable, store.ErrReviewRoundSeatsInvalid, store.ErrReviewHeadStale,
		store.ErrDocumentationGateUnresolved, store.ErrDocumentationUnresolved, store.ErrReviewSeatModelNotDistinct,
		store.ErrCheckStatusHeadRequired, store.ErrVerificationAccess, store.ErrVerificationConflict,
		store.ErrVerificationState, store.ErrVerificationInvalid,
	} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

// writeOperationError answers an operation's refusal with status and the
// refusal's text, and any other error as a store failure.
func writeOperationError(w http.ResponseWriter, r *http.Request, err error, status int) {
	if isOperationRefusal(err) {
		http.Error(w, err.Error(), status)
		return
	}
	writeStoreFailure(w, r, err)
}
