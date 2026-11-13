package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// req-verification-kits AC-3.2 through AC-3.4 and AC-7.3; component-verification-service.

// VerificationRecoveryDisposition is one entry of the verification object of
// the operator recovery request: one attempt of the verify order, the
// attempt's safe-input digest, every unresolved operation of the attempt, and
// the operator's disposition. It is never admitted through agent-facing
// verification tools (component-verification-service invariant 45).
type VerificationRecoveryDisposition struct {
	ContextID    string   `json:"context_id"`
	RunID        string   `json:"run_id"`
	OperationIDs []string `json:"operation_ids"`
	InputDigest  string   `json:"input_digest"`
	Disposition  string   `json:"disposition"`
}

// VerificationRecovery is the verification object of
// POST /v1/work-orders/{id}/recover: one reason and the disposition of every
// member of the order's disposition set (component-verification-service
// invariant 45; DEC-133(1)).
type VerificationRecovery struct {
	Reason       string                            `json:"reason"`
	Dispositions []VerificationRecoveryDisposition `json:"dispositions"`
}

type verificationRecoveryKey struct{}

func WithVerificationRecovery(ctx context.Context, recovery *VerificationRecovery) context.Context {
	return context.WithValue(ctx, verificationRecoveryKey{}, recovery)
}

func VerificationRecoveryFromContext(ctx context.Context) *VerificationRecovery {
	v, _ := ctx.Value(verificationRecoveryKey{}).(*VerificationRecovery)
	return v
}

func AuthorizeVerificationRecovery(ctx context.Context, membership MembershipStore) error {
	if VerificationRecoveryFromContext(ctx) == nil {
		return nil
	}
	actor := ActorFromContext(ctx)
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || actor.Role != core.ActorUser || !strings.HasPrefix(actor.ID, "user:") {
		return ErrVerificationAccess
	}
	allowed, err := membership.AuthorizeWorkspace(ctx, strings.TrimPrefix(actor.ID, "user:"), ws, core.CapabilityOperateGates)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrVerificationAccess
	}
	return nil
}

// verificationRecoveryRefusal is a refusal of a recovery request or of the
// disposition set behind it. It matches its class with errors.Is, and its
// text names the attempt or operation at fault.
type verificationRecoveryRefusal struct {
	class error
	text  string
}

func (r *verificationRecoveryRefusal) Error() string { return r.class.Error() + ": " + r.text }

func (r *verificationRecoveryRefusal) Unwrap() error { return r.class }

func refuseVerificationRecovery(class error, format string, args ...any) error {
	return &verificationRecoveryRefusal{class: class, text: fmt.Sprintf(format, args...)}
}

