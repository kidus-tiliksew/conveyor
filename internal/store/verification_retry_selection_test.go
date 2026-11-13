package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// retrySelectionFixture is one verify order with a sealed first context that
// ended a gated attempt, and a second context of the same order that starts
// its retry. Each case places authorizations on the attempts of the first
// context and reads what the shared start mutation selects.
type retrySelectionFixture struct {
	rows    []VerificationRow
	vc      VerificationContext
	subject core.VerificationSubject
	digest  string
	base    time.Time
}

func newRetrySelectionFixture() retrySelectionFixture {
	head := strings.Repeat("a", 40)
	revisions := []core.VerificationRevision{{Repository: "conveyor", SHA: head}}
	contract := verification.Exercise{ID: "gated", Kind: "script", Argv: []string{"fixture"}, TimeoutSeconds: 30, RequiredAssertions: []verification.Assertion{}, RetryPolicy: "operator_action_required"}
	subject := core.VerificationSubject{Kind: "ordinary", ObligationID: "gated", ContractDigest: "digest-gated"}
	sealed := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	first := VerificationContext{ID: "ctx-a", TaskID: "task", WorkOrderID: "verify", WorkOrderAttemptID: "attempt-1", Revisions: revisions, SealedAt: &sealed}
	second := VerificationContext{ID: "ctx-b", TaskID: "task", WorkOrderID: "verify", WorkOrderAttemptID: "attempt-2", Revisions: revisions}
	rows := []VerificationRow{
		verificationRow("verification_contexts", first.ID, "task", first.ID, "", "", "sealed", first),
		verificationRow("verification_contexts", second.ID, "task", second.ID, "", "", "open", second),
	}
	for _, c := range []VerificationContext{first, second} {
		rows = append(rows, verificationRow("verification_obligations", c.ID+":gated", "task", c.ID, "", "", "", VerificationObligation{ID: "gated", ContextID: c.ID, Digest: subject.ContractDigest, Contract: contract}))
	}
	grant := VerificationPermissionGrant{ID: "grant", ContextID: second.ID, TaskID: "task", WorkOrderID: "verify", WorkOrderAttemptID: "attempt-2", Subject: subject, Revisions: revisions, Actions: []core.VerificationPermission{}, ContractDigest: verificationHash(verificationJSON(contract))}
	rows = append(rows, verificationRow("verification_permission_grants", grant.ID, "task", second.ID, "", "", "", grant))
	safe := map[string]json.RawMessage{}
	return retrySelectionFixture{rows: rows, vc: second, subject: subject, digest: verificationHash(verificationJSON(safe)), base: sealed.Add(-time.Hour)}
}

// attempt adds an ended attempt of the gated subject to the first context.
// minute orders the attempts in time.
func (f *retrySelectionFixture) attempt(id string, minute int, recovery ...VerificationReplayAuthorization) {
	started := f.base.Add(time.Duration(minute) * time.Minute)
	ended := started.Add(time.Second)
	run := VerificationAttempt{ID: id, ContextID: "ctx-a", WorkOrderAttemptID: "attempt-1", Subject: f.subject, SafeInputs: map[string]json.RawMessage{}, State: "failed", StartedAt: started, EndedAt: &ended, Recovery: recovery}
	f.rows = append(f.rows, verificationRow("verification_attempts", id, "task", "ctx-a", id, "ctx-a:"+id, run.State, run))
}

// authorization is an operator authorization of the original attempt runID
// that passes every check of the retry gate.
func (f *retrySelectionFixture) authorization(id, runID string, minute int) VerificationReplayAuthorization {
	return VerificationReplayAuthorization{ID: id, Actor: "user:operator", Disposition: "not_applied", InputDigest: f.digest, At: f.base.Add(time.Duration(minute) * time.Minute), Original: core.VerificationBinding{ContextID: "ctx-a", WorkOrderID: "verify", WorkOrderAttemptID: "attempt-1", RunID: runID, Revisions: f.vc.Revisions}}
}

// start applies one start of the gated subject in the second context. The
// command carries a caller value in the retained audit field, which the
// server must ignore.
func (f *retrySelectionFixture) start(t *testing.T) (VerificationMutation, error) {
	t.Helper()
	c := VerificationCommand{Kind: VerificationStartAttempt, ContextID: f.vc.ID, Key: "retry", Coverage: &VerificationCoverage{}, Access: VerificationAccess{TaskID: "task", WorkOrderID: "verify", WorkOrderAttemptID: "attempt-2"},
		Attempt: &VerificationAttempt{Subject: f.subject, SafeInputs: map[string]json.RawMessage{}, GrantID: "grant", EffectiveActions: []core.VerificationPermission{}, ReplayAuthorizationID: "recovery:caller-named"}}
	var out VerificationMutation
	err := verificationAttemptMutation(c, f.rows, f.vc, "claim:worker", f.base.Add(3*time.Hour), redact.New(nil), &out)
	return out, err
}

