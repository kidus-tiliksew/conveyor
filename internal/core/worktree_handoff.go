package core

// WorktreeIdentity is bounded preservation provenance, never a credential
// (component-git-delivery CP-1; component-work-orders HO-2).
type WorktreeIdentity struct {
	Workspace         string `json:"workspace"`
	TaskID            string `json:"task_id"`
	Repository        string `json:"repository"`
	Branch            string `json:"branch"`
	WorkOrderID       string `json:"work_order_id"`
	AttemptID         string `json:"attempt_id"`
	SessionID         string `json:"session_id"`
	Generation        string `json:"generation"`
	ClaimEventID      int64  `json:"claim_event_id"`
	ReleaseEventID    int64  `json:"release_event_id,omitempty"`
	CheckpointEventID int64  `json:"checkpoint_event_id,omitempty"`
	CommitSHA         string `json:"commit_sha,omitempty"`
	Reason            string `json:"reason,omitempty"`
}

// WorktreeHandoffRequest admits a writer, checks preservation authority, or
// acknowledges an immutable checkpoint under that writer's authority.
type WorktreeHandoffRequest struct {
	SessionID      string                      `json:"session_id"`
	Generation     string                      `json:"generation"`
	Action         string                      `json:"action"`
	Producer       *WorktreeIdentity           `json:"producer,omitempty"`
	CommitSHA      string                      `json:"commit_sha,omitempty"`
	Transcript     *WorkOrderAttemptTranscript `json:"transcript,omitempty"`
	OriginalReason string                      `json:"original_reason,omitempty"`
}
type WorktreeHandoff struct {
	Writer      WorktreeIdentity  `json:"writer"`
	Predecessor *WorktreeIdentity `json:"predecessor,omitempty"`
	Created     bool              `json:"created,omitempty"`
}
