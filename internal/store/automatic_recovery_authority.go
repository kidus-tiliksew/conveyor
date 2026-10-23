package store

import (
	"encoding/json"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// AuthorityConfirmedRecoveryReason is the event reason of the answered
// authority-conflict recovery (feature-bounded-automatic-recovery AR-3a).
const AuthorityConfirmedRecoveryReason = "authority_confirmed"

// AuthorityDocument is one cited document's authority snapshot, read under the
// backend's document lock. Found is false for a missing or archived document.
// PendingVersions counts unconfirmed System Design versions that are not
// dismissed, or unconfirmed requirement versions above the current version
// that are not retired, from every origin (req-260820-394cac AC-4.2).
type AuthorityDocument struct {
	ID              string
	Kind            string
	Found           bool
	CurrentVersion  int
	PendingVersions int
}

// AnsweredAuthorityCitation records one citation and the confirmed version
// that answered it in the recovery event.
type AnsweredAuthorityCitation struct {
	DocumentID       string `json:"document_id"`
	CitedVersion     int    `json:"cited_version"`
	ConfirmedVersion int    `json:"confirmed_version"`
}

// AnsweredAuthorityRecovery is the pure AR-3a decision for one order.
type AnsweredAuthorityRecovery struct {
	Recover   bool
	RequestID string
	Direction string
	Citations []AnsweredAuthorityCitation
}

// AnsweredAuthorityCandidate holds every AR-3a condition that needs no
// document read. Callers fill order.RecoveryRefusal from
// WorkOrderRecoveryRefusal first (req-bounded-automatic-recovery AC-2.2).
func AnsweredAuthorityCandidate(task core.Task, order core.WorkOrder) bool {
	if task.State != core.TaskQueued && task.State != core.TaskRunning {
		return false
	}
	if order.Stage != core.StageImplement || order.State != core.WorkOrderQueued {
		return false
	}
	if order.LastAttemptOutcome != core.WorkOrderOutcomeReleased ||
		order.LastFailureMessage != core.WorkOrderReleaseReasonOperatorCheckpointReached ||
		!order.RetrySuppressed || order.RetrySuppressionReason != "" || order.LastAttemptID == "" {
		return false
	}
	checkpoint := order.Checkpoint
	if checkpoint == nil || checkpoint.Class != core.WorkOrderCheckpointClassAuthorityConflict ||
		strings.TrimSpace(checkpoint.DecisionRequest) == "" || len(checkpoint.Citations) == 0 || checkpoint.Verification != nil {
		return false
	}
	return order.RecoveryRefusal == ""
}

// AnsweredAuthorityDocumentIDs lists each cited document once, in
// first-citation order.
func AnsweredAuthorityDocumentIDs(order core.WorkOrder) []string {
	if order.Checkpoint == nil {
		return nil
	}
	return nextaction.CitedDocumentIDs(order.Checkpoint.Citations)
}

// DecideAnsweredAuthorityRecovery recovers only when every citation's document
// is found with a confirmed version newer than that citation's cited version
// and no cited document has a pending version. Anything else stays released
// for the operator (req-260820-394cac AC-2.6, AC-2.7; DEC-73).
func DecideAnsweredAuthorityRecovery(task core.Task, order core.WorkOrder, documents map[string]AuthorityDocument) AnsweredAuthorityRecovery {
	if !AnsweredAuthorityCandidate(task, order) {
		return AnsweredAuthorityRecovery{}
	}
	citations := make([]AnsweredAuthorityCitation, 0, len(order.Checkpoint.Citations))
	for _, citation := range order.Checkpoint.Citations {
		document, ok := documents[citation.DocumentID]
		if !ok || !document.Found || document.PendingVersions > 0 || document.CurrentVersion <= citation.CitedVersion {
			return AnsweredAuthorityRecovery{}
		}
		citations = append(citations, AnsweredAuthorityCitation{DocumentID: citation.DocumentID, CitedVersion: citation.CitedVersion, ConfirmedVersion: document.CurrentVersion})
	}
	ids := AnsweredAuthorityDocumentIDs(order)
	versions := make(map[string]int, len(ids))
	for _, id := range ids {
		versions[id] = documents[id].CurrentVersion
	}
	return AnsweredAuthorityRecovery{
		Recover:   true,
		RequestID: AuthorityConfirmedRequestID(order.ID, order.LastAttemptID),
		Direction: nextaction.ConfirmedAuthorityDirection(ids, versions),
		Citations: citations,
	}
}

// AuthorityConfirmedRequestID keys the recovery on the release it answers, so
// a later checkpoint release of the same order recovers independently.
func AuthorityConfirmedRequestID(orderID, attemptID string) string {
	return "authority-confirmed/" + orderID + "/" + attemptID
}

// AuthorityConfirmedEventPayload is the work_order.recovered payload of the
// act (feature-bounded-automatic-recovery AR-3a). The ledger is not named:
// the act does not count against the bound (AC-2.6).
func AuthorityConfirmedEventPayload(workspace string, order core.WorkOrder, decision AnsweredAuthorityRecovery, releaseEventID int64) map[string]any {
	return map[string]any{
		"workspace_id": workspace, "work_order_id": order.ID, "request_id": decision.RequestID,
		"prior_state": core.WorkOrderQueued, "new_state": core.WorkOrderQueued, "prior_outcome": order.LastAttemptOutcome,
		"command": "", "reason": AuthorityConfirmedRecoveryReason, "automatic": true, "direction": decision.Direction,
		"release_attempt_id": order.LastAttemptID, "release_event_id": releaseEventID, "citations": decision.Citations,
	}
}

// ReleaseEventAttemptID reads the attempt a work_order.released payload names.
func ReleaseEventAttemptID(event core.Event) string {
	var payload struct {
		AttemptID string `json:"attempt_id"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return ""
	}
	return payload.AttemptID
}
