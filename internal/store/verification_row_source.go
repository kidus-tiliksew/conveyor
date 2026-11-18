package store

import "context"

// VerificationTableSet names the verification tables one command decodes, so a
// backend reads only those tables for that command: a read's bytes grow with
// the rows the command uses and never with rows it never looks at
// (component-persistence, Metrics and database read accounting;
// component-verification-service).
type VerificationTableSet []string

// index resolves the set once for a row loop.
func (s VerificationTableSet) index() map[string]bool {
	out := make(map[string]bool, len(s))
	for _, table := range s {
		out[table] = true
	}
	return out
}

// VerificationSnapshotTables is every table VerificationSnapshotFromRows
// decodes. Upload chunks are staging rows no snapshot exposes.
var VerificationSnapshotTables = VerificationTableSet{"verification_contexts", "verification_selections", "verification_obligations", "verification_attempts", "verification_operations", "verification_evidence", "verification_evidence_links", "verification_publications", "verification_permission_grants", "verification_permission_revocations"}

// VerificationClaimTables is what VerificationReconciliationCommands reads: the
// task's live attempts and the contexts that bind them.
var VerificationClaimTables = VerificationTableSet{"verification_contexts", "verification_attempts"}

// VerificationArtifactTables is what VerificationArtifactAccess and
// VerificationArtifactMedia decode: the evidence row, and the context that
// authorizes reading it.
var VerificationArtifactTables = VerificationTableSet{"verification_contexts", "verification_evidence"}

// VerificationReplayTables is what VerifyVerificationCheckpointReplay decodes
// to recognize a retained checkpoint claim.
var VerificationReplayTables = VerificationTableSet{"verification_contexts", "verification_attempts", "verification_operations"}

// VerificationDeliveryTables is what PrepareVerificationDelivery decodes when a
// publication or an evidence write records its delivery intent.
var VerificationDeliveryTables = VerificationTableSet{"verification_contexts", "verification_attempts", "verification_operations", "verification_evidence", "verification_publications"}

// VerificationRepinTables is what VerifyClaimRepinsAuthority decodes.
var VerificationRepinTables = VerificationTableSet{"verification_contexts", "verification_attempts", "verification_operations"}

// VerificationRecoveryTables is what PrepareVerificationRecovery decodes.
var VerificationRecoveryTables = VerificationTableSet{"verification_contexts", "verification_selections", "verification_obligations", "verification_attempts", "verification_operations"}

// verificationOperationTables is the set every operation and attempt command
// decodes: the binding, the writable run, its live grant, and the contract.
var verificationOperationTables = VerificationTableSet{"verification_contexts", "verification_selections", "verification_obligations", "verification_attempts", "verification_operations", "verification_permission_grants", "verification_permission_revocations"}

// VerificationCommandTables names the tables a command's mutation preparation
// decodes, so its read carries no row it never looks at. Each set is covered by
// that command's conformance cases.
func VerificationCommandTables(c VerificationCommand) VerificationTableSet {
	switch c.Kind {
	case VerificationCreateContext, VerificationRecordSelection:
		return VerificationTableSet{"verification_contexts", "verification_selections"}
	case VerificationRegisterObligation:
		return VerificationTableSet{"verification_contexts", "verification_obligations"}
	case VerificationCreatePublication:
		// Creating a publication records its delivery intent, which decodes the
		// context, its run, the operations, the evidence and the publications.
		return VerificationDeliveryTables
	case VerificationStageChunk:
		return VerificationTableSet{"verification_contexts", "verification_selections", "verification_obligations", "verification_attempts", "verification_evidence", "verification_upload_chunks", "verification_permission_grants", "verification_permission_revocations"}
	case VerificationExpireChunks:
		return VerificationTableSet{"verification_contexts", "verification_upload_chunks"}
	case VerificationStartAttempt:
		// A start validates coverage against the whole context snapshot.
		return VerificationSnapshotTables
	case VerificationWriteEvidence, VerificationFinalizeArtifact:
		return VerificationTableSet{"verification_contexts", "verification_selections", "verification_obligations", "verification_attempts", "verification_operations", "verification_evidence", "verification_evidence_links", "verification_publications", "verification_upload_chunks", "verification_permission_grants", "verification_permission_revocations"}
	case VerificationTerminateAttempt:
		// Success evaluation reads the attempt's evidence through the context
		// snapshot.
		return VerificationSnapshotTables
	case VerificationAuthorizeRetry, VerificationPrepareOperation, VerificationObserveOperation, VerificationReconcileOperation, VerificationGrantPermissions, VerificationRevokePermissions:
		return verificationOperationTables
	case VerificationReconcileClaimLoss:
		return VerificationTableSet{"verification_contexts", "verification_attempts", "verification_operations"}
	}
	// The seal reads the whole context to judge coverage, and an unknown kind is
	// refused after the read, so both take every table.
	return VerificationTableSet(VerificationTables)
}

// VerificationDeliveryReconcileWindow bounds one reconciler pass's due
// deliveries, so a pass does a bounded amount of work; a row an unfinished
// job serves takes no place (component-verification-publication invariant
// 19).
const VerificationDeliveryReconcileWindow = 100

// VerificationOperationScope reads the operation rows a registration must
// consider outside the task's own rows, inside the command's transaction. It
// replaces the workspace-wide operation read a store no longer performs
// (component-verification-service; component-persistence, Metrics and database
// read accounting).
type VerificationOperationScope interface {
	// VerificationOperationByKey returns at most the operation that owns a
	// logical key anywhere in the workspace.
	VerificationOperationByKey(ctx context.Context, key string) (VerificationRow, bool, error)
	// UnresolvedVerificationOperations returns the operations another task
	// holds that are unresolved for one subject, step, and target: changing the
	// source cannot hide an unresolved external action.
	UnresolvedVerificationOperations(ctx context.Context, subjectKey, stepID, target string) ([]VerificationRow, error)
}