// PrepareVerificationRecovery stages immutable operator dispositions
// separately from later reconciliation observations. The request must name
// every member of the order's disposition set exactly once and nothing else.
// It records recovery:<request_id> on every named attempt and its named
// operations and returns one verification.recovery_authorized event per entry,
// or it returns a refusal and stages nothing. A replay must match the whole
// verification object (component-verification-service invariant 45;
// DEC-133(1), (2)).
func PrepareVerificationRecovery(ctx context.Context, task core.Task, order core.WorkOrder, rows []VerificationRow, requestID string, now time.Time) ([]VerificationRow, []core.Event, error) {
	recovery := VerificationRecoveryFromContext(ctx)
	if recovery == nil {
		return nil, nil, nil
	}
	actor := ActorFromContext(ctx)
	if actor.Role != core.ActorUser || order.Stage != core.StageVerify || strings.TrimSpace(recovery.Reason) == "" {
		return nil, nil, ErrVerificationAccess
	}
	if len(recovery.Dispositions) == 0 {
		return nil, nil, refuseVerificationRecovery(ErrVerificationInvalid, "the recovery names no disposition")
	}
	named := make(map[string]VerificationRecoveryDisposition, len(recovery.Dispositions))
	for _, d := range recovery.Dispositions {
		if d.OperationIDs == nil {
			return nil, nil, ErrVerificationAccess
		}
		switch d.Disposition {
		case "applied", "not_applied", "unknown":
		default:
			return nil, nil, ErrVerificationInvalid
		}
		if _, dup := named[d.RunID]; dup {
			return nil, nil, refuseVerificationRecovery(ErrVerificationInvalid, "the recovery names run %s more than once", d.RunID)
		}
		named[d.RunID] = d
	}
	id := "recovery:" + requestID
	requestDigest := verificationHash(verificationJSON(recovery))
	if originalDigest, ok := ctx.Value(verificationRecoveryDigestKey{}).(string); ok {
		requestDigest = originalDigest
	}
	// A replay is checked before the set is computed again, because the first
	// recovery changed the set: its authorizations are now unused.
	replayed, err := verificationRecoveryReplayed(task, order, rows, id, requestDigest, actor.ID)
	if err != nil || replayed {
		return nil, nil, err
	}
	if order.State == core.WorkOrderClaimed || order.State == core.WorkOrderCompleted {
		return nil, nil, ErrVerificationState
	}
	set, err := verificationDispositionSet(task, order, rows)
	if err != nil {
		return nil, nil, err
	}
	members := make(map[string]bool, len(set))
	for _, member := range set {
		members[member.run.ID] = true
	}
	for _, d := range recovery.Dispositions {
		if err := verificationCheckDisposition(task, order, rows, d); err != nil {
			return nil, nil, err
		}
		if !members[d.RunID] {
			return nil, nil, refuseVerificationRecovery(ErrVerificationInvalid, "run %s is not in the disposition set of verify order %s", d.RunID, order.ID)
		}
	}
	for _, member := range set {
		if _, ok := named[member.run.ID]; !ok {
			return nil, nil, refuseVerificationRecovery(ErrVerificationInvalid, "the recovery omits run %s of the disposition set of verify order %s", member.run.ID, order.ID)
		}
	}
	var changed []VerificationRow
	var events []core.Event
	for _, member := range set {
		d := named[member.run.ID]
		run, rr := member.run, member.row
		original := core.VerificationBinding{ReviewScope: member.context.ReviewScope, BaselineSHA: member.context.BaselineSHA, ContextID: member.context.ID, WorkOrderID: member.context.WorkOrderID, WorkOrderAttemptID: member.context.WorkOrderAttemptID, RunID: run.ID, Revisions: member.context.Revisions, GoverningPins: member.context.GoverningPins}
		if run.EndedAt == nil {
			run.State, run.Explanation, run.EndedAt = "cancelled", "operator recovery after claim loss", &now
		}
		for _, opID := range d.OperationIDs {
			r, _ := verificationFind(rows, "verification_operations", opID)
			op := verificationDecode[VerificationOperation](r)
			op.Recovery = append(op.Recovery, VerificationOperationAuthorization{ID: id, RequestID: requestID, Actor: actor.ID, Reason: recovery.Reason, Disposition: d.Disposition, InputDigest: d.InputDigest, Original: original, At: now})
			r.Body = verificationJSON(op)
			changed = append(changed, r)
		}
		run.Recovery = append(run.Recovery, VerificationReplayAuthorization{ID: id, Actor: actor.ID, Reason: recovery.Reason, At: now, Original: original, RequestDigest: requestDigest, Disposition: d.Disposition, InputDigest: d.InputDigest})
		rr.State, rr.Body = run.State, verificationJSON(run)
		changed = append(changed, rr)
		payload := map[string]any{"request_id": requestID, "authorization_id": id, "original": original, "disposition": d.Disposition, "reason": recovery.Reason, "operation_ids": d.OperationIDs}
		// A removal is recorded by count and class, never by value
		// (req-security-boundaries AC-3.1). Each event carries the shared
		// reason, so each records that reason's removals.
		if stats, ok := ctx.Value(verificationRecoveryRedactionsKey{}).(core.RedactionStats); ok && stats.Total() > 0 {
			payload["redactions"] = stats
		}
		events = append(events, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: "verification.recovery_authorized", ActorID: actor.ID, ActorRole: actor.Role, At: now, Payload: core.JSONPayload(payload)})
	}
	return changed, events, nil
}

// verificationRecoveryReplayed reports whether an attempt of the order already
// records the authorization id. The recorded request digest covers the whole
// verification object, so a replay that changes the reason, an entry, a
// disposition, or the entry list is a conflict, and so is another actor.
func verificationRecoveryReplayed(task core.Task, order core.WorkOrder, rows []VerificationRow, id, requestDigest, actorID string) (bool, error) {
	replayed := false
	for _, r := range rows {
		if r.Table != "verification_attempts" || r.TaskID != task.ID {
			continue
		}
		for _, a := range verificationDecode[VerificationAttempt](r).Recovery {
			if a.ID != id || a.Original.WorkOrderID != order.ID {
				continue
			}
			if a.RequestDigest != requestDigest || a.Actor != actorID {
				return false, ErrVerificationConflict
			}
			replayed = true
		}
	}
	return replayed, nil
}

