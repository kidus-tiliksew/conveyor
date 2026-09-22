package store

import (
	"context"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

const VerificationGrantPermissions = "permissions.grant"
const VerificationRevokePermissions = "permissions.revoke"

type VerificationPermissionGrant struct {
	ID, ContextID, WorkspaceID, TaskID, WorkOrderID, WorkOrderAttemptID string
	Key, Actor, ContractDigest                                          string
	Subject                                                             core.VerificationSubject
	Revisions                                                           []core.VerificationRevision
	Actions                                                             []core.VerificationPermission
	CreatedAt                                                           time.Time
}

type VerificationPermissionRevocation struct {
	ID, GrantID, ContextID, Actor, Reason, Key string
	CreatedAt                                  time.Time
}

type VerificationPermissionRequest struct {
	ContextID     string                        `json:"context_id"`
	RequestKey    string                        `json:"request_key"`
	Subject       core.VerificationSubject      `json:"subject"`
	Actions       []core.VerificationPermission `json:"actions"`
	RevokeGrantID string                        `json:"revoke_grant_id,omitempty"`
	Reason        string                        `json:"reason,omitempty"`
}

func VerificationPermissionCommand(c VerificationCommand) bool {
	return c.Kind == VerificationGrantPermissions || c.Kind == VerificationRevokePermissions
}

// BindVerificationPermissionOrder runs while the backend holds task/order
// locks. The operator supplies no claim, revision, or contract authority.
func BindVerificationPermissionOrder(ctx context.Context, c *VerificationCommand, task core.Task, order core.WorkOrder, now time.Time) error {
	if !VerificationPermissionCommand(*c) {
		return nil
	}
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || !c.Authority.OperateGates || c.Access.UserID == "" || order.ID != c.Access.WorkOrderID || order.TaskID != task.ID || task.Workspace != ws || order.Stage != core.StageVerify || order.State != core.WorkOrderClaimed || !order.LeaseExpiresAt.After(now) || (!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now)) || order.HeadSHA != core.VerifyStageHead(task) {
		return ErrVerificationAccess
	}
	c.Access.WorkOrderAttemptID = order.AttemptID
	return nil
}

func verificationPermissionMutation(c VerificationCommand, rows []VerificationRow, vc VerificationContext, actor string, now time.Time, out *VerificationMutation) error {
	if !c.Authority.OperateGates || c.Access.UserID == "" || c.Permissions == nil || c.Key == "" {
		return ErrVerificationAccess
	}
	p := c.Permissions
	if c.Kind == VerificationRevokePermissions {
		row, ok := verificationFind(rows, "verification_permission_grants", p.RevokeGrantID)
		if !ok || row.ContextID != vc.ID || p.Reason == "" {
			return ErrVerificationAccess
		}
		v := VerificationPermissionRevocation{ID: vc.ID + ":" + c.Key, GrantID: row.ID, ContextID: vc.ID, Actor: actor, Reason: p.Reason, Key: c.Key}
		if prior, ok := verificationFind(rows, "verification_permission_revocations", v.ID); ok {
			old := verificationDecode[VerificationPermissionRevocation](prior)
			old.CreatedAt = time.Time{}
			if !verificationEqual(old, v) {
				return ErrVerificationConflict
			}
			out.Receipt.ID = old.ID
			return nil
		}
		v.CreatedAt = now
		out.Rows = append(out.Rows, verificationRow("verification_permission_revocations", v.ID, vc.TaskID, vc.ID, "", v.ID, "revoked", v))
		// Revocation and cancellation share one transaction. Keep unresolved
		// provider outcomes uncertain instead of labelling them rolled back.
		for _, runRow := range rows {
			if runRow.Table != "verification_attempts" || runRow.ContextID != vc.ID {
				continue
			}
			run := verificationDecode[VerificationAttempt](runRow)
			if run.GrantID != row.ID || run.EndedAt != nil {
				continue
			}
			run.State, run.Explanation, run.EndedAt = "cancelled", "permission grant revoked", &now
			runRow.State, runRow.Body = run.State, verificationJSON(run)
			out.Rows = append(out.Rows, runRow)
			for _, opRow := range rows {
				if opRow.Table != "verification_operations" || verificationOperationRun(opRow) != run.ID || verificationOperationResolved(opRow.State) {
					continue
				}
				op := verificationDecode[VerificationOperation](opRow)
				op.History = append(op.History, VerificationOperationObservation{State: "outcome_unknown", Actor: actor, Source: "grant_revoked", CapturedAt: now, ContextID: vc.ID, RunID: run.ID, WorkOrderAttemptID: vc.WorkOrderAttemptID})
				opRow.State, opRow.Body = "outcome_unknown", verificationJSON(op)
				out.Rows = append(out.Rows, opRow)
			}
		}
		out.Receipt.ID = v.ID
		return nil
	}
	contract, err := verificationContract(rows, vc.ID, p.Subject)
	if err != nil {
		return err
	}
	actions, err := verification.NormalizeVerificationPermissions(p.Actions)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVerificationInvalid, err)
	}
	v := VerificationPermissionGrant{ContextID: vc.ID, WorkspaceID: vc.WorkspaceID, TaskID: vc.TaskID, WorkOrderID: vc.WorkOrderID, WorkOrderAttemptID: vc.WorkOrderAttemptID, Key: c.Key, Actor: actor, ContractDigest: verificationHash(verificationJSON(contract)), Subject: p.Subject, Revisions: vc.Revisions, Actions: actions}
	v.ID = vc.ID + ":" + c.Key
	if prior, ok := verificationFind(rows, "verification_permission_grants", v.ID); ok {
		old := verificationDecode[VerificationPermissionGrant](prior)
		old.CreatedAt = time.Time{}
		if !verificationEqual(old, v) {
			return ErrVerificationConflict
		}
		out.Receipt.ID = old.ID
		return nil
	}
	v.CreatedAt = now
	out.Rows = append(out.Rows, verificationRow("verification_permission_grants", v.ID, vc.TaskID, vc.ID, "", v.ID, "granted", v))
	out.Receipt.ID = v.ID
	return nil
}

func verificationLiveGrant(rows []VerificationRow, vc VerificationContext, id string, subject core.VerificationSubject) (VerificationPermissionGrant, error) {
	r, ok := verificationFind(rows, "verification_permission_grants", id)
	if !ok || r.ContextID != vc.ID || r.TaskID != vc.TaskID {
		return VerificationPermissionGrant{}, fmt.Errorf("%w: missing work-order permission grant", ErrVerificationAccess)
	}
	g := verificationDecode[VerificationPermissionGrant](r)
	contract, err := verificationContract(rows, vc.ID, subject)
	if err != nil {
		return g, err
	}
	if g.WorkspaceID != vc.WorkspaceID || g.WorkOrderID != vc.WorkOrderID || g.WorkOrderAttemptID != vc.WorkOrderAttemptID || !verificationEqual(g.Revisions, vc.Revisions) || !verificationEqual(g.Subject, subject) || g.ContractDigest != verificationHash(verificationJSON(contract)) {
		return g, ErrVerificationAccess
	}
	for _, r := range rows {
		if r.Table == "verification_permission_revocations" && verificationDecode[VerificationPermissionRevocation](r).GrantID == id {
			return g, fmt.Errorf("%w: permission grant revoked", ErrVerificationAccess)
		}
	}
	return g, nil
}
