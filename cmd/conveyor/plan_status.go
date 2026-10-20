package main

// Draft status projection (feature-local-planning-drafts LP-4;
// req-local-planning AC-2.5). Status reads the draft and its state and
// changes nothing.

import "fmt"

// Item approval states printed by `conveyor plan status`.
const (
	PlanItemApproved = "approved"
	PlanItemPending  = "pending"
	PlanItemChanged  = "changed since approval"
)

// PlanItemStatus is one status row.
type PlanItemStatus struct {
	ItemID string
	Layer  string
	// Digest is empty when the item cannot be digested; Err says why.
	Digest string
	State  string
	Push   *PlanPushRecord
	Err    error
}

// PlanStatus projects every item's current digest against the recorded
// approval: approved when they match, changed since approval when an
// approval exists for another digest, and pending otherwise.
func PlanStatus(draft *PlanDraft) []PlanItemStatus {
	rows := make([]PlanItemStatus, 0, len(draft.Items))
	for _, item := range draft.Items {
		row := PlanItemStatus{ItemID: item.ID, Layer: item.Layer}
		if record, ok := draft.State.Pushes[item.ID]; ok {
			record := record
			row.Push = &record
		}
		digest, err := PlanItemDigest(item)
		approved, hasApproval := draft.State.Approvals[item.ID]
		switch {
		case err != nil:
			row.Err = err
		case !hasApproval:
			row.State = PlanItemPending
		case approved == digest:
			row.State = PlanItemApproved
		default:
			row.State = PlanItemChanged
		}
		row.Digest = digest
		rows = append(rows, row)
	}
	return rows
}

// pushed renders the push record column: the server-assigned ID and version
// once posted, `intent` for an unconfirmed post attempt, and `-` otherwise.
func (s PlanItemStatus) pushed() string {
	switch {
	case s.Push == nil:
		return "-"
	case s.Push.ServerID != "" && s.Push.Version > 0:
		return fmt.Sprintf("%s v%d", s.Push.ServerID, s.Push.Version)
	case s.Push.ServerID != "":
		return s.Push.ServerID
	case s.Push.Phase != "":
		return s.Push.Phase
	default:
		return "-"
	}
}
