package store

import "errors"

// ErrNotImplemented marks an explicit gap in an experimental backend (DEC-38).
var ErrNotImplemented = errors.New("store operation is not implemented")

// ErrBackendNotAdmitted prevents deploying a backend before full conformance.
var ErrBackendNotAdmitted = errors.New("store backend is not admitted for deployment")

// ErrRetryable marks a transaction rejected by lock contention. Retry the whole command.
var ErrRetryable = errors.New("store transaction may be retried")
