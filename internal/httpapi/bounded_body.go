package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// taskRouteBodyLimit bounds the JSON body of a task, gate, and work-order route.
// taskCreateBodyLimit bounds JSON task intake, whose body field is the largest
// text those routes carry; it matches the MCP request bound, so one task body
// fits either intake (DEC-105(3); component-http-api invariant 17).
const (
	taskRouteBodyLimit  = 1 << 20
	taskCreateBodyLimit = 8 << 20
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
	body, err := readBoundedBody(w, r, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// decodeOptionalBoundedJSON is decodeBoundedJSON for a route whose body may be
// empty, which leaves v unchanged.
func decodeOptionalBoundedJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	body, err := readBoundedBody(w, r, limit)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// decodeStrictBoundedJSON is decodeBoundedJSON for a route that refuses unknown
// fields.
func decodeStrictBoundedJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	body, err := readBoundedBody(w, r, limit)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid JSON body: expected one JSON value")
	}
	return nil
}

// readBoundedBody reads the whole request body through limit. A declared
// Content-Length over limit is refused before any read, and an unknown-length
// or chunked body is refused as soon as it passes limit. An oversized body
// returns errBodyTooLarge.
func readBoundedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, errBodyTooLarge
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errBodyTooLarge
		}
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return body, nil
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