// TestRetryGateSelectsAuthorization pins the server-side selection of the
// retry gate: the earliest qualifying unused operator authorization on the
// subject's latest attempt in the order, with record order breaking a tie.
// A refusal stages no row (component-verification-service invariant 19;
// DEC-133(3)).
func TestRetryGateSelectsAuthorization(t *testing.T) {
	type change func(*VerificationReplayAuthorization)
	used := func(a *VerificationReplayAuthorization) {
		a.Successor = &core.VerificationBinding{ContextID: "ctx-b", RunID: "earlier-successor"}
	}
	cases := []struct {
		name string
		// recovery lists the authorizations on the latest attempt as
		// (minute, change) pairs. A nil change keeps it qualifying.
		recovery []change
		minutes  []int
		// older places one qualifying authorization on an earlier attempt.
		older bool
		want  int
	}{
		{name: "earliest by time", recovery: []change{nil, nil}, minutes: []int{20, 10}, want: 1},
		{name: "record order breaks a tie", recovery: []change{nil, nil}, minutes: []int{10, 10}, want: 0},
		{name: "used authorization", recovery: []change{used}, minutes: []int{10}, want: -1},
		{name: "used then unused", recovery: []change{used, nil}, minutes: []int{10, 20}, want: 1},
		{name: "agent actor", recovery: []change{func(a *VerificationReplayAuthorization) { a.Actor = "claim:worker" }}, minutes: []int{10}, want: -1},
		{name: "another order", recovery: []change{func(a *VerificationReplayAuthorization) { a.Original.WorkOrderID = "verify-earlier" }}, minutes: []int{10}, want: -1},
		{name: "another authority", recovery: []change{func(a *VerificationReplayAuthorization) { a.Original.BaselineSHA = strings.Repeat("b", 40) }}, minutes: []int{10}, want: -1},
		{name: "another input digest", recovery: []change{func(a *VerificationReplayAuthorization) { a.InputDigest = strings.Repeat("0", 64) }}, minutes: []int{10}, want: -1},
		{name: "unknown disposition", recovery: []change{func(a *VerificationReplayAuthorization) { a.Disposition = "unknown" }}, minutes: []int{10}, want: -1},
		{name: "applied disposition", recovery: []change{func(a *VerificationReplayAuthorization) { a.Disposition = "applied" }}, minutes: []int{10}, want: 0},
		{name: "no original binding", recovery: []change{func(a *VerificationReplayAuthorization) { a.Original.ContextID = "" }}, minutes: []int{10}, want: -1},
		{name: "rejected earlier, qualifying later", recovery: []change{func(a *VerificationReplayAuthorization) { a.InputDigest = strings.Repeat("0", 64) }, nil}, minutes: []int{5, 30}, want: 1},
		{name: "only an older attempt holds one", older: true, want: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetrySelectionFixture()
			if tc.older {
				f.attempt("run-older", 1, f.authorization("recovery:older", "run-older", 2))
			}
			var recovery []VerificationReplayAuthorization
			for i, c := range tc.recovery {
				// Every authorization shares the recovery ID shape that one
				// recovery request writes, so only the checks tell them apart.
				a := f.authorization("recovery:shared", "run-latest", tc.minutes[i])
				a.Reason = string(rune('a' + i))
				if c != nil {
					c(&a)
				}
				recovery = append(recovery, a)
			}
			f.attempt("run-latest", 3, recovery...)
			out, err := f.start(t)
			if tc.want < 0 {
				var refusal *VerificationRefusal
				if !errors.As(err, &refusal) || refusal.Code != VerificationCodeRetryAuthorizationRequired || refusal.Field != "subject" {
					t.Fatalf("start err = %v, want %s on field subject", err, VerificationCodeRetryAuthorizationRequired)
				}
				if len(out.Rows) != 0 || out.Receipt.LaunchAuthorized {
					t.Fatalf("refused start staged %d rows, receipt %+v", len(out.Rows), out.Receipt)
				}
				return
			}
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if !out.Receipt.LaunchAuthorized {
				t.Fatal("admitted start did not authorize a launch")
			}
			var original, successor *VerificationAttempt
			for _, row := range out.Rows {
				run := verificationDecode[VerificationAttempt](row)
				switch row.ID {
				case "run-latest":
					original = &run
				case out.Receipt.ID:
					successor = &run
				default:
					t.Fatalf("start staged an unexpected row %s", row.ID)
				}
			}
			if original == nil || successor == nil {
				t.Fatalf("start staged rows %+v, want the original and the successor", out.Rows)
			}
			for i, a := range original.Recovery {
				consumedBefore := tc.recovery[i] != nil && a.Reason == recovery[i].Reason && recovery[i].Successor != nil
				switch {
				case i == tc.want:
					if a.Successor == nil || a.Successor.RunID != successor.ID || a.Successor.ContextID != "ctx-b" || a.Successor.WorkOrderID != "verify" {
						t.Fatalf("selected authorization %d successor = %+v, want run %s", i, a.Successor, successor.ID)
					}
				case consumedBefore:
					if a.Successor == nil || a.Successor.RunID != "earlier-successor" {
						t.Fatalf("used authorization %d was rebound: %+v", i, a.Successor)
					}
				case a.Successor != nil:
					t.Fatalf("authorization %d was consumed, want only %d", i, tc.want)
				}
			}
			if successor.ReplayAuthorizationID != recovery[tc.want].ID {
				t.Fatalf("successor records %q, want the selected %q and never the caller's value", successor.ReplayAuthorizationID, recovery[tc.want].ID)
			}
		})
	}
}

