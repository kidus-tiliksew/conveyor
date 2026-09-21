package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// VerificationRow is the backend-neutral relational binding. Body is the typed
// immutable contract; indexed identity and lifecycle columns are also stored
// outside JSON. Backend adapters never accept a table name from a caller.
type VerificationRow struct {
	Table, ID, TaskID, ContextID, RunID, LogicalKey, State string
	Body                                                   json.RawMessage
	ExpiresAt                                              *time.Time
}

var VerificationTables = []string{"verification_contexts", "verification_selections", "verification_obligations", "verification_attempts", "verification_operations", "verification_evidence", "verification_evidence_links", "verification_publications", "verification_upload_chunks"}

type VerificationMutation struct {
	ArtifactReferences []core.VerificationArtifactReference
	Rows               []VerificationRow
	DeleteChunks       []string
	Artifacts          []VerificationRetainedArtifact
	Receipt            VerificationReceipt
	Event              core.Event
	Publication        *VerificationPublication
}
type VerificationRetainedArtifact struct {
	Artifact core.Artifact
	Content  []byte
}

type verificationFaultKey struct{}

// WithVerificationFault supplies deterministic transaction failures to backend
// conformance fixtures. It is an in-process test hook, never a request field.
func WithVerificationFault(ctx context.Context, fn func(string) error) context.Context {
	return context.WithValue(ctx, verificationFaultKey{}, fn)
}
func VerificationFault(ctx context.Context, step string) error {
	if fn, ok := ctx.Value(verificationFaultKey{}).(func(string) error); ok {
		return fn(step)
	}
	return nil
}

func verificationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func verificationHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func verificationJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.UseNumber()
	if err = decoder.Decode(&value); err != nil {
		panic(err)
	}
	b, err = json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}

// VerificationEvidenceDigest hashes canonical sanitized JSON, independent of
// relational JSON formatting or object-key order after restart (VK-6).
func VerificationEvidenceDigest(e core.VerificationEvidence) string {
	return verificationHash(verificationJSON(e))
}
func verificationEqual(a, b any) bool {
	return reflect.DeepEqual(verificationJSON(a), verificationJSON(b))
}
func verificationRow(table, id, task, contextID, run, key, state string, v any) VerificationRow {
	return VerificationRow{Table: table, ID: id, TaskID: task, ContextID: contextID, RunID: run, LogicalKey: key, State: state, Body: verificationJSON(v)}
}
func verificationFind(rows []VerificationRow, table, id string) (VerificationRow, bool) {
	for _, r := range rows {
		if r.Table == table && r.ID == id {
			return r, true
		}
	}
	return VerificationRow{}, false
}
func verificationDecode[T any](r VerificationRow) T {
	var v T
	_ = json.Unmarshal(r.Body, &v)
	return v
}

// VerifyVerificationClaim runs after the adapter locks both the task command
// boundary and the work-order row. All foreign and stale scopes return the
// same sentinel, without a record identity in the error (AC-7.2).
func VerifyVerificationClaim(ctx context.Context, a VerificationAccess, task core.Task, o core.WorkOrder, write bool, now time.Time) error {
	ws, ok := WorkspaceFromContext(ctx)
	actor := ActorFromContext(ctx)
	if !ok || actor.ID == "conveyor" || a.TaskID == "" || task.ID != a.TaskID || task.Workspace != ws || o.TaskID != a.TaskID || o.ID != a.WorkOrderID || o.AttemptID != a.WorkOrderAttemptID || a.Claim.SessionID == "" || a.Claim.SessionID != o.SessionID || a.Claim.WorkerID != o.WorkerID || a.Claim.ClaimantID != o.ClaimantID || a.ClientToken == "" || verificationHash([]byte(a.ClientToken)) != o.ClientTokenHash || o.State != core.WorkOrderClaimed || !o.LeaseExpiresAt.After(now) || (!o.ExecutionDeadline.IsZero() && !o.ExecutionDeadline.After(now)) {
		return ErrVerificationAccess
	}

	switch actor.Role {
	case core.ActorWorker:
		if o.WorkerID == "" || actor.ID != WorkerActorID(o.WorkerID) {
			return ErrVerificationAccess
		}
	case core.ActorUser:
		if o.ClaimantID != core.TaskRunClaimantID(strings.TrimPrefix(actor.ID, "user:")) {
			return ErrVerificationAccess
		}
	case core.ActorAgent:
		credential, ok := CredentialFromContext(ctx)
		if !ok || actor.ID != AgentActorID(credential.ID) || credential.RunWorkspaceID != ws || credential.RunWorkOrderID != o.ID || credential.RunSessionID != o.SessionID {
			return ErrVerificationAccess
		}
	case core.ActorSystem:
		if actor.ID != "verification-runner" {
			return ErrVerificationAccess
		}
	default:
		return ErrVerificationAccess
	}
	if write && o.Stage != core.StageVerify {
		return ErrVerificationAccess
	}
	if !write && o.Stage != core.StageVerify && o.Stage != core.StageReview {
		return ErrVerificationAccess
	}
	return nil
}

