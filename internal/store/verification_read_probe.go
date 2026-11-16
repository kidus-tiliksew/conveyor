package store

import (
	"context"
	"sync/atomic"
)

// VerificationReadOperation names one verification read a conformance case can
// measure. Each name is one statement or one row-source read, so a case asserts
// that a read's volume grows with the rows a command uses and never with rows
// it never looks at (component-persistence, Database read accounting;
// component-verification-service).
const (
	// VerificationReadRows is the verification rows a command, a snapshot read,
	// or a reconciliation sweep decoded.
	VerificationReadRows = "rows"
	// VerificationReadWorkspace is a workspace-wide operation lookup a
	// registration needs: at most one row by logical key, or another task's
	// unresolved operations for one subject.
	VerificationReadWorkspace = "workspace"
	// VerificationReadMetadata is the batched publication-metadata read.
	VerificationReadMetadata = "metadata"
	// VerificationReadDelivery is the delivery-generation read one delivery run
	// performs.
	VerificationReadDelivery = "delivery"
	// VerificationReadDue is the delivery reconciler's due window, read without
	// any retained body.
	VerificationReadDue = "due"
	// VerificationReadBodies is the delivery reconciler's body read, bounded to
	// the rows that pass acts on.
	VerificationReadBodies = "bodies"
)

// VerificationReadSample is one measured verification read: the operation, the
// workspace and task it was scoped to, and the rows it returned.
type VerificationReadSample struct {
	Operation string
	Workspace string
	TaskID    string
	Rows      int
}

var verificationReadProbe atomic.Pointer[func(VerificationReadSample)]

// ProbeVerificationReadsForTest installs observe as the process's verification
// read probe and returns the function that removes it. Production never
// installs one, and a store reports nothing when it is absent.
func ProbeVerificationReadsForTest(observe func(VerificationReadSample)) func() {
	verificationReadProbe.Store(&observe)
	return func() { verificationReadProbe.Store(nil) }
}

// NoteVerificationRead reports one measured verification read to the installed
// probe. A store calls it where it issues a verification statement.
func NoteVerificationRead(operation, workspace, taskID string, rows int) {
	if observe := verificationReadProbe.Load(); observe != nil {
		(*observe)(VerificationReadSample{Operation: operation, Workspace: workspace, TaskID: taskID, Rows: rows})
	}
}

// noteVerificationReadTask reports one read scoped to a task.
func noteVerificationReadTask(ctx context.Context, operation, taskID string, rows int) {
	ws, _ := WorkspaceFromContext(ctx)
	NoteVerificationRead(operation, ws, taskID, rows)
}

// verificationFoundRows is the row count of an at-most-one-row lookup.
func verificationFoundRows(found bool) int {
	if found {
		return 1
	}
	return 0
}