// verificationCheckDisposition applies the per-attempt checks to one entry:
// its context belongs to the task and the order under the current authority,
// its run belongs to that context and the context's attempt, the digest is
// the run's safe-input digest, and its operations are exactly the run's
// unresolved operations, prepared over the same inputs.
func verificationCheckDisposition(task core.Task, order core.WorkOrder, rows []VerificationRow, d VerificationRecoveryDisposition) error {
	cr, ok := verificationFind(rows, "verification_contexts", d.ContextID)
	if !ok || cr.TaskID != task.ID {
		return ErrVerificationAccess
	}
	vc := verificationDecode[VerificationContext](cr)
	if vc.WorkOrderID != order.ID || order.HeadSHA != core.VerifyStageHead(task) {
		return ErrVerificationState
	}
	if err := VerifyVerificationAuthority(VerificationCommand{Kind: VerificationCreateContext, Context: &vc}, VerificationAuthoritySnapshot{Task: task}, order); err != nil {
		return err
	}
	rr, ok := verificationFind(rows, "verification_attempts", d.RunID)
	if !ok || rr.ContextID != vc.ID || rr.TaskID != task.ID {
		return ErrVerificationAccess
	}
	run := verificationDecode[VerificationAttempt](rr)
	if run.WorkOrderAttemptID != vc.WorkOrderAttemptID || verificationRunInputDigest(run) != d.InputDigest {
		return ErrVerificationConflict
	}
	seen := map[string]bool{}
	for _, opID := range d.OperationIDs {
		if seen[opID] {
			return ErrVerificationInvalid
		}
		seen[opID] = true
		r, ok := verificationFind(rows, "verification_operations", opID)
		if !ok || r.TaskID != task.ID || r.ContextID != vc.ID || r.RunID != run.ID {
			return ErrVerificationAccess
		}
		op := verificationDecode[VerificationOperation](r)
		if op.InputDigest != d.InputDigest || verificationOperationResolved(r.State) {
			return ErrVerificationConflict
		}
	}
	for _, r := range rows {
		if r.Table == "verification_operations" && r.RunID == run.ID && !verificationOperationResolved(r.State) && !seen[r.ID] {
			return ErrVerificationState
		}
	}
	return nil
}

// verificationRunInputDigest is the safe-input digest a recovery request must
// carry for one verification attempt. PrepareVerificationRecovery checks it,
// and the next-action disposition set states it.
func verificationRunInputDigest(run VerificationAttempt) string {
	return verificationHash(verificationJSON(run.SafeInputs))
}

// verificationSetMember is one attempt of a verify order's disposition set.
type verificationSetMember struct {
	context      VerificationContext
	row          VerificationRow
	run          VerificationAttempt
	operationIDs []string
	digest       string
}