func VerificationSnapshotFromRows(rows []VerificationRow, contextID string) (VerificationSnapshot, error) {
	var s VerificationSnapshot
	for _, r := range rows {
		if r.ContextID != contextID {
			continue
		}
		switch r.Table {
		case "verification_contexts":
			s.Contexts = append(s.Contexts, verificationDecode[VerificationContext](r))
		case "verification_selections":
			s.Selections = append(s.Selections, verificationDecode[VerificationSelection](r))
		case "verification_obligations":
			s.Obligations = append(s.Obligations, verificationDecode[VerificationObligation](r))
		case "verification_attempts":
			s.Attempts = append(s.Attempts, verificationDecode[VerificationAttempt](r))
		case "verification_operations":
			s.Operations = append(s.Operations, verificationDecode[VerificationOperation](r))
		case "verification_evidence":
			if r.State == "evidence" {
				s.Evidence = append(s.Evidence, verificationDecode[VerificationEvidenceRecord](r))
			}
		case "verification_evidence_links":
			s.Links = append(s.Links, verificationDecode[VerificationEvidenceLink](r))
		case "verification_publications":
			s.Publications = append(s.Publications, verificationDecode[VerificationPublication](r))
		}
	}
	if len(s.Contexts) != 1 {
		return VerificationSnapshot{}, ErrVerificationAccess
	}
	return s, nil
}

