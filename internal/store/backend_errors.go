package store

import "errors"

// ErrRetryable marks a transaction rejected by lock contention. Retry the whole command.
var ErrRetryable = errors.New("store transaction may be retried")
