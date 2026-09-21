package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// VerificationStore is the VK-6 transaction boundary (req-verification-kits
// REQ-5, REQ-6 and REQ-7). The caller supplies authenticated access separately
// from submitted evidence. Each command commits its records, artifacts, audit
// event and publication queue intent together; a replay never repeats a write.
type VerificationStore interface {
	ReconcileVerificationClaims(context.Context) (int, error)
	ApplyVerification(context.Context, VerificationCommand) (VerificationReceipt, error)
	ReadVerification(context.Context, VerificationAccess, string) (VerificationSnapshot, error)
	ReadVerificationArtifact(context.Context, VerificationAccess, string, string) (core.Artifact, []byte, error)
}

var (
	ErrVerificationAccess   = errors.New("verification access refused")
	ErrVerificationConflict = errors.New("verification idempotency conflict")
	ErrVerificationState    = errors.New("verification transition refused")
	ErrVerificationInvalid  = errors.New("invalid verification record")
)

// Access is not a wire payload. Users require durable workspace membership;
// executions require the exact claim identity, token and current attempt.
type VerificationAccess struct {
	TaskID, WorkOrderID, WorkOrderAttemptID string
	Claim                                   core.WorkOrderClaimIdentity
	ClientToken                             string
	UserID                                  string
}

type VerificationContext struct {
	Discovery     []json.RawMessage
	RequestDigest string

	ID, WorkspaceID, TaskID, WorkOrderID, WorkOrderAttemptID string
	RequestKey, CreatedBy                                    string
	Revisions                                                []core.VerificationRevision
	GoverningPins                                            []core.VerificationPin
	CreatedAt                                                time.Time
	SealedAt                                                 *time.Time
}
type VerificationSelection struct {
	ID, ContextID string
	Receipt       verification.SelectionReceipt
	// Contracts are the selected, exact-revision subjects supplied by trusted
	// discovery. Selection freezes them before any attempt can start.
	Subjects []VerificationSubjectContract
}
type VerificationSubjectContract struct {
	Subject  core.VerificationSubject
	Contract verification.Exercise
}
type VerificationCitation struct {
	DocumentID string
	Version    int
	SectionID  string
}
type VerificationObligation struct {
	ObservationProcedure                          string
	ID, ContextID, Description, Digest, CreatedBy string
	Sources                                       []VerificationCitation
	Contract                                      verification.Exercise
	CreatedAt                                     time.Time
}
type VerificationReplayAuthorization struct {
	ID, ContextID, Actor, Reason string
	At                           time.Time
}

type VerificationAttempt struct {
	Recovery                                               []VerificationReplayAuthorization
	ReplayAuthorizationID                                  string
	ID, ContextID, StartKey, WorkOrderAttemptID, CreatedBy string
	Subject                                                core.VerificationSubject
	Ordinal                                                int
	State, Explanation                                     string
	EffectivePermissions                                   []verification.Permission
	SafeInputs                                             map[string]json.RawMessage
	Environment                                            core.VerificationEnvironment
	StartedAt                                              time.Time
	EndedAt                                                *time.Time
	ExitCode                                               *int
}
type VerificationOperationObservation struct {
	State, Actor, Source, ProviderReference string
	CapturedAt                              time.Time
}
type VerificationOperation struct {
	ID, ContextID, RunID, Key, SubjectKey, StepID, Target, InputDigest, RetryPolicy string
	CreatedBy                                                                       string
	CreatedAt                                                                       time.Time
	History                                                                         []VerificationOperationObservation
}
type VerificationArtifactPolicy struct {
	ArtifactID, MediaType, SanitationRecord, MaskingAttestation string
}

type VerificationEvidenceRecord struct {
	ArtifactPolicies []VerificationArtifactPolicy
	Envelope         core.VerificationEvidence
	Digest           string
}
type VerificationEvidenceLink struct{ From, To string }
type VerificationPublication struct {
	ID, ContextID, TaskID, State, BodyDigest, HeadSHA string
	Generation                                        int
	PullRequestNumber                                 int
	CreatedAt                                         time.Time
}
type VerificationUploadChunk struct {
	UploadID, ContextID, RunID string
	Index                      int
	Content                    []byte
	CreatedAt, ExpiresAt       time.Time
}
type VerificationArtifactInput struct {
	UploadID, Name, ContentType, SHA256 string
	SizeBytes                           int64
	// Binary sanitation remains a capture-time attestation, not a claim that
	// server text redaction detects sensitive pixels or speech (VK-6).
	SanitationRecord, MaskingAttestation string
}
type VerificationReceipt struct {
	State         string
	ID, Digest    string
	EvidenceIDs   []string
	ArtifactIDs   []string
	PublicationID string
}

type VerificationCommand struct {
	ValidateSuccess       func(VerificationSnapshot) error `json:"-"`
	Access                VerificationAccess
	Kind                  string
	ContextID, RunID, Key string
	Context               *VerificationContext
	Selection             *VerificationSelection
	Obligation            *VerificationObligation
	Attempt               *VerificationAttempt
	Operation             *VerificationOperation
	Observation           *VerificationOperationObservation
	Evidence              []json.RawMessage
	Links                 []VerificationEvidenceLink
	Chunk                 *VerificationUploadChunk
	Artifacts             []VerificationArtifactInput
	Publication           *VerificationPublication
	// Authority is derived by a trusted caller, never decoded from evidence.
	Authority core.VerificationEvidenceAuthority
}

const (
	VerificationReconcileClaimLoss = "attempt.reconcile_claim_loss"
	VerificationFinalizeArtifact   = "artifact.finalize"
	VerificationAuthorizeRetry     = "attempt.authorize_retry"
	VerificationCreateContext      = "context.create"
	VerificationRecordSelection    = "selection.record"
	VerificationRegisterObligation = "obligation.register"
	VerificationStartAttempt       = "attempt.start"
	VerificationTerminateAttempt   = "attempt.terminate"
	VerificationPrepareOperation   = "operation.prepare"
	VerificationObserveOperation   = "operation.observe"
	VerificationWriteEvidence      = "evidence.write"
	VerificationStageChunk         = "chunk.stage"
	VerificationExpireChunks       = "chunk.expire"
	VerificationCreatePublication  = "publication.create"
	VerificationSeal               = "context.seal"
)

type VerificationSnapshot struct {
	Contexts     []VerificationContext
	Selections   []VerificationSelection
	Obligations  []VerificationObligation
	Attempts     []VerificationAttempt
	Operations   []VerificationOperation
	Evidence     []VerificationEvidenceRecord
	Links        []VerificationEvidenceLink
	Publications []VerificationPublication
}