// PrepareVerificationMutation has no persistence side effects. It redacts
// before validating or hashing and produces a complete transaction write set.
// Adapters discard the write set on every failed check or write.
func PrepareVerificationMutation(ctx context.Context, source redact.SecretSource, c VerificationCommand, rows []VerificationRow, now time.Time) (VerificationMutation, error) {
	var out VerificationMutation
	if encoded, err := json.Marshal(c); err != nil || len(encoded) > 8<<20 {
		return out, ErrVerificationInvalid
	}
	if clock, ok := ctx.Value(verificationClockKey{}).(func() time.Time); ok {
		now = clock().UTC()
	}
	ws, ok := WorkspaceFromContext(ctx)
	if !ok {
		return out, ErrVerificationAccess
	}
	actor := ActorFromContext(ctx).ID
	if strings.ContainsRune(actor, 0) {
		return out, ErrVerificationInvalid
	}
	for _, id := range []string{c.Key, c.ContextID, c.RunID} {
		if id != "" && !verificationIdentifier(id) {
			return out, ErrVerificationInvalid
		}
	}
	redactor, err := redact.WithSecrets(ctx, source, nil)
	if err != nil {
		return out, fmt.Errorf("verification redaction unavailable: %w", err)
	}
	// Keep identity/claim credentials out of redaction input. Submitted contract
	// fields are sanitized individually below; credentials are never persisted.
	clean := func(v any) ([]byte, error) { return verificationSanitizeJSON(redactor, verificationJSON(v)) }
	put := func(r VerificationRow) { out.Rows = append(out.Rows, r) }
	if c.Kind == VerificationReconcileClaimLoss {
		return prepareVerificationClaimLoss(ctx, c, rows, now)
	}
	if c.Kind == VerificationExpireChunks {
		for _, row := range rows {
			if row.Table == "verification_upload_chunks" && row.TaskID == c.Access.TaskID && row.ExpiresAt != nil && !row.ExpiresAt.After(now) {
				out.DeleteChunks = append(out.DeleteChunks, row.ID)
			}
		}
	} else if c.Kind == VerificationCreateContext {
		if c.Context == nil || c.Key == "" {
			return out, ErrVerificationInvalid
		}
		v := *c.Context
		v.ID = ""
		v.RequestKey = c.Key
		v.WorkspaceID = ws
		v.TaskID = c.Access.TaskID
		v.WorkOrderID = c.Access.WorkOrderID
		v.WorkOrderAttemptID = c.Access.WorkOrderAttemptID
		v.CreatedAt = time.Time{}
		v.CreatedBy = actor
		v.SealedAt = nil
		if len(v.Revisions) == 0 || v.GoverningPins == nil {
			return out, ErrVerificationInvalid
		}
		for _, rev := range v.Revisions {
			if rev.Repository == "" || rev.RemoteIdentity == "" || (len(rev.SHA) != 40 && len(rev.SHA) != 64) {
				return out, ErrVerificationInvalid
			}
			if _, err := hex.DecodeString(rev.SHA); err != nil {
				return out, ErrVerificationInvalid
			}
		}
		for _, r := range rows {
			if r.Table == "verification_contexts" && r.LogicalKey == c.Access.WorkOrderID+":"+c.Key {
				old := verificationDecode[VerificationContext](r)
				original := old
				old.ID = ""
				old.CreatedAt = time.Time{}
				old.SealedAt = nil
				if !verificationEqual(old, v) {
					return out, ErrVerificationConflict
				}
				out.Receipt.ID = original.ID
				return out, nil
			}
		}
		v.ID = verificationID()
		v.CreatedAt = now
		put(verificationRow("verification_contexts", v.ID, c.Access.TaskID, v.ID, "", c.Access.WorkOrderID+":"+c.Key, "open", v))
		out.Receipt.ID = v.ID
		if c.Selection != nil {
			selectionCommand := c
			selectionCommand.Kind, selectionCommand.ContextID = VerificationRecordSelection, v.ID
			selected, err := PrepareVerificationMutation(ctx, source, selectionCommand, append(append([]VerificationRow{}, rows...), out.Rows...), now)
			if err != nil {
				return VerificationMutation{}, err
			}
			out.Rows = append(out.Rows, selected.Rows...)
		}

	} else {
		r, ok := verificationFind(rows, "verification_contexts", c.ContextID)
		if !ok || r.TaskID != c.Access.TaskID {
			return out, ErrVerificationAccess
		}
		vc := verificationDecode[VerificationContext](r)
		if vc.WorkOrderID != c.Access.WorkOrderID || vc.WorkOrderAttemptID != c.Access.WorkOrderAttemptID {
			return out, ErrVerificationAccess
		}
		if vc.SealedAt != nil {
			return out, ErrVerificationState
		}
		switch c.Kind {
		case VerificationRecordSelection:
			if c.Selection == nil {
				return out, ErrVerificationInvalid
			}
			v := *c.Selection
			v.ID = c.ContextID
			v.ContextID = c.ContextID
			if v.Receipt.SchemaVersion != 1 || v.Receipt.Stage != "verify" || !verificationEqual(v.Receipt.ContextPins, vc.GoverningPins) {
				return out, ErrVerificationInvalid
			}
			for _, subject := range v.Subjects {
				if subject.Subject.Kind != "kit" || subject.Subject.ExerciseID != subject.Contract.ID {
					return out, ErrVerificationInvalid
				}
				selected := false
				for _, k := range v.Receipt.Kits {
					if k.KitID == subject.Subject.KitID && k.Digest == subject.Subject.ContentDigest && k.Eligibility == "eligible" {
						selected = true
					}
				}
				if !selected {
					return out, ErrVerificationInvalid
				}
			}
			nr := verificationRow("verification_selections", v.ID, c.Access.TaskID, c.ContextID, "", c.ContextID, "", v)
			if old, found := verificationFind(rows, nr.Table, nr.ID); found {
				if !verificationEqual(old.Body, nr.Body) {
					return out, ErrVerificationConflict
				}
				out.Receipt.ID = old.ID
				return out, nil
			}
			put(nr)
			out.Receipt.ID = v.ID
		case VerificationRegisterObligation:
			if c.Obligation == nil {
				return out, ErrVerificationInvalid
			}
			v := *c.Obligation
			v.ContextID = c.ContextID
			v.CreatedBy = actor
			v.CreatedAt = time.Time{}
			v.Digest = ""
			if !verificationIdentifier(v.ID) || v.Description == "" || len(v.Sources) == 0 || v.Contract.TimeoutSeconds <= 0 || v.Contract.RequiredAssertions == nil {
				return out, ErrVerificationInvalid
			}
			for _, citation := range v.Sources {
				found := citation.DocumentID == "approved_plan"
				for _, pin := range vc.GoverningPins {
					if pin.DocumentID == citation.DocumentID && pin.Version == citation.Version && citation.SectionID != "" {
						found = true
					}
				}
				if !found {
					return out, ErrVerificationInvalid
				}
			}
			if err := validateVerificationObligation(v); err != nil {
				return out, err
			}
			if v.Contract.RetryPolicy == "" {
				v.Contract.RetryPolicy = "operator_action_required"
			}
			b, err := clean(v)
			if err != nil {
				return out, err
			}
			if err = json.Unmarshal(b, &v); err != nil {
				return out, err
			}
			v.Digest = verificationHash(verificationJSON(v))
			id := c.ContextID + ":" + v.ID
			if old, found := verificationFind(rows, "verification_obligations", id); found {
				prior := verificationDecode[VerificationObligation](old)
				if prior.Digest != v.Digest {
					return out, ErrVerificationConflict
				}
				out.Receipt = VerificationReceipt{ID: prior.ID, Digest: prior.Digest}
				return out, nil
			}
			v.CreatedAt = now
			put(verificationRow("verification_obligations", id, c.Access.TaskID, c.ContextID, "", id, "", v))
			out.Receipt = VerificationReceipt{ID: v.ID, Digest: v.Digest}
		case VerificationStartAttempt, VerificationTerminateAttempt, VerificationAuthorizeRetry:
			if err := verificationAttemptMutation(c, rows, vc, actor, now, redactor, &out); err != nil {
				return out, err
			}
		case VerificationPrepareOperation, VerificationObserveOperation:
			if err := verificationOperationMutation(c, rows, actor, now, redactor, &out); err != nil {
				return out, err
			}
		case VerificationStageChunk:
			if c.Chunk == nil || !verificationIdentifier(c.Chunk.UploadID) || c.Chunk.Index < 0 || c.Chunk.Index >= 50 || len(c.Chunk.Content) == 0 || len(c.Chunk.Content) > 512<<10 {
				return out, ErrVerificationInvalid
			}
			if _, err := verificationWritableRun(c, rows); err != nil {
				return out, err
			}
			for _, saved := range rows {
				if saved.Table == "verification_evidence" && saved.State == "finalized" {
					for _, upload := range verificationDecode[verificationBatch](saved).Uploads {
						if upload.Input.UploadID == c.Chunk.UploadID {
							return out, ErrVerificationConflict
						}
					}
				}
			}
			v := *c.Chunk
			v.ContextID = c.ContextID
			v.RunID = c.RunID
			v.CreatedAt = now
			v.ExpiresAt = now.Add(24 * time.Hour)
			for _, row := range rows {
				if row.Table == "verification_upload_chunks" && row.LogicalKey == v.UploadID {
					if row.ContextID != c.ContextID || row.RunID != c.RunID {
						return out, ErrVerificationAccess
					}
					chunk := verificationDecode[VerificationUploadChunk](row)
					if !chunk.ExpiresAt.After(now) {
						return out, ErrVerificationState
					}
					v.CreatedAt = chunk.CreatedAt
					v.ExpiresAt = chunk.ExpiresAt
				}
			}
			id := v.UploadID + fmt.Sprintf(":%d", v.Index)
			if old, found := verificationFind(rows, "verification_upload_chunks", id); found {
				if !verificationEqual(verificationDecode[VerificationUploadChunk](old).Content, v.Content) {
					return out, ErrVerificationConflict
				}
				out.Receipt.ID = v.UploadID
				return out, nil
			}
			nr := verificationRow("verification_upload_chunks", id, c.Access.TaskID, c.ContextID, c.RunID, v.UploadID, "staging", v)
			nr.ExpiresAt = &v.ExpiresAt
			put(nr)
			out.Receipt.ID = v.UploadID
		case VerificationWriteEvidence, VerificationFinalizeArtifact:
			if err := verificationEvidenceMutation(c, rows, vc, actor, now, redactor, &out); err != nil {
				return out, err
			}
		case VerificationCreatePublication:
			if c.Publication == nil {
				return out, ErrVerificationInvalid
			}
			if err := verificationPublicationMutation(c, *c.Publication, rows, now, &out); err != nil {
				return out, err
			}
		case VerificationSeal:
			for _, row := range rows {
				if row.ContextID == c.ContextID && ((row.Table == "verification_attempts" && (row.State == "running" || row.State == "pending")) || (row.Table == "verification_operations" && !verificationOperationResolved(row.State))) {
					return out, ErrVerificationState
				}
			}
			vc.SealedAt = &now
			r.State = "sealed"
			r.Body = verificationJSON(vc)
			put(r)
			out.Receipt.ID = vc.ID
		default:
			return out, ErrVerificationInvalid
		}
	}
	if len(out.Rows) > 0 || len(out.DeleteChunks) > 0 {
		eventKind := c.Kind
		if eventKind == VerificationFinalizeArtifact {
			eventKind = VerificationWriteEvidence
		}
		out.Event = core.Event{TaskID: c.Access.TaskID, Kind: "verification." + eventKind, ActorID: actor, ActorRole: ActorFromContext(ctx).Role, Payload: verificationJSON(out.Receipt)}
	}
	return out, nil
}

