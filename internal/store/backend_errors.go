package store

import "errors"

// ErrRetryable marks a transaction rejected by lock contention. Retry the whole command.
var ErrRetryable = errors.New("store transaction may be retried")

// ErrBackendOperation marks a database failure that has no store meaning.
// Durable backends wrap it in place of every unmapped driver error, so no
// driver type, code, or message crosses the store boundary (DEC-38;
// component-persistence).
var ErrBackendOperation = errors.New("database operation failed")