// verificationDispositionSet computes the disposition set of a verify order
// from its verification records. Recovery validation and the next action's
// details.dispositions both read it, so a request built from the stated set
// passes validation unchanged. The set holds each attempt of the order with
// an unresolved operation, and the latest attempt of each
// operator_action_required subject in the order's contexts that holds no
// unused authorization, each attempt once, ordered by start time and then run
// ID. A still-running attempt is a member: recovery cancels it. A record the
// validator would refuse yields that refusal instead of a partial set
// (component-verification-service invariant 45; component-next-action
// invariant 15; DEC-133(1)).
func verificationDispositionSet(task core.Task, order core.WorkOrder, rows []VerificationRow) ([]verificationSetMember, error) {
	if order.Stage != core.StageVerify {
		return nil, refuseVerificationRecovery(ErrVerificationState, "work order %s is not a verify order", order.ID)
	}
	if order.HeadSHA != core.VerifyStageHead(task) {
		return nil, refuseVerificationRecovery(ErrVerificationState, "verify order %s no longer matches the task head", order.ID)
	}
	contexts := map[string]VerificationContext{}
	for _, r := range rows {
		if r.Table != "verification_contexts" || r.TaskID != task.ID {
			continue
		}
		if vc := verificationDecode[VerificationContext](r); vc.WorkOrderID == order.ID {
			contexts[r.ID] = vc
		}
	}
	authorized := func(run VerificationAttempt, vc VerificationContext) bool {
		return run.WorkOrderAttemptID == vc.WorkOrderAttemptID && VerifyVerificationAuthority(VerificationCommand{Kind: VerificationCreateContext, Context: &vc}, VerificationAuthoritySnapshot{Task: task}, order) == nil
	}
	members := map[string]*verificationSetMember{}
	for _, r := range rows {
		if r.Table != "verification_operations" || r.TaskID != task.ID || verificationOperationResolved(r.State) {
			continue
		}
		vc, ok := contexts[r.ContextID]
		if !ok {
			continue
		}
		member := members[r.RunID]
		if member == nil {
			rr, ok := verificationFind(rows, "verification_attempts", r.RunID)
			if !ok || rr.ContextID != vc.ID || rr.TaskID != task.ID {
				return nil, refuseVerificationRecovery(ErrVerificationAccess, "verification run %s of unresolved operation %s is not readable", r.RunID, r.ID)
			}
			run := verificationDecode[VerificationAttempt](rr)
			if !authorized(run, vc) {
				return nil, refuseVerificationRecovery(ErrVerificationState, "verification run %s no longer matches the attempt or authority of verify order %s", run.ID, order.ID)
			}
			member = &verificationSetMember{context: vc, row: rr, run: run, operationIDs: []string{}, digest: verificationRunInputDigest(run)}
			members[r.RunID] = member
		}
		if verificationDecode[VerificationOperation](r).InputDigest != member.digest {
			return nil, refuseVerificationRecovery(ErrVerificationConflict, "operation %s was prepared over other inputs than verification run %s", r.ID, r.RunID)
		}
		member.operationIDs = append(member.operationIDs, r.ID)
	}
	type subjectAttempt struct {
		row VerificationRow
		run VerificationAttempt
	}
	latest := map[string]subjectAttempt{}
	for _, r := range rows {
		if r.Table != "verification_attempts" || r.TaskID != task.ID {
			continue
		}
		if _, ok := contexts[r.ContextID]; !ok {
			continue
		}
		run := verificationDecode[VerificationAttempt](r)
		key := verificationSubjectKey(run.Subject)
		prior, ok := latest[key]
		if !ok || run.StartedAt.After(prior.run.StartedAt) || (run.StartedAt.Equal(prior.run.StartedAt) && run.ID > prior.run.ID) {
			latest[key] = subjectAttempt{row: r, run: run}
		}
	}
	for _, candidate := range latest {
		run, vc := candidate.run, contexts[candidate.row.ContextID]
		if members[run.ID] != nil || verificationRecoveryUnused(run, order.ID) || !authorized(run, vc) {
			continue
		}
		contract, err := verificationContract(rows, vc.ID, run.Subject)
		if err != nil || contract.RetryPolicy != "operator_action_required" {
			continue
		}
		members[run.ID] = &verificationSetMember{context: vc, row: candidate.row, run: run, operationIDs: []string{}, digest: verificationRunInputDigest(run)}
	}
	set := make([]verificationSetMember, 0, len(members))
	for _, member := range members {
		sort.Strings(member.operationIDs)
		set = append(set, *member)
	}
	sort.Slice(set, func(i, j int) bool {
		if !set[i].run.StartedAt.Equal(set[j].run.StartedAt) {
			return set[i].run.StartedAt.Before(set[j].run.StartedAt)
		}
		return set[i].run.ID < set[j].run.ID
	})
	return set, nil
}

// verificationSubjectDisplay names a subject in a disposition entry as
// kit:<kit-id>/<exercise-id> or ordinary:<obligation-id>
// (component-next-action invariant 15).
func verificationSubjectDisplay(s core.VerificationSubject) string {
	if s.Kind == "kit" {
		return "kit:" + s.KitID + "/" + s.ExerciseID
	}
	return "ordinary:" + s.ObligationID
}

