package httpapi

import (
	"errors"
	"net/http"
)

// errWorkspaceNotVisible is the one workspace-visibility refusal: no binding,
// a missing capability, a run child's or worker's foreign workspace, or a
// missing membership authority. Its text is the long-standing MCP in-band
// error, so tool callers see no change; REST callers get the canonical 404
// that a nonexistent workspace gets (req-accounts-and-membership AC-4.2;
// DEC-19; component-http-api).
var errWorkspaceNotVisible = errors.New("workspace_not_found: workspace not found")

// invalidToolArgumentError marks a malformed or missing tool argument. Error
// and Unwrap pass the cause through unchanged so MCP text is preserved.
type invalidToolArgumentError struct{ err error }

func (e *invalidToolArgumentError) Error() string { return e.err.Error() }
func (e *invalidToolArgumentError) Unwrap() error { return e.err }

func invalidToolArgument(err error) error {
	if err == nil {
		return nil
	}
	return &invalidToolArgumentError{err: err}
}

// writeWorkOrderToolError answers the agent work-order routes
// (pull-request-template, submit-for-review, context-refresh). Only typed
// categories are classified, never error text: a workspace-visibility refusal
// answers the canonical 404 workspace_not_found, an invalid argument answers
// 400, and every other claim or lifecycle error keeps 409
// (req-accounts-and-membership AC-4.2; component-http-api).
func writeWorkOrderToolError(w http.ResponseWriter, err error) {
	if errors.Is(err, errWorkspaceNotVisible) {
		writeWorkspaceNotFound(w)
		return
	}
	var invalid *invalidToolArgumentError
	if errors.As(err, &invalid) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Error(w, err.Error(), http.StatusConflict)
}
