package store

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// ErrRetryable marks a transaction rejected by lock contention. Retry the whole command.
var ErrRetryable = errors.New("store transaction may be retried")

// ErrBackendOperation marks a database failure that has no store meaning.
// Durable backends wrap it in place of every unmapped driver error, so no
// driver type, code, or message crosses the store boundary (DEC-38;
// component-persistence).
var ErrBackendOperation = errors.New("database operation failed")

// ErrRefusal marks a refusal of a request's input or of the state it addresses,
// or a forge failure that names its own remedy, when no narrower sentinel names
// it. A route answers it with its 4xx and the error's text. An error without a
// sentinel, a typed error, or this mark is a store failure, so it answers the
// generic 500 (DEC-105(1), (2); component-http-api invariants 15, 16).
var ErrRefusal = errors.New("request refused")

type refusalError struct{ err error }

func (e *refusalError) Error() string        { return e.err.Error() }
func (e *refusalError) Unwrap() error        { return e.err }
func (e *refusalError) Is(target error) bool { return target == ErrRefusal }

// Refuse marks err as a refusal and keeps its text and its errors.Is and
// errors.As chain. A nil err stays nil.
func Refuse(err error) error {
	if err == nil {
		return nil
	}
	return &refusalError{err: err}
}

// Refusef is Refuse of fmt.Errorf(format, args...).
func Refusef(format string, args ...any) error {
	return Refuse(fmt.Errorf(format, args...))
}

// DriverLogMessageLimit bounds the driver message a durable backend writes to
// the server log.
const DriverLogMessageLimit = 1024

// DriverLogMessage prepares a driver message for a durable backend's one
// server log line (component-persistence; req-security-boundaries REQ-2). It
// redacts first and cuts second, so a cut can never split a secret past the
// redactor's matchers. redact.New includes every secret registered with
// redact.RegisterSecret when it runs. It takes no driver type, so each backend
// keeps its own driver (DEC-38).
func DriverLogMessage(message string) string {
	clean, _ := redact.New(nil).Redact(message)
	return truncateUTF8(clean, DriverLogMessageLimit)
}

// truncateUTF8 cuts text to at most limit bytes at a rune boundary.
func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// BackendOperation names the nearest exported Store method or exported package
// function of backendPackage on the caller's stack, such as
// ClaimWorkOrderCommand or Open. Closures report their enclosing function. A
// stack without one, such as a background goroutine's, reports "unknown".
func BackendOperation(backendPackage string) string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if name, ok := ExportedBackendOperation(backendPackage, frame.Function); ok {
			return name
		}
		if !more {
			return "unknown"
		}
	}
}

// ExportedBackendOperation reports the exported operation a runtime function
// name belongs to, when it is declared in backendPackage.
func ExportedBackendOperation(backendPackage, function string) (string, bool) {
	rest, ok := strings.CutPrefix(function, backendPackage+".")
	if !ok {
		return "", false
	}
	rest = strings.TrimPrefix(rest, "(*Store).")
	rest = strings.TrimPrefix(rest, "Store.")
	name, _, _ := strings.Cut(rest, ".")
	first, _ := utf8.DecodeRuneInString(name)
	if name == "" || !unicode.IsUpper(first) {
		return "", false
	}
	return name, true
}