// verificationDispositionBinding states the disposition set of a verify order
// released at a sealed verification checkpoint for row 11 of the next action.
// It requires a checkpoint sealed by the released attempt under the current
// head and authority, then states the set that PrepareVerificationRecovery
// validates against. A checkpoint or set the validator would refuse, or an
// empty set, yields a refusal naming the limitation instead of a partial set
// (component-next-action invariant 15; component-verification-service
// invariant 45).
func verificationDispositionBinding(task core.Task, order core.WorkOrder, rows []VerificationRow) nextaction.DispositionInput {
	refuse := func(format string, args ...any) nextaction.DispositionInput {
		return nextaction.DispositionInput{Refusal: fmt.Sprintf(format, args...)}
	}
	if order.Checkpoint == nil || order.Checkpoint.Verification == nil {
		return refuse("verify order %s has no sealed verification checkpoint to dispose", order.ID)
	}
	checkpoint := order.Checkpoint.Verification
	cr, ok := verificationFind(rows, "verification_contexts", checkpoint.ContextID)
	if !ok || cr.TaskID != task.ID {
		return refuse("verification checkpoint %s is not readable for task %s", checkpoint.ContextID, task.ID)
	}
	vc := verificationDecode[VerificationContext](cr)
	if order.Stage != core.StageVerify || vc.WorkOrderID != order.ID || vc.WorkOrderAttemptID == "" || vc.WorkOrderAttemptID != order.LastAttemptID {
		return refuse("verification checkpoint %s was not sealed by the released attempt of verify order %s", vc.ID, order.ID)
	}
	if order.HeadSHA != core.VerifyStageHead(task) || VerifyVerificationAuthority(VerificationCommand{Kind: VerificationCreateContext, Context: &vc}, VerificationAuthoritySnapshot{Task: task}, order) != nil {
		return refuse("verification checkpoint %s no longer matches the task head or authority of verify order %s", vc.ID, order.ID)
	}
	for _, id := range checkpoint.OperationIDs {
		if r, ok := verificationFind(rows, "verification_operations", id); !ok || r.TaskID != task.ID {
			return refuse("operation %s of verification checkpoint %s is not readable", id, vc.ID)
		}
	}
	set, err := verificationDispositionSet(task, order, rows)
	if err != nil {
		var refusal *verificationRecoveryRefusal
		if errors.As(err, &refusal) {
			return refuse("%s", refusal.text)
		}
		return refuse("the disposition set of verify order %s cannot be stated", order.ID)
	}
	if len(set) == 0 {
		return refuse("verify order %s has no unresolved operations and no retry-gated attempts to dispose", order.ID)
	}
	entries := make([]nextaction.Disposition, 0, len(set))
	for _, member := range set {
		entries = append(entries, nextaction.Disposition{ContextID: member.context.ID, RunID: member.run.ID, OperationIDs: append([]string{}, member.operationIDs...), InputDigest: member.digest, Subject: verificationSubjectDisplay(member.run.Subject), State: member.run.State})
	}
	return nextaction.DispositionInput{Set: entries}
}

// verificationRecoveryUnused reports whether run still carries an operator
// authorization for orderID that the start-path retry gate would accept and
// no successor attempt has consumed.
func verificationRecoveryUnused(run VerificationAttempt, orderID string) bool {
	for _, a := range run.Recovery {
		if a.Successor == nil && strings.HasPrefix(a.Actor, "user:") && a.Original.ContextID != "" && a.Original.WorkOrderID == orderID && (a.Disposition == "applied" || a.Disposition == "not_applied") {
			return true
		}
	}
	return false
}

type verificationRecoveryDigestKey struct{}

type verificationRecoveryRedactionsKey struct{}

func SanitizeVerificationRecovery(ctx context.Context, source redact.SecretSource) (context.Context, error) {
	d := VerificationRecoveryFromContext(ctx)
	if d == nil {
		return ctx, nil
	}
	// The replay digest covers the whole verification object as the operator
	// sent it, before redaction.
	if _, ok := ctx.Value(verificationRecoveryDigestKey{}).(string); !ok {
		ctx = context.WithValue(ctx, verificationRecoveryDigestKey{}, verificationHash(verificationJSON(d)))
	}
	clean := *d
	clean.Dispositions = append([]VerificationRecoveryDisposition(nil), d.Dispositions...)
	reason, stats, err := redact.Text(ctx, source, d.Reason)
	if err != nil {
		return ctx, err
	}
	clean.Reason = reason
	// A backend may sanitize more than once on the way to its transaction, and
	// each pass removes only what earlier passes left, so the counts accumulate.
	if prior, ok := ctx.Value(verificationRecoveryRedactionsKey{}).(core.RedactionStats); ok {
		stats.Add(prior)
	}
	ctx = context.WithValue(ctx, verificationRecoveryRedactionsKey{}, stats)
	return WithVerificationRecovery(ctx, &clean), nil
}