// TestOperationAuthorizationSelection pins the operation side: only a
// re-preparation consumes an operation authorization, and it takes the
// earliest unused not_applied one that matches the operation's digest and
// authority (component-verification-service invariants 25 and 45).
func TestOperationAuthorizationSelection(t *testing.T) {
	base := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	digest := strings.Repeat("d", 64)
	revisions := []core.VerificationRevision{{Repository: "conveyor", SHA: strings.Repeat("a", 40)}}
	original := core.VerificationBinding{ContextID: "ctx-a", WorkOrderID: "verify", RunID: "run-a", Revisions: revisions}
	successor := core.VerificationBinding{ContextID: "ctx-b", WorkOrderID: "verify", RunID: "run-b", Revisions: revisions}
	authorization := func(reason, disposition string, minute int) VerificationOperationAuthorization {
		return VerificationOperationAuthorization{ID: "recovery:shared", Reason: reason, Actor: "user:operator", Disposition: disposition, InputDigest: digest, Original: original, At: base.Add(time.Duration(minute) * time.Minute)}
	}
	cases := []struct {
		name     string
		recovery []VerificationOperationAuthorization
		edit     func([]VerificationOperationAuthorization)
		want     int
	}{
		{name: "earliest by time", recovery: []VerificationOperationAuthorization{authorization("late", "not_applied", 20), authorization("early", "not_applied", 10)}, want: 1},
		{name: "record order breaks a tie", recovery: []VerificationOperationAuthorization{authorization("first", "not_applied", 10), authorization("second", "not_applied", 10)}, want: 0},
		{name: "used", recovery: []VerificationOperationAuthorization{authorization("used", "not_applied", 10)}, edit: func(r []VerificationOperationAuthorization) {
			r[0].Successor = &core.VerificationBinding{RunID: "earlier"}
		}, want: -1},
		{name: "applied disposition", recovery: []VerificationOperationAuthorization{authorization("applied", "applied", 10)}, want: -1},
		{name: "another digest", recovery: []VerificationOperationAuthorization{authorization("digest", "not_applied", 10)}, edit: func(r []VerificationOperationAuthorization) { r[0].InputDigest = strings.Repeat("0", 64) }, want: -1},
		{name: "another authority", recovery: []VerificationOperationAuthorization{authorization("authority", "not_applied", 10)}, edit: func(r []VerificationOperationAuthorization) { r[0].Original.BaselineSHA = strings.Repeat("b", 40) }, want: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := VerificationOperation{InputDigest: digest, Recovery: append([]VerificationOperationAuthorization{}, tc.recovery...)}
			if tc.edit != nil {
				tc.edit(op.Recovery)
			}
			before := verificationJSON(op)
			err := consumeOperationAuthorization(&op, successor)
			if tc.want < 0 {
				if !errors.Is(err, ErrVerificationState) {
					t.Fatalf("consume err = %v, want %v", err, ErrVerificationState)
				}
				if string(verificationJSON(op)) != string(before) {
					t.Fatal("a refused re-preparation changed the operation")
				}
				return
			}
			if err != nil {
				t.Fatalf("consume: %v", err)
			}
			for i, a := range op.Recovery {
				if i == tc.want {
					if a.Successor == nil || a.Successor.RunID != "run-b" {
						t.Fatalf("selected authorization %d successor = %+v", i, a.Successor)
					}
				} else if a.Successor != nil {
					t.Fatalf("authorization %d was consumed, want only %d", i, tc.want)
				}
			}
			if err := consumeOperationAuthorization(&op, successor); !errors.Is(err, ErrVerificationState) && len(tc.recovery) == 1 {
				t.Fatalf("a second re-preparation consumed again: %v", err)
			}
		})
	}
	// Reconciliation selects without consuming: the same authorization stays
	// available for one re-preparation.
	op := VerificationOperation{InputDigest: digest, Recovery: []VerificationOperationAuthorization{authorization("only", "not_applied", 10)}}
	if operationAuthorization(&op, "not_applied", func(VerificationOperationAuthorization) bool { return true }) != 0 || op.Recovery[0].Successor != nil {
		t.Fatal("reconciliation selection consumed the authorization")
	}
	if operationAuthorization(&op, "applied", func(VerificationOperationAuthorization) bool { return true }) != -1 {
		t.Fatal("reconciliation selected an authorization of another disposition")
	}
	requireConsume := consumeOperationAuthorization(&op, successor)
	if requireConsume != nil || op.Recovery[0].Successor == nil {
		t.Fatalf("re-preparation after reconciliation: %v", requireConsume)
	}
	if operationAuthorization(&op, "not_applied", func(VerificationOperationAuthorization) bool { return true }) != -1 {
		t.Fatal("a consumed authorization admitted a later reconciliation")
	}
}
