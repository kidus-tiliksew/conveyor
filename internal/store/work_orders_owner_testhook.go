package store

import "context"

// Worker owner admission test-hook stages. Each backend calls the context hook
// inside a worker claim's transaction or mutex: after it has read the worker
// row and before it reads the owner's user and binding rows, and after it has
// locked those rows and before the claim commits. Shared and native tests hold
// one side of the deactivation race there with channels; no production caller
// sets a hook (component-work-orders, Worker owner admission).
const (
	WorkerOwnerHookClaimOwnerRead   = "worker_claim_owner_read"
	WorkerOwnerHookClaimOwnerLocked = "worker_claim_owner_locked"
)

type workerOwnerTestHookKey struct{}

// WithWorkerOwnerTestHook attaches a test-only hook that worker claims call at
// the named stages. A non-nil error aborts the claim.
func WithWorkerOwnerTestHook(ctx context.Context, hook func(stage string) error) context.Context {
	return context.WithValue(ctx, workerOwnerTestHookKey{}, hook)
}

// RunWorkerOwnerTestHook calls the context's worker owner hook for stage, if
// any, and returns its error.
func RunWorkerOwnerTestHook(ctx context.Context, stage string) error {
	hook, _ := ctx.Value(workerOwnerTestHookKey{}).(func(string) error)
	if hook == nil {
		return nil
	}
	return hook(stage)
}
