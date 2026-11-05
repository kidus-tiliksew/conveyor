package httpapi

import (
	"errors"
	"log"
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
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
