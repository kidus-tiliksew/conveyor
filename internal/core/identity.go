package core

import "errors"

// ErrInvalidCredential identifies authentication failures that are safe to
// report as unauthorized. Other verifier errors are infrastructure failures.
var ErrInvalidCredential = errors.New("invalid credential")
