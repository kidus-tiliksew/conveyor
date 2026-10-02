package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// Stable grant refusal reasons for an authorized operator (feature-verification-
// kit-execution VK-12.2; component-http-api VK-HTTP-8). Unauthorized and
// foreign callers never receive one.
const (
	VerificationRefusalNotClaimed          = "not_claimed"
	VerificationRefusalClaimExpired        = "claim_expired"
	VerificationRefusalHeadChanged         = "head_changed"
	VerificationRefusalContextMissing      = "context_missing"
	VerificationRefusalContextStale        = "context_stale"
	VerificationRefusalSubjectUnregistered = "subject_unregistered"
	VerificationRefusalRequestConflict     = "request_conflict"
	VerificationRefusalGrantUnknown        = "grant_unknown"
	VerificationRefusalGrantRevoked        = "grant_revoked"
)

var verificationRefusalText = map[string][2]string{
	VerificationRefusalNotClaimed:          {"The verify work order is not claimed.", "Grants can be issued only while a verifier holds the claim. Wait for the verifier to claim and prepare the context, or recover the order."},
	VerificationRefusalClaimExpired:        {"The verifier's claim lease or execution deadline has passed.", "Recover the verify order. The next verifier claim prepares a new context; grant against that context."},
	VerificationRefusalHeadChanged:         {"The work order head differs from the task's submitted head.", "A newer submission replaces this verification. Grant against the verify order for the current head."},
	VerificationRefusalContextMissing:      {"No verification context exists for the current claim attempt.", "Wait for the verifier to call prepare_verification, then inspect again."},
	VerificationRefusalContextStale:        {"The selected context belongs to an earlier attempt or is sealed.", "Inspect the order again without a context ID and grant against the current context."},
	VerificationRefusalSubjectUnregistered: {"The subject or its digest is not registered in this context.", "Select a subject from the current inspection; ordinary obligations must be registered by the verifier first."},
	VerificationRefusalRequestConflict:     {"The request key was already used for a different request.", "Retry with the original request unchanged, or use a new request key."},
	VerificationRefusalGrantUnknown:        {"The named grant is not part of this context.", "Inspect the order and revoke a grant listed for the current context."},
	VerificationRefusalGrantRevoked:        {"The grant issued under this request key has been revoked.", "Issue a new grant with a new request key."},
}

// VerificationRefusal keeps its sentinel for errors.Is while carrying a stable
// operator-facing reason.
type VerificationRefusal struct {
	Reason string
	err    error
}

func (r VerificationRefusal) Error() string { return r.err.Error() + ": " + r.Reason }
func (r VerificationRefusal) Unwrap() error { return r.err }

func verificationRefuse(err error, reason string) error {
	return VerificationRefusal{Reason: reason, err: err}
}

// VerificationRefusalDetail returns the stable reason with its fixed message
// and recovery text when err carries one.
func VerificationRefusalDetail(err error) (reason, message, recovery string, ok bool) {
	var r VerificationRefusal
	if !errors.As(err, &r) {
		return "", "", "", false
	}
	text := verificationRefusalText[r.Reason]
	return r.Reason, text[0], text[1], true
}

// VerificationPermissionOrderState names why an authorized operator cannot
// grant against order now, or returns "" while the claim window is open.
func VerificationPermissionOrderState(task core.Task, order core.WorkOrder, now time.Time) string {
	switch {
	case order.State != core.WorkOrderClaimed:
		return VerificationRefusalNotClaimed
	case !order.LeaseExpiresAt.After(now) || (!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now)):
		return VerificationRefusalClaimExpired
	case order.HeadSHA != core.VerifyStageHead(task):
		return VerificationRefusalHeadChanged
	}
	return ""
}

type VerificationPermissionEligibility struct {
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Recovery string `json:"recovery,omitempty"`
}

type VerificationPermissionContextView struct {
	ID                 string                      `json:"id"`
	WorkOrderAttemptID string                      `json:"work_order_attempt_id"`
	Revisions          []core.VerificationRevision `json:"revisions"`
	GoverningPins      []core.VerificationPin      `json:"governing_pins"`
	Sealed             bool                        `json:"sealed"`
	CreatedAt          time.Time                   `json:"created_at"`
}

type VerificationPermissionSubjectView struct {
	Subject       core.VerificationSubject         `json:"subject"`
	Description   string                           `json:"description"`
	Kind          string                           `json:"exercise_kind"`
	Permissions   []verification.Permission        `json:"permissions"`
	Actions       []verification.ActionRequirement `json:"action_requirements"`
	Prerequisites []verification.Prerequisite      `json:"prerequisites"`
	Inputs        []verification.Input             `json:"inputs"`
	RetryPolicy   string                           `json:"retry_policy"`
	GrantIDs      []string                         `json:"grant_ids"`
}