func verificationSanitizeJSON(r *redact.Redactor, b []byte) ([]byte, error) {
	// Decode strings only after rejecting duplicate JSON keys. RedactJSON alone
	// would discard duplicate keys before the envelope validator could see them.
	if err := verificationUniqueJSON(b); err != nil {
		return nil, err
	}
	return verificationFilterJSON(r, b)
}
func verificationUniqueJSON(b []byte) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	var walk func() error
	walk = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				key := k.(string)
				if seen[key] {
					return ErrVerificationInvalid
				}
				seen[key] = true
				if e = walk(); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := walk(); e != nil {
					return e
				}
			}
		default:
			return ErrVerificationInvalid
		}
		_, err = d.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if d.More() {
		return ErrVerificationInvalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrVerificationInvalid
	}
	return nil
}

// VerificationStorageKey distinguishes chunk indexes while retaining the
// upload identity for bounded assembly and expiry lookup.
func VerificationStorageKey(v VerificationRow) string {
	if v.Table == "verification_upload_chunks" {
		return v.ID
	}
	return v.LogicalKey
}

type verificationClockKey struct{}

// WithVerificationClockForTest advances staging time in conformance fixtures.
// Claim validity always uses the real clock independently of this test hook.
func WithVerificationClockForTest(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, verificationClockKey{}, now)
}

func verificationIdentifier(id string) bool {
	return id != "" && len(id) <= 128 && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}
