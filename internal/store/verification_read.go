package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// feature-verification-kit-execution VK-9 / DEC-43: user inspection has its own bounded read contract. Neither
// pages nor summaries expose execution inputs, evidence payloads or artifacts.
const VerificationPageLimit = 50
const VerificationReadTimeFormat = "2006-01-02T15:04:05.000000000Z"

type VerificationOperatorObservation struct {
	ContextID  string                       `json:"context_id"`
	RunID      string                       `json:"run_id"`
	Key        string                       `json:"idempotency_key"`
	Fact       string                       `json:"fact"`
	Supporting []core.VerificationReference `json:"supporting"`
}

type VerificationPageRequest struct {
	Kind      string `json:"kind"`
	ContextID string `json:"context_id"`
	Cursor    string `json:"cursor"`
	Limit     int    `json:"limit"`
}

type VerificationReadCursor struct {
	WorkspaceID string `json:"workspace"`
	TaskID      string `json:"task"`
	ContextID   string `json:"context"`
	Kind        string `json:"kind"`
	At          string `json:"at"`
	ID          string `json:"id"`
}

// Metadata is an explicitly projected JSON object, never a retained body.
// The schema is shared by the SQL views and the volatile projection.
type VerificationReadItem struct {
	ID        string          `json:"id"`
	ContextID string          `json:"context_id"`
	RunID     string          `json:"run_id"`
	State     string          `json:"state"`
	At        string          `json:"at"`
	Metadata  json.RawMessage `json:"metadata"`
}

// Fields name only scalar metadata. Each value is capped at 2048 Unicode
// characters by both backends; truncation is explicit in the projection.
var VerificationReadFields = map[string]map[string][]string{
	"contexts":     {"work_order_id": {"WorkOrderID"}, "created_by": {"CreatedBy"}, "sealed_at": {"SealedAt"}, "outcome": {"Result", "Submission", "outcome"}, "deciding_actor": {"Result", "Actor"}, "disposition": {"Result", "Disposition"}, "required_action": {"Result", "Submission", "feedback"}, "scope": {"ReviewScope"}, "baseline_sha": {"BaselineSHA"}},
	"attempts":     {"created_by": {"CreatedBy"}, "started_at": {"StartedAt"}, "ended_at": {"EndedAt"}, "outcome": {"State"}, "required_action": {"Explanation"}, "kind": {"Subject", "kind"}, "kit_id": {"Subject", "kit_id"}, "kit_version": {"Subject", "kit_version"}, "digest": {"Subject", "content_digest"}, "exercise_id": {"Subject", "exercise_id"}, "obligation_id": {"Subject", "obligation_id"}, "contract_digest": {"Subject", "contract_digest"}},
	"evidence":     {"type": {"Envelope", "type"}, "captured_at": {"Envelope", "captured_at"}, "submitted_by": {"Envelope", "submitted_by"}, "digest": {"Digest"}, "assertion_id": {"Envelope", "payload", "assertion_id"}, "outcome": {"Envelope", "payload", "outcome"}},
	"operations":   {"step_id": {"StepID"}, "target": {"Target"}, "retry_policy": {"RetryPolicy"}, "created_by": {"CreatedBy"}},
	"publications": {"head_sha": {"HeadSHA"}, "body_digest": {"BodyDigest"}},
	"obligations":  {"obligation_id": {"ID"}, "description": {"Description"}, "digest": {"Digest"}, "created_by": {"CreatedBy"}},
}

func VerificationReadFieldsFor(kind string) map[string][]string { return VerificationReadFields[kind] }

type VerificationReadPage struct {
	Items      []VerificationReadItem `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

type VerificationReader interface {
	ReadVerificationPage(context.Context, VerificationAccess, VerificationPageRequest) (VerificationReadPage, error)
	ReadVerificationDetail(context.Context, VerificationAccess, string, string) (VerificationEvidenceRecord, error)
}

func ValidateVerificationPage(ctx context.Context, a VerificationAccess, p *VerificationPageRequest) (VerificationReadCursor, error) {
	var c VerificationReadCursor
	workspace, _ := WorkspaceFromContext(ctx)
	if a.UserID == "" || !verificationIdentifier(a.TaskID) {
		return c, ErrVerificationAccess
	}
	switch p.Kind {
	case "contexts", "attempts", "assertions", "evidence", "operations", "publications", "selections", "obligations":
	default:
		return c, ErrVerificationInvalid
	}
	if p.Kind != "contexts" && !verificationIdentifier(p.ContextID) {
		return c, ErrVerificationInvalid
	}
	if p.ContextID != "" && !verificationIdentifier(p.ContextID) {
		return c, ErrVerificationInvalid
	}
	if p.Limit == 0 {
		p.Limit = 10
	}
	if p.Limit < 1 || p.Limit > VerificationPageLimit || len(p.Cursor) > 2048 {
		return c, ErrVerificationInvalid
	}
	if p.Cursor == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(p.Cursor)
	if err != nil || core.DecodeVerificationRequest(b, &c) != nil {
		return c, ErrVerificationInvalid
	}
	at, err := time.Parse(VerificationReadTimeFormat, c.At)
	if err != nil || at.Format(VerificationReadTimeFormat) != c.At || c.ID == "" || len(c.ID) > 512 || strings.ContainsRune(c.ID, 0) || c.WorkspaceID != workspace || c.TaskID != a.TaskID || c.ContextID != p.ContextID || c.Kind != p.Kind {
		return c, ErrVerificationInvalid
	}
	return c, nil
}

func VerificationPageResult(ctx context.Context, a VerificationAccess, p VerificationPageRequest, items []VerificationReadItem) VerificationReadPage {
	workspace, _ := WorkspaceFromContext(ctx)
	result := VerificationReadPage{Items: items}
	if result.Items == nil {
		result.Items = []VerificationReadItem{}
	}
	if len(items) > p.Limit {
		result.Items = items[:p.Limit]
		last := result.Items[len(result.Items)-1]
		b, _ := json.Marshal(VerificationReadCursor{WorkspaceID: workspace, TaskID: a.TaskID, ContextID: p.ContextID, Kind: p.Kind, At: last.At, ID: last.ID})
		result.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return result
}

func RecordVerificationOperatorObservation(ctx context.Context, b VerificationStore, a VerificationAccess, input VerificationOperatorObservation) (VerificationReceipt, error) {
	if a.UserID == "" || !verificationIdentifier(input.ContextID) || !verificationIdentifier(input.RunID) || !verificationIdentifier(input.Key) || strings.TrimSpace(input.Fact) == "" || len(input.Fact) > 16384 || len(input.Supporting) == 0 || len(input.Supporting) > 50 {
		return VerificationReceipt{}, ErrVerificationInvalid
	}
	// All provenance is resolved by PrepareVerificationMutation under the
	// existing backend lock. The adapter never opens another transaction.
	a.WorkOrderID, a.WorkOrderAttemptID = "", ""
	a.ClientToken, a.Claim = "", core.WorkOrderClaimIdentity{}
	return b.ApplyVerification(ctx, VerificationCommand{Access: a, Kind: VerificationWriteEvidence, ContextID: input.ContextID, RunID: input.RunID, Key: input.Key, OperatorObservation: &input})
}
