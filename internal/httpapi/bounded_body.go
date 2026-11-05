package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// errBodyTooLarge reports a request body over its route's whole-body bound.
var errBodyTooLarge = errors.New("request body exceeds the size limit")

// decodeBoundedJSON decodes the whole request body through limit into v. A
// declared Content-Length over limit is refused before any read, and an
// unknown-length or chunked body is refused as soon as it passes limit, so
// bytes after a JSON value count toward the bound. Trailing whitespace is
// accepted and any other trailing data is refused. An oversized body returns
// errBodyTooLarge. The route answers the result with writeBodyDecodeError, or
// in its own failure shape (DEC-105(3); component-http-api invariants 15, 17).
func decodeBoundedJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	if r.ContentLength > limit {
		return errBodyTooLarge
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return fmt.Errorf("read request body: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// writeBodyTooLarge answers a body over its route's bound with 413.
func writeBodyTooLarge(w http.ResponseWriter) {
	http.Error(w, errBodyTooLarge.Error(), http.StatusRequestEntityTooLarge)
}

// writeBodyDecodeError answers a decodeBoundedJSON failure in the plain-text
// failure shape: 413 for an oversized body and 400 naming the problem for any
// other refusal.
func writeBodyDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyTooLarge) {
		writeBodyTooLarge(w)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}