type VerificationPermissionRevocationView struct {
	ID         string    `json:"id"`
	RequestKey string    `json:"request_key"`
	Reason     string    `json:"reason"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}

type VerificationPermissionGrantView struct {
	ID                 string                                `json:"id"`
	RequestKey         string                                `json:"request_key"`
	ContextID          string                                `json:"context_id"`
	WorkOrderID        string                                `json:"work_order_id"`
	WorkOrderAttemptID string                                `json:"work_order_attempt_id"`
	Subject            core.VerificationSubject              `json:"subject"`
	Actions            []core.VerificationPermission         `json:"actions"`
	ContractDigest     string                                `json:"contract_digest"`
	Revisions          []core.VerificationRevision           `json:"revisions"`
	Actor              string                                `json:"actor"`
	CreatedAt          time.Time                             `json:"created_at"`
	Revocation         *VerificationPermissionRevocationView `json:"revocation,omitempty"`
}

// VerificationPermissionView is the whitelisted operator projection of
// component-http-api VK-HTTP-8. It never carries evidence payloads, safe-input
// values, artifact content, tokens or credential values.
type VerificationPermissionView struct {
	TaskID             string                              `json:"task_id"`
	WorkOrderID        string                              `json:"work_order_id"`
	OrderState         string                              `json:"order_state"`
	WorkOrderAttemptID string                              `json:"work_order_attempt_id"`
	ObservedAt         time.Time                           `json:"observed_at"`
	LeaseExpiresAt     *time.Time                          `json:"lease_expires_at,omitempty"`
	ExecutionDeadline  *time.Time                          `json:"execution_deadline,omitempty"`
	SubmittedHead      string                              `json:"submitted_head"`
	Context            *VerificationPermissionContextView  `json:"context"`
	Eligibility        VerificationPermissionEligibility   `json:"eligibility"`
	Subjects           []VerificationPermissionSubjectView `json:"subjects"`
	Grants             []VerificationPermissionGrantView   `json:"grants"`
	NextCursor         string                              `json:"next_cursor,omitempty"`
}

// VerificationPermissionViewRequest selects one context page. Explicit is true
// when the caller named the context instead of accepting the order's latest.
type VerificationPermissionViewRequest struct {
	ContextID, GrantID, Cursor string
	Explicit                   bool
	Limit                      int
}

type verificationPermissionCursor struct {
	ContextID string `json:"context"`
	Offset    int    `json:"offset"`
}

const verificationPermissionPageDefault = 25

// BuildVerificationPermissionView projects the retained context snapshot for
// an operator. snapshot is nil when the order has no context. The result is
// advisory: the locked grant mutation remains the admission authority.
func BuildVerificationPermissionView(task core.Task, order core.WorkOrder, snapshot *VerificationSnapshot, req VerificationPermissionViewRequest, now time.Time) (VerificationPermissionView, error) {
	limit := req.Limit
	if limit == 0 {
		limit = verificationPermissionPageDefault
	}
	if limit < 1 || limit > VerificationPageLimit || len(req.Cursor) > 2048 {
		return VerificationPermissionView{}, ErrVerificationInvalid
	}
	v := VerificationPermissionView{TaskID: task.ID, WorkOrderID: order.ID, OrderState: string(order.State), WorkOrderAttemptID: order.AttemptID, ObservedAt: now, SubmittedHead: core.VerifyStageHead(task), Subjects: []VerificationPermissionSubjectView{}, Grants: []VerificationPermissionGrantView{}}
	if !order.LeaseExpiresAt.IsZero() {
		at := order.LeaseExpiresAt.UTC()
		v.LeaseExpiresAt = &at
	}
	if !order.ExecutionDeadline.IsZero() {
		at := order.ExecutionDeadline.UTC()
		v.ExecutionDeadline = &at
	}
	state := VerificationPermissionOrderState(task, order, now)
	var vc *VerificationContext
	if snapshot != nil {
		for i := range snapshot.Contexts {
			if snapshot.Contexts[i].ID == req.ContextID {
				vc = &snapshot.Contexts[i]
			}
		}
		if vc == nil || vc.WorkOrderID != order.ID {
			return VerificationPermissionView{}, ErrVerificationAccess
		}
		v.Context = &VerificationPermissionContextView{ID: vc.ID, WorkOrderAttemptID: vc.WorkOrderAttemptID, Revisions: vc.Revisions, GoverningPins: vc.GoverningPins, Sealed: vc.SealedAt != nil, CreatedAt: vc.CreatedAt}
		if v.Context.Revisions == nil {
			v.Context.Revisions = []core.VerificationRevision{}
		}
		if v.Context.GoverningPins == nil {
			v.Context.GoverningPins = []core.VerificationPin{}
		}
	}
	if state == "" {
		switch {
		case vc == nil || (!req.Explicit && vc.WorkOrderAttemptID != order.AttemptID):
			state = VerificationRefusalContextMissing
		case vc.WorkOrderAttemptID != order.AttemptID || vc.SealedAt != nil:
			state = VerificationRefusalContextStale
		}
	}
	v.Eligibility = VerificationPermissionEligibility{State: "eligible"}
	if state != "" {
		text := verificationRefusalText[state]
		v.Eligibility = VerificationPermissionEligibility{State: state, Reason: text[0], Recovery: text[1]}
	}
	if vc == nil {
		if req.GrantID != "" || req.Cursor != "" {
			return VerificationPermissionView{}, ErrVerificationAccess
		}
		return v, nil
	}
	revocations := map[string]VerificationPermissionRevocationView{}
	for _, r := range snapshot.PermissionRevocations {
		if prior, ok := revocations[r.GrantID]; r.ContextID != vc.ID || (ok && !r.CreatedAt.Before(prior.CreatedAt)) {
			continue
		}
		revocations[r.GrantID] = VerificationPermissionRevocationView{ID: r.ID, RequestKey: r.Key, Reason: r.Reason, Actor: r.Actor, CreatedAt: r.CreatedAt}
	}
	var grants []VerificationPermissionGrantView
	for _, g := range snapshot.PermissionGrants {
		if g.ContextID != vc.ID {
			continue
		}
		view := VerificationPermissionGrantView{ID: g.ID, RequestKey: g.Key, ContextID: g.ContextID, WorkOrderID: g.WorkOrderID, WorkOrderAttemptID: g.WorkOrderAttemptID, Subject: g.Subject, Actions: g.Actions, ContractDigest: g.ContractDigest, Revisions: g.Revisions, Actor: g.Actor, CreatedAt: g.CreatedAt}
		if view.Actions == nil {
			view.Actions = []core.VerificationPermission{}
		}
		if r, ok := revocations[g.ID]; ok {
			view.Revocation = &r
		}
		grants = append(grants, view)
	}
	sort.Slice(grants, func(i, j int) bool {
		if !grants[i].CreatedAt.Equal(grants[j].CreatedAt) {
			return grants[i].CreatedAt.Before(grants[j].CreatedAt)
		}
		return grants[i].ID < grants[j].ID
	})
	if req.GrantID != "" {
		if req.Cursor != "" {
			return VerificationPermissionView{}, ErrVerificationInvalid
		}
		for _, g := range grants {
			if g.ID == req.GrantID {
				v.Grants = []VerificationPermissionGrantView{g}
				return v, nil
			}
		}
		return VerificationPermissionView{}, ErrVerificationAccess
	}
	var subjects []VerificationPermissionSubjectView
	add := func(subject core.VerificationSubject, description string, e verification.Exercise) {
		row := VerificationPermissionSubjectView{Subject: subject, Description: description, Kind: e.Kind, Permissions: e.Permissions, Actions: verification.ActionRequirements(e), Prerequisites: e.Prerequisites, Inputs: e.Inputs, RetryPolicy: e.RetryPolicy, GrantIDs: []string{}}
		if row.Permissions == nil {
			row.Permissions = []verification.Permission{}
		}
		if row.Actions == nil {
			row.Actions = []verification.ActionRequirement{}
		}
		if row.Prerequisites == nil {
			row.Prerequisites = []verification.Prerequisite{}
		}
		if row.Inputs == nil {
			row.Inputs = []verification.Input{}
		}
		for _, g := range grants {
			if g.Subject == subject {
				row.GrantIDs = append(row.GrantIDs, g.ID)
			}
		}
		subjects = append(subjects, row)
	}
	for _, s := range snapshot.Selections {
		if s.ContextID != vc.ID {
			continue
		}
		for _, subject := range s.Subjects {
			add(subject.Subject, subject.Contract.Description, subject.Contract)
		}
	}
	obligations := append([]VerificationObligation(nil), snapshot.Obligations...)
	sort.Slice(obligations, func(i, j int) bool {
		if !obligations[i].CreatedAt.Equal(obligations[j].CreatedAt) {
			return obligations[i].CreatedAt.Before(obligations[j].CreatedAt)
		}
		return obligations[i].ID < obligations[j].ID
	})
	for _, o := range obligations {
		if o.ContextID != vc.ID {
			continue
		}
		add(core.VerificationSubject{Kind: "ordinary", ObligationID: o.ID, ContractDigest: o.Digest}, o.Description, o.Contract)
	}
	offset := 0
	if req.Cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		var c verificationPermissionCursor
		if err != nil || core.DecodeVerificationRequest(data, &c) != nil || c.ContextID != vc.ID || c.Offset < 1 || (c.Offset >= len(subjects) && c.Offset >= len(grants)) {
			return VerificationPermissionView{}, ErrVerificationInvalid
		}
		offset = c.Offset
	}
	page := func(n int) (int, int) {
		start, end := min(offset, n), min(offset+limit, n)
		return start, end
	}
	start, end := page(len(subjects))
	v.Subjects = append(v.Subjects, subjects[start:end]...)
	start, end = page(len(grants))
	v.Grants = append(v.Grants, grants[start:end]...)
	if offset+limit < len(subjects) || offset+limit < len(grants) {
		data, _ := json.Marshal(verificationPermissionCursor{ContextID: vc.ID, Offset: offset + limit})
		v.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return v, nil
}
