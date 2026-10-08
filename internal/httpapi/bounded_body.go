package httpapi

import (
	"errors"
	"io"
	"net/http"
)

// errBodyTooLarge reports a request body over its route's whole-body cap.
var errBodyTooLarge = errors.New("request body exceeds the size limit")

// readWholeBoundedBody reads the entire request body through one cap. A
// declared Content-Length over the cap is refused before any read, and an
// unknown-length or chunked body is refused as soon as it passes the cap, so
// bytes after a JSON value (trailing whitespace or data) count toward the
// limit. Callers decode the returned bytes with json.Unmarshal, which accepts
// trailing whitespace and refuses any other trailing data (component-http-api
// Request conventions; component-harness-execution heartbeat bound).
func readWholeBoundedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, errBodyTooLarge
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errBodyTooLarge
		}
		return nil, err
	}
	return body, nil
}
